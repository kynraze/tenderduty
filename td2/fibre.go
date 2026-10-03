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
	"google.golang.org/protobuf/encoding/protowire"
)

// fibreState is what the last check found about the validator's Fibre provider.
type fibreState struct {
	Registered    bool
	Host          string
	Reachable     bool
	KeyKnown      bool // without the consensus key the identity can't be checked
	IdentityOK    bool
	IdentityError string
	DownSince     time.Time // when the host was first seen unreachable, zero while it answers
	DownReason    string    // why the last handshake failed
	NoKeyLogged   bool
}

// fibreDialReason is a failed handshake in the operator's terms, the cause decides what to look at.
func fibreDialReason(err error) string {
	var netErr net.Error
	var dnsErr *net.DNSError
	switch {
	// before the timeout case, a lookup that times out is a DNS problem, not the host
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

// fibreCertOID is the extension Fibre puts its signed TLS key binding in.
var fibreCertOID = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 66463, 1, 1}

// fibreClockSkew is the slack Fibre's own verifier allows on the certificate's validity.
const fibreClockSkew = 5 * time.Minute

// parseFibreProvider reads QueryFibreProviderInfoResponse{info: {host: 1}, found: 2}. Protobuf leaves out false, so a
// validator that isn't registered has no found field in the bytes and it reads as false.
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

// What Fibre assigns, from celestia-app fibre/protocol_params.go DefaultProtocolParams (protocol v0, these are not chain
// params). The minimum of 148 rows is derived there from MaxValidatorCount 100 and 100 bits of unique decoding security.
const (
	fibreOriginalRows = 4096
	fibreMinRows      = 148
	// the liveness threshold 1/3, a validator with a third of the stake gets every row
	fibreLivenessNum = 1
	fibreLivenessDen = 3
)

// fibreRows is how many of the 4096 rows Fibre assigns a validator, the way celestia-app's Set.AssignedRows does:
// ceil(4096 * power * 3 / total), at least 148 and at most 4096. minimum is that the 148 raised it. A validator with no
// power, or outside the active set, is assigned nothing.
func fibreRows(power, total int64) (rows int, minimum bool) {
	if power <= 0 || total <= 0 {
		return 0, false
	}
	// a share over 100% can't be, and would not fit the quotient below
	power = min(power, total)
	hi, lo := bits.Mul64(uint64(power), fibreOriginalRows*fibreLivenessDen)
	quo, rem := bits.Div64(hi, lo, uint64(total)*fibreLivenessNum)
	if rem != 0 {
		quo++
	}
	rows = int(quo)
	if rows < fibreMinRows {
		rows, minimum = fibreMinRows, true
	}
	return min(rows, fibreOriginalRows), minimum
}

// fibreBudget is the most data Fibre keeps for rows of the 4096, fullStake * rows / 4096 as Fibre's deriveBudget works it
// out. The product can pass 64 bits, so it is done in 128.
func fibreBudget(fullStake int64, rows int) int64 {
	if fullStake <= 0 || rows <= 0 {
		return 0
	}
	hi, lo := bits.Mul64(uint64(fullStake), uint64(rows))
	quo, _ := bits.Div64(hi, lo, fibreOriginalRows)
	return int64(quo)
}

// parseFibreFullStake reads QueryParamsResponse{params: {full_stake_storage_budget: 5}}, 0 when it isn't in the bytes.
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

// refreshFibreParams reads the budget Fibre derives from. A failure keeps the last value and is logged once, until a
// read works again: it says nothing about Fibre itself, so it never touches an alarm.
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
		l(cc.ChainId, "has no x/fibre, not showing the storage budget the chain gives the validator")
	default:
		l(cc.ChainId, "could not read the Fibre params, keeping the last storage budget:", err)
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

// fibreCertificate completes a TLS handshake with the host and returns the certificate it presents. Fibre serves gRPC
// over TLS 1.3 only, with no plaintext port and no health endpoint, so a finished handshake is what reachable means.
func fibreCertificate(ctx context.Context, host string) (*x509.Certificate, error) {
	dialer := &tls.Dialer{
		NetDialer: &net.Dialer{Timeout: 10 * time.Second},
		Config: &tls.Config{
			NextProtos: []string{"h2"},
			MinVersion: tls.VersionTLS13,
			// Fibre's certificate is self-signed by design, so it can't be verified the usual way. verifyFibreIdentity checks it
			// against the consensus key from the chain instead.
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

// verifyFibreIdentity does what Fibre's own verifier does (celestia-app fibre/internal/tlsid): the certificate carries
// a payload binding its TLS key to a validity window, signed with the validator's consensus key.
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

// refreshFibre checks the validator's Fibre registration and host. The host is dialed from wherever tenderduty runs, so
// what it proves is what the outside world can reach. A chain without x/valaddr is logged once and not asked again.
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
	// a flaky node must not raise or clear Fibre alarms
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
				// a host that doesn't answer proves nothing about its identity, the last check stands until it answers
				state.KeyKnown, state.IdentityOK, state.IdentityError = previous.KeyKnown, previous.IdentityOK, previous.IdentityError
			}
		} else {
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
}

const (
	fibreNotRegisteredPrefix = "Validator is not a registered Fibre provider on "
	fibreHostPrefix          = "Fibre at "
	fibreDownSuffix          = " is unreachable on "
	fibreIdentityInfix       = " does not prove it belongs to this validator on "
)

// the messages are the alarms' keys, so the stored identity error is not in them
func fibreNotRegisteredMessage(chainID string) string {
	return fibreNotRegisteredPrefix + chainID
}

func fibreDownMessage(host, chainID string) string {
	return fibreHostPrefix + host + fibreDownSuffix + chainID
}

func fibreIdentityMessage(host, chainID string) string {
	return fibreHostPrefix + host + fibreIdentityInfix + chainID
}

func isFibreMessage(message string) bool {
	return strings.HasPrefix(message, fibreNotRegisteredPrefix) ||
		strings.HasPrefix(message, fibreHostPrefix) && (strings.Contains(message, fibreDownSuffix) || strings.Contains(message, fibreIdentityInfix))
}

func isFibreIdentity(message string) bool {
	return strings.HasPrefix(message, fibreHostPrefix) && strings.Contains(message, fibreIdentityInfix)
}

// fibreAlarmId is the alarm's id, from its message so alarms restored from a saved state resolve with the same one. The
// host is in the two host ids: the new host's alarm is raised before the old one is resolved and both go out as the
// pagerduty dedup key, so sharing one would close the incident just raised.
func fibreAlarmId(valInfo *ValInfo, message string) string {
	switch {
	case strings.HasPrefix(message, fibreNotRegisteredPrefix):
		return valInfo.Valcons + "fibrenotregistered"
	case isFibreIdentity(message):
		return valInfo.Valcons + "fibreidentity" + fibreMessageHost(message)
	}
	return valInfo.Valcons + "fibredown" + fibreMessageHost(message)
}

// fibreMessageHost is the host a down or identity message is about.
func fibreMessageHost(message string) string {
	host := strings.TrimPrefix(message, fibreHostPrefix)
	if isFibreIdentity(message) {
		host, _, _ = strings.Cut(host, fibreIdentityInfix)
		return host
	}
	host, _, _ = strings.Cut(host, fibreDownSuffix)
	return host
}

// fibreResolution is why a Fibre alarm cleared: registered, back up, verified, or the host was replaced or dropped
// from the registry, which is not the host getting better.
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
	case !isFibreIdentity(message):
		return resolution{"✅", "Fibre up", fmt.Sprintf("Fibre at %s answers again on %s", host, cc.ChainId)}
	case state.KeyKnown && state.IdentityOK:
		return resolution{"✅", "Fibre verified", fmt.Sprintf("Fibre at %s proves it belongs to %s on %s", host, moniker, cc.ChainId)}
	}
	return resolution{"ℹ️", "Not checked", fmt.Sprintf("Fibre at %s is no longer checked, the validator's consensus key isn't known", host)}
}

// fibreLabel is the header of a Fibre notification.
func fibreLabel(msg *alertMsg) (icon, state string) {
	switch {
	case strings.HasPrefix(msg.message, fibreNotRegisteredPrefix):
		if msg.resolved {
			return "✅", "Fibre registered"
		}
		return "⚠️", "Fibre not registered"
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

// fibreDetail is why the chain's Fibre host is down or failed the identity check, for the second line of that alert.
func fibreDetail(chain, message string) string {
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

// fibreAlarms keeps the Fibre alarms the way signalAlarms does: the wanted ones are worked out each time and compared
// with the active ones.
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
		// an alarm already raised stays while the host doesn't answer: the grace period is for raising one, and the
		// state behind it isn't saved, so after a restart the host looks like it only just went down
		down, identity := fibreDownMessage(state.Host, cc.ChainId), fibreIdentityMessage(state.Host, cc.ChainId)
		if !state.Reachable && (active[down] || !state.DownSince.IsZero() && time.Since(state.DownSince) >= time.Duration(td.NodeDownMin)*time.Minute) {
			want(down)
		}
		// refreshFibre carries the last identity check over while the host is down, a restart loses it
		if state.KeyKnown && !state.IdentityOK || !state.Reachable && active[identity] {
			want(identity)
		}
	}
	// what cleared goes out before what is new, so "registered" reads before the problem found on the new host. The
	// ids differ by host, so no new incident shares a dedup key with one being resolved.
	for message := range active {
		if _, ok := wanted[message]; !ok && isFibreMessage(message) {
			resolve(cc.name, message, cc.fibreResolution(message, state, valInfo.Moniker))
			id := fibreAlarmId(valInfo, message)
			td.alert(cc.name, message, "info", true, &id)
		}
	}
	for message, id := range wanted {
		if !active[message] || reconcile {
			id := id
			td.alert(cc.name, message, cc.Alerts.FibrePriority, false, &id)
		}
	}
}
