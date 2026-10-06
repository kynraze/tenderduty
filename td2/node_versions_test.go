package tenderduty

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	rpchttp "github.com/tendermint/tendermint/rpc/client/http"
)

func TestSignalOwnNodeVersions(t *testing.T) {
	c := auditConfig(t)
	a := &NodeConfig{Url: "http://user:secret@localhost:26657/path?token=secret", AlertIfDown: true}
	b := &NodeConfig{Url: "https://rpc.kynraze.com", AlertIfDown: true}
	public := &NodeConfig{Url: "https://public.example"}
	a.recordBinary("10.4.0", true)
	b.recordBinary("v11.0.0", true)
	public.recordBinary("11.0.0", true)
	cc := &ChainConfig{name: "Chain", ChainId: "celestia", Nodes: []*NodeConfig{a, b, public}, signalLoaded: true, lastBlockNum: 1000, signal: signalState{Version: 11, UpgradeVersion: 11, UpgradeHeight: 1365, BinaryVersion: "11.0.0"}}
	cc.Alerts.UpgradeAlerts = true
	cc.Alerts.SignalRemindMinutes = 60
	c.Chains[cc.name] = cc
	upgradeETAs.Store("Chain@1365", time.Now().Add(30*time.Minute))
	defer upgradeETAs.Delete("Chain@1365")
	val := &ValInfo{Valcons: "cons"}
	cc.signalAlarms(val, false)
	scheduled, soon := signalUpgradeMessage(11, 1365), signalUpgradeSoonMessage(11, 1365)
	want := "localhost:26657 · celestia-app 10.4.0 · needs v11\nrpc.kynraze.com · celestia-app 11.0.0 · ready"
	if got := signalUpgradeDetail(cc.name, scheduled); got != want {
		t.Fatalf("scheduled detail %q, want %q", got, want)
	}
	if got := signalUpgradeDetail(cc.name, soon); got != strings.Split(want, "\n")[0] || !alarmIsActive(cc.name, soon) {
		t.Fatalf("reminder detail %q", got)
	}
	checkCard(t, "own nodes scheduled", (&alertMsg{chain: cc.name, message: scheduled, severity: "critical"}).card())
	checkCard(t, "own node reminder", (&alertMsg{chain: cc.name, message: soon, severity: "critical"}).card())
	a.recordBinary("11.0.0-rc1", true)
	cc.signalAlarms(val, false)
	if alarmIsActive(cc.name, soon) {
		t.Fatal("ready reminder remains active")
	}
	if r, ok := storedResolution(cc.name, soon); !ok || r.text != "all nodes ready" {
		t.Fatalf("ready resolution %+v", r)
	}
	checkCard(t, "all own nodes ready", (&alertMsg{chain: cc.name, message: soon, resolved: true}).card())
	a.recordBinary("10.4.0", true)
	cc.signalAlarms(val, false)
	cc.lastBlockNum = 1365
	cc.markNodeDown(a, "down", false)
	cc.signalAlarms(val, false)
	if r, ok := storedResolution(cc.name, scheduled); !ok || r.text != "upgrade height reached · not upgraded: localhost:26657 (10.4.0)" {
		t.Fatalf("height resolution %+v", r)
	}
	if alarmIsActive(cc.name, signalStoppedMessage(11)) {
		t.Fatal("duplicate stopped alarm")
	}
	message := nodeDownMessage(a.Url, cc.ChainId)
	detail := "stopped at the version 11 upgrade height 1,365: runs celestia-app 10.4.0, upgrade to v11"
	if got := nodeDownDetail(cc.name, message); got != detail {
		t.Fatalf("node down detail %q", got)
	}
	if got := (&alertMsg{chain: cc.name, message: message}).summary(); !strings.HasSuffix(got, "\n"+detail) {
		t.Fatalf("node down summary %q", got)
	}
	checkCard(t, "own node stopped", (&alertMsg{chain: cc.name, message: message, severity: "critical"}).card())
	grouped := &alertMsg{chain: cc.name, message: message, severity: "critical", group: &groupCard{Server: "local", Members: []groupLine{{Chain: cc.name, URL: a.Url}}, Down: "3 minutes"}}
	if !strings.Contains(grouped.summary(), detail) {
		t.Fatal("grouped node alert lost upgrade detail")
	}
	checkCard(t, "grouped own node stopped", grouped.card())
	if got := signalUpgradeDetail(cc.name, scheduled); !strings.HasPrefix(got, "localhost:26657 · version unknown\n") {
		t.Fatalf("down node detail %q", got)
	}
	cc.signal.UpgradeHeight = 0
	cc.signal.Version = 12
	cc.lastBlockNum = 1400
	cc.signalAlarms(val, false)
	if got := nodeDownDetail(cc.name, message); got != detail {
		t.Fatalf("upgrade context lost after public RPC advanced: %q", got)
	}
	a.stateMux.Lock()
	a.downHeight = 1200
	a.stateMux.Unlock()
	if got := nodeDownDetail(cc.name, message); got != "" {
		t.Fatalf("pre-upgrade downtime mislabelled: %q", got)
	}
}

func TestSignalOwnNodeUnknownAndFallback(t *testing.T) {
	c := auditConfig(t)
	node := &NodeConfig{Url: "http://localhost:26657", AlertIfDown: true}
	cc := &ChainConfig{name: "Chain", Nodes: []*NodeConfig{node}, lastBlockNum: 1000, signalLoaded: true, signal: signalState{Version: 11, UpgradeVersion: 11, UpgradeHeight: 1365, BinaryVersion: "11.0.0"}}
	cc.Alerts.UpgradeAlerts = true
	cc.Alerts.SignalRemindMinutes = 60
	c.Chains[cc.name] = cc
	upgradeETAs.Store("Chain@1365", time.Now().Add(30*time.Minute))
	defer upgradeETAs.Delete("Chain@1365")
	soon := signalUpgradeSoonMessage(11, 1365)
	for _, binary := range []string{"", "abc", "10.4.0"} {
		node.recordBinary(binary, false)
		cc.signalAlarms(&ValInfo{Valcons: "cons"}, false)
		if !alarmIsActive(cc.name, soon) || signalUpgradeDetail(cc.name, soon) != "localhost:26657 · version unknown" {
			t.Fatal("unknown own node treated as ready")
		}
		checkCard(t, "unknown own node", (&alertMsg{chain: cc.name, message: soon}).card())
	}
	node.AlertIfDown = false
	if !cc.signalNodesReady(cc.signal.BinaryVersion, 11) || signalUpgradeDetail(cc.name, soon) != "node ready: celestia-app 11.0.0" {
		t.Fatal("active-node fallback changed")
	}
	cc.signalAlarms(&ValInfo{Valcons: "cons"}, false)
	if r, ok := storedResolution(cc.name, soon); !ok || r.text != "node ready: celestia-app 11.0.0" {
		t.Fatalf("fallback resolution %+v", r)
	}
}

func TestOwnNodeBinaryPolling(t *testing.T) {
	reads := 0
	readable := true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			ID     interface{} `json:"id"`
			Method string      `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		reads++
		if request.Method != "abci_info" {
			t.Errorf("unexpected method %s", request.Method)
		}
		if !readable {
			http.Error(w, "unreadable", http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"jsonrpc": "2.0", "id": request.ID, "result": map[string]interface{}{"response": map[string]interface{}{"version": "v10.4.0-corto", "app_version": "10"}}})
	}))
	defer server.Close()
	client, err := rpchttp.New(server.URL, "/websocket")
	if err != nil {
		t.Fatal(err)
	}
	node := &NodeConfig{Url: server.URL, AlertIfDown: true}
	cc := &ChainConfig{lastBlockNum: 1000}
	now := time.Now()
	for minute := 0; minute <= 5; minute++ {
		cc.refreshNodeBinary(context.Background(), node, client, now.Add(time.Duration(minute)*time.Minute))
		want := 1
		if minute == 5 {
			want = 2
		}
		if reads != want {
			t.Fatalf("minute %d: %d reads, want %d", minute, reads, want)
		}
	}
	if status := node.snapshot(); status.binaryVersion != "v10.4.0-corto" || !status.binaryReadable {
		t.Fatalf("software version not stored: %+v", status)
	}
	cc.signal = signalState{UpgradeHeight: 1365}
	cc.refreshNodeBinary(context.Background(), node, client, now.Add(6*time.Minute))
	cc.refreshNodeBinary(context.Background(), node, client, now.Add(6*time.Minute+30*time.Second))
	if reads != 3 {
		t.Fatalf("scheduled cadence: %d reads", reads)
	}
	readable = false
	cc.refreshNodeBinary(context.Background(), node, client, now.Add(7*time.Minute))
	if status := node.snapshot(); status.binaryVersion != "v10.4.0-corto" || status.binaryReadable {
		t.Fatalf("failed read lost last known version: %+v", status)
	}
	node.AlertIfDown = false
	cc.refreshNodeBinary(context.Background(), node, client, now.Add(20*time.Minute))
	if reads != 4 {
		t.Fatalf("public node polled: %d reads", reads)
	}
}

// TestOwnNodeDownDetailAtUpgradeHalt: devnet nodes halt at upgrade height while the chain is one block behind.
func TestOwnNodeDownDetailAtUpgradeHalt(t *testing.T) {
	for _, tc := range []struct {
		name       string
		downHeight int64
		gone       bool // the upgrade left x/signal's state by the time the alert is sent
		want       bool
	}{
		{"block before the height", 1368, false, true},
		{"upgrade gone from state", 1368, true, true},
		{"chain height lagging", 1300, true, true},
		{"unknown height, chain at the halt", 0, true, true},
		{"down long before", 1100, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := auditConfig(t)
			a := &NodeConfig{Url: "http://127.0.0.1:26707", AlertIfDown: true}
			b := &NodeConfig{Url: "http://127.0.0.1:26667", AlertIfDown: true}
			public := &NodeConfig{Url: "http://127.0.0.1:26657"}
			a.recordBinary("10.4.0-corto", true)
			b.recordBinary("11.0.0-test", true)
			cc := &ChainConfig{name: "test", ChainId: "test", Nodes: []*NodeConfig{a, b, public}, signalLoaded: true, lastBlockNum: 1300,
				signal: signalState{Version: 11, UpgradeVersion: 11, UpgradeHeight: 1369, BinaryVersion: "10.4.0-corto"}}
			cc.Alerts.UpgradeAlerts = true
			cc.Alerts.SignalRemindMinutes = 10
			c.Chains[cc.name] = cc
			val := &ValInfo{Valcons: "cons"}
			cc.signalAlarms(val, false)
			cc.stateMux.Lock()
			cc.lastBlockNum = 1368
			cc.stateMux.Unlock()
			cc.signalAlarms(val, false)
			a.markDownAt("down", false, tc.downHeight)
			b.markDownAt("down", false, tc.downHeight)
			if tc.gone {
				cc.stateMux.Lock()
				cc.signal = signalState{Version: 11, Threshold: 670, Total: 1000}
				cc.stateMux.Unlock()
				cc.signalAlarms(val, false)
			}
			detail := "stopped at the version 11 upgrade height 1,369: runs celestia-app 10.4.0-corto, upgrade to v11"
			got := nodeDownDetail(cc.name, nodeDownMessage(a.Url, cc.ChainId))
			if tc.want && got != detail || !tc.want && got != "" {
				t.Fatalf("node A detail %q", got)
			}
			if got := nodeDownDetail(cc.name, nodeDownMessage(b.Url, cc.ChainId)); got != "" {
				t.Fatalf("upgraded node B detail %q", got)
			}
			if got := nodeDownDetail(cc.name, nodeDownMessage(public.Url, cc.ChainId)); got != "" {
				t.Fatalf("public node detail %q", got)
			}
			if status := a.snapshot(); status.binaryVersion != "10.4.0-corto" {
				t.Fatalf("down node lost its binary version: %+v", status)
			}
		})
	}
}

func TestOwnNodeVersionsVisibility(t *testing.T) {
	c := auditConfig(t)
	node := &NodeConfig{Url: "https://user:secret@rpc.example:26657/path?token=secret", AlertIfDown: true}
	node.recordBinary("v10.4.0-corto", true)
	cc := &ChainConfig{name: "Chain", ChainId: "celestia", Nodes: []*NodeConfig{node}}
	c.Chains[cc.name] = cc
	status := cc.dashboardStatus()
	if len(status.RPCNodes) != 1 || status.RPCNodes[0].Endpoint != "rpc.example:26657" || status.RPCNodes[0].Version != "v10.4.0-corto" {
		t.Fatalf("dashboard nodes %+v", status.RPCNodes)
	}
	if got := cc.statusDetail(cc.name, time.Now()); !strings.Contains(got, "rpc.example:26657 · v10.4.0-corto") || strings.Contains(got, "secret") {
		t.Fatalf("telegram nodes %q", got)
	}
	cc.markNodeDown(node, "down", false)
	if status := cc.dashboardStatus(); status.RPCNodes[0].Version != "v10.4.0-corto" || !status.RPCNodes[0].Down {
		t.Fatal("down node list lost last known software version")
	}
	c.HideLogs = true
	if len(cc.dashboardStatus().RPCNodes) != 0 {
		t.Fatal("hidden dashboard contains nodes")
	}
	if got := cc.statusDetail(cc.name, time.Now()); strings.Contains(got, "rpc.example") || strings.Contains(got, "10.4.0") {
		t.Fatalf("hidden telegram contains node version: %q", got)
	}
}
