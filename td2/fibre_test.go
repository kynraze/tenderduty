package tenderduty

import (
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
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	rpchttp "github.com/tendermint/tendermint/rpc/client/http"
	"google.golang.org/protobuf/encoding/protowire"
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

// captured from Mocha: Kynraze is registered, a validator that isn't gets an empty info and no found field
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

// a node answering every abci_query with a value, or an error code when value is nil
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

// a chain without x/valaddr is asked once, so fibre_enabled can sit in alert_defaults for every chain
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

// any other failure is asked again and leaves what was stored alone, a flaky node must not raise or clear alarms
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

// a Fibre host the way Fibre builds it: a self-signed certificate with the extension binding its TLS key to the validator
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

// serveFibre answers TLS 1.3 handshakes with h2 on a local port until the test ends
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

	// another validator's key: reachable, but it does not prove anything
	chain.valInfo.ConsKey, _, _ = ed25519.GenerateKey(rand.Reader)
	chain.refreshFibre(context.Background(), fibreNode(t, fibreRegistration(good), &queries))
	if s := chain.fibre; !s.Reachable || s.IdentityOK || s.IdentityError == "" {
		t.Fatalf("a host that is not ours gave %+v", s)
	}

	// no key known: the identity is not checked and it is not a failure
	chain.valInfo.ConsKey = nil
	chain.refreshFibre(context.Background(), fibreNode(t, fibreRegistration(good), &queries))
	if s := chain.fibre; !s.Reachable || s.KeyKnown || !s.IdentityOK {
		t.Fatalf("no key gave %+v", s)
	}

	// down since the first failure, not each one, and a new host starts over
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
		// a host that stops answering proves nothing about its identity, both stay until it answers
		{"goes down after failing identity", forged, downFor(10 * time.Minute), []string{down, identity}, nil},
		{"unregisters while down", downFor(10 * time.Minute), unregistered, []string{notRegistered}, []string{down}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := auditConfig(t)
			c.NodeDownMin = 5
			cc := &ChainConfig{name: "Chain", ChainId: chain, fibre: tc.before, fibreLoaded: true}
			cc.Alerts.FibreAlerts, cc.Alerts.FibrePriority = true, "warning"
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
			// nothing is left once the condition stops holding
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

// a changed host raises the new host's alarm before resolving the old one, they must not go out under one dedup key
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
			cc.Alerts.FibreAlerts, cc.Alerts.FibrePriority = true, "warning"
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
	// the reason belongs to the host checked last, an older host's alarm doesn't borrow it
	if got := (&alertMsg{chain: "Chain", message: fibreDownMessage("9.9.9.9:7980", "mocha-5")}).summary(); strings.Contains(got, "\n") {
		t.Errorf("another host's alarm got this host's reason: %q", got)
	}
}

// the Fibre state isn't saved, so after a restart a host that is still down looks like it only just went down. The
// alarms restored from the state file must stay until the host answers, not read "Fibre up" and come back minutes later.
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
			chain.Alerts.FibreAlerts, chain.Alerts.FibrePriority = true, "warning"
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

	// and once it answers again it does clear
	c := auditConfig(t)
	consPub, consPriv, _ := ed25519.GenerateKey(rand.Reader)
	good := serveFibre(t, fibreCertificateFor(t, consPriv, "mocha-5", true))
	queries := 0
	chain := fibreChain()
	chain.name, chain.ChainId, chain.valInfo.ConsKey = "Chain", "mocha-5", consPub
	chain.Alerts.FibreAlerts, chain.Alerts.FibrePriority = true, "warning"
	c.Chains["Chain"] = chain
	alarms.AllAlarms["Chain"] = map[string]time.Time{fibreDownMessage(good, "mocha-5"): time.Now().Add(-time.Hour)}
	chain.refreshFibre(context.Background(), fibreNode(t, fibreRegistration(good), &queries))
	chain.fibreAlarms(chain.valInfo, false)
	if alarmIsActive("Chain", fibreDownMessage(good, "mocha-5")) {
		t.Fatal("a restored down alarm stayed after the host answered")
	}
}

// x/valaddr takes DNS names as well as IPs, and the identity check doesn't care which
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

// a host that stops answering proves nothing about its identity: the failed check stays until the host answers again
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
	chain.Alerts.FibreAlerts, chain.Alerts.FibrePriority = true, "warning"
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

// a host alarm cleared because the host was replaced or the registration went away doesn't say the host came back
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
			cc.Alerts.FibreAlerts, cc.Alerts.FibrePriority = true, "warning"
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
