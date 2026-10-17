package tenderduty

import (
	"reflect"
	"strings"
	"testing"
	"time"

	dash "github.com/kynraze/tenderduty/v2/td2/dashboard"
)

func TestDashboardAlertLines(t *testing.T) {
	c := auditConfig(t)
	cc := &ChainConfig{name: "Chain"}
	c.Chains[cc.name] = cc
	messages := map[string]string{
		"🚨 Severity: critical\nRPC node down":       "critical",
		"🚨 Severity: critical Alpha":                "critical",
		"Beta":                                      "critical",
		"Warning":                                   "warning",
		"Upgrade v4 at height 1000":                 "critical",
		"Information":                               "info",
		"Version 11 upgrade at height 394":          "critical",
		"Version 11 upgrade at height 394 is close": "info",
	}
	alarms.AllAlarms[cc.name] = make(map[string]time.Time)
	alarms.AlarmSeverities = make(map[string]string)
	for message, severity := range messages {
		alarms.AllAlarms[cc.name][message] = time.Unix(1000, 0)
		alarms.AlarmSeverities[cc.name+"\x00"+message] = severity
	}
	want := []dash.AlertLine{
		{Text: "Alpha", Severity: "critical", Since: 1000},
		{Text: "Beta", Severity: "critical", Since: 1000},
		{Text: "RPC node down", Severity: "critical", Since: 1000},
		{Text: "Warning", Severity: "warning", Since: 1000},
		{Text: "Version 11 upgrade at height 394 is close", Severity: "info", Since: 1000},
		{Text: "Upgrade v4 at height 1000", Notice: true, Severity: "critical", Since: 1000},
		{Text: "Information", Notice: true, Severity: "info", Since: 1000},
	}
	if got := cc.dashboardStatus().Alerts; !reflect.DeepEqual(got, want) {
		t.Fatalf("alerts: %+v, want %+v", got, want)
	}
	// hide_logs keeps the kind, never the host
	c.HideLogs = true
	down := nodeDownMessage("http://secret-host:26657", "chain-1")
	inactive := "Kynraze is no longer in the active set on chain-1"
	alarms.AllAlarms[cc.name] = map[string]time.Time{down: time.Unix(1000, 0), inactive: time.Unix(1000, 0)}
	alarms.AlarmSeverities = map[string]string{cc.name + "\x00" + down: "critical", cc.name + "\x00" + inactive: "critical"}
	got := cc.dashboardStatus().Alerts
	if len(got) != 2 {
		t.Fatalf("hidden alerts: %+v", got)
	}
	kinds := map[string]bool{}
	for _, a := range got {
		if strings.Contains(a.Text, "secret-host") || a.Detail != "" || a.Since != 1000 {
			t.Fatalf("hidden alert leaks or lost its start: %+v", a)
		}
		kinds[a.Text] = true
	}
	if !kinds["Not in the active set"] {
		t.Fatalf("hidden kinds: %+v", got)
	}
}

func TestDashboardAlertDetail(t *testing.T) {
	c := auditConfig(t)
	node := &NodeConfig{Url: "http://localhost:26657", AlertIfDown: true}
	node.recordBinary("10.4.0", true)
	node.markDownAt("raw connection refused", false, 1365)
	cc := &ChainConfig{name: "Chain", ChainId: "celestia", Nodes: []*NodeConfig{node},
		signal: signalState{Version: 11, UpgradeVersion: 11, UpgradeHeight: 1365}}
	c.Chains[cc.name] = cc
	message := nodeDownMessage(node.Url, cc.ChainId)
	alarms.AlarmSeverities = make(map[string]string)
	alarms.AllAlarms[cc.name] = map[string]time.Time{message: time.Now(), "Custom\n≈ ETA\nDetail one\nDetail two": time.Now()}
	alarms.AlarmSeverities[cc.name+"\x00"+message] = "critical"
	status := cc.dashboardStatus()
	if len(status.Alerts) != 2 {
		t.Fatalf("alerts: %+v", status.Alerts)
	}
	if status.Alerts[0].Detail != "stopped at the version 11 upgrade height 1,365: runs celestia-app 10.4.0, upgrade to v11" {
		t.Fatalf("node detail: %+v", status.Alerts[0])
	}
	if status.Alerts[1].Text != "Custom" || status.Alerts[1].Detail != "Detail one" {
		t.Fatalf("multiline alert: %+v", status.Alerts[1])
	}
	if !strings.Contains(status.LastError, message) || !strings.Contains(status.LastError, node.lastMsg) {
		t.Fatalf("legacy errors lost: %q", status.LastError)
	}
}

func TestDashboardAlertZeroStart(t *testing.T) {
	c := auditConfig(t)
	cc := &ChainConfig{name: "Chain"}
	c.Chains[cc.name] = cc
	alarms.AllAlarms[cc.name] = map[string]time.Time{"Warning": {}}
	alarms.AlarmSeverities = map[string]string{cc.name + "\x00Warning": "warning"}
	lines := cc.dashboardAlertLines()
	if len(lines) != 1 || lines[0].Since != 0 || lines[0].Severity != "warning" {
		t.Fatalf("zero start: %+v", lines)
	}
}
