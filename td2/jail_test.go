package tenderduty

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestLegacyJailMessages(t *testing.T) {
	until := time.Now().UTC().Truncate(time.Minute).Add(10 * time.Minute)
	jailed := &alertMsg{chain: "AtomOne", severity: "critical", message: jailMessage(&ValInfo{Moniker: "Validator", Jailed: true, JailedUntil: until}, "atomone-1")}
	if icon, label := jailed.header(); icon != "🚨" || label != "Jailed" {
		t.Fatalf("jailed header %s %s", icon, label)
	}
	if got := buildTelegramMessage(jailed); !strings.Contains(got, "🔒 <b>Status:</b> 🔴 jailed\n🔓 <b>Unjail after:</b> ") || !strings.Contains(got, " · <b>9m left</b>") {
		t.Fatalf("jailed message %q", got)
	}
	jailed.resolved = true
	if _, label := jailed.header(); label != "Active again" {
		t.Fatalf("jailed cleared is %q", label)
	}
	tomb := &alertMsg{chain: "AtomOne", severity: "critical", message: jailMessage(&ValInfo{Moniker: "Validator", Jailed: true, Tombstoned: true}, "atomone-1")}
	if icon, label := tomb.header(); icon != "☠️" || label != "Tombstoned" || !strings.Contains(tomb.summary(), "can't be unjailed") {
		t.Fatalf("tombstoned %s %s %q", icon, label, tomb.summary())
	}
	if got := jailMessage(&ValInfo{Moniker: "Validator", JailedUntil: time.Unix(0, 0)}, "atomone-1"); got != "Validator is no longer in the active set on atomone-1" {
		t.Fatalf("inactive is %q", got)
	}
}

// watchAlerts runs a chain's alarm loop and collects what it sends for a while.
func watchAlerts(t *testing.T, c *Config, chain *ChainConfig) map[string]*alertMsg {
	t.Helper()
	fastWatch(t)
	c.Chains["Chain"] = chain
	c.startWorker(chain.watch)
	got := make(map[string]*alertMsg)
	deadline := time.After(250 * time.Millisecond)
	for {
		select {
		case alert := <-c.alertChan:
			got[alert.message] = alert
		case <-deadline:
			c.cancel()
			c.workers.Wait()
			return got
		}
	}
}

func TestJailedPastUnjailTime(t *testing.T) {
	c := auditConfig(t)
	chain := &ChainConfig{name: "Chain", ChainId: "test-chain", valInfo: &ValInfo{Moniker: "validator", Valcons: "consensus", Jailed: true, Window: 100, JailedUntil: time.Now().Add(-time.Minute).UTC().Truncate(time.Minute)}}
	chain.Alerts.MissedBlocksAlerts, chain.Alerts.AlertIfInactive = true, true
	got := watchAlerts(t, c, chain)
	if len(got) != 1 || got["validator is jailed on test-chain"] == nil || got["validator is jailed on test-chain"].severity != "critical" {
		t.Fatalf("alerts %v", got)
	}
	if summary := got["validator is jailed on test-chain"].summary(); !strings.Contains(summary, "jailed since ") || !strings.HasSuffix(summary, "can unjail now") {
		t.Fatalf("jailed detail %q", summary)
	}
}

func TestTombstonedIsItsOwnAlert(t *testing.T) {
	c := auditConfig(t)
	chain := &ChainConfig{name: "Chain", ChainId: "test-chain", valInfo: &ValInfo{Moniker: "validator", Valcons: "consensus", Jailed: true, Tombstoned: true, Window: 100, JailedUntil: time.Now().Add(-time.Minute)}}
	chain.Alerts.MissedBlocksAlerts, chain.Alerts.AlertIfInactive = true, true
	got := watchAlerts(t, c, chain)
	if len(got) != 1 || got["validator is no longer in the active set on test-chain"] == nil {
		t.Fatalf("a tombstoned validator sent %v", got)
	}
}

func TestOldInactiveAlarmClearsWhenActive(t *testing.T) {
	c := auditConfig(t)
	old := "validator is no longer active: validator is jailed"
	alarms.AllAlarms["Chain"] = map[string]time.Time{old: time.Now()}
	chain := &ChainConfig{name: "Chain", ChainId: "test-chain", valInfo: &ValInfo{Moniker: "validator", Valcons: "consensus", Bonded: true, Window: 100}}
	chain.Alerts.MissedBlocksAlerts, chain.Alerts.AlertIfInactive = true, true
	got := watchAlerts(t, c, chain)
	if alert := got[old]; alert == nil || !alert.resolved {
		t.Fatalf("the alarm saved by an older version wasn't cleared: %v", got)
	}
}

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

// devnet 17 Oct: signing info said 1 miss while 8 in a row were seen, and the jail was noticed 53s late.
func TestJailDetailsFromStaleSigningInfo(t *testing.T) {
	c := auditConfig(t)
	val := &ValInfo{Moniker: "validator", Valcons: "cons", Bonded: true, Missed: 1, MaxMissed: 10}
	cc := &ChainConfig{name: "Chain", ChainId: "chain-1", valInfo: val, blockTime: time.Second, statConsecutiveMiss: 8}
	c.Chains[cc.name] = cc
	if got := jailDetail(cc.name, "validator is close to jail on chain-1"); !strings.HasPrefix(got, "8 of 10 misses used") {
		t.Fatal(got)
	}
	now := time.Date(2026, 10, 16, 17, 41, 45, 0, time.UTC)
	jailed := &ValInfo{JailedUntil: now.Add(7 * time.Second), JailDuration: time.Minute}
	if got := jailStart(jailed, now); !got.Equal(now.Add(-53 * time.Second)) {
		t.Fatalf("jail start %s", got)
	}
	// an old jailed_until, e.g. jailed for low self-delegation: when it was noticed
	jailed.JailedUntil = now.Add(-time.Hour)
	if got := jailStart(jailed, now); !got.Equal(now) {
		t.Fatalf("old jailed_until %s", got)
	}
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
		if a.jailRiskEnabled() != tc.risk || a.jailInactiveEnabled() != tc.inactive || a.JailRiskPercent != 90 && tc.risk || a.JailRiskMinutes != 90 && tc.risk {
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
			dot    string
		}{
			{ValInfo{Bonded: true, Jailed: true, JailedUntil: since.Add(10 * time.Minute)}, since, "🚨 Jailed since 6 Oct 02:00", dotRed},
			{ValInfo{Bonded: true, Jailed: true, JailedUntil: since.Add(10 * time.Minute)}, time.Time{}, "🚨 Jailed", dotRed},
			{ValInfo{Tombstoned: true}, since, "🚨 Tombstoned", dotRed},
			{ValInfo{}, time.Time{}, "⚪ Not in the active set", dotWhite},
		} {
			c := auditConfig(t)
			c.Reports.Timezone = "UTC+7"
			c.Chains["Chain"] = &ChainConfig{name: "Chain", valInfo: &tc.info, jailedSince: tc.since}
			r := c.buildReport(time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC), weekly, false, nil)
			if r.Rows[0].Dot != tc.dot || !containsMessage(r.Rows[0].Lines, tc.status) || !strings.Contains(r.telegram(), tc.status) || strings.Contains(r.telegram(), "unjail") {
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

func TestMissedRecoveryWaitsForAQuietRun(t *testing.T) {
	c := auditConfig(t)
	val := &ValInfo{Moniker: "validator", Valcons: "cons", Bonded: true, Missed: 100, MaxMissed: 9990}
	cc := &ChainConfig{name: "Chain", ChainId: "chain-1", valInfo: val, observedBlock: true, blockTime: 6 * time.Second}
	cc.Alerts.MissedBlocksAlerts, cc.Alerts.ConsecutiveAlerts, cc.Alerts.ConsecutiveMissed = true, true, 5
	c.Chains[cc.name] = cc
	const message = "validator is missing blocks on chain-1"
	cc.jailAlarms(val, 5, false)
	if !alarmIsActive(cc.name, message) {
		t.Fatal("missing blocks alarm not raised")
	}
	cc.jailAlarms(val, 0, false)
	if !alarmIsActive(cc.name, message) {
		t.Fatal("recovered on the first signed block")
	}
	cc.stateMux.Lock()
	cc.signedSince = time.Now().Add(-3 * time.Minute)
	cc.stateMux.Unlock()
	cc.jailAlarms(val, 5, false)
	cc.jailAlarms(val, 0, false)
	if !alarmIsActive(cc.name, message) {
		t.Fatal("a new run of misses did not restart the wait")
	}
	cc.stateMux.Lock()
	cc.signedSince = time.Now().Add(-3 * time.Minute)
	cc.stateMux.Unlock()
	cc.jailAlarms(val, 2, false)
	if alarmIsActive(cc.name, message) {
		t.Fatal("a few misses below the threshold held the recovery")
	}
}

func TestJailRiskFastChainNeedsConsecutiveThreshold(t *testing.T) {
	c := auditConfig(t)
	val := &ValInfo{Moniker: "validator", Valcons: "cons", Bonded: true, Missed: 18, MaxMissed: 5000}
	cc := &ChainConfig{name: "Chain", ChainId: "chain-1", valInfo: val, blockTime: 400 * time.Millisecond}
	cc.Alerts.MissedBlocksAlerts = true
	c.Chains[cc.name] = cc
	cc.jailAlarms(val, 1, false)
	if len(c.alertChan) != 0 || alarms.getCount(cc.name) != 0 {
		t.Fatal("single miss raised an alarm on a fast chain")
	}
	cc.jailAlarms(val, 5, false)
	select {
	case alert := <-c.alertChan:
		if alert.message != "validator is close to jail on chain-1" || alert.severity != "critical" || alert.resolved {
			t.Fatalf("threshold alarm: %+v", alert)
		}
	default:
		t.Fatal("consecutive threshold did not raise jail risk")
	}
}

func TestJailStagesKeepIncidentStart(t *testing.T) {
	c := auditConfig(t)
	val := &ValInfo{Moniker: "validator", Valcons: "cons", Bonded: true, Missed: 9000, MaxMissed: 9990}
	cc := &ChainConfig{name: "Chain", ChainId: "chain-1", valInfo: val}
	cc.Alerts.MissedBlocksAlerts, cc.Alerts.AlertIfInactive = true, true
	c.Chains[cc.name] = cc
	cc.jailAlarms(val, 0, false)
	first := <-c.alertChan
	start := time.Now().Add(-2 * time.Minute)
	alarms.notifyMux.Lock()
	alarms.AllAlarms[cc.name][first.message] = start
	alarms.notifyMux.Unlock()
	val.Jailed = true
	cc.jailAlarms(val, 0, false)
	jailed := "validator is jailed on chain-1"
	if got := alarms.AllAlarms[cc.name][jailed]; !got.Equal(start) {
		t.Fatalf("stage start %s, want %s", got, start)
	}
	saved, err := json.Marshal(alarms.AllAlarms)
	if err != nil {
		t.Fatal(err)
	}
	alarms.AllAlarms = make(map[string]map[string]time.Time)
	if err := json.Unmarshal(saved, &alarms.AllAlarms); err != nil {
		t.Fatal(err)
	}
	// Reconcile the saved stage as after a restart, without resetting its start.
	cc.jailAlarms(val, 0, true)
	if got := alarms.AllAlarms[cc.name][jailed]; !got.Equal(start) {
		t.Fatalf("restored start %s", got)
	}
	val.Jailed, val.Missed = false, 0
	cc.jailAlarms(val, 0, false)
	found := false
	for len(c.alertChan) > 0 {
		a := <-c.alertChan
		if a.resolved && !a.quiet {
			found = true
			if a.lasted < 2*time.Minute || a.uniqueId != first.uniqueId {
				t.Fatalf("recovery lasted %s, id %s", a.lasted, a.uniqueId)
			}
		}
	}
	if !found {
		t.Fatal("no recovery")
	}
	for _, incident := range c.reportData.Incidents {
		if incident.Key == jailed && !incident.Start.Equal(start) {
			t.Fatalf("report start %s", incident.Start)
		}
	}
}
