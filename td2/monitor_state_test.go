package tenderduty

import (
	"encoding/json"
	"testing"
	"time"
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
		{"Proposal #1", "warning", false, false},
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
