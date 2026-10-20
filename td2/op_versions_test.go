package tenderduty

import (
	"strings"
	"testing"
	"time"
)

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
