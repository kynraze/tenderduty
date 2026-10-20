package tenderduty

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"github.com/gorilla/websocket"
	dash "github.com/kynraze/tenderduty/v2/td2/dashboard"
	rpchttp "github.com/tendermint/tendermint/rpc/client/http"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// the certificate Kynraze's Fibre at 195.3.222.20:7980 presented on mocha-5, valid 2026-09-24 21:00 to 2027-09-24 21:05 UTC
const kynrazeCert = "308201bb3082016da00302010202104a2568ce0fd85a9cc1eba7634a792ec6300506032b65703019311730150603550403130e63656c65737469612d6669627265301e170d3236303932343231303030305a170d3237303932343231303530305a3019311730150603550403130e63656c65737469612d6669627265302a300506032b65700321007fa13ac3eb0add623baef45c47f133ba1188c29b5b75d6ebccdfe38bef8116a6a381ca3081c7300e0603551d0f0101ff040403020780301d0603551d250416301406082b0601050507030106082b06010505070302308195060a2b0601040184871f0101048186308183043f303d02010102046ab58f5002046c96c3fc042c302a300506032b65700321007fa13ac3eb0add623baef45c47f133ba1188c29b5b75d6ebccdfe38bef8116a60440203426b1522d8f0c9f257ac73c19a9ecda1004e7772967fd950add070f234ee1430f2370a8470cb14bc4214a1fd14c52f456d777f6101a194476732089b53e0c300506032b6570034100c9db045c136965a14d4f4fbc21ec09fce419723cff9fa85e15f22d708a2e46ed60a8290c5418e65d774abd2ba2ef1a7b024df6536bdb786a9090453beb808701"

const kynrazeKey = "j+t/8avW2mZMqf5jVSGntyAkYdJQmudK98G9Lt00RHk="

func kynraze(t *testing.T) (*x509.Certificate, []byte) {
	t.Helper()
	der, err := hex.DecodeString(kynrazeCert)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	key, err := base64.StdEncoding.DecodeString(kynrazeKey)
	if err != nil {
		t.Fatal(err)
	}
	return cert, key
}

func TestVerifyFibreIdentity(t *testing.T) {
	cert, key := kynraze(t)
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	otherKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	flipped := *cert
	flipped.Extensions = append([]pkix.Extension(nil), cert.Extensions...)
	for i, ext := range flipped.Extensions {
		if ext.Id.Equal(fibreCertOID) {
			value := append([]byte(nil), ext.Value...)
			value[len(value)-1] ^= 1 // the last byte is in the signature
			flipped.Extensions[i].Value = value
		}
	}
	plain := *cert
	plain.Extensions = nil
	for _, tc := range []struct {
		name    string
		cert    *x509.Certificate
		key     []byte
		chainID string
		now     time.Time
		wantErr string // empty is a pass
	}{
		{"valid", cert, key, "mocha-5", now, ""},
		{"another validator's key", cert, otherKey, "mocha-5", now, "not signed by this validator"},
		{"another chain", cert, key, "celestia", now, "not signed by this validator"},
		{"expired", cert, key, "mocha-5", time.Date(2027, 9, 25, 0, 0, 0, 0, time.UTC), "not valid at this time"},
		{"not yet valid", cert, key, "mocha-5", time.Date(2026, 9, 24, 20, 0, 0, 0, time.UTC), "not valid at this time"},
		{"within the clock skew", cert, key, "mocha-5", time.Date(2026, 9, 24, 20, 56, 0, 0, time.UTC), ""},
		{"flipped signature", &flipped, key, "mocha-5", now, "not signed by this validator"},
		{"no extension", &plain, key, "mocha-5", now, "not a Fibre certificate"},
		{"short key", cert, key[:31], "mocha-5", now, "not an ed25519 key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := verifyFibreIdentity(tc.cert, tc.key, tc.chainID, tc.now)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("rejected: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("got %v, want an error with %q", err, tc.wantErr)
			}
		})
	}
}

// TestParseFibreProvider: mocha omits found for unregistered validators.
func TestParseFibreProvider(t *testing.T) {
	for _, tc := range []struct {
		name  string
		hex   string
		host  string
		found bool
	}{
		{"registered", "0a130a113139352e332e3232322e32303a373938301001", "195.3.222.20:7980", true},
		{"not registered", "0a00", "", false},
		{"nothing at all", "", "", false},
	} {
		b, err := hex.DecodeString(tc.hex)
		if err != nil {
			t.Fatal(err)
		}
		host, found, err := parseFibreProvider(b)
		if err != nil || host != tc.host || found != tc.found {
			t.Errorf("%s: got %q, %v, %v", tc.name, host, found, err)
		}
	}
}

// Answer every abci_query with value, or an error code when nil.
func fibreNode(t *testing.T, value []byte, queries *int) *rpchttp.HTTP {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			ID interface{} `json:"id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		*queries++
		response := map[string]interface{}{"code": 6, "codespace": "sdk", "log": "unknown request"}
		if value != nil {
			response = map[string]interface{}{"code": 0, "value": base64.StdEncoding.EncodeToString(value)}
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"jsonrpc": "2.0", "id": request.ID, "result": map[string]interface{}{"response": response}})
	}))
	t.Cleanup(server.Close)
	client, err := rpchttp.New(server.URL, "/websocket")
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func fibreChain() *ChainConfig {
	return &ChainConfig{ChainId: "celestia", valInfo: &ValInfo{Valcons: "celestiavalcons1test"}}
}

// TestChainWithoutFibreIsAskedOnce: fibre_enabled can sit in alert_defaults for every chain
func TestChainWithoutFibreIsAskedOnce(t *testing.T) {
	queries := 0
	client := signalNode(t, "sdk", 6, &queries)
	chain := fibreChain()
	chain.refreshFibre(context.Background(), client)
	chain.refreshFibre(context.Background(), client)
	if queries != 1 || !chain.fibreUnsupported || chain.fibreLoaded {
		t.Fatalf("%d queries, unsupported %v, loaded %v", queries, chain.fibreUnsupported, chain.fibreLoaded)
	}
}

// TestFibreFailureIsRetried: a flaky node must not raise or clear alarms
func TestFibreFailureIsRetried(t *testing.T) {
	for _, tc := range []struct {
		codespace string
		code      int
	}{{"sdk", 1}, {"valaddr", 6}} {
		queries := 0
		client := signalNode(t, tc.codespace, tc.code, &queries)
		chain := fibreChain()
		chain.fibre, chain.fibreLoaded = fibreState{Registered: true, Host: "a:1", Reachable: true}, true
		chain.refreshFibre(context.Background(), client)
		chain.refreshFibre(context.Background(), client)
		if queries != 2 || chain.fibreUnsupported || !chain.fibre.Registered || !chain.fibre.Reachable {
			t.Fatalf("%s %d: %d queries, unsupported %v, state %+v", tc.codespace, tc.code, queries, chain.fibreUnsupported, chain.fibre)
		}
	}
}

// self-signed, with the extension binding its TLS key to the validator, as Fibre builds it
func fibreCertificateFor(t *testing.T, consKey ed25519.PrivateKey, chainID string, withExtension bool) tls.Certificate {
	t.Helper()
	tlsPub, tlsPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "celestia-fibre"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	if withExtension {
		tlsKey, err := x509.MarshalPKIXPublicKey(tlsPub)
		if err != nil {
			t.Fatal(err)
		}
		payload, err := asn1.Marshal(struct {
			Version, NotBefore, NotAfter int64
			TLSPubKey                    []byte
		}{1, template.NotBefore.Unix(), template.NotAfter.Unix(), tlsKey})
		if err != nil {
			t.Fatal(err)
		}
		request := protowire.AppendString(protowire.AppendTag(nil, 1, protowire.BytesType), chainID)
		request = protowire.AppendBytes(protowire.AppendTag(request, 2, protowire.BytesType), append([]byte("celestia-fibre-tls:"), payload...))
		request = protowire.AppendString(protowire.AppendTag(request, 3, protowire.BytesType), "celestia-fibre-tls-v1")
		signed := append([]byte("COMET::RAW_BYTES::SIGN"), protowire.AppendVarint(nil, uint64(len(request)))...)
		value, err := asn1.Marshal(struct{ Payload, Signature []byte }{payload, ed25519.Sign(consKey, append(signed, request...))})
		if err != nil {
			t.Fatal(err)
		}
		template.ExtraExtensions = []pkix.Extension{{Id: fibreCertOID, Value: value}}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, tlsPub, tlsPriv)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: tlsPriv}
}

// TLS 1.3 with h2 on a local port
func serveFibre(t *testing.T, cert tls.Certificate) string {
	t.Helper()
	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13, NextProtos: []string{"h2"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				_ = conn.(*tls.Conn).Handshake()
				_ = conn.Close()
			}()
		}
	}()
	return listener.Addr().String()
}

func closedPort(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	_ = listener.Close()
	return addr
}

func TestFibreHostRoundTrip(t *testing.T) {
	consPub, consPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	cert, err := fibreCertificate(ctx, serveFibre(t, fibreCertificateFor(t, consPriv, "mocha-5", true)))
	if err != nil {
		t.Fatalf("a Fibre host was not reached: %v", err)
	}
	if err := verifyFibreIdentity(cert, consPub, "mocha-5", time.Now()); err != nil {
		t.Fatalf("a Fibre host was rejected: %v", err)
	}
	if err := verifyFibreIdentity(cert, consPub, "celestia", time.Now()); err == nil {
		t.Fatal("a certificate for mocha-5 passed on celestia")
	}

	cert, err = fibreCertificate(ctx, serveFibre(t, fibreCertificateFor(t, consPriv, "mocha-5", false)))
	if err != nil {
		t.Fatalf("a plain TLS host was not reached: %v", err)
	}
	if err := verifyFibreIdentity(cert, consPub, "mocha-5", time.Now()); err == nil || !strings.Contains(err.Error(), "not a Fibre certificate") {
		t.Fatalf("a plain certificate gave %v", err)
	}

	if _, err := fibreCertificate(ctx, closedPort(t)); err == nil {
		t.Fatal("a closed port was reached")
	}
}

func fibreRegistration(host string) []byte {
	info := protowire.AppendString(protowire.AppendTag(nil, 1, protowire.BytesType), host)
	b := protowire.AppendBytes(protowire.AppendTag(nil, 1, protowire.BytesType), info)
	return protowire.AppendVarint(protowire.AppendTag(b, 2, protowire.VarintType), 1)
}

func TestRefreshFibre(t *testing.T) {
	consPub, consPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	queries := 0
	good := serveFibre(t, fibreCertificateFor(t, consPriv, "mocha-5", true))
	chain := fibreChain()
	chain.ChainId = "mocha-5"
	chain.valInfo.ConsKey = consPub
	chain.refreshFibre(context.Background(), fibreNode(t, fibreRegistration(good), &queries))
	if s := chain.fibre; !chain.fibreLoaded || !s.Registered || s.Host != good || !s.Reachable || !s.KeyKnown || !s.IdentityOK || s.IdentityError != "" || !s.DownSince.IsZero() {
		t.Fatalf("a healthy host gave %+v", s)
	}

	// another validator's key does not verify this host
	chain.valInfo.ConsKey, _, _ = ed25519.GenerateKey(rand.Reader)
	chain.refreshFibre(context.Background(), fibreNode(t, fibreRegistration(good), &queries))
	if s := chain.fibre; !s.Reachable || s.IdentityOK || s.IdentityError == "" {
		t.Fatalf("a host that is not ours gave %+v", s)
	}

	// unknown consensus key leaves identity unchecked
	chain.valInfo.ConsKey = nil
	chain.refreshFibre(context.Background(), fibreNode(t, fibreRegistration(good), &queries))
	if s := chain.fibre; !s.Reachable || s.KeyKnown || !s.IdentityOK {
		t.Fatalf("no key gave %+v", s)
	}

	// retain the first failure time; reset it when the host changes
	dead := closedPort(t)
	chain.refreshFibre(context.Background(), fibreNode(t, fibreRegistration(dead), &queries))
	first := chain.fibre.DownSince
	if s := chain.fibre; s.Reachable || first.IsZero() || s.Host != dead {
		t.Fatalf("a dead host gave %+v", s)
	}
	time.Sleep(10 * time.Millisecond)
	chain.refreshFibre(context.Background(), fibreNode(t, fibreRegistration(dead), &queries))
	if !chain.fibre.DownSince.Equal(first) {
		t.Fatalf("down since moved from %v to %v", first, chain.fibre.DownSince)
	}
	other := closedPort(t)
	time.Sleep(20 * time.Millisecond) // a new host gets a later timestamp on coarse clocks
	chain.refreshFibre(context.Background(), fibreNode(t, fibreRegistration(other), &queries))
	if !chain.fibre.DownSince.After(first) {
		t.Fatalf("a new host kept the old down since %v", chain.fibre.DownSince)
	}
	chain.refreshFibre(context.Background(), fibreNode(t, fibreRegistration(good), &queries))
	if !chain.fibre.DownSince.IsZero() {
		t.Fatalf("down since stayed after it answered: %v", chain.fibre.DownSince)
	}

	// registered with nobody: found is absent
	chain.refreshFibre(context.Background(), fibreNode(t, []byte{0x0a, 0x00}, &queries))
	if s := chain.fibre; !chain.fibreLoaded || s.Registered || s.Host != "" {
		t.Fatalf("an unregistered validator gave %+v", s)
	}
}

func TestFibreAlarms(t *testing.T) {
	const (
		host  = "195.3.222.20:7980"
		chain = "mocha-5"
	)
	notRegistered, down, identity := fibreNotRegisteredMessage(chain), fibreDownMessage(host, chain), fibreIdentityMessage(host, chain)
	healthy := fibreState{Registered: true, Host: host, Reachable: true, KeyKnown: true, IdentityOK: true}
	unregistered := fibreState{}
	downFor := func(d time.Duration) fibreState {
		return fibreState{Registered: true, Host: host, DownSince: time.Now().Add(-d)}
	}
	forged := fibreState{Registered: true, Host: host, Reachable: true, KeyKnown: true, IdentityError: "x"}
	unchecked := fibreState{Registered: true, Host: host, Reachable: true, IdentityError: "x"}
	for _, tc := range []struct {
		name         string
		before       fibreState
		after        fibreState
		active, gone []string
	}{
		{"not registered", healthy, unregistered, []string{notRegistered}, nil},
		{"registers", unregistered, healthy, nil, []string{notRegistered}},
		{"unreachable but not for long", healthy, downFor(time.Minute), nil, []string{down}},
		{"unreachable long enough", healthy, downFor(10 * time.Minute), []string{down}, nil},
		{"comes back", downFor(10 * time.Minute), healthy, nil, []string{down}},
		{"identity fails", healthy, forged, []string{identity}, nil},
		{"identity fails with no key to check", healthy, unchecked, nil, []string{identity}},
		{"identity recovers", forged, healthy, nil, []string{identity}},
		// an unreachable host retains the previous identity result and alarm
		{"goes down after failing identity", forged, downFor(10 * time.Minute), []string{down, identity}, nil},
		{"unregisters while down", downFor(10 * time.Minute), unregistered, []string{notRegistered}, []string{down}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := auditConfig(t)
			c.NodeDownMin = 5
			cc := &ChainConfig{name: "Chain", ChainId: chain, fibre: tc.before, fibreLoaded: true}
			cc.Alerts.FibreAlerts, cc.Alerts.FibreSeverity = true, "warning"
			c.Chains["Chain"] = cc
			valInfo := &ValInfo{Valcons: "cons"}
			cc.fibreAlarms(valInfo, false)
			cc.stateMux.Lock()
			cc.fibre = tc.after
			cc.stateMux.Unlock()
			cc.fibreAlarms(valInfo, false)
			for _, message := range tc.active {
				if !alarmIsActive("Chain", message) {
					t.Errorf("%q should be active", message)
				}
			}
			for _, message := range tc.gone {
				if alarmIsActive("Chain", message) {
					t.Errorf("%q should have resolved", message)
				}
			}
			cc.stateMux.Lock()
			cc.fibre = healthy
			cc.stateMux.Unlock()
			cc.fibreAlarms(valInfo, false)
			if left := activeMessages("Chain", fibreHostPrefix); len(left) != 0 {
				t.Errorf("alarms did not resolve: %v", left)
			}
			if left := activeMessages("Chain", fibreNotRegisteredPrefix); len(left) != 0 {
				t.Errorf("alarms did not resolve: %v", left)
			}
		})
	}
}

func TestFibreAlarmIds(t *testing.T) {
	valInfo := &ValInfo{Valcons: "cons"}
	seen := make(map[string]string)
	for _, message := range []string{
		fibreNotRegisteredMessage("mocha-5"),
		fibreDownMessage("1.2.3.4:7980", "mocha-5"), fibreDownMessage("5.6.7.8:7980", "mocha-5"),
		fibreIdentityMessage("1.2.3.4:7980", "mocha-5"), fibreIdentityMessage("5.6.7.8:7980", "mocha-5"),
	} {
		if !isFibreMessage(message) || isSignalMessage(message) {
			t.Errorf("%q is not recognised as a Fibre message only", message)
		}
		id := fibreAlarmId(valInfo, message)
		if other, ok := seen[id]; ok {
			t.Errorf("%q and %q share the id %s", message, other, id)
		}
		seen[id] = message
	}
	if got := fibreAlarmId(valInfo, fibreDownMessage("1.2.3.4:7980", "mocha-5")); got != "consfibredown1.2.3.4:7980" {
		t.Errorf("id is %s", got)
	}
	if isFibreMessage("Version 10 upgrade at height 12") || isFibreMessage("Validator has not signalled version 10") {
		t.Error("another module's message was taken for a Fibre one")
	}
}

// TestChangedFibreHostKeepsItsIncident: the old host resolves before the new host raises; their dedup keys must differ.
func TestChangedFibreHostKeepsItsIncident(t *testing.T) {
	for _, tc := range []struct {
		name  string
		old   func(host, chain string) string
		state fibreState
	}{
		{"unreachable", fibreDownMessage, fibreState{Registered: true, Host: "5.6.7.8:7980", DownSince: time.Now().Add(-time.Hour)}},
		{"identity", fibreIdentityMessage, fibreState{Registered: true, Host: "5.6.7.8:7980", Reachable: true, KeyKnown: true, IdentityError: "x"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := auditConfig(t)
			c.NodeDownMin = 5
			cc := &ChainConfig{name: "Chain", ChainId: "mocha-5", fibre: tc.state, fibreLoaded: true}
			cc.Alerts.FibreAlerts, cc.Alerts.FibreSeverity = true, "warning"
			c.Chains["Chain"] = cc
			valInfo := &ValInfo{Valcons: "cons"}
			alarms.AllAlarms["Chain"] = map[string]time.Time{tc.old("1.2.3.4:7980", "mocha-5"): time.Now()}
			cc.fibreAlarms(valInfo, false)

			triggered, resolved := make(map[string]string), make(map[string]string)
			for len(c.alertChan) > 0 {
				alert := <-c.alertChan
				if alert.resolved {
					resolved[alert.uniqueId] = alert.message
				} else {
					triggered[alert.uniqueId] = alert.message
				}
			}
			if _, ok := resolved[fibreAlarmId(valInfo, tc.old("1.2.3.4:7980", "mocha-5"))]; !ok || len(triggered) != 1 {
				t.Fatalf("the old host was not resolved and the new one raised: %v, %v", resolved, triggered)
			}
			for id, message := range triggered {
				if old, ok := resolved[id]; ok {
					t.Fatalf("%q and the resolved %q share the dedup key %s", message, old, id)
				}
			}
		})
	}
}

func TestFibreAlertsShowWhy(t *testing.T) {
	c := auditConfig(t)
	cc := &ChainConfig{name: "Chain", fibre: fibreState{Host: "1.2.3.4:7980", IdentityError: "the certificate is not valid at this time"}}
	c.Chains["Chain"] = cc
	message := fibreIdentityMessage("1.2.3.4:7980", "mocha-5")
	msg := &alertMsg{chain: "Chain", message: message}
	if got, want := msg.summary(), message+"\nthe certificate is not valid at this time"; got != want {
		t.Errorf("summary is %q, want %q", got, want)
	}
	msg.resolved = true
	if got := msg.summary(); got != message {
		t.Errorf("a resolved summary is %q", got)
	}

	down := fibreDownMessage("1.2.3.4:7980", "mocha-5")
	if got := (&alertMsg{chain: "Chain", message: down}).summary(); strings.Contains(got, "\n") {
		t.Errorf("an unreachable summary with no reason has a second line: %q", got)
	}
	cc.fibre.DownReason = "connection refused, Fibre is not listening on that port"
	if got, want := (&alertMsg{chain: "Chain", message: down}).summary(), down+"\n"+cc.fibre.DownReason; got != want {
		t.Errorf("summary is %q, want %q", got, want)
	}
	// use the last host's failure reason only
	if got := (&alertMsg{chain: "Chain", message: fibreDownMessage("9.9.9.9:7980", "mocha-5")}).summary(); strings.Contains(got, "\n") {
		t.Errorf("another host's alarm got this host's reason: %q", got)
	}
}

// TestRestartKeepsFibreAlarmsWhileTheHostIsDown: restored down alarms wait for reachability because DownSince is not persisted.
func TestRestartKeepsFibreAlarmsWhileTheHostIsDown(t *testing.T) {
	dead := closedPort(t)
	for _, tc := range []struct {
		name     string
		restored string
		host     string
	}{
		{"down", fibreDownMessage(dead, "mocha-5"), dead},
		{"identity", fibreIdentityMessage(dead, "mocha-5"), dead},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := auditConfig(t)
			c.NodeDownMin = 3
			queries := 0
			chain := fibreChain()
			chain.name, chain.ChainId = "Chain", "mocha-5"
			chain.Alerts.FibreAlerts, chain.Alerts.FibreSeverity = true, "warning"
			c.Chains["Chain"] = chain
			alarms.AllAlarms["Chain"] = map[string]time.Time{tc.restored: time.Now().Add(-time.Hour)}

			chain.refreshFibre(context.Background(), fibreNode(t, fibreRegistration(tc.host), &queries))
			chain.fibreAlarms(chain.valInfo, false)
			if !alarmIsActive("Chain", tc.restored) {
				t.Fatalf("%q cleared after a restart while the host is still down", tc.restored)
			}
			for len(c.alertChan) > 0 {
				if alert := <-c.alertChan; alert.resolved {
					icon, label := alert.header()
					t.Fatalf("a restart sent %s %s for %q", icon, label, alert.message)
				}
			}
		})
	}

	c := auditConfig(t)
	consPub, consPriv, _ := ed25519.GenerateKey(rand.Reader)
	good := serveFibre(t, fibreCertificateFor(t, consPriv, "mocha-5", true))
	queries := 0
	chain := fibreChain()
	chain.name, chain.ChainId, chain.valInfo.ConsKey = "Chain", "mocha-5", consPub
	chain.Alerts.FibreAlerts, chain.Alerts.FibreSeverity = true, "warning"
	c.Chains["Chain"] = chain
	alarms.AllAlarms["Chain"] = map[string]time.Time{fibreDownMessage(good, "mocha-5"): time.Now().Add(-time.Hour)}
	chain.refreshFibre(context.Background(), fibreNode(t, fibreRegistration(good), &queries))
	chain.fibreAlarms(chain.valInfo, false)
	if alarmIsActive("Chain", fibreDownMessage(good, "mocha-5")) {
		t.Fatal("a restored down alarm stayed after the host answered")
	}
}

// TestFibreHostByName: x/valaddr takes DNS names as well as IPs, and the identity check doesn't care which
func TestFibreHostByName(t *testing.T) {
	consPub, consPriv, _ := ed25519.GenerateKey(rand.Reader)
	_, port, _ := net.SplitHostPort(serveFibre(t, fibreCertificateFor(t, consPriv, "mocha-5", true)))
	cert, err := fibreCertificate(context.Background(), net.JoinHostPort("localhost", port))
	if err != nil {
		t.Fatalf("a host given by name was not reached: %v", err)
	}
	if err := verifyFibreIdentity(cert, consPub, "mocha-5", time.Now()); err != nil {
		t.Fatalf("a host given by name failed the identity check: %v", err)
	}
	// .invalid never resolves (RFC 2606)
	if _, err := fibreCertificate(context.Background(), "fibre.tenderduty.invalid:7980"); err == nil || !strings.HasPrefix(fibreDialReason(err), "DNS lookup failed") {
		t.Errorf("a name that doesn't resolve reads %q", fibreDialReason(err))
	}
}

func TestFibreDialReason(t *testing.T) {
	if _, err := fibreCertificate(context.Background(), closedPort(t)); err == nil || !strings.HasPrefix(fibreDialReason(err), "connection refused") {
		t.Errorf("a closed port reads %q", fibreDialReason(err))
	}
	// something that answers TCP but not TLS
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_, _ = conn.Write([]byte("HTTP/1.1 400 Bad Request\r\n\r\n"))
			_ = conn.Close()
		}
	}()
	if _, err := fibreCertificate(context.Background(), listener.Addr().String()); err == nil || !strings.HasPrefix(fibreDialReason(err), "TLS handshake failed") {
		t.Errorf("a plain TCP service reads %q", fibreDialReason(err))
	}
}

func TestFibreIdentityKeptWhileHostIsDown(t *testing.T) {
	c := auditConfig(t)
	c.NodeDownMin = 5
	consPub, _, _ := ed25519.GenerateKey(rand.Reader)
	_, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{fibreCertificateFor(t, otherPriv, "mocha-5", true)}, MinVersion: tls.VersionTLS13, NextProtos: []string{"h2"}})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.(*tls.Conn).Handshake()
			_ = conn.Close()
		}
	}()
	host := listener.Addr().String()
	chain := fibreChain()
	chain.name, chain.ChainId, chain.valInfo.ConsKey = "Chain", "mocha-5", consPub
	chain.Alerts.FibreAlerts, chain.Alerts.FibreSeverity = true, "warning"
	c.Chains["Chain"] = chain
	queries := 0
	node := fibreNode(t, fibreRegistration(host), &queries)

	chain.refreshFibre(context.Background(), node)
	chain.fibreAlarms(chain.valInfo, false)
	identity := fibreIdentityMessage(host, "mocha-5")
	if !alarmIsActive("Chain", identity) {
		t.Fatalf("a host signed by another validator did not raise %q: %+v", identity, chain.fibre)
	}
	for len(c.alertChan) > 0 {
		<-c.alertChan
	}

	_ = listener.Close()
	chain.refreshFibre(context.Background(), node)
	chain.fibreAlarms(chain.valInfo, false)
	if s := chain.fibre; s.Reachable || !s.KeyKnown || s.IdentityOK || s.IdentityError == "" {
		t.Fatalf("the failed check was not carried over: %+v", s)
	}
	if !alarmIsActive("Chain", identity) || alarmIsActive("Chain", fibreDownMessage(host, "mocha-5")) {
		t.Fatal("a short outage changed the alarms")
	}
	for len(c.alertChan) > 0 {
		if alert := <-c.alertChan; alert.resolved {
			icon, label := alert.header()
			t.Fatalf("a short outage sent %s %s for %q", icon, label, alert.message)
		}
	}
}

func TestReplacedFibreHostIsNotCalledUp(t *testing.T) {
	for _, tc := range []struct {
		name  string
		old   string
		after fibreState
		icon  string
		label string
	}{
		{"host changed while down", fibreDownMessage("1.2.3.4:7980", "mocha-5"), fibreState{Registered: true, Host: "5.6.7.8:7980", Reachable: true, KeyKnown: true, IdentityOK: true}, "🔁", "Host changed"},
		{"host changed while unproven", fibreIdentityMessage("1.2.3.4:7980", "mocha-5"), fibreState{Registered: true, Host: "5.6.7.8:7980", Reachable: true, KeyKnown: true, IdentityOK: true}, "🔁", "Host changed"},
		{"unregistered while down", fibreDownMessage("1.2.3.4:7980", "mocha-5"), fibreState{}, "ℹ️", "No longer registered"},
		{"came back", fibreDownMessage("1.2.3.4:7980", "mocha-5"), fibreState{Registered: true, Host: "1.2.3.4:7980", Reachable: true, KeyKnown: true, IdentityOK: true}, "✅", "Fibre up"},
		{"verified", fibreIdentityMessage("1.2.3.4:7980", "mocha-5"), fibreState{Registered: true, Host: "1.2.3.4:7980", Reachable: true, KeyKnown: true, IdentityOK: true}, "✅", "Fibre verified"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := auditConfig(t)
			cc := &ChainConfig{name: "Chain", ChainId: "mocha-5", fibre: tc.after, fibreLoaded: true}
			cc.Alerts.FibreAlerts, cc.Alerts.FibreSeverity = true, "warning"
			c.Chains["Chain"] = cc
			alarms.AllAlarms["Chain"] = map[string]time.Time{tc.old: time.Now()}
			cc.fibreAlarms(&ValInfo{Valcons: "cons"}, false)
			for len(c.alertChan) > 0 {
				alert := <-c.alertChan
				if !alert.resolved || alert.message != tc.old {
					continue
				}
				if icon, label := alert.header(); icon != tc.icon || label != tc.label {
					t.Fatalf("resolved as %s %s, want %s %s", icon, label, tc.icon, tc.label)
				}
				return
			}
			t.Fatalf("%q was not resolved", tc.old)
		})
	}
}

func TestFibreLabels(t *testing.T) {
	for _, tc := range []struct {
		message, icon, state string
		resolved             bool
	}{
		{fibreNotRegisteredMessage("mocha-5"), "⚠️", "Fibre not registered", false},
		{fibreNotRegisteredMessage("mocha-5"), "✅", "Fibre registered", true},
		{fibreDownMessage("h:1", "mocha-5"), "🚨", "Fibre down", false},
		{fibreDownMessage("h:1", "mocha-5"), "✅", "Fibre up", true},
		{fibreIdentityMessage("h:1", "mocha-5"), "⚠️", "Fibre identity", false},
		{fibreIdentityMessage("h:1", "mocha-5"), "✅", "Fibre verified", true},
	} {
		icon, state := (&alertMsg{message: tc.message, resolved: tc.resolved}).header()
		if icon != tc.icon || state != tc.state {
			t.Errorf("%q resolved %v: got %s %s", tc.message, tc.resolved, icon, state)
		}
	}
}

// A Fibre token one character off: every request gets 401, and the reply leaks no secret.
func TestOpFibreWrongTokenRejected(t *testing.T) {
	r := newFibreRig(t)
	r.cc.Alerts.FibreInstance = "fibre-host-1"
	const token = "op-fibre-token"
	r.c.FibreMetrics.Token = token
	body, err := proto.Marshal(r.feed(time.Minute).data())
	if err != nil {
		t.Fatal(err)
	}
	handler := r.c.fibreMetricsHandler()
	post := func(path, auth string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/x-protobuf")
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	for _, auth := range []string{"", "Bearer op-fibre-tokem", "Basic " + token, token, "Bearer " + token + " "} {
		if rec := post("/v1/metrics", auth); rec.Code != http.StatusUnauthorized || strings.Contains(rec.Body.String(), token) {
			t.Errorf("Authorization %q answered %d", auth, rec.Code)
		}
	}
	if rec := post("/v1/traces", "Bearer nope"); rec.Code != http.StatusUnauthorized {
		t.Errorf("traces with a wrong token answered %d", rec.Code)
	}
	if len(r.st.senders) != 0 {
		t.Fatal("a rejected export was ingested")
	}
	if rec := post("/v1/metrics", "Bearer "+token); rec.Code != http.StatusOK || len(r.st.senders) != 1 {
		t.Fatalf("the right token answered %d, senders %d", rec.Code, len(r.st.senders))
	}
}

// A typo in service.namespace must not fall back to the chain whose fibre_instance matches.
func TestOpFibreNamespaceTypoIsIgnored(t *testing.T) {
	r := newFibreRig(t)
	r.cc.ChainId, r.cc.Alerts.FibreInstance = "mocha-5", "host-a"
	other := &ChainConfig{name: "Other", ChainId: "celestia"}
	other.Alerts.FibreAlerts, other.Alerts.FibreInstance = true, "host-b"
	r.c.Chains["Other"] = other
	r.c.ingestFibreMetrics(fibreNamespaceData("mocha6", "host-a", gauge("upload_shard.occupancy_bytes", 7)))
	if r.st.snapshot("Chain").Found || r.st.snapshot("Other").Found || len(r.st.senders) != 0 {
		t.Fatalf("a namespace typo was routed: senders %d", len(r.st.senders))
	}
	if len(r.st.ignored) != 1 {
		t.Fatalf("ignored notes %d, want 1", len(r.st.ignored))
	}
}

// Two chains, two hosts: B goes silent while A keeps sending; only B's chain raises silence.
func TestOpFibreTwoChainsSilenceIsolated(t *testing.T) {
	r := newFibreRig(t)
	r.cc.ChainId, r.cc.Alerts.FibreInstance = "celestia", "fibre-host-1"
	other := &ChainConfig{name: "Other", ChainId: "mocha-5", valInfo: &ValInfo{Valcons: "valcons2", Moniker: "Other"}, fibreLoaded: true}
	other.Alerts.FibreAlerts, other.Alerts.FibreSeverity, other.Alerts.FibreInstance = true, "warning", "host-2"
	r.c.Chains["Other"] = other
	send := func(namespace, instance string) {
		r.c.ingestFibreMetrics(fibreNamespaceData(namespace, instance, gauge("upload_shard.occupancy_bytes", 7)))
	}
	drain := func() []*alertMsg {
		var sent []*alertMsg
		for len(r.c.alertChan) > 0 {
			sent = append(sent, <-r.c.alertChan)
		}
		return sent
	}
	send("celestia", "fibre-host-1")
	send("mocha-5", "host-2")
	for i := 0; i < 3; i++ {
		r.advance(time.Minute)
		send("mocha-5", "host-2")
	}
	other.fibreMetricsAlarms(other.valInfo, false)
	r.quiet(drain(), "the healthy chain")
	sent := r.alarms()
	if len(sent) != 1 || sent[0].resolved || sent[0].message != r.msg(kindSilent) {
		t.Fatalf("the silent chain: %s", describe(sent))
	}
	r.quiet(r.alarms(), "the silent chain alerts once")
}

// Fibre stops for 3 minutes (node_down_alert_minutes), raises once, then the first export clears it.
func TestOpFibreSilentThenResumes(t *testing.T) {
	r := newFibreRig(t)
	r.cc.Alerts.FibreInstance = "fibre-host-1"
	r.feed(time.Minute).send()
	r.advance(2 * time.Minute)
	r.quiet(r.alarms(), "two minutes of silence")
	r.advance(time.Minute)
	sent := r.alarms()
	if len(sent) != 1 || sent[0].resolved || sent[0].message != r.msg(kindSilent) {
		t.Fatalf("silence at 3 minutes: %s", describe(sent))
	}
	r.quiet(r.alarms(), "silence raised once")
	r.feed(time.Minute).send()
	r.cleared(r.alarms(), kindSilent, "Fibre sending", "Fibre metrics are arriving again on celestia")
}

// A Fibre restart resets counters: no fake spike, and the signing alarm stays quiet.
func TestOpFibreRestartNoFakeSpike(t *testing.T) {
	r := newFibreRig(t)
	r.cc.Alerts.FibreInstance = "fibre-host-1"
	f := r.feed(time.Minute)
	f.signFast, f.signSlow = 2000, 0
	f.send()
	f.signFast = 2001
	f.run(1)
	f.start = uint64(r.clock.now.Add(-20 * time.Second).UnixNano())
	f.signFast = 5
	f.run(1)
	if w := r.st.snapshot("Chain").Window; w.signOK != 2006 || w.signFast != 2006 || w.signFailed != 0 {
		t.Fatalf("after restart the window counted %+v", w)
	}
	r.quiet(r.alarms(), "restart with healthy signing")
}

// node_down_alert_minutes lowered by reload while Fibre is silent: no all-clear yet.
func TestOpFibreNodeDownMinChangeWhileSilent(t *testing.T) {
	r := newFibreRig(t)
	r.c.NodeDownMin = 5
	r.cc.Alerts.FibreInstance = "fibre-host-1"
	r.feed(time.Minute).send()
	r.advance(5 * time.Minute)
	sent := r.alarms()
	if len(sent) != 1 || sent[0].resolved || sent[0].message != "Fibre has sent no metrics for over 5 minutes on celestia" {
		t.Fatalf("silence at 5 minutes: %s", describe(sent))
	}
	r.c.NodeDownMin = 3
	for _, a := range r.alarms() {
		if a.resolved && fibreMetricsKind(a.message) == kindSilent {
			t.Errorf("false all-clear while still silent: %s", describe([]*alertMsg{a}))
		}
	}
}

// Bridge 30 blocks behind for 4 minutes, under the 5 minute window, then catches up: silent.
func TestOpBridgeBehindShortCatchesUpSilently(t *testing.T) {
	cc, f := newBridgeFixture(t)
	cc.lastBlockNum = 130
	bridgeTick(cc, time.Now().Add(-4*time.Minute))
	if len(activeMessages(cc.name, "Bridge ")) != 0 || cc.bridge.BehindSince.IsZero() {
		t.Fatal("lag not tracked under the window")
	}
	f.head = 130
	bridgeTick(cc, time.Now())
	if len(td.alertChan) != 0 || !cc.bridge.BehindSince.IsZero() {
		t.Fatalf("brief lag sent %d notices", len(td.alertChan))
	}
}

// bridge_enabled off: the bridge is unreachable, but nothing is sent. Status still shows it.
func TestOpBridgeAlertsOffShowsStatusOnly(t *testing.T) {
	cc, f := newBridgeFixture(t)
	cc.Alerts.BridgeAlerts = false
	f.code = http.StatusInternalServerError
	for i := 0; i < 3; i++ {
		bridgeTick(cc, time.Now())
	}
	if n := len(activeMessages(cc.name, "Bridge ")); n != 0 || len(td.alertChan) != 0 {
		t.Fatalf("%d alarms and %d notices with bridge alerts off", n, len(td.alertChan))
	}
	if text, tone := cc.bridgeStatus(); !strings.HasPrefix(text, "unreachable since ") || tone != "critical" {
		t.Fatalf("status %q %q", text, tone)
	}
	if got := cc.bridgeSummary(); got != " · bridge unreachable" {
		t.Fatalf("summary %q", got)
	}
}

// Token file rotated: the bridge rejects the old token until tenderduty reloads, then recovers.
func TestOpBridgeTokenFileRotated(t *testing.T) {
	cc, f := newBridgeFixture(t)
	file := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(file, []byte("old-token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cc.Bridge.TokenFile = file
	if fatal, problems := validateBridge(cc.name, cc); fatal || len(problems) != 0 {
		t.Fatalf("token config failed: %v", problems)
	}
	f.token = "Bearer old-token"
	bridgeTick(cc, time.Now())
	if len(activeMessages(cc.name, "Bridge ")) != 0 {
		t.Fatal("healthy bridge alerted")
	}
	if err := os.WriteFile(file, []byte("new-token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	f.code = http.StatusUnauthorized
	bridgeTick(cc, time.Now())
	token := "Bridge on mocha-5 rejected the token, check token_file"
	if !alarmIsActive(cc.name, token) {
		t.Fatal("old token accepted after rotation")
	}
	if fatal, problems := validateBridge(cc.name, cc); fatal || len(problems) != 0 {
		t.Fatalf("reload failed: %v", problems)
	}
	f.code, f.token = http.StatusOK, "Bearer new-token"
	bridgeTick(cc, time.Now())
	if alarmIsActive(cc.name, token) || len(activeMessages(cc.name, "Bridge ")) != 0 {
		t.Fatal("token alarm still active after reload")
	}
	if r, ok := storedResolution(cc.name, token); !ok || r.label != "Bridge OK" {
		t.Fatal("missing recovery after reload")
	}
}

// Two chains with one chain_id, both with a bridge.url: metrics land on one chain, never hop.
func TestOpBridgeSameChainIdNoSplit(t *testing.T) {
	cc, _ := newBridgeFixture(t)
	other := &ChainConfig{name: "Other", ChainId: "mocha-5", Bridge: BridgeConfig{URL: "http://127.0.0.1:9"}}
	other.Alerts = cc.Alerts
	td.Chains["Other"] = other
	seen := make(map[string]bool)
	for i := 0; i < 40; i++ {
		cc.bridge, other.bridge = bridgeState{}, bridgeState{}
		td.ingestFibreMetrics(bridgeExport("mocha-5", "Bridge", runtimeGauge("full_discovery_amount_of_peers", runtimePoint(4))))
		for _, chain := range []*ChainConfig{cc, other} {
			if !chain.bridge.MetricsAt.IsZero() {
				seen[chain.name] = true
			}
		}
	}
	if len(seen) > 1 {
		t.Errorf("bridge metrics for mocha-5 landed on %v", seen)
	}
}

// Mainnet and testnet bridges in one tenderduty: each export lands on its own network.
func TestOpBridgeMainnetAndTestnet(t *testing.T) {
	testnet, _ := newBridgeFixture(t)
	mainnet := &ChainConfig{name: "Celestia", ChainId: "celestia", Bridge: BridgeConfig{URL: "http://127.0.0.1:9"}}
	mainnet.Alerts = testnet.Alerts
	td.Chains["Celestia"] = mainnet
	td.ingestFibreMetrics(bridgeExport("celestia", "Bridge", runtimeGauge("full_discovery_amount_of_peers", runtimePoint(7))))
	td.ingestFibreMetrics(bridgeExport(testnet.ChainId, "Bridge", runtimeGauge("full_discovery_amount_of_peers", runtimePoint(3))))
	if mainnet.bridge.FullPeers != 7 || testnet.bridge.FullPeers != 3 {
		t.Fatalf("peers mainnet %d testnet %d, want 7 and 3", mainnet.bridge.FullPeers, testnet.bridge.FullPeers)
	}
}

// hide_logs: the dashboard status must not carry the RPC failure reason or the token.
func TestOpBridgeHideLogsDashboardNoReasonOrToken(t *testing.T) {
	cc, f := newBridgeFixture(t)
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("op-bridge-token"), 0600); err != nil {
		t.Fatal(err)
	}
	cc.Bridge.TokenFile = tokenFile
	if fatal, problems := validateBridge(cc.name, cc); fatal || len(problems) != 0 {
		t.Fatalf("token config failed: %v", problems)
	}
	f.token = "Bearer op-bridge-token"
	f.code = http.StatusInternalServerError
	for i := 0; i < 3; i++ {
		bridgeTick(cc, time.Now())
	}
	td.HideLogs = false
	shown := opStatusPayload(t, cc.dashboardStatus(), false)
	if !strings.Contains(shown, "HTTP 500") {
		t.Fatal("the unhidden status lost the reason, test is blind")
	}
	td.HideLogs = true
	hidden := opStatusPayload(t, cc.dashboardStatus(), true)
	if strings.Contains(hidden, "op-bridge-token") {
		t.Error("token in hidden dashboard status")
	}
	if strings.Contains(hidden, "HTTP 500") {
		t.Errorf("hide_logs dashboard shows the bridge failure reason: %s", hidden)
	}
}

// opStatusPayload sends one status through the real dashboard server and returns the websocket payload.
func opStatusPayload(t *testing.T, status *dash.ChainStatus, hideLogs bool) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	listener.Close()
	ctx, cancel := context.WithCancel(context.Background())
	updates := make(chan *dash.ChainStatus, 1)
	done := make(chan error, 1)
	go func() {
		done <- dash.Serve(ctx, port, updates, make(chan dash.LogMessage), hideLogs, func() dash.HealthStatus { return dash.HealthStatus{} }, nil, nil, nil, nil)
	}()
	defer func() { cancel(); <-done }()
	var conn *websocket.Conn
	for deadline := time.Now().Add(5 * time.Second); conn == nil; {
		conn, _, err = websocket.DefaultDialer.Dial("ws://127.0.0.1:"+port+"/ws", nil)
		if err != nil && time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	defer conn.Close()
	updates <- status
	for {
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, body, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("no status payload: %v", err)
		}
		if strings.Contains(string(body), `"Status":[{`) {
			return string(body)
		}
	}
}

// hide_logs: the Fibre DownReason is stripped from the status payload; it is shown without hide_logs.
func TestOpFibreHideLogsDropsDownReason(t *testing.T) {
	r := newFibreRig(t)
	r.cc.fibre.Reachable, r.cc.fibre.DownSince, r.cc.fibre.DownReason = false, r.clock.now.Add(-5*time.Minute), "dial tcp 10.0.0.5:7980: connection refused"
	shown := opStatusPayload(t, r.cc.dashboardStatus(), false)
	if !strings.Contains(shown, "10.0.0.5") {
		t.Fatal("unhidden status lost the reason, test is blind")
	}
	hidden := opStatusPayload(t, r.cc.dashboardStatus(), true)
	if strings.Contains(hidden, "10.0.0.5") || strings.Contains(hidden, "connection refused") {
		t.Fatalf("hidden status leaked the down reason: %s", hidden)
	}
}
