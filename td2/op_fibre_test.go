package tenderduty

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	dash "github.com/kynraze/tenderduty/v2/td2/dashboard"
	"google.golang.org/protobuf/proto"
)

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
