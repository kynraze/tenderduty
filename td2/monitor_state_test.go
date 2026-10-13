package tenderduty

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestNoticeAlarm(t *testing.T) {
	own := &NodeConfig{AlertIfDown: true}
	own.recordBinary("11.0.0", true)
	cc := &ChainConfig{Nodes: []*NodeConfig{own}, signal: signalState{BinaryVersion: "11.0.0"}}
	for _, tc := range []struct {
		message, severity string
		notice, upgrade   bool
	}{
		{"Upgrade v4.0.0 at height 1000", "critical", true, true},
		{"Upgrade v4.0.0 at height 1000 is close", "info", false, false},
		{"Version 11 upgrade at height 394", "critical", true, true},
		{"Version 12 upgrade at height 394", "info", false, true},
		{"Version 11 upgrade at height 394 is close", "info", false, false},
		{"Validator has not signalled version 11", "warning", false, false},
		{"missed blocks", "critical", false, false},
		{"jailed", "critical", false, false},
		{"node down", "critical", false, false},
		{"stalled", "critical", false, false},
		{"Proposal #1", "warning", true, false},
		{"fibre warning", "warning", false, false},
		{"informational alarm", "info", true, false},
		{"old alarm without severity", "", false, false},
	} {
		if notice, upgrade := cc.noticeAlarm(tc.message, tc.severity); notice != tc.notice || upgrade != tc.upgrade {
			t.Errorf("%q: notice=%v upgrade=%v", tc.message, notice, upgrade)
		}
	}
	own.recordBinary("10.4.0", true)
	if notice, _ := cc.noticeAlarm("Version 11 upgrade at height 394", "critical"); notice {
		t.Fatal("old own binary is ready")
	}
	own.recordBinary("11.0.0", true)
	own.markDown("down", false)
	if notice, _ := cc.noticeAlarm("Version 11 upgrade at height 394", "critical"); notice {
		t.Fatal("down own node is ready")
	}
	cc.Nodes = nil
	if notice, _ := cc.noticeAlarm("Version 11 upgrade at height 394", "critical"); !notice {
		t.Fatal("public binary readiness was ignored")
	}
}

func TestDashboardGovernanceAlerts(t *testing.T) {
	auditConfig(t)
	cc := &ChainConfig{name: "Chain"}
	p := govProposal{ID: 1, Title: "Proposal", VotingEnd: time.Now().Add(time.Hour)}
	open := cc.govOpenMessage(p)
	for _, reminder := range []string{cc.govRemindMessage(p, "Validator"), cc.govEarlyRemindMessage(p, "Validator")} {
		alarms.AllAlarms[cc.name] = map[string]time.Time{reminder: time.Now()}
		alarms.AlarmSeverities = map[string]string{cc.name + "\x00" + reminder: "critical"}
		status := cc.dashboardStatus()
		if status.ActiveAlerts != 1 || status.NoticeAlerts != 1 || status.NoticeLabel != "Vote pending" {
			t.Fatalf("reminder status: %+v", status)
		}
		if len(status.Alerts) != 0 {
			t.Fatalf("proposal alert lines: %+v", status.Alerts)
		}
		alarms.AllAlarms[cc.name][open] = time.Now()
		alarms.AllAlarms[cc.name]["RPC unavailable"] = time.Now()
		lines := cc.dashboardAlertLines()
		if len(lines) != 1 || strings.HasPrefix(lines[0].Text, "Proposal #") {
			t.Fatalf("mixed alert lines: %+v", lines)
		}
		delete(alarms.AllAlarms[cc.name], "RPC unavailable")
		upgrade := "Upgrade v4 at height 1000"
		alarms.AllAlarms[cc.name][upgrade] = time.Now()
		if _, _, label := cc.dashboardAlerts(); label != "Upgrade scheduled" {
			t.Fatalf("upgrade label: %q", label)
		}
	}
	alarms.AllAlarms[cc.name] = map[string]time.Time{open: time.Now()}
	if active, notices, label := cc.dashboardAlerts(); active != 1 || notices != 1 || label != "Proposal open" {
		t.Fatalf("open status: %d %d %q", active, notices, label)
	}
}

func TestDashboardGovernanceProposals(t *testing.T) {
	auditConfig(t)
	end := time.Now().Add(time.Hour).Truncate(time.Second)
	cc := &ChainConfig{
		name: "Chain", Alerts: AlertConfig{GovAlerts: true},
		proposals: []govProposal{
			{ID: 1, Title: "Proposal", VotingEnd: end},
			{ID: 2, Title: strings.Repeat("界", 100), VotingEnd: end, Voted: true},
		},
	}
	if got := cc.dashboardStatus().Proposals; len(got) != 0 {
		t.Fatalf("unloaded proposals: %+v", got)
	}
	cc.proposalsLoaded = true
	got := cc.dashboardStatus().Proposals
	if len(got) != 2 || got[0].ID != 1 || got[0].Title != "Proposal" || got[0].End != end.Unix() || got[0].Voted || !got[1].Voted {
		t.Fatalf("proposals: %+v", got)
	}
	if len(got[1].Title) > 200 || len(got[1].Title) != 198 || !utf8.ValidString(got[1].Title) {
		t.Fatalf("clipped title: %q", got[1].Title)
	}
	cc.Alerts.GovAlerts = false
	if got := cc.dashboardStatus().Proposals; len(got) != 0 {
		t.Fatalf("disabled proposals: %+v", got)
	}
}

func TestDashboardNoticeAlerts(t *testing.T) {
	auditConfig(t)
	cc := &ChainConfig{name: "Chain"}
	upgrade, info := "Upgrade v4 at height 1000", "informational alarm"
	alarms.AllAlarms[cc.name] = map[string]time.Time{upgrade: time.Now(), info: time.Now(), "missed blocks": time.Now()}
	alarms.AlarmSeverities = map[string]string{cc.name + "\x00" + info: "info"}
	status := cc.dashboardStatus()
	if status.ActiveAlerts != 3 || status.NoticeAlerts != 2 || status.NoticeLabel != "Upgrade scheduled" {
		t.Fatalf("mixed status: %+v", status)
	}
	delete(alarms.AllAlarms[cc.name], "missed blocks")
	status = cc.dashboardStatus()
	if status.ActiveAlerts != 2 || status.NoticeAlerts != 2 {
		t.Fatalf("notice-only status: %+v", status)
	}
	alarms.AllAlarms[cc.name]["missed blocks"] = time.Now()
	delete(alarms.AllAlarms[cc.name], upgrade)
	status = cc.dashboardStatus()
	if status.ActiveAlerts != 2 || status.NoticeAlerts != 1 || status.NoticeLabel != "Notice" {
		t.Fatalf("info status: %+v", status)
	}
	delete(alarms.AllAlarms[cc.name], info)
	status = cc.dashboardStatus()
	if status.ActiveAlerts != 1 || status.NoticeAlerts != 0 || status.NoticeLabel != "" {
		t.Fatalf("action status: %+v", status)
	}
}

func TestAlarmOriginalSeverity(t *testing.T) {
	c := auditConfig(t)
	cc := &ChainConfig{name: "Chain"}
	c.Chains[cc.name] = cc
	id := "notice"
	c.alert(cc.name, "informational alarm", "info", false, &id)
	c.alert(cc.name, "informational alarm", "critical", false, &id)
	if got := alarms.AlarmSeverities[cc.name+"\x00informational alarm"]; got != "info" {
		t.Fatalf("original severity: %q", got)
	}
	data, err := json.Marshal(alarms)
	if err != nil {
		t.Fatal(err)
	}
	var restored alarmCache
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}
	if restored.AlarmSeverities[cc.name+"\x00informational alarm"] != "info" {
		t.Fatal("severity missing from saved cache")
	}
	c.alert(cc.name, "informational alarm", "info", true, &id)
	if len(alarms.AlarmSeverities) != 0 {
		t.Fatal("resolved severity retained")
	}
	c.alert(cc.name, "informational alarm", "critical", false, &id)
	if cc.dashboardStatus().NoticeAlerts != 0 {
		t.Fatal("new alarm retained old severity")
	}
}
