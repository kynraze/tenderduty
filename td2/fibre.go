package tenderduty

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/asn1"
	"errors"
	"fmt"
	"math"
	"math/bits"
	"net"
	"strings"
	"syscall"
	"time"

	rpchttp "github.com/tendermint/tendermint/rpc/client/http"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
)

type fibreState struct {
	Registered    bool
	Host          string
	Reachable     bool
	KeyKnown      bool // the identity can't be checked without the consensus key
	IdentityOK    bool
	IdentityError string
	DownSince     time.Time // zero while the host answers
	DownReason    string
	NoKeyLogged   bool
	HealthKnown   bool
	Ready         bool
	NotReadySince time.Time // zero while ready or unknown
}

func fibreDialReason(err error) string {
	var netErr net.Error
	var dnsErr *net.DNSError
	switch {
	// classify lookup timeouts as DNS errors
	case errors.As(err, &dnsErr) && dnsErr.IsNotFound:
		return "DNS lookup failed, " + dnsErr.Name + " does not resolve"
	case errors.As(err, &dnsErr):
		return "DNS lookup failed for " + dnsErr.Name + ": " + dnsErr.Err
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused, Fibre is not listening on that port"
	case errors.As(err, &netErr) && netErr.Timeout():
		return "timed out, the host or a firewall is not answering"
	case strings.Contains(err.Error(), "tls:"):
		return "TLS handshake failed, something other than Fibre may be on that port: " + err.Error()
	}
	return err.Error()
}

// Extension carrying Fibre's signed TLS key binding.
var fibreCertOID = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 66463, 1, 1}

// Slack Fibre's own verifier allows on certificate validity.
const fibreClockSkew = 5 * time.Minute

// QueryFibreProviderInfoResponse{info: {host: 1}, found: 2}. Protobuf omits false, so an unregistered validator has no found field.
func parseFibreProvider(b []byte) (host string, found bool, err error) {
	err = eachField(b, func(num protowire.Number, typ protowire.Type, value []byte, varint uint64) error {
		switch {
		case num == 1 && typ == protowire.BytesType:
			return eachField(value, func(num protowire.Number, typ protowire.Type, value []byte, _ uint64) error {
				if num == 1 && typ == protowire.BytesType {
					host = string(value)
				}
				return nil
			})
		case num == 2 && typ == protowire.VarintType:
			found = varint != 0
		}
		return nil
	})
	return
}

// From celestia-app fibre/protocol_params.go DefaultProtocolParams (protocol v0, not chain params).
// The 148 minimum derives from MaxValidatorCount 100 and 100 bits of unique decoding security.
const (
	fibreOriginalRows = 4096
	fibreMinRows      = 148
	// liveness threshold 1/3: a third of the stake gets every row
	fibreLivenessNum = 1
	fibreLivenessDen = 3
)

// Mirrors celestia-app's AssignedRows: ceil(4096 * power * 3 / total), clamped to 148..4096.
// minimum reports whether the floor applied; zero power gets no rows.
func fibreRows(power, total int64) (rows int, minimum bool) {
	if power <= 0 || total <= 0 {
		return 0, false
	}
	// a share over 100% would overflow the quotient below
	power = min(power, total)
	hi, lo := bits.Mul64(uint64(power), fibreOriginalRows*fibreLivenessDen)
	quo, rem := bits.Div64(hi, lo, uint64(total)*fibreLivenessNum)
	if rem != 0 {
		quo++
	}
	// #nosec G115 -- quotient clamped to fibreOriginalRows
	rows = int(min(quo, fibreOriginalRows))
	if rows < fibreMinRows {
		rows, minimum = fibreMinRows, true
	}
	return min(rows, fibreOriginalRows), minimum
}

// Fibre derives budget as fullStake * rows / 4096; the product needs 128-bit math.
func fibreBudget(fullStake int64, rows int) int64 {
	if fullStake <= 0 || rows <= 0 {
		return 0
	}
	rows = min(rows, fibreOriginalRows)
	hi, lo := bits.Mul64(uint64(fullStake), uint64(rows))
	quo, _ := bits.Div64(hi, lo, fibreOriginalRows)
	// #nosec G115 -- rows bounded, quotient at most fullStake
	return int64(quo)
}

// QueryParamsResponse{params: {full_stake_storage_budget: 5}}; 0 when absent.
func parseFibreFullStake(b []byte) (int64, error) {
	var stake uint64
	err := eachField(b, func(num protowire.Number, typ protowire.Type, value []byte, _ uint64) error {
		if num != 1 || typ != protowire.BytesType {
			return nil
		}
		return eachField(value, func(num protowire.Number, typ protowire.Type, _ []byte, varint uint64) error {
			if num == 5 && typ == protowire.VarintType {
				stake = varint
			}
			return nil
		})
	})
	if err == nil && stake > math.MaxInt64 {
		err = fmt.Errorf("full_stake_storage_budget %d is out of range", stake)
	}
	return int64(stake), err
}

// Failed reads keep the last budget and log once per failure streak.
func (cc *ChainConfig) refreshFibreParams(ctx context.Context, client *rpchttp.HTTP) {
	value, err := abciQuery(ctx, client, "/celestia.fibre.v1.Query/Params", nil)
	var stake int64
	if err == nil {
		if stake, err = parseFibreFullStake(value); err == nil && stake <= 0 {
			err = errors.New("full_stake_storage_budget is not set")
		}
	}
	cc.stateMux.Lock()
	logged := cc.fibreParamsFailing
	cc.fibreParamsFailing = err != nil
	if err == nil {
		cc.fibreFullStake = stake
	}
	cc.stateMux.Unlock()
	switch {
	case err == nil || logged:
	case moduleMissing(err):
		l(cc.ChainId, "has no x/fibre, not showing the shard budget the chain gives the validator")
	default:
		l(cc.ChainId, "could not read the Fibre params, keeping the last shard budget:", err)
	}
}

func getFibreProvider(ctx context.Context, client *rpchttp.HTTP, valcons string) (host string, found bool, err error) {
	request := protowire.AppendString(protowire.AppendTag(nil, 1, protowire.BytesType), valcons)
	value, err := abciQuery(ctx, client, "/celestia.valaddr.v1.Query/FibreProviderInfo", request)
	if err != nil {
		return "", false, err
	}
	return parseFibreProvider(value)
}

// Fibre uses TLS 1.3 without a health endpoint; reachability means a completed handshake.
func fibreCertificate(ctx context.Context, host string) (*x509.Certificate, error) {
	dialer := &tls.Dialer{
		NetDialer: &net.Dialer{Timeout: 10 * time.Second},
		Config: &tls.Config{
			NextProtos: []string{"h2"},
			MinVersion: tls.VersionTLS13,
			// self-signed by design; verifyFibreIdentity checks it against the consensus key
			InsecureSkipVerify: true, //#nosec G402 -- identity is verified against the validator's consensus key
		},
	}
	conn, err := dialer.DialContext(ctx, "tcp", host)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	certs := conn.(*tls.Conn).ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return nil, errors.New("no certificate presented")
	}
	return certs[0], nil
}

func fibreHealth(ctx context.Context, host string) (known, ready bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, err := grpc.NewClient("passthrough:///"+host, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
		MinVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true, //#nosec G402 -- identity is verified against the validator's consensus key
	})))
	if err != nil {
		return false, false, err
	}
	defer func() { _ = conn.Close() }()
	response, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{Service: ""})
	if status.Code(err) == codes.Unimplemented {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	return true, response.Status == healthpb.HealthCheckResponse_SERVING, nil
}

// As Fibre's verifier (celestia-app fibre/internal/tlsid): a payload binding the TLS key to a validity window, signed by the consensus key.
func verifyFibreIdentity(cert *x509.Certificate, consKey []byte, chainID string, now time.Time) error {
	var value []byte
	for _, ext := range cert.Extensions {
		if ext.Id.Equal(fibreCertOID) {
			value = ext.Value
			break
		}
	}
	if value == nil {
		return errors.New("the certificate is not a Fibre certificate")
	}
	var binding struct{ Payload, Signature []byte }
	if rest, err := asn1.Unmarshal(value, &binding); err != nil || len(rest) != 0 {
		return errors.New("the Fibre certificate extension is malformed")
	}
	var payload struct {
		Version, NotBefore, NotAfter int64
		TLSPubKey                    []byte
	}
	if rest, err := asn1.Unmarshal(binding.Payload, &payload); err != nil || len(rest) != 0 {
		return errors.New("the Fibre certificate payload is malformed")
	}
	if payload.Version != 1 {
		return fmt.Errorf("the Fibre certificate has version %d, expected 1", payload.Version)
	}
	if len(consKey) != ed25519.PublicKeySize {
		return errors.New("the validator's consensus key is not an ed25519 key")
	}
	// celestia-core's RawBytesMessageSignBytes: SignRawBytesRequest{chain_id: 1, raw_bytes: 2, unique_id: 3}
	raw := append([]byte("celestia-fibre-tls:"), binding.Payload...)
	request := protowire.AppendString(protowire.AppendTag(nil, 1, protowire.BytesType), chainID)
	request = protowire.AppendBytes(protowire.AppendTag(request, 2, protowire.BytesType), raw)
	request = protowire.AppendString(protowire.AppendTag(request, 3, protowire.BytesType), "celestia-fibre-tls-v1")
	signed := append([]byte("COMET::RAW_BYTES::SIGN"), protowire.AppendVarint(nil, uint64(len(request)))...)
	signed = append(signed, request...)
	if !ed25519.Verify(consKey, signed, binding.Signature) {
		return errors.New("the certificate is not signed by this validator's consensus key for " + chainID)
	}
	key, err := x509.MarshalPKIXPublicKey(cert.PublicKey)
	if err != nil || string(key) != string(payload.TLSPubKey) {
		return errors.New("the certificate's TLS key is not the one the validator signed")
	}
	if now.Before(time.Unix(payload.NotBefore, 0).Add(-fibreClockSkew)) || now.After(time.Unix(payload.NotAfter, 0).Add(fibreClockSkew)) {
		return errors.New("the certificate is not valid at this time")
	}
	return nil
}

// Dial from tenderduty; stop querying chains without x/valaddr after logging once.
func (cc *ChainConfig) refreshFibre(ctx context.Context, client *rpchttp.HTTP) {
	cc.stateMux.RLock()
	unsupported := cc.fibreUnsupported
	cc.stateMux.RUnlock()
	if unsupported {
		return
	}
	valInfo, _ := cc.validatorState()
	if valInfo.Valcons == "" {
		return
	}
	host, found, err := getFibreProvider(ctx, client, valInfo.Valcons)
	if moduleMissing(err) {
		cc.stateMux.Lock()
		cc.fibreUnsupported = true
		cc.stateMux.Unlock()
		l(cc.ChainId, "has no x/valaddr, not watching Fibre")
		return
	}
	// a flaky node must not raise or clear alarms
	if err != nil {
		l(cc.ChainId, "could not read the Fibre provider:", err)
		return
	}
	cc.refreshFibreParams(ctx, client)
	state := fibreState{}
	cc.stateMux.RLock()
	previous := cc.fibre
	cc.stateMux.RUnlock()
	state.NoKeyLogged = previous.NoKeyLogged
	if found && host != "" {
		state.Registered, state.Host = true, host
		if previous.Registered && previous.Host == host {
			state.HealthKnown, state.Ready, state.NotReadySince = previous.HealthKnown, previous.Ready, previous.NotReadySince
		}
		cert, dialErr := fibreCertificate(ctx, host)
		state.Reachable = dialErr == nil
		if !state.Reachable {
			state.DownReason = fibreDialReason(dialErr)
			l(cc.ChainId, "Fibre at", host, "is unreachable:", dialErr)
			state.DownSince = time.Now()
			if previous.Registered && previous.Host == host {
				if !previous.DownSince.IsZero() {
					state.DownSince = previous.DownSince
				}
				// an unreachable host proves nothing: the last identity check stands
				state.KeyKnown, state.IdentityOK, state.IdentityError = previous.KeyKnown, previous.IdentityOK, previous.IdentityError
			}
		} else {
			known, ready, healthErr := fibreHealth(ctx, host)
			if healthErr != nil {
				l(cc.ChainId, "could not read Fibre health at", host, ":", healthErr)
			} else {
				state.HealthKnown, state.Ready = known, ready
				if known && !ready {
					if state.NotReadySince.IsZero() {
						state.NotReadySince = time.Now()
					}
				} else {
					state.NotReadySince = time.Time{}
				}
			}
			state.KeyKnown = len(valInfo.ConsKey) > 0
			state.IdentityOK = true
			if state.KeyKnown {
				if err := verifyFibreIdentity(cert, valInfo.ConsKey, cc.ChainId, time.Now()); err != nil {
					state.IdentityOK, state.IdentityError = false, err.Error()
				}
			} else if !state.NoKeyLogged {
				state.NoKeyLogged = true
				l(cc.ChainId, "has no ed25519 consensus key for the validator, not checking that the Fibre host belongs to it")
			}
		}
	}
	cc.stateMux.Lock()
	cc.fibre, cc.fibreLoaded = state, true
	cc.stateMux.Unlock()
	if cc.alertSettings().FibreAlerts {
		td.fibreData().recordReach(cc.name, state)
	}
}

const (
	fibreNotRegisteredPrefix = "Validator is not a registered Fibre provider on "
	fibreHostPrefix          = "Fibre at "
	fibreDownSuffix          = " is unreachable on "
	fibreNotReadyInfix       = " is not ready on "
	fibreIdentityInfix       = " does not prove it belongs to this validator on "
)

// messages are alarm keys, so the identity error is not in them
func fibreNotRegisteredMessage(chainID string) string {
	return fibreNotRegisteredPrefix + chainID
}

func fibreDownMessage(host, chainID string) string {
	return fibreHostPrefix + host + fibreDownSuffix + chainID
}

func fibreIdentityMessage(host, chainID string) string {
	return fibreHostPrefix + host + fibreIdentityInfix + chainID
}

func fibreNotReadyMessage(host, chainID string) string {
	return fibreHostPrefix + host + fibreNotReadyInfix + chainID
}

func isFibreNotReady(message string) bool {
	return strings.HasPrefix(message, fibreHostPrefix) && strings.Contains(message, fibreNotReadyInfix)
}

func isFibreMessage(message string) bool {
	return strings.HasPrefix(message, fibreNotRegisteredPrefix) ||
		strings.HasPrefix(message, fibreHostPrefix) && (strings.Contains(message, fibreDownSuffix) || strings.Contains(message, fibreIdentityInfix) || isFibreNotReady(message))
}

func isFibreIdentity(message string) bool {
	return strings.HasPrefix(message, fibreHostPrefix) && strings.Contains(message, fibreIdentityInfix)
}

// Host-specific IDs keep restored alarms separate from replacement hosts.
func fibreAlarmId(valInfo *ValInfo, message string) string {
	switch {
	case strings.HasPrefix(message, fibreNotRegisteredPrefix):
		return valInfo.Valcons + "fibrenotregistered"
	case isFibreNotReady(message):
		return valInfo.Valcons + "fibrenotready" + fibreMessageHost(message)
	case isFibreIdentity(message):
		return valInfo.Valcons + "fibreidentity" + fibreMessageHost(message)
	}
	return valInfo.Valcons + "fibredown" + fibreMessageHost(message)
}

func fibreMessageHost(message string) string {
	host := strings.TrimPrefix(message, fibreHostPrefix)
	if isFibreNotReady(message) {
		host, _, _ = strings.Cut(host, fibreNotReadyInfix)
		return host
	}
	if isFibreIdentity(message) {
		host, _, _ = strings.Cut(host, fibreIdentityInfix)
		return host
	}
	host, _, _ = strings.Cut(host, fibreDownSuffix)
	return host
}

func (cc *ChainConfig) fibreResolution(message string, state fibreState, moniker string) resolution {
	if strings.HasPrefix(message, fibreNotRegisteredPrefix) {
		return resolution{"✅", "Fibre registered", fmt.Sprintf("%s is a registered Fibre provider on %s at %s", moniker, cc.ChainId, state.Host)}
	}
	host := fibreMessageHost(message)
	switch {
	case !state.Registered:
		return resolution{"ℹ️", "No longer registered", fmt.Sprintf("Fibre at %s is no longer registered on %s", host, cc.ChainId)}
	case host != state.Host:
		return resolution{"🔁", "Host changed", fmt.Sprintf("Fibre moved from %s to %s on %s", host, state.Host, cc.ChainId)}
	case isFibreNotReady(message):
		return resolution{"✅", "Fibre ready", fmt.Sprintf("Fibre at %s is ready again on %s", host, cc.ChainId)}
	case !isFibreIdentity(message):
		return resolution{"✅", "Fibre up", fmt.Sprintf("Fibre at %s answers again on %s", host, cc.ChainId)}
	case state.KeyKnown && state.IdentityOK:
		return resolution{"✅", "Fibre verified", fmt.Sprintf("Fibre at %s proves it belongs to %s on %s", host, moniker, cc.ChainId)}
	}
	return resolution{"ℹ️", "Not checked", fmt.Sprintf("Fibre at %s is no longer checked, the validator's consensus key isn't known", host)}
}

func fibreLabel(msg *alertMsg) (icon, state string) {
	switch {
	case strings.HasPrefix(msg.message, fibreNotRegisteredPrefix):
		if msg.resolved {
			return "✅", "Fibre registered"
		}
		return "⚠️", "Fibre not registered"
	case isFibreNotReady(msg.message):
		if msg.resolved {
			return "✅", "Fibre ready"
		}
		return "⚠️", "Fibre not ready"
	case isFibreIdentity(msg.message):
		if msg.resolved {
			return "✅", "Fibre verified"
		}
		return "⚠️", "Fibre identity"
	}
	if msg.resolved {
		return "✅", "Fibre up"
	}
	return "🚨", "Fibre down"
}

// Second line of a down or identity alert.
func fibreDetail(chain, message string) string {
	if isFibreNotReady(message) {
		return "the Fibre log names the failing check"
	}
	td.chainsMux.RLock()
	cc := td.Chains[chain]
	td.chainsMux.RUnlock()
	if cc == nil {
		return ""
	}
	cc.stateMux.RLock()
	defer cc.stateMux.RUnlock()
	if cc.fibre.Host != fibreMessageHost(message) {
		return ""
	}
	if isFibreIdentity(message) {
		return cc.fibre.IdentityError
	}
	return cc.fibre.DownReason
}

// Compare wanted alarms with active alarms.
func (cc *ChainConfig) fibreAlarms(valInfo *ValInfo, reconcile bool) {
	cc.stateMux.RLock()
	state, loaded := cc.fibre, cc.fibreLoaded
	cc.stateMux.RUnlock()
	if !loaded {
		return
	}
	active := activeMessages(cc.name, fibreHostPrefix)
	for message := range activeMessages(cc.name, fibreNotRegisteredPrefix) {
		active[message] = true
	}
	wanted := make(map[string]string)
	want := func(message string) {
		wanted[message] = fibreAlarmId(valInfo, message)
	}
	if !state.Registered {
		want(fibreNotRegisteredMessage(cc.ChainId))
	} else {
		// a raised alarm stays while the host is down: DownSince isn't saved, so a restart makes it look new
		down, identity := fibreDownMessage(state.Host, cc.ChainId), fibreIdentityMessage(state.Host, cc.ChainId)
		if !state.Reachable && (active[down] || !state.DownSince.IsZero() && time.Since(state.DownSince) >= time.Duration(td.settings().NodeDownMin)*time.Minute) {
			want(down)
		}
		notReady := fibreNotReadyMessage(state.Host, cc.ChainId)
		if state.Reachable && state.HealthKnown && !state.Ready && (active[notReady] || !state.NotReadySince.IsZero() && time.Since(state.NotReadySince) >= time.Duration(td.settings().NodeDownMin)*time.Minute) || !state.Reachable && active[notReady] {
			want(notReady)
		}
		// retain the identity alarm during downtime, including after a restart
		if state.KeyKnown && !state.IdentityOK || !state.Reachable && active[identity] {
			want(identity)
		}
	}
	// resolutions go out before new alarms; the old host's alarms clear together with one message
	said := make(map[string]bool)
	for message := range active {
		if _, ok := wanted[message]; !ok && isFibreMessage(message) {
			r := cc.fibreResolution(message, state, valInfo.Moniker)
			resolve(cc.name, message, r)
			id := fibreAlarmId(valInfo, message)
			if said[r.label+"\x00"+r.text] {
				td.resolveQuietly(cc.name, message, &id)
				continue
			}
			said[r.label+"\x00"+r.text] = true
			td.alert(cc.name, message, "info", true, &id)
		}
	}
	for message, id := range wanted {
		if !active[message] || reconcile {
			id := id
			td.alert(cc.name, message, cc.alertSettings().FibreSeverity, false, &id)
		}
	}
}
