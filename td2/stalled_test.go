package tenderduty

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestStalledMessage(t *testing.T) {
	plan := &upgradePlan{Name: "v2.20.0", Height: 31_812_210}
	plain := "stalled: have not seen a new block on shentu-2.2 in 10 minutes"
	prefix := "shentu-2.2 stopped at upgrade v2.20.0 height 31,812,210: "
	for _, tc := range []struct {
		name     string
		sources  int
		fallback bool
		height   int64
		nodes    []stalledNode
		want     string
		priority string
	}{
		{"single", 1, false, 1, nil, plain + "\nrpc is the only source, so either it or the chain stopped", "critical"},
		{"fallback", 1, true, 1, nil, plain, "critical"},
		{"multiple", 2, false, 1, nil, plain, "critical"},
		{"no sources", 0, false, 1, nil, plain, "critical"},
		{"old", 2, false, plan.Height - 1, []stalledNode{{"rpc", " v2.19.0 ", "v2.19.0", true}}, prefix + "rpc still runs v2.19.0, switch the binary", "critical"},
		{"several old", 2, false, plan.Height, []stalledNode{{"a", "old", "old", true}, {"b", "old", "old", true}}, prefix + "a still runs old, b still runs old, switch the binary", "critical"},
		{"upgraded", 2, false, plan.Height, []stalledNode{{"rpc", " v2.20.0 ", "v2.19.0", true}}, prefix + "rpc runs v2.20.0, waiting for the network", "warning"},
		{"unknown", 2, false, plan.Height, []stalledNode{{"rpc", "", "v2.19.0", false}}, plain, "critical"},
		{"no baseline", 2, false, plan.Height, []stalledNode{{"rpc", "v2.20.0", "", true}}, plain, "critical"},
		{"no own nodes", 2, false, plan.Height, nil, plain, "critical"},
		{"partly unknown", 2, false, plan.Height, []stalledNode{{"a", "new", "old", true}, {"b", "old", "old", false}}, plain, "critical"},
		{"past height", 2, false, plan.Height + 1, []stalledNode{{"rpc", "old", "old", true}}, plain, "critical"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			message, priority := stalledMessage("shentu-2.2", 10, tc.sources, tc.fallback, "rpc", plan, tc.height, tc.nodes)
			if message != tc.want || priority != tc.priority {
				t.Fatalf("got %q (%s), want %q (%s)", message, priority, tc.want, tc.priority)
			}
		})
	}
}

func TestPreUpgradeVersion(t *testing.T) {
	plan := &upgradePlan{Name: "v2", Height: 1000}
	node := &NodeConfig{AlertIfDown: true}
	node.recordPreUpgrade("old", true, nil, nil, 900)
	if node.preUpgradeVersion != "" {
		t.Fatal("baseline without plan")
	}
	node.recordPreUpgrade(" old ", true, plan, nil, 999)
	if node.preUpgradeVersion != "old" {
		t.Fatal("scheduled baseline missing")
	}
	node.recordPreUpgrade("new", true, plan, nil, 999) // binary swapped in early
	node.recordPreUpgrade("new", true, plan, nil, 1000)
	node.recordPreUpgrade("new", false, plan, nil, 999)
	node.recordPreUpgrade("new", true, nil, plan, 1000)
	if node.preUpgradeVersion != "old" {
		t.Fatal("halt or unreadable version changed baseline")
	}
	node.recordPreUpgrade("new", true, nil, nil, 1001)
	if node.preUpgradeVersion != "" {
		t.Fatal("baseline not cleared")
	}
	node.AlertIfDown = false
	node.recordPreUpgrade("old", true, plan, nil, 999)
	if node.preUpgradeVersion != "" {
		t.Fatal("public node recorded a baseline")
	}
}

func TestStalledAlarmRestoreAndResolve(t *testing.T) {
	c := auditConfig(t)
	cc := &ChainConfig{name: "Chain", ChainId: "chain-1", PublicFallback: true}
	cc.Alerts.StalledAlerts, cc.Alerts.Stalled = true, 10
	c.Chains[cc.name] = cc
	val := &ValInfo{Valcons: "cons"}
	for _, sent := range []string{
		"stalled: have not seen a new block on chain-1 in 10 minutes",
		"stalled: have not seen a new block on chain-1 in 10 minutes\nrpc is the only source, so either it or the chain stopped",
		"chain-1 stopped at upgrade v2 height 1,000: rpc still runs old, switch the binary",
		"chain-1 stopped at upgrade v2 height 1,000: rpc runs new, waiting for the network",
	} {
		alarms.AllAlarms[cc.name] = map[string]time.Time{sent: time.Now()}
		cc.restoreStalledAlarm()
		cc.clearOrphanedAlarms(val, "norpc")
		if !cc.lastBlockAlarm || cc.lastBlockAlarmMsg != sent || !alarmIsActive(cc.name, sent) {
			t.Fatalf("restore lost %q", sent)
		}
		checkCard(t, "raised stall", (&alertMsg{chain: cc.name, message: sent, severity: "critical"}).card())
		cc.stalledAlarm(val, false)
		if id := c.alarmID(cc.name, sent); id != val.Valcons {
			t.Fatalf("stall id %q", id)
		}
		cc.stalledAlarm(val, true)
		for _, msg := range opDrain(c) {
			if msg.message != sent {
				t.Fatalf("sent text changed: %q", msg.message)
			}
		}
		if alarmIsActive(cc.name, sent) || cc.lastBlockAlarm || cc.lastBlockAlarmMsg != "" {
			t.Fatal("stall remains active")
		}
		if got := resolvedText(cc.name, sent); got != "chain-1 is producing blocks again" {
			t.Fatalf("recovery %q", got)
		}
		checkCard(t, "resolved stall", (&alertMsg{chain: cc.name, message: sent, resolved: true}).card())
	}
	cc.stalledAlarm(val, true)
	for _, msg := range opDrain(c) {
		if strings.Contains(msg.message, "\n") || !strings.HasPrefix(msg.message, "stalled:") {
			t.Fatalf("plain fallback %q", msg.message)
		}
	}
}

func TestHaltPlanRetained(t *testing.T) {
	client := govStoreNode(t, map[string][]byte{"/cosmos.upgrade.v1beta1.Query/CurrentPlan ": nil}, "")
	plan := &upgradePlan{Name: "v2.20.0", Height: 31_812_210}
	cc := &ChainConfig{name: "Chain", ChainId: "chain-1", upgrade: plan, lastBlockNum: plan.Height}
	cc.refreshUpgrade(context.Background(), client)
	if cc.upgrade != nil || cc.haltPlan != plan {
		t.Fatal("vanished plan not retained")
	}
	cc.refreshUpgrade(context.Background(), client)
	if cc.haltPlan != plan {
		t.Fatal("second refresh lost halt plan")
	}
	cc.lastBlockNum++
	cc.refreshUpgrade(context.Background(), client)
	if cc.haltPlan != nil {
		t.Fatal("passed plan not cleared")
	}
}

func TestStalledSignalPlanUnchanged(t *testing.T) {
	c := auditConfig(t)
	c.AlertNodeAddress = yes(false)
	node := &NodeConfig{Url: "http://localhost:26657", AlertIfDown: true, binaryVersion: "old", preUpgradeVersion: "old", binaryReadable: true}
	cc := &ChainConfig{name: "Chain", ChainId: "chain-1", Nodes: []*NodeConfig{node}, lastBlockNum: 999,
		haltPlan: &upgradePlan{Name: "v11", Height: 1000}, signalLoaded: true, signal: signalState{Version: 11}}
	c.Chains[cc.name] = cc
	message, priority := cc.stalledMessage()
	if !strings.HasPrefix(message, "stalled:") || !strings.Contains(message, "own node 1 is the only source") || priority != "critical" {
		t.Fatalf("signal plan or hidden address changed: %q (%s)", message, priority)
	}
}
