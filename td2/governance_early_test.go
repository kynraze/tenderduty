package tenderduty

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestGovEarlyReminderTiming(t *testing.T) {
	start := time.Now().UTC().Truncate(time.Minute)
	for _, tc := range []struct {
		name    string
		period  time.Duration
		left    time.Duration
		percent int
		final   time.Duration
		noStart bool
		voted   bool
		want    bool
	}{
		{"7 days", 7 * 24 * time.Hour, 42 * time.Hour, 25, time.Hour, false, false, true},
		{"2 days", 2 * 24 * time.Hour, 12 * time.Hour, 25, time.Hour, false, false, true},
		{"1 day", 24 * time.Hour, 6 * time.Hour, 25, time.Hour, false, false, true},
		{"3 hours", 3 * time.Hour, 45 * time.Minute, 25, time.Hour, false, false, false},
		{"no start", 24 * time.Hour, 6 * time.Hour, 25, time.Hour, true, false, false},
		{"already voted", 24 * time.Hour, 6 * time.Hour, 25, time.Hour, false, true, false},
		{"off", 24 * time.Hour, 6 * time.Hour, 0, time.Hour, false, false, false},
		{"near voting start", 24 * time.Hour, 23 * time.Hour, 99, time.Hour, false, false, false},
		{"near final", 24 * time.Hour, 6 * time.Hour, 25, 5 * time.Hour, false, false, false},
		{"two hour gap", 12 * time.Hour, 3 * time.Hour, 25, time.Hour, false, false, true},
		{"two hours after start", 4 * time.Hour, 2 * time.Hour, 50, 0, false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := govProposal{VotingStart: start, VotingEnd: start.Add(tc.period), Voted: tc.voted}
			if tc.noStart {
				p.VotingStart = time.Time{}
			}
			now := p.VotingEnd.Add(-tc.left)
			if got := govEarlyReminderDue(p, now, tc.percent, tc.final); got != tc.want {
				t.Fatalf("due = %v, want %v", got, tc.want)
			}
			if tc.want && govEarlyReminderDue(p, now.Add(-time.Nanosecond), tc.percent, tc.final) {
				t.Fatal("reminded before the threshold")
			}
			if govEarlyReminderDue(p, p.VotingEnd.Add(-tc.final), tc.percent, tc.final) {
				t.Fatal("early reminder overlapped final reminder")
			}
		})
	}
}

func govEarlyTestChain(t *testing.T) (*Config, *ChainConfig, *ValInfo, govProposal) {
	t.Helper()
	c := auditConfig(t)
	cc := &ChainConfig{name: "Chain", ChainId: "chain-1", proposalsLoaded: true,
		ValAddress: "atonevaloper1zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3dqqvw0"}
	cc.Alerts.GovSeverity = "info"
	cc.Alerts.GovEarlyRemindPercent, cc.Alerts.GovRemindMinutes = 25, 60
	c.Chains[cc.name] = cc
	alarms.GovSeen = map[string]bool{cc.name: true}
	p := govProposal{ID: 7, Title: "Raise the limit", VotingStart: time.Now().UTC().Truncate(time.Minute)}
	p.VotingEnd = p.VotingStart.Add(7 * 24 * time.Hour)
	cc.proposals = []govProposal{p}
	return c, cc, &ValInfo{Valcons: "cons", Moniker: "Kynraze"}, p
}

func drainGovAlerts(c *Config) []*alertMsg {
	var got []*alertMsg
	for {
		select {
		case a := <-c.alertChan:
			got = append(got, a)
		default:
			return got
		}
	}
}

func expectGovRaised(t *testing.T, c *Config, wanted map[string]string) {
	t.Helper()
	got := drainGovAlerts(c)
	if len(got) != len(wanted) {
		t.Fatalf("got %d alerts, want %d", len(got), len(wanted))
	}
	for _, a := range got {
		severity, ok := wanted[a.message]
		if !ok || a.resolved || a.severity != severity {
			t.Fatalf("unexpected alert %+v", a)
		}
		delete(wanted, a.message)
	}
	if len(wanted) != 0 {
		t.Fatalf("missing alerts %v", wanted)
	}
}

func TestGovVoteBeforeEarlyReminder(t *testing.T) {
	c, cc, val, p := govEarlyTestChain(t)
	cc.proposals[0].Voted = true
	cc.govAlarmsAt(val, false, p.VotingEnd.Add(-42*time.Hour))
	expectGovRaised(t, c, map[string]string{})
	cc.govAlarmsAt(val, false, p.VotingEnd.Add(-time.Hour))
	expectGovRaised(t, c, map[string]string{})
}

func TestGovVoteBetweenReminders(t *testing.T) {
	c, cc, val, p := govEarlyTestChain(t)
	open, early, final := cc.govOpenMessage(p), cc.govEarlyRemindMessage(p, val.Moniker), cc.govRemindMessage(p, val.Moniker)
	cc.govAlarmsAt(val, false, p.VotingEnd.Add(-42*time.Hour))
	expectGovRaised(t, c, map[string]string{open: "info", early: "warning"})
	cc.proposals[0].Voted = true
	cc.govAlarmsAt(val, false, p.VotingEnd.Add(-24*time.Hour))
	expectOne(t, c, early, open)
	msg := &alertMsg{chain: cc.name, message: early, resolved: true}
	if _, label := msg.header(); label != "Voted" || !strings.Contains(msg.summary(), val.Moniker+" voted") {
		t.Fatalf("closing = %q, %q", label, msg.summary())
	}
	cc.govAlarmsAt(val, false, p.VotingEnd.Add(-time.Hour))
	expectGovRaised(t, c, map[string]string{})
	if alarmIsActive(cc.name, final) || len(activeMessages(cc.name, "Proposal #")) != 0 {
		t.Fatal("voted proposal still has alarms")
	}
}

func TestGovNeverVotedReminderLifecycle(t *testing.T) {
	for _, outcome := range []string{"Passed", "Rejected"} {
		t.Run(outcome, func(t *testing.T) {
			c, cc, val, p := govEarlyTestChain(t)
			open, early, final := cc.govOpenMessage(p), cc.govEarlyRemindMessage(p, val.Moniker), cc.govRemindMessage(p, val.Moniker)
			cc.govAlarmsAt(val, false, p.VotingEnd.Add(-42*time.Hour-time.Nanosecond))
			expectGovRaised(t, c, map[string]string{open: "info"})
			cc.govAlarmsAt(val, false, p.VotingEnd.Add(-42*time.Hour))
			expectGovRaised(t, c, map[string]string{early: "warning"})
			cc.govAlarmsAt(val, false, p.VotingEnd.Add(-time.Hour))
			got := drainGovAlerts(c)
			if len(got) != 2 {
				t.Fatalf("handoff produced %d alerts", len(got))
			}
			for _, a := range got {
				if a.resolved {
					if a.message != early || !a.quiet {
						t.Fatalf("early reminder did not resolve quietly: %+v", a)
					}
				} else if a.message != final || a.severity != "critical" {
					t.Fatalf("unexpected final reminder: %+v", a)
				}
			}
			if alarmIsActive(cc.name, early) || !alarmIsActive(cc.name, final) {
				t.Fatal("wrong active reminder after handoff")
			}
			cc.proposals = nil
			govOutcomes.Store("Chain#7", outcome)
			defer govOutcomes.Delete("Chain#7")
			cc.govAlarmsAt(val, false, p.VotingEnd.Add(time.Minute))
			expectOne(t, c, final, open)
			msg := &alertMsg{chain: cc.name, message: final, resolved: true}
			text := buildTelegramMessage(msg)
			if _, label := msg.header(); label != outcome || !strings.Contains(text, "no vote cast") || !strings.Contains(text, strings.ToLower(outcome)) {
				t.Fatalf("closing = %q, %q", label, text)
			}
		})
	}
}

func TestGovRestoredRemindersCloseOnce(t *testing.T) {
	c, cc, val, p := govEarlyTestChain(t)
	open, early, final := cc.govOpenMessage(p), cc.govEarlyRemindMessage(p, val.Moniker), cc.govRemindMessage(p, val.Moniker)
	alarms.AllAlarms[cc.name] = map[string]time.Time{open: time.Now(), early: time.Now(), final: time.Now()}
	cc.proposals[0].Voted = true
	cc.govAlarmsAt(val, false, p.VotingEnd.Add(-time.Minute))
	expectOne(t, c, final, open, early)
}

func TestGovEarlyReminderCardAndID(t *testing.T) {
	_, cc, val, p := govEarlyTestChain(t)
	early, final := cc.govEarlyRemindMessage(p, val.Moniker), cc.govRemindMessage(p, val.Moniker)
	if govAlarmId(val, early) != "consgovearly7" || govAlarmId(val, final) != "consgovremind7" {
		t.Fatal("reminder IDs are not distinct")
	}
	msg := &alertMsg{chain: cc.name, message: early, severity: "warning"}
	text := buildTelegramMessage(msg)
	for _, want := range []string{"Vote reminder", "<b>Validator:</b> Kynraze", "<b>Vote:</b>", "not voted yet", "<b>Severity:</b> warning"} {
		if !strings.Contains(text, want) {
			t.Fatalf("card missing %q: %q", want, text)
		}
	}
}

func TestGovEarlyReminderDefaults(t *testing.T) {
	for _, tc := range []struct {
		name    string
		setting string
		want    int
		warn    bool
	}{
		{"default", "", 25, false},
		{"off", "gov_early_remind_percent: 0", 0, false},
		{"minimum", "gov_early_remind_percent: 1", 1, false},
		{"maximum", "gov_early_remind_percent: 99", 99, false},
		{"negative", "gov_early_remind_percent: -1", 25, true},
		{"too large", "gov_early_remind_percent: 100", 25, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := auditConfig(t)
			cc := &ChainConfig{ChainId: "chain-1", ValAddress: "valoper", PublicFallback: true}
			c.Chains["Chain"] = cc
			data := []byte(fmt.Sprintf("alert_defaults:\n  %s\n", tc.setting))
			if err := applyAlertDefaults(c, data, nil); err != nil {
				t.Fatal(err)
			}
			_, problems := validateConfig(c)
			warned := strings.Contains(strings.Join(problems, "\n"), "gov_early_remind_percent")
			if cc.Alerts.GovEarlyRemindPercent != tc.want || warned != tc.warn {
				t.Fatalf("percent = %d, warnings %v", cc.Alerts.GovEarlyRemindPercent, problems)
			}
		})
	}
	c := &Config{Chains: map[string]*ChainConfig{"Chain": {}}}
	data := []byte("alert_defaults:\n  gov_early_remind_percent: 40\nchains:\n  Chain:\n    alerts:\n      gov_early_remind_percent: 0\n")
	if err := applyAlertDefaults(c, data, nil); err != nil || c.Chains["Chain"].Alerts.GovEarlyRemindPercent != 0 {
		t.Fatalf("chain override lost: %+v, %v", c.Chains["Chain"].Alerts, err)
	}
}

func TestGovShortOrUnknownVotesOnlyGetFinalReminder(t *testing.T) {
	for _, noStart := range []bool{false, true} {
		t.Run(fmt.Sprintf("no start %v", noStart), func(t *testing.T) {
			c, cc, val, p := govEarlyTestChain(t)
			p.VotingEnd = p.VotingStart.Add(3 * time.Hour)
			if noStart {
				p.VotingEnd = p.VotingStart.Add(7 * 24 * time.Hour)
				p.VotingStart = time.Time{}
			}
			cc.proposals[0] = p
			cc.govAlarmsAt(val, false, p.VotingEnd.Add(-2*time.Hour))
			expectGovRaised(t, c, map[string]string{cc.govOpenMessage(p): "info"})
			cc.govAlarmsAt(val, false, p.VotingEnd.Add(-time.Hour))
			expectGovRaised(t, c, map[string]string{cc.govRemindMessage(p, val.Moniker): "critical"})
			if alarmIsActive(cc.name, cc.govEarlyRemindMessage(p, val.Moniker)) {
				t.Fatal("unexpected early reminder")
			}
		})
	}
}
