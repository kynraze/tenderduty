package tenderduty

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestJailStageTransitions(t *testing.T) {
	c := auditConfig(t)
	val := &ValInfo{Moniker: "validator", Valcons: "cons", Bonded: true, Missed: 9000, MaxMissed: 9990}
	cc := &ChainConfig{name: "Chain", ChainId: "chain-1", valInfo: val, blockTime: time.Minute, statConsecutiveMiss: 3}
	cc.Alerts.MissedBlocksAlerts, cc.Alerts.AlertIfInactive = true, true
	c.Chains[cc.name] = cc
	stages := []struct {
		name, severity string
		sent           int
		change         func()
	}{
		{"validator is close to jail on chain-1", "critical", 1, func() {}},
		{"validator is jailed on chain-1", "critical", 1, func() {
			val.Jailed = true
			val.JailedUntil = time.Now().Add(time.Hour)
		}},
		{"validator is jailed on chain-1", "critical", 0, func() {
			val.JailedUntil = time.Now().Add(-time.Minute)
		}},
		{"", "info", 1, func() {
			val.Jailed = false
			val.Missed = 0
		}},
	}
	var since time.Time
	for _, stage := range stages {
		stage.change()
		cc.jailAlarms(val, 0, false)
		if val.Jailed {
			if cc.jailSince().IsZero() {
				t.Fatal("jail time was not recorded")
			}
			if since.IsZero() {
				since = cc.jailSince()
			} else if !cc.jailSince().Equal(since) {
				t.Fatal("jail time changed while jailed")
			}
			line := jailDetail(cc.name, stage.name)
			if !strings.HasPrefix(line, "jailed since "+alertDate(since, alertLocation())) {
				t.Fatalf("jail detail %q", line)
			}
			if stage.sent == 0 {
				if !strings.HasSuffix(line, "can unjail now") {
					t.Fatalf("past unjail time: %q", line)
				}
			} else if !strings.Contains(line, "can unjail from ") {
				t.Fatalf("future unjail time: %q", line)
			}
		} else if !cc.jailSince().IsZero() {
			t.Fatal("jail time was not cleared")
		}
		sent := 0
		for len(c.alertChan) > 0 {
			alert := <-c.alertChan
			if alert.quiet {
				continue
			}
			sent++
			if alert.uniqueId != "consjail" {
				t.Fatalf("stage id %+v", alert)
			}
			if stage.name == "" {
				if !alert.resolved || alert.summary() != "back in the active set" {
					t.Fatalf("recovery %+v", alert)
				}
			} else if alert.resolved || alert.message != stage.name || alert.severity != stage.severity {
				t.Fatalf("stage %+v", alert)
			}
			checkCard(t, "jail stage", alert.card())
		}
		if sent != stage.sent {
			t.Fatalf("stage %q sent %d messages, want %d", stage.name, sent, stage.sent)
		}
		if count := alarms.getCount(cc.name); (stage.name == "" && count != 0) || (stage.name != "" && count != 1) {
			t.Fatalf("stage %q has %d alarms", stage.name, count)
		}
	}
}

func TestJailRiskRecoveryAndRate(t *testing.T) {
	c := auditConfig(t)
	val := &ValInfo{Moniker: "validator", Valcons: "cons", Bonded: true, Missed: 9000, MaxMissed: 9990}
	cc := &ChainConfig{name: "Chain", ChainId: "chain-1", valInfo: val, blockTime: 3 * time.Second, statConsecutiveMiss: 3}
	cc.Alerts.MissedBlocksAlerts, cc.Alerts.AlertIfInactive = true, true
	c.Chains[cc.name] = cc
	cc.jailAlarms(val, 3, false)
	alert := <-c.alertChan
	if got := alert.summary(); !strings.Contains(got, "9,000 of 9,990 misses used · jailed in ~49 min at this rate") {
		t.Fatal(got)
	}
	val.Missed = 8790
	cc.jailAlarms(val, 0, false)
	alert = <-c.alertChan
	if !alert.resolved || alert.summary() != "signing again · 1,200 misses left" {
		t.Fatalf("recovery %q", alert.summary())
	}
	checkCard(t, "jail risk recovered", alert.card())
}

func TestJailSettingsAndDefaults(t *testing.T) {
	for _, tc := range []struct {
		config         string
		risk, inactive bool
	}{
		{"", true, true},
		{"missed_blocks_enabled: no", false, false},
		{"alert_if_inactive: no", true, false},
		{"consecutive_enabled: no", true, true},
		{"missed_blocks_enabled: no\n  alert_if_inactive: yes", false, false},
	} {
		c := auditConfig(t)
		cc := &ChainConfig{name: "Chain", ChainId: "chain-1", ValAddress: "valoper", PublicFallback: true}
		c.Chains[cc.name] = cc
		if err := applyAlertDefaults(c, []byte("alert_defaults:\n  "+tc.config+"\n"), nil); err != nil {
			t.Fatal(err)
		}
		_, _ = validateConfig(c)
		a := cc.Alerts
		if a.jailRiskEnabled() != tc.risk || a.jailInactiveEnabled() != tc.inactive || a.JailRiskPercent != 90 && tc.risk || a.JailRiskMinutes != 60 && tc.risk {
			t.Fatalf("%q: %+v", tc.config, a)
		}
	}
	// A chain file wins over alert_defaults.
	c := auditConfig(t)
	c.Chains["Chain"] = &ChainConfig{}
	if err := applyAlertDefaults(c, []byte("alert_defaults:\n  alert_if_inactive: yes\n"), map[string][]byte{"Chain": []byte("alerts:\n  alert_if_inactive: no\n")}); err != nil {
		t.Fatal(err)
	}
	if c.Chains["Chain"].Alerts.jailInactiveEnabled() || !c.Chains["Chain"].Alerts.jailRiskEnabled() {
		t.Fatal("chain override ignored")
	}
}

func TestJailRestoredAlarmMigration(t *testing.T) {
	for _, old := range []string{"validator can unjail on chain-1", "validator has used 50% of the misses allowed before jailing on chain-1", "validator is no longer active: validator is jailed", "validator is jailed on chain-1\nCan unjail after 6 Oct 2026 02:10 UTC"} {
		c := auditConfig(t)
		val := &ValInfo{Moniker: "validator", Valcons: "cons", Jailed: true, JailedUntil: time.Now().Add(time.Hour)}
		cc := &ChainConfig{name: "Chain", ChainId: "chain-1", valInfo: val}
		cc.Alerts.MissedBlocksAlerts, cc.Alerts.AlertIfInactive = true, true
		c.Chains[cc.name] = cc
		alarms.AllAlarms[cc.name] = map[string]time.Time{old: time.Now()}
		alarms.AlarmIDs = map[string]string{cc.name + "\x00" + old: "saved-id"}
		cc.clearOrphanedAlarms(val, "norpc")
		cc.jailAlarms(val, 0, false)
		quiet, raised := 0, 0
		for len(c.alertChan) > 0 {
			a := <-c.alertChan
			if a.resolved {
				if !a.quiet || a.uniqueId != "saved-id" {
					t.Fatalf("migration %+v", a)
				}
				quiet++
			} else {
				if a.uniqueId != "consjail" {
					t.Fatalf("new stage id %+v", a)
				}
				raised++
			}
		}
		if quiet != 1 || raised != 1 || alarms.getCount(cc.name) != 1 {
			t.Fatalf("migration quiet=%d raised=%d", quiet, raised)
		}
	}
}

func TestJailInactiveReasons(t *testing.T) {
	for _, tc := range []struct {
		info   ValInfo
		reason string
	}{
		{ValInfo{Tombstoned: true}, "tombstoned"}, {ValInfo{Unbonding: true}, "unbonding"}, {ValInfo{MaxValidators: 100}, "not in the top 100 by stake"},
	} {
		c := auditConfig(t)
		val := tc.info
		val.Moniker, val.Valcons = "validator", "cons"
		cc := &ChainConfig{name: "Chain", ChainId: "chain-1", valInfo: &val}
		cc.Alerts.MissedBlocksAlerts, cc.Alerts.AlertIfInactive = true, true
		c.Chains[cc.name] = cc
		cc.jailAlarms(&val, 0, false)
		a := <-c.alertChan
		if !strings.HasSuffix(a.summary(), "\n"+tc.reason) || reportAlarmKind(cc.name, a.message) != "Inactive" {
			t.Fatalf("reason %q: %q", tc.reason, a.summary())
		}
		checkCard(t, fmt.Sprint(tc.info), a.card())
	}
}

func TestReportValidatorStatus(t *testing.T) {
	since := time.Date(2026, 10, 5, 19, 0, 0, 0, time.UTC)
	for _, weekly := range []bool{false, true} {
		for _, tc := range []struct {
			info   ValInfo
			since  time.Time
			status string
		}{
			{ValInfo{Bonded: true, Jailed: true, JailedUntil: since.Add(10 * time.Minute)}, since, "🚨 Jailed since 6 Oct 02:00"},
			{ValInfo{Bonded: true, Jailed: true, JailedUntil: since.Add(10 * time.Minute)}, time.Time{}, "🚨 Jailed"},
			{ValInfo{Tombstoned: true}, since, "🚨 Tombstoned"},
			{ValInfo{}, time.Time{}, "🚨 Not in the active set"},
		} {
			c := auditConfig(t)
			c.Reports.Timezone = "UTC+7"
			c.Chains["Chain"] = &ChainConfig{name: "Chain", valInfo: &tc.info, jailedSince: tc.since}
			r := c.buildReport(time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC), weekly, false, nil)
			if r.Rows[0].Dot != dotRed || !containsMessage(r.Rows[0].Lines, tc.status) || !strings.Contains(r.telegram(), tc.status) || strings.Contains(r.telegram(), "unjail") {
				t.Fatalf("status %+v", r.Rows[0])
			}
		}
	}
}

func TestJailDetailTimes(t *testing.T) {
	loc := time.FixedZone("WIB", 7*60*60)
	since := time.Date(2026, 10, 6, 2, 0, 0, 0, loc)
	for _, tc := range []struct {
		name         string
		since, until time.Time
		want         string
	}{
		{"same day", since, since.Add(10 * time.Minute), "jailed since Tue 6 Oct 02:00 WIB · can unjail from 02:10"},
		{"next day", since, since.Add(24*time.Hour + 10*time.Minute), "jailed since Tue 6 Oct 02:00 WIB · can unjail from 7 Oct 02:10"},
		{"ready", since, since.Add(-time.Minute), "jailed since Tue 6 Oct 02:00 WIB · can unjail now"},
		{"exact time", since, since, "jailed since Tue 6 Oct 02:00 WIB · can unjail now"},
		{"unknown since", time.Time{}, since.Add(10 * time.Minute), "can unjail from 02:10"},
		{"unknown since ready", time.Time{}, since, "can unjail now"},
		{"unknown until", since, time.Time{}, "jailed since Tue 6 Oct 02:00 WIB"},
		{"epoch until", since, time.Unix(0, 0), "jailed since Tue 6 Oct 02:00 WIB"},
		{"unknown times", time.Time{}, time.Time{}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatJailDetail(tc.since, tc.until, since, loc); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestJailActiveRestoredID(t *testing.T) {
	c := auditConfig(t)
	val := &ValInfo{Moniker: "validator", Valcons: "cons", Jailed: true, JailedUntil: time.Now().Add(time.Hour)}
	cc := &ChainConfig{name: "Chain", ChainId: "chain-1", valInfo: val}
	cc.Alerts.MissedBlocksAlerts, cc.Alerts.AlertIfInactive = true, true
	c.Chains[cc.name] = cc
	message := "validator is jailed on chain-1"
	alarms.AllAlarms[cc.name] = map[string]time.Time{message: time.Now()}
	alarms.AlarmIDs = map[string]string{cc.name + "\x00" + message: "saved-id"}
	cc.jailAlarms(val, 0, true)
	if len(c.alertChan) != 1 {
		t.Fatalf("reconciled alerts: %d", len(c.alertChan))
	}
	if a := <-c.alertChan; a.resolved || a.uniqueId != "saved-id" {
		t.Fatalf("restored stage %+v", a)
	}
}

func TestLegacyCanUnjailClearsQuietly(t *testing.T) {
	c := auditConfig(t)
	val := &ValInfo{Moniker: "validator", Valcons: "cons", Bonded: true}
	cc := &ChainConfig{name: "Chain", ChainId: "chain-1", valInfo: val}
	cc.Alerts.MissedBlocksAlerts, cc.Alerts.AlertIfInactive = true, true
	c.Chains[cc.name] = cc
	message := "validator can unjail on chain-1"
	alarms.AllAlarms[cc.name] = map[string]time.Time{message: time.Now()}
	alarms.AlarmIDs = map[string]string{cc.name + "\x00" + message: "saved-id"}
	cc.jailAlarms(val, 0, false)
	if len(c.alertChan) != 1 || alarms.getCount(cc.name) != 0 {
		t.Fatalf("legacy cleanup: %d messages, %d alarms", len(c.alertChan), alarms.getCount(cc.name))
	}
	if a := <-c.alertChan; !a.resolved || !a.quiet || a.uniqueId != "saved-id" {
		t.Fatalf("legacy cleanup %+v", a)
	}
}

// TestCloseToJailDoesNotStepBack: a stale signing refresh must retain close-to-jail state while missing.
func TestCloseToJailDoesNotStepBack(t *testing.T) {
	c := auditConfig(t)
	val := &ValInfo{Moniker: "validator", Valcons: "cons", Bonded: true, Missed: 9000, MaxMissed: 9990}
	cc := &ChainConfig{name: "Chain", ChainId: "chain-1", valInfo: val, blockTime: time.Minute}
	cc.Alerts.MissedBlocksAlerts, cc.Alerts.ConsecutiveMissed = true, 5
	c.Chains[cc.name] = cc
	cc.jailAlarms(val, 6, false)
	first := <-c.alertChan
	if first.message != "validator is close to jail on chain-1" {
		t.Fatalf("first %q", first.message)
	}
	for _, change := range []func(){
		func() { val.SigningStale = true },
		func() { val.SigningStale, val.Missed = false, 100 },
	} {
		change()
		cc.jailAlarms(val, 7, false)
		for len(c.alertChan) > 0 {
			if alert := <-c.alertChan; !alert.quiet {
				t.Fatalf("stepped back: %q resolved=%v", alert.message, alert.resolved)
			}
		}
	}
}
