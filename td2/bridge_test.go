package tenderduty

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

type bridgeFixture struct {
	code                   int
	head, sampled, network int64
	running, rpcError      bool
	methods                []string
	token                  string
	rpcBody                string
}

func newBridgeFixture(t *testing.T) (*ChainConfig, *bridgeFixture) {
	t.Helper()
	c := auditConfig(t)
	f := &bridgeFixture{code: http.StatusOK, head: 100, sampled: 100, network: 100, running: true}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
			t.Error("expected JSON POST")
		}
		if r.Header.Get("Authorization") != f.token {
			t.Error("unexpected Authorization header")
		}
		var request struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      int             `json:"id"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params"`
		}
		if json.NewDecoder(r.Body).Decode(&request) != nil || request.JSONRPC != "2.0" || request.ID != 1 || len(request.Params) != 0 {
			t.Error("invalid JSON-RPC request")
		}
		f.methods = append(f.methods, request.Method)
		if f.code != http.StatusOK {
			w.WriteHeader(f.code)
			_, _ = fmt.Fprint(w, f.token)
			return
		}
		if f.rpcBody != "" {
			_, _ = fmt.Fprint(w, f.rpcBody)
			return
		}
		if f.rpcError {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"error": map[string]interface{}{"code": -32603, "message": f.token}})
			return
		}
		var result interface{}
		switch request.Method {
		case "header.LocalHead":
			result = map[string]interface{}{"header": map[string]interface{}{"height": fmt.Sprint(f.head), "time": time.Now().UTC().Format(time.RFC3339Nano)}}
		case "das.SamplingStats":
			result = map[string]interface{}{"head_of_sampled_chain": f.sampled, "network_head_height": f.network, "catch_up_done": true, "is_running": f.running}
		default:
			t.Error("unexpected RPC method")
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "result": result})
	}))
	t.Cleanup(server.Close)
	cc := &ChainConfig{name: "Chain", ChainId: "mocha-5", Bridge: BridgeConfig{URL: server.URL}, lastBlockNum: 100}
	cc.Alerts = AlertConfig{BridgeAlerts: true, BridgeSeverity: "warning", BridgeLagBlocks: 20, BridgeLagMinutes: 5}
	c.Chains[cc.name] = cc
	return cc, f
}

func bridgeTick(cc *ChainConfig, now time.Time) {
	cc.refreshBridge(context.Background(), now)
	cc.bridgeAlarms(false)
}

func TestBridgeHealthyAndToken(t *testing.T) {
	cc, f := newBridgeFixture(t)
	file := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(file, []byte("  secret-bridge-token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cc.Bridge.TokenFile = file
	if fatal, problems := validateBridge(cc.name, cc); fatal || len(problems) != 0 {
		t.Fatalf("token config failed: %v", problems)
	}
	f.token = "Bearer secret-bridge-token"
	bridgeTick(cc, time.Now())
	text, tone := cc.bridgeStatus()
	if text != "head 100 (0 behind) · sampled 100" || tone != "ok" || len(activeMessages(cc.name, "Bridge ")) != 0 {
		t.Fatalf("healthy bridge: %q %q", text, tone)
	}
	if strings.Join(f.methods, ",") != "header.LocalHead,das.SamplingStats" {
		t.Fatal("both read methods must run each tick")
	}
	f.rpcError = true
	for i := 0; i < 3; i++ {
		bridgeTick(cc, time.Now())
	}
	detail := cc.statusDetail(cc.name, time.Now())
	if !strings.Contains(detail, "Bridge") || strings.Contains(detail, "secret-bridge-token") {
		t.Fatal("invalid bridge status detail")
	}
	dashboard, _ := json.Marshal(cc.dashboardStatus())
	if strings.Contains(string(dashboard), "secret-bridge-token") {
		t.Fatal("token leaked in dashboard")
	}
	for len(td.alertChan) > 0 {
		msg := <-td.alertChan
		if strings.Contains(msg.summary(), "secret-bridge-token") {
			t.Fatal("token leaked in alert/log text")
		}
	}
	text, _ = cc.bridgeStatus()
	if strings.Contains(text, "secret-bridge-token") || cc.bridge.Reason != "JSON-RPC error" {
		t.Fatal("RPC error text leaked credentials")
	}
}

func TestBridgeFailureRecoveryAndTokenRejection(t *testing.T) {
	cc, f := newBridgeFixture(t)
	f.code = 500
	for i := 1; i <= 3; i++ {
		bridgeTick(cc, time.Now())
		if got := len(activeMessages(cc.name, "Bridge ")); got != i/3 {
			t.Fatalf("tick %d: %d alarms", i, got)
		}
	}
	down := "Bridge on mocha-5 is unreachable: HTTP 500"
	id := cc.bridgeAlarmID(down)
	cc.bridgeAlarms(false)
	if len(td.alertChan) != 1 {
		t.Fatal("duplicate alarm")
	}
	f.code = 401
	bridgeTick(cc, time.Now())
	token := "Bridge on mocha-5 rejected the token, check token_file"
	if alarmIsActive(cc.name, down) || !alarmIsActive(cc.name, token) || len(activeMessages(cc.name, "Bridge ")) != 1 {
		t.Fatal("token rejection did not replace unreachable")
	}
	if text, _ := cc.bridgeStatus(); text != "token rejected" {
		t.Fatal(text)
	}
	f.code = 200
	bridgeTick(cc, time.Now())
	if len(activeMessages(cc.name, "Bridge ")) != 0 {
		t.Fatal("bridge did not recover")
	}
	if r, ok := storedResolution(cc.name, token); !ok || r.label != "Bridge OK" {
		t.Fatal("missing recovery message")
	}
	copy := &ChainConfig{name: cc.name, ChainId: cc.ChainId}
	if copy.bridgeAlarmID(down) != id || copy.bridgeAlarmID("Bridge on mocha-5 is unreachable: timed out") != id {
		t.Fatal("unstable alarm ID")
	}
	f.code = 403
	bridgeTick(cc, time.Now())
	if !alarmIsActive(cc.name, token) || cc.bridge.Failures != 1 {
		t.Fatal("403 must alert on first tick")
	}
}

func TestBridgeMissingPermission(t *testing.T) {
	cc, f := newBridgeFixture(t)
	f.rpcBody = `{"error":{"code":1,"message":"missing permission to invoke 'LocalHead' (need 'read')"},"id":1,"jsonrpc":"2.0"}`
	bridgeTick(cc, time.Now())
	token := "Bridge on mocha-5 rejected the token, check token_file"
	if !alarmIsActive(cc.name, token) || len(activeMessages(cc.name, "Bridge ")) != 1 || cc.bridge.Failures != 1 {
		t.Fatal("missing permission must reject token on first tick")
	}
	if text, _ := cc.bridgeStatus(); text != "token rejected" || cc.bridge.Reason != "token rejected" {
		t.Fatal("missing permission must not expose server text")
	}
}

func TestBridgeUnreachableRecovery(t *testing.T) {
	cc, f := newBridgeFixture(t)
	f.code = 500
	for i := 0; i < 3; i++ {
		bridgeTick(cc, time.Now())
	}
	message := "Bridge on mocha-5 is unreachable: HTTP 500"
	f.code = 200
	bridgeTick(cc, time.Now())
	if alarmIsActive(cc.name, message) {
		t.Fatal("unreachable did not resolve")
	}
	if r, ok := storedResolution(cc.name, message); !ok || r.label != "Bridge OK" {
		t.Fatal("missing unreachable recovery")
	}
	for len(td.alertChan) > 0 {
		msg := <-td.alertChan
		if msg.resolved && msg.message == message {
			return
		}
	}
	t.Fatal("recovery was not sent")
}

func TestBridgeLagWindows(t *testing.T) {
	for _, sampling := range []bool{false, true} {
		t.Run(fmt.Sprint(sampling), func(t *testing.T) {
			cc, f := newBridgeFixture(t)
			message := "Bridge on mocha-5 is 21 blocks behind"
			cc.lastBlockNum = 121
			if sampling {
				cc.lastBlockNum = 100
				f.running = false
				message = "Bridge sampling on mocha-5 is stuck at 100"
			}
			bridgeTick(cc, time.Now().Add(-4*time.Minute))
			if len(activeMessages(cc.name, "Bridge ")) != 0 {
				t.Fatal("lag alerted before window")
			}
			cc.stateMux.Lock()
			if sampling {
				cc.bridge.SamplingSince = time.Now().Add(-6 * time.Minute)
			} else {
				cc.bridge.BehindSince = time.Now().Add(-6 * time.Minute)
			}
			cc.stateMux.Unlock()
			bridgeTick(cc, time.Now())
			if !alarmIsActive(cc.name, message) {
				t.Fatal("missing lag alarm")
			}
			cc.lastBlockNum, f.running = 100, true
			bridgeTick(cc, time.Now())
			if alarmIsActive(cc.name, message) {
				t.Fatal("lag alarm did not recover")
			}
			if r, ok := storedResolution(cc.name, message); !ok || !strings.Contains(r.text, map[bool]string{false: "back in sync", true: "sampling resumed"}[sampling]) {
				t.Fatal("missing lag recovery text")
			}
		})
	}
}

func TestBridgeSamplingLagAndChainDown(t *testing.T) {
	cc, f := newBridgeFixture(t)
	f.sampled = 79
	bridgeTick(cc, time.Now().Add(-6*time.Minute))
	if !alarmIsActive(cc.name, "Bridge sampling on mocha-5 is stuck at 79") {
		t.Fatal("sampling height lag ignored")
	}
	f.sampled, cc.lastBlockNum, cc.noNodes = 100, 200, true
	bridgeTick(cc, time.Now().Add(-6*time.Minute))
	if len(activeMessages(cc.name, "Bridge ")) != 0 || !cc.bridge.BehindSince.IsZero() {
		t.Fatal("chain down must skip behind")
	}
	cc.noNodes, cc.lastBlockNum = false, 0
	bridgeTick(cc, time.Now())
	if !cc.bridge.BehindSince.IsZero() {
		t.Fatal("unknown chain height must skip behind")
	}
}

func TestBridgeConfigAndReload(t *testing.T) {
	for _, tc := range []struct {
		url            string
		fatal, warning bool
	}{
		{"", true, false},
		{"http://203.0.113.10:26658", false, true},
		{"http://127.0.0.1:26658", false, false},
		{"http://[::1]:26658", false, false},
		{"http://10.0.0.1:26658", false, false},
		{"https://bridge.example:26658", false, false},
	} {
		cc := &ChainConfig{Bridge: BridgeConfig{URL: tc.url}, Alerts: AlertConfig{BridgeAlerts: true}}
		fatal, problems := validateBridge("Chain", cc)
		if fatal != tc.fatal || (len(problems) > 0 && strings.HasPrefix(problems[0], "warning:")) != tc.warning {
			t.Fatalf("%s: %v %v", tc.url, fatal, problems)
		}
	}
	cc, _ := newBridgeFixture(t)
	next := &ChainConfig{ChainId: cc.ChainId, Bridge: cc.Bridge, Alerts: cc.Alerts}
	if restartReason(cc, next) != "" {
		t.Fatal("unchanged bridge restarts")
	}
	if err := validateChainSettings([]string{"bridge.url=http://127.0.0.1:26658", "bridge.token_file=/tmp/token", "alerts.bridge_enabled=yes"}); err != nil {
		t.Fatal(err)
	}
	next.Bridge.token = "rotated"
	if restartReason(cc, next) != "bridge changed" {
		t.Fatal("token rotation must apply on reload")
	}
	c := &Config{Chains: map[string]*ChainConfig{"Chain": {}}}
	if err := applyAlertDefaults(c, []byte("chains:\n  Chain: {}\n"), nil); err != nil {
		t.Fatal(err)
	}
	a := c.Chains["Chain"].Alerts
	if a.BridgeAlerts || a.BridgeLagBlocks != 20 || a.BridgeLagMinutes != 5 || a.BridgeSeverity != "warning" {
		t.Fatal("incorrect bridge defaults")
	}
}

func TestBridgePrometheus(t *testing.T) {
	cc, _ := newBridgeFixture(t)
	bridgeTick(cc, time.Now())
	cc.lastBlockNum = 110
	if text, tone := cc.bridgeStatus(); text != "head 100 (0 behind) · sampled 100" || tone != "ok" {
		t.Fatalf("lag changed without polling: %q %q", text, tone)
	}
	registry := prometheus.NewRegistry()
	registry.MustRegister(&bridgeCollector{config: td})
	families, err := registry.Gather()
	if err != nil || len(families) != 4 {
		t.Fatalf("metrics: %v, %d families", err, len(families))
	}
	for _, family := range families {
		if len(family.Metric) != 1 || len(family.Metric[0].Label) != 3 {
			t.Fatal("incorrect bridge labels")
		}
		if family.GetName() == "tenderduty_bridge_up" && family.Metric[0].Gauge.GetValue() != 1 {
			t.Fatal("healthy bridge reported down")
		}
		if family.GetName() == "tenderduty_bridge_lag_blocks" && family.Metric[0].Gauge.GetValue() != 0 {
			t.Fatal("lag changed without polling")
		}
	}
}

func TestBridgeStatusSummary(t *testing.T) {
	cc, _ := newBridgeFixture(t)
	cc.monitoring = true
	cc.valInfo = &ValInfo{Bonded: true, Missed: 2, Window: 100}
	now := time.Now()
	for _, tc := range []struct {
		name  string
		state bridgeState
		want  string
	}{
		{"unchecked", bridgeState{}, ""},
		{"healthy", bridgeState{Loaded: true, Up: true}, ""},
		{"unreachable", bridgeState{Loaded: true}, " · bridge unreachable"},
		{"token", bridgeState{Loaded: true, TokenRejected: true}, " · bridge token rejected"},
		{"behind", bridgeState{Loaded: true, Up: true, Lag: 21, BehindSince: now, SamplingSince: now}, " · bridge 21 behind"},
		{"sampling", bridgeState{Loaded: true, Up: true, SamplingSince: now}, " · bridge sampling stuck"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cc.bridge = tc.state
			summary := td.telegramCommand("status", "", now, tgMention{})
			want := " · missed 2/100" + tc.want
			if !strings.HasSuffix(summary, want) {
				t.Fatalf("summary %q, want %q", summary, want)
			}
		})
	}
	cc.Bridge.URL = ""
	if got := cc.bridgeSummary(); got != "" {
		t.Fatalf("unconfigured bridge summary: %q", got)
	}
}
