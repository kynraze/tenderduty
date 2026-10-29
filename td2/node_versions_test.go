package tenderduty

import (
	"context"
	"encoding/json"
	rpchttp "github.com/tendermint/tendermint/rpc/client/http"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestOwnNodeStuckDetail(t *testing.T) {
	c := auditConfig(t)
	node := &NodeConfig{Url: "http://localhost:26657", AlertIfDown: true}
	cc := &ChainConfig{name: "Chain", ChainId: "chain-1", Nodes: []*NodeConfig{node}, lastBlockNum: 3000}
	c.Chains[cc.name] = cc
	detail := "stuck at height 1,000, 2,000 blocks behind"
	status := cc.markNodeDown(node, detail, false, detail)
	if !status.down || status.syncing || status.stuckDetail != detail || node.snapshot().stuckDetail != detail {
		t.Fatalf("stuck state %+v", status)
	}
	message := nodeDownMessage(node.Url, cc.ChainId)
	if got := nodeDownDetail(cc.name, message); got != detail {
		t.Fatalf("stuck detail %q", got)
	}
	if got := (&alertMsg{chain: cc.name, message: message}).summary(); !strings.HasSuffix(got, "\n"+detail) {
		t.Fatalf("node down summary %q", got)
	}
	cc.signal = signalState{UpgradeVersion: 11, UpgradeHeight: 3000}
	node.recordBinary("10.4.0", true)
	if got := ownNodeDownDetail(cc.name, node.Url); !strings.HasPrefix(got, "stopped at the version 11 upgrade height") {
		t.Fatalf("upgrade detail lost priority: %q", got)
	}
	cc.signal = signalState{}
	node.markHealthy()
	if node.snapshot().stuckDetail != "" || ownNodeDownDetail(cc.name, node.Url) != "" {
		t.Fatal("healthy node kept stuck detail")
	}
	cc.markNodeDown(node, detail, false, detail)
	node.markDownAt("down", false, 3000)
	if node.snapshot().stuckDetail != "" || ownNodeDownDetail(cc.name, node.Url) != "" {
		t.Fatal("other downtime kept stuck detail")
	}
}

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
	if !cc.signalNodesReady(cc.signal.BinaryVersion, 11) || signalUpgradeDetail(cc.name, soon) != "RPC node ready: celestia-app 11.0.0 · own node unknown (none with alert_if_down)" {
		t.Fatal("active-node fallback changed")
	}
	cc.signalAlarms(&ValInfo{Valcons: "cons"}, false)
	if r, ok := storedResolution(cc.name, soon); !ok || r.text != "RPC node ready: celestia-app 11.0.0 · own node unknown (none with alert_if_down)" {
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
	if got := cc.statusDetail(cc.name, time.Now()); strings.Contains(got, "rpc.example") || strings.Contains(got, "10.4.0") || !strings.Contains(got, "Node 1:</b> down") {
		t.Fatalf("hidden telegram contains node version: %q", got)
	}
}

// opDrain empties the alert channel.
func opDrain(c *Config) []*alertMsg {
	var out []*alertMsg
	for len(c.alertChan) > 0 {
		out = append(out, <-c.alertChan)
	}
	return out
}

// Two own nodes, upgrade at 1365 ahead: the reminder names only the node that is behind.
func TestOpVersionsNamesOnlyTheNodeBehind(t *testing.T) {
	c := auditConfig(t)
	a := &NodeConfig{Url: "https://rpc-a.example:26657", AlertIfDown: true}
	b := &NodeConfig{Url: "https://rpc-b.example:26657", AlertIfDown: true}
	a.recordBinary("10.4.0", true)
	b.recordBinary("v11.0.0", true)
	cc := &ChainConfig{name: "Chain", ChainId: "celestia", Nodes: []*NodeConfig{a, b}, signalLoaded: true, lastBlockNum: 1000,
		signal: signalState{Version: 11, UpgradeVersion: 11, UpgradeHeight: 1365, BinaryVersion: "11.0.0"}}
	cc.Alerts.UpgradeAlerts = true
	cc.Alerts.SignalRemindMinutes = 60
	c.Chains[cc.name] = cc
	upgradeETAs.Store("Chain@1365", time.Now().Add(30*time.Minute))
	defer upgradeETAs.Delete("Chain@1365")
	cc.signalAlarms(&ValInfo{Valcons: "cons"}, false)
	soon := signalUpgradeSoonMessage(11, 1365)
	want := "rpc-a.example:26657 · celestia-app 10.4.0 · needs v11"
	if got := signalUpgradeDetail(cc.name, soon); got != want {
		t.Fatalf("reminder detail %q", got)
	}
	cc.lastBlockNum = 1365
	cc.signal.UpgradeHeight = 0
	cc.signalAlarms(&ValInfo{Valcons: "cons"}, false)
	if r, ok := storedResolution(cc.name, soon); !ok || r.text != "upgrade height reached · not upgraded: rpc-a.example:26657 (10.4.0)" {
		t.Fatalf("height reached resolution %+v", r)
	}
}

// Binary strings vary; only a readable major version at or above the target counts as ready.
func TestOpVersionStrings(t *testing.T) {
	for _, tc := range []struct {
		binary string
		ready  bool
		label  string
	}{
		{"v11.0.0", true, "v11.0.0"},
		{"11.0.0-rc1", true, "v11.0.0-rc1"},
		{"11", true, "v11"},
		{"v10.4.0-rc1", false, "v10.4.0-rc1"},
		{"10.4.0+abc1234", false, "v10.4.0+abc1234"},
		{"a1b2c3d", false, "version unknown"},
		{"", false, "version unknown"},
		{"v", false, "version unknown"},
	} {
		t.Run(tc.binary, func(t *testing.T) {
			c := auditConfig(t)
			node := &NodeConfig{Url: "http://localhost:26657", AlertIfDown: true}
			node.recordBinary(tc.binary, true)
			cc := &ChainConfig{name: "Chain", Nodes: []*NodeConfig{node}, signalLoaded: true, lastBlockNum: 1000,
				signal: signalState{Version: 11, UpgradeVersion: 11, UpgradeHeight: 1365, BinaryVersion: "11.0.0"}}
			c.Chains[cc.name] = cc
			if got := cc.signalNodesReady("11.0.0", 11); got != tc.ready {
				t.Fatalf("ready %v, want %v", got, tc.ready)
			}
			if got := nodeBinaryLabel(node.snapshot()); got != tc.label {
				t.Fatalf("label %q, want %q", got, tc.label)
			}
			if got := cc.notUpgradedNodes(11); tc.ready != (got == "") {
				t.Fatalf("not upgraded list %q", got)
			}
		})
	}
}

// Unreachable node: its last version is not shown as current, and it is never counted ready.
func TestOpVersionUnreachableNodeIsUnknown(t *testing.T) {
	c := auditConfig(t)
	node := &NodeConfig{Url: "http://localhost:26657", AlertIfDown: true}
	node.recordBinary("11.0.0", true)
	cc := &ChainConfig{name: "Chain", ChainId: "celestia", Nodes: []*NodeConfig{node}, signalLoaded: true, lastBlockNum: 1000,
		signal: signalState{Version: 11, UpgradeVersion: 11, UpgradeHeight: 1365, BinaryVersion: "11.0.0"}}
	cc.Alerts.UpgradeAlerts = true
	c.Chains[cc.name] = cc
	node.markDownAt("down", false, 1000)
	if cc.signalNodesReady("11.0.0", 11) {
		t.Fatal("a down node counted as ready")
	}
	if got := cc.ownNodeDetails(11, false); got != "localhost:26657 · version unknown" {
		t.Fatalf("detail %q", got)
	}
	// at the height a node halted on the old binary keeps its last version: that is why it stopped
	if got := cc.notUpgradedNodes(11); got != " · not upgraded: localhost:26657 (11.0.0)" {
		t.Fatalf("not upgraded %q", got)
	}
}

// Alerts name hosts per alert_node_address, never credentials; hide_logs hides them on the dashboard.
func TestOpVersionHostsHiddenByHideLogs(t *testing.T) {
	c := auditConfig(t)
	c.HideLogs = true
	node := &NodeConfig{Url: "https://user:secret@rpc-a.example:26657/?token=secret", AlertIfDown: true}
	node.recordBinary("10.4.0", true)
	cc := &ChainConfig{name: "Chain", ChainId: "celestia", Nodes: []*NodeConfig{node}, signalLoaded: true, lastBlockNum: 1000,
		signal: signalState{Version: 11, UpgradeVersion: 11, UpgradeHeight: 1365, BinaryVersion: "11.0.0"}}
	c.Chains[cc.name] = cc
	soon := signalUpgradeSoonMessage(11, 1365)
	if got := cc.ownNodeDetails(11, false); strings.Contains(got, "secret") {
		t.Fatalf("credentials in the alert: %q", got)
	}
	alarms.AllAlarms[cc.name] = map[string]time.Time{soon: time.Now()}
	for _, line := range cc.dashboardAlertLines() {
		if strings.Contains(line.Text+line.Detail, "rpc-a.example") {
			t.Fatalf("hide_logs dashboard shows the host: %+v", line)
		}
	}
}

// alert_node_address off: nodes are named by number, never by host.
func TestOpVersionAddressOff(t *testing.T) {
	c := auditConfig(t)
	c.AlertNodeAddress = yes(false)
	a := &NodeConfig{Url: "https://rpc-a.example:26657", AlertIfDown: true}
	b := &NodeConfig{Url: "https://rpc-b.example:26657", AlertIfDown: true}
	a.recordBinary("10.4.0", true)
	b.recordBinary("11.0.0", true)
	cc := &ChainConfig{name: "Chain", ChainId: "celestia", Nodes: []*NodeConfig{a, b}, signalLoaded: true, lastBlockNum: 1000,
		signal: signalState{Version: 11, UpgradeVersion: 11, UpgradeHeight: 1365, BinaryVersion: "11.0.0"}}
	c.Chains[cc.name] = cc
	want := "own node 1 · celestia-app 10.4.0 · needs v11\nown node 2 · celestia-app 11.0.0 · ready"
	if got := cc.ownNodeDetails(11, false); got != want {
		t.Fatalf("detail %q", got)
	}
	if got := cc.notUpgradedNodes(11); strings.Contains(got, "rpc-") || !strings.Contains(got, "own node 1 (10.4.0)") {
		t.Fatalf("not upgraded %q", got)
	}
}

// Public fallback RPC on 11.0.0 is not the operator's node: the own node still needs the upgrade.
func TestOpVersionPublicFallbackNotCounted(t *testing.T) {
	c := auditConfig(t)
	own := &NodeConfig{Url: "http://localhost:26657", AlertIfDown: true}
	public := &NodeConfig{Url: "https://public.example"}
	own.recordBinary("10.4.0", true)
	public.recordBinary("11.0.0", true)
	cc := &ChainConfig{name: "Chain", ChainId: "celestia", Nodes: []*NodeConfig{own, public}, signalLoaded: true, lastBlockNum: 1000,
		signal: signalState{Version: 11, UpgradeVersion: 11, UpgradeHeight: 1365, BinaryVersion: "11.0.0"}}
	cc.Alerts.UpgradeAlerts = true
	cc.Alerts.SignalRemindMinutes = 60
	c.Chains[cc.name] = cc
	upgradeETAs.Store("Chain@1365", time.Now().Add(30*time.Minute))
	defer upgradeETAs.Delete("Chain@1365")
	cc.signalAlarms(&ValInfo{Valcons: "cons"}, false)
	soon := signalUpgradeSoonMessage(11, 1365)
	if !alarmIsActive(cc.name, soon) || cc.signalNodesReady("11.0.0", 11) {
		t.Fatal("public node made the own node look upgraded")
	}
	if got := signalUpgradeDetail(cc.name, soon); strings.Contains(got, "public.example") || !strings.HasPrefix(got, "localhost:26657 · celestia-app 10.4.0") {
		t.Fatalf("detail %q", got)
	}
}

// Upgrade done: the reminder and the scheduled notice each clear once.
func TestOpVersionUpgradeDoneClearsOnce(t *testing.T) {
	c := auditConfig(t)
	a := &NodeConfig{Url: "https://rpc-a.example:26657", AlertIfDown: true}
	a.recordBinary("11.0.0", true)
	cc := &ChainConfig{name: "Chain", ChainId: "celestia", Nodes: []*NodeConfig{a}, signalLoaded: true, lastBlockNum: 1000,
		signal: signalState{Version: 11, UpgradeVersion: 11, UpgradeHeight: 1365, BinaryVersion: "11.0.0"}}
	cc.Alerts.UpgradeAlerts = true
	cc.Alerts.SignalRemindMinutes = 60
	c.Chains[cc.name] = cc
	upgradeETAs.Store("Chain@1365", time.Now().Add(30*time.Minute))
	defer upgradeETAs.Delete("Chain@1365")
	val := &ValInfo{Valcons: "cons"}
	cc.signalAlarms(val, false)
	if got := opDrain(c); len(got) != 1 || got[0].resolved {
		t.Fatalf("scheduled upgrade alerts: %d", len(got))
	}
	cc.lastBlockNum = 1365
	cc.signal = signalState{Version: 12, BinaryVersion: "11.0.0"}
	resolved := 0
	for i := 0; i < 3; i++ {
		cc.signalAlarms(val, false)
		for _, a := range opDrain(c) {
			if a.resolved {
				resolved++
			}
		}
	}
	// the node was already on 11, so no reminder was ever raised; only the scheduled upgrade clears
	if resolved != 1 {
		t.Fatalf("resolved messages %d, want the scheduled upgrade once", resolved)
	}
	if alarmIsActive(cc.name, signalUpgradeMessage(11, 1365)) || alarmIsActive(cc.name, signalUpgradeSoonMessage(11, 1365)) {
		t.Fatal("upgrade alarm still active after the upgrade")
	}
	if r, ok := storedResolution(cc.name, signalUpgradeMessage(11, 1365)); !ok || r.text != "upgrade height reached" {
		t.Fatalf("resolution %+v", r)
	}
}

// Misses climb to 150 of 1,000 at 6s a block: the close to jail warning comes once, at 90 minutes.
func TestOpJailRiskClimbsOnce(t *testing.T) {
	c, cc, val := opJailChain(t, 6*time.Second, 1000)
	const closeMsg = "validator is close to jail on chain-1"
	raised := 0
	for missed := int64(0); missed <= 150; missed += 10 {
		val.Missed = missed
		cc.statConsecutiveMiss = 6
		cc.jailAlarms(val, 6, false)
		for _, a := range opDrain(c) {
			if a.message == closeMsg && !a.resolved {
				raised++
				if missed < 100 {
					t.Fatalf("warned at %d misses, 90 minutes away is 100 misses", missed)
				}
				if got := jailDetail(cc.name, closeMsg); !strings.Contains(got, "jailed in ~90 min at this rate") {
					t.Fatalf("detail %q", got)
				}
			}
		}
	}
	if raised != 1 {
		t.Fatalf("close to jail raised %d times", raised)
	}
}

// Misses stop and the window rolls off: the warning clears once, quietly, and nothing more is sent.
func TestOpJailRiskRollsOffClearsOnce(t *testing.T) {
	c, cc, val := opJailChain(t, 6*time.Second, 1000)
	val.Missed = 950
	cc.statConsecutiveMiss = 6
	cc.jailAlarms(val, 6, false)
	opDrain(c)
	val.Missed = 0
	cc.statConsecutiveMiss = 0
	recovered := 0
	for i := 0; i < 3; i++ {
		cc.jailAlarms(val, 0, false)
		for _, a := range opDrain(c) {
			if a.message == "validator is close to jail on chain-1" {
				if !a.resolved || a.quiet {
					t.Fatalf("recovery %+v", a)
				}
				recovered++
				if got := a.summary(); got != "signing again · 1,000 misses left" {
					t.Fatalf("recovery text %q", got)
				}
			}
		}
	}
	if recovered != 1 {
		t.Fatalf("recovered %d times", recovered)
	}
}

// Block time unknown (0): no "0 minutes" in any line, and no time based warning from misses alone.
func TestOpJailBlockTimeUnknown(t *testing.T) {
	c, cc, val := opJailChain(t, 0, 1000)
	const closeMsg = "validator is close to jail on chain-1"
	val.Missed = 500
	cc.statConsecutiveMiss = 6
	cc.jailAlarms(val, 6, false)
	for _, a := range opDrain(c) {
		if a.message == closeMsg {
			t.Fatal("time based warning raised with unknown block time at 500 of 1,000 misses")
		}
	}
	if _, known := cc.timeToJail(); known {
		t.Fatal("time to jail known without block time")
	}
	val.Missed = 950
	cc.jailAlarms(val, 6, false)
	raised := false
	for _, a := range opDrain(c) {
		if a.message == closeMsg && !a.resolved {
			raised = true
			if got := jailDetail(cc.name, closeMsg); strings.Contains(got, "min") {
				t.Fatalf("unknown block time shows minutes: %q", got)
			}
		}
	}
	if !raised {
		t.Fatal("percent of window used did not warn with unknown block time")
	}
}

// MaxMissed 0 (window not read yet): no warning, no division, no "misses used" line.
func TestOpJailMaxMissedZero(t *testing.T) {
	c, cc, val := opJailChain(t, 6*time.Second, 0)
	val.Missed = 5
	cc.statConsecutiveMiss = 10
	cc.jailAlarms(val, 10, false)
	for _, a := range opDrain(c) {
		if strings.Contains(a.message, "close to jail") && !a.resolved {
			t.Fatalf("warned with no window: %q", a.message)
		}
	}
	if _, known := cc.timeToJail(); known {
		t.Fatal("time to jail known with no window")
	}
	if got := jailDetail(cc.name, "validator is close to jail on chain-1"); got != "" {
		t.Fatalf("detail with no window: %q", got)
	}
}

// Tiny window: 10 misses at 6s a block; a first run of 5 misses puts jail 30 seconds away.
func TestOpJailTinyWindowFirstSight(t *testing.T) {
	c, cc, val := opJailChain(t, 6*time.Second, 10)
	const closeMsg = "validator is close to jail on chain-1"
	cc.statConsecutiveMiss = 5
	cc.jailAlarms(val, 5, false)
	raised := 0
	for _, a := range opDrain(c) {
		if a.message == closeMsg && !a.resolved {
			raised++
		}
	}
	if raised != 1 {
		t.Fatalf("close to jail raised %d times at first sight", raised)
	}
	if got := jailDetail(cc.name, closeMsg); strings.Contains(got, "~0 min") {
		t.Fatalf("30 seconds to jail shown as %q", got)
	}
}

// jail_risk_minutes 0 means unset: validation and the runtime both use 90.
func TestOpJailRiskMinutesZeroIsDefault(t *testing.T) {
	c := auditConfig(t)
	cc := &ChainConfig{name: "Chain", ChainId: "chain-1", ValAddress: "valoper", PublicFallback: true}
	c.Chains[cc.name] = cc
	if err := applyAlertDefaults(c, []byte("alert_defaults:\n  missed_blocks_enabled: yes\n  jail_risk_minutes: 0\n"), nil); err != nil {
		t.Fatal(err)
	}
	_, _ = validateConfig(c)
	if got := cc.Alerts.JailRiskMinutes; got != 90 {
		t.Fatalf("jail_risk_minutes 0 became %d", got)
	}
	c, cc, val := opJailChain(t, 6*time.Second, 1000)
	cc.Alerts.JailRiskMinutes = 0
	const closeMsg = "validator is close to jail on chain-1"
	val.Missed = 0
	cc.statConsecutiveMiss = 6
	cc.jailAlarms(val, 6, false)
	for _, a := range opDrain(c) {
		if a.message == closeMsg {
			t.Fatal("warned 100 minutes out with the 90 minute default")
		}
	}
	val.Missed = 200
	cc.jailAlarms(val, 6, false)
	raised := 0
	for _, a := range opDrain(c) {
		if a.message == closeMsg && !a.resolved {
			raised++
		}
	}
	if raised != 1 {
		t.Fatalf("80 minutes out with the 90 minute default: %d warnings", raised)
	}
}

// Close to jail, then jailed: one message per stage, the warning clears quietly.
func TestOpJailStagesOneMessageEach(t *testing.T) {
	c, cc, val := opJailChain(t, 6*time.Second, 1000)
	const closeMsg = "validator is close to jail on chain-1"
	const jailedMsg = "validator is jailed on chain-1"
	val.Missed = 950
	cc.statConsecutiveMiss = 6
	counts := map[string]int{}
	for i := 0; i < 3; i++ {
		cc.jailAlarms(val, 6, false)
		for _, a := range opDrain(c) {
			if !a.resolved {
				counts[a.message]++
			}
		}
	}
	if counts[closeMsg] != 1 {
		t.Fatalf("close to jail sent %d times", counts[closeMsg])
	}
	val.Jailed, val.JailedUntil = true, time.Now().Add(time.Hour)
	for i := 0; i < 3; i++ {
		cc.jailAlarms(val, 0, false)
		for _, a := range opDrain(c) {
			if a.resolved {
				if a.message != closeMsg || !a.quiet {
					t.Fatalf("unexpected recovery %+v", a)
				}
				continue
			}
			counts[a.message]++
		}
	}
	if counts[jailedMsg] != 1 {
		t.Fatalf("jailed sent %d times", counts[jailedMsg])
	}
}

// opJailChain is a bonded validator on one chain with the missed blocks alerts on.
func opJailChain(t *testing.T, blockTime time.Duration, maxMissed int64) (*Config, *ChainConfig, *ValInfo) {
	t.Helper()
	c := auditConfig(t)
	val := &ValInfo{Moniker: "validator", Valcons: "cons", Bonded: true, MaxMissed: maxMissed}
	cc := &ChainConfig{name: "Chain", ChainId: "chain-1", valInfo: val, blockTime: blockTime}
	cc.Alerts.MissedBlocksAlerts, cc.Alerts.ConsecutiveAlerts, cc.Alerts.AlertIfInactive = true, true, true
	c.Chains[cc.name] = cc
	return c, cc, val
}
