package tenderduty

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/cosmos/cosmos-sdk/types"
	staking "github.com/cosmos/cosmos-sdk/x/staking/types"
)

func TestValidatorChangeComparison(t *testing.T) {
	now := time.Now()
	amount := func(n float64) *float64 { return &n }
	text := func(s string) *string { return &s }
	old := reportSnapshot{At: now.Add(-9 * time.Hour), Stake: amount(100), Symbol: "KII", Self: amount(10)}
	settings := AlertConfig{ChangesDropPercent: 10, ChangesRisePercent: 10}
	for _, tc := range []struct {
		name        string
		stake, self float64
		rise        int
		want        string
	}{
		{"drop over", 87.7, 10, 10, "Stake -12.3% in 9h"},
		{"drop under", 95, 10, 10, ""},
		{"drop equal", 90, 10, 10, ""},
		{"rise", 112, 10, 10, "Stake +12.0% in 9h"},
		{"rise under", 105, 10, 10, ""},
		{"rise equal", 110, 10, 10, ""},
		{"rise off", 112, 10, 0, ""},
		{"self rounding", 100, 9.95, 10, ""},
		{"self down", 100, 9.8, 10, "Self-bond"},
		{"self boundary", 100, 9.9, 10, "Self-bond"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			current := old
			current.At, current.Stake, current.Self = now, amount(tc.stake), amount(tc.self)
			a := settings
			a.ChangesRisePercent = tc.rise
			got := validatorChanges(&old, current, a)
			if tc.want == "" && got != "" || tc.want != "" && !strings.Contains(got, tc.want) {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
	current := old
	current.At = now
	current.Commission, current.MinSelfDelegation = text("0.1"), text("20")
	current.Moniker, current.Identity, current.Website = text("new"), text("id"), text("site")
	current.SecurityContact, current.Details = text("contact"), text("details")
	if got := validatorChanges(&old, current, settings); got != "" {
		t.Fatalf("unknown old fields: %s", got)
	}
	if got := validatorChanges(nil, current, settings); got != "" {
		t.Fatal("no previous snapshot")
	}
	old.Commission, old.MinSelfDelegation, old.Moniker = text("0.05"), text("10"), text("old")
	old.Identity, old.Website, old.SecurityContact, old.Details = text(""), text(""), text(""), text("")
	got := validatorChanges(&old, current, settings)
	for _, want := range []string{"Validator changed\n", "Commission 5%", "10%", "Min self delegation 10", "Moniker old", "Description: identity, website, security contact, details"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %s", want, got)
		}
	}
	// only delegations moved: titled by the stake, no separate span line
	stake := old
	stake.At, stake.Stake = now, amount(700)
	stake.Commission, stake.MinSelfDelegation, stake.Moniker = old.Commission, old.MinSelfDelegation, old.Moniker
	stake.Identity, stake.Website, stake.SecurityContact, stake.Details = old.Identity, old.Website, old.SecurityContact, old.Details
	if got := validatorChanges(&old, stake, settings); got != "Stake changed\nStake +600.0% in 9h · 100 → 700 KII" {
		t.Fatalf("stake only: %q", got)
	}
	if _, state := (&alertMsg{transient: true, message: "Stake changed\nStake +600.0% in 9h · 100 → 700 KII"}).header(); state != "Stake changed" {
		t.Fatalf("header %q", state)
	}
	power := int64(1)
	old.Power = &power
	current = reportSnapshot{At: now, Power: &power, Self: amount(1)}
	if got := validatorChanges(&old, current, settings); got != "" {
		t.Fatalf("mixed units: %s", got)
	}
	old = reportSnapshot{At: now.Add(-time.Hour), Power: &power}
	larger := int64(2)
	current.Power = &larger
	if got := validatorChanges(&old, current, settings); !strings.Contains(got, "power") {
		t.Fatalf("power fallback: %s", got)
	}
}

func TestValidatorChangeEscaping(t *testing.T) {
	msg := &alertMsg{chain: "Chain", transient: true, severity: "info", message: "Validator changed\nMoniker <old&> \u2192 <new&>"}
	got := buildTelegramMessage(msg)
	if strings.Contains(got, "<old&>") || !strings.Contains(got, "&lt;old&amp;&gt;") {
		t.Fatalf("unsafe Telegram text: %s", got)
	}
	fields := msg.card().slackFields()
	if len(fields) != 1 || !strings.Contains(fields[0].Value, "&lt;old&amp;&gt;") {
		t.Fatalf("unsafe Slack text: %+v", fields)
	}
	if !strings.Contains(strings.Join(msg.card().markdownLines(), "\n"), "<old&>") {
		t.Fatal("Discord must use the existing proposal/card text renderer")
	}
	var old reportSnapshot
	if err := json.Unmarshal([]byte(`{"at":"2026-10-01T00:00:00Z"}`), &old); err != nil || old.Moniker != nil {
		t.Fatal("old snapshot compatibility", err)
	}
}

func TestValidatorChangeDefaultsAndOverrides(t *testing.T) {
	c := &Config{Chains: map[string]*ChainConfig{"Default": {}, "Override": {}}}
	data := []byte("chains:\n  Override:\n    alerts:\n      changes_enabled: no\n      changes_drop_percent: 20\n      changes_rise_percent: 0\n      changes_severity: warning\n")
	if err := applyAlertDefaults(c, data, nil); err != nil {
		t.Fatal(err)
	}
	a, b := c.Chains["Default"].Alerts, c.Chains["Override"].Alerts
	if !a.ChangesEnabled || a.ChangesDropPercent != 10 || a.ChangesRisePercent != 10 || a.ChangesSeverity != "info" {
		t.Fatalf("defaults: %+v", a)
	}
	if b.ChangesEnabled || b.ChangesDropPercent != 20 || b.ChangesRisePercent != 0 || b.ChangesSeverity != "warning" {
		t.Fatalf("overrides: %+v", b)
	}
	b.ChangesDropPercent, b.ChangesRisePercent, b.ChangesSeverity = -1, 101, "bogus"
	if problems := validateChanges(&b, "Chain"); len(problems) != 3 || b.ChangesDropPercent != 10 || b.ChangesRisePercent != 10 || b.ChangesSeverity != "info" {
		t.Fatalf("validation: %+v, %v", b, problems)
	}
	keys, _ := configSettingTypes()
	for _, key := range []string{"changes_enabled", "changes_drop_percent", "changes_rise_percent", "changes_severity"} {
		if keys["alert_defaults."+key] == nil || chainSettingTypes()["alerts."+key] == nil {
			t.Fatalf("missing CLI support: %s", key)
		}
	}
	for _, assignment := range []string{"alerts.changes_enabled=no", "alerts.changes_drop_percent=20", "alerts.changes_rise_percent=0", "alerts.changes_severity=warning"} {
		if _, err := parseSetting(chainSettingTypes(), assignment); err != nil {
			t.Fatal(err)
		}
	}
}

func TestValidatorChangeDelivery(t *testing.T) {
	c := auditConfig(t)
	c.stateFile = ""
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte("ok"))
	}))
	defer server.Close()
	c.Slack.Enabled, c.Slack.Webhook = yes(true), server.URL
	c.Pagerduty.Enabled, c.Pagerduty.MinSeverity = yes(true), "critical"
	chain := &ChainConfig{}
	chain.Alerts.Slack = c.Slack
	chain.Alerts.Pagerduty = c.Pagerduty
	c.Chains["Chain"] = chain
	now := time.Now().UTC()
	c.QuietHours = QuietHoursConfig{Enabled: true, Start: now.Add(-time.Hour).Format("15:04"), End: now.Add(time.Hour).Format("15:04"), Timezone: "UTC"}
	if err := c.QuietHours.parse(); err != nil {
		t.Fatal(err)
	}
	c.validatorChangeNotice("Chain", "Validator changed\nCommission 5% to 10%", "info", now)
	if len(alarms.Outbox) != 1 || !alarms.Outbox[0].Transient {
		t.Fatalf("severity routing: %+v", alarms.Outbox)
	}
	key := alarms.Outbox[0].workerKey()
	original := c.ctx
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	c.ctx = ctx
	c.deliverQueue(key)
	c.ctx = original
	if calls.Load() != 0 || len(alarms.Outbox) != 1 {
		t.Fatal("notice escaped quiet hours")
	}
	// Restore from the persisted queue representation, without the in-memory message.
	data, err := json.Marshal(alarms.Outbox)
	if err != nil {
		t.Fatal(err)
	}
	alarms.Outbox = nil
	if err := json.Unmarshal(data, &alarms.Outbox); err != nil {
		t.Fatal(err)
	}
	c.QuietHours.Enabled = false
	c.deliverQueue(key)
	if calls.Load() != 1 || len(alarms.Outbox) != 0 || alarms.getCount("Chain") != 0 || len(alarms.PendingRecoveries) != 0 || len(alarms.SentSlkAlarms) != 0 {
		t.Fatalf("notice retained lifecycle state: calls=%d, alarms=%+v", calls.Load(), alarms)
	}
	c.validatorChangeNotice("Chain", "Validator changed\nCommission 5% to 10%", "info", now.Add(time.Hour))
	c.validatorChangeNotice("Chain", "Validator changed\nCommission 5% to 10%", "info", now.Add(2*time.Hour))
	if len(alarms.Outbox) != 2 {
		t.Fatal("separate snapshots with identical changes were deduplicated")
	}
}

func TestValidatorChangeSnapshotFields(t *testing.T) {
	v := staking.Validator{
		Commission:        staking.Commission{CommissionRates: staking.CommissionRates{Rate: sdk.NewDecWithPrec(5, 2)}},
		MinSelfDelegation: sdk.NewInt(10),
		Description:       staking.Description{Moniker: strings.Repeat("\u754c", 301), Website: "https://example.org"},
	}
	var s reportSnapshot
	s.captureValidatorChanges(v)
	if s.Commission == nil || *s.Commission != "0.050000000000000000" || s.MinSelfDelegation == nil || *s.MinSelfDelegation != "10" {
		t.Fatalf("missing validator rates: %+v", s)
	}
	if s.Moniker == nil || len([]rune(*s.Moniker)) != 300 || s.Identity == nil || *s.Identity != "" || s.Website == nil || *s.Website != v.Description.Website {
		t.Fatalf("missing or unclipped description: %+v", s)
	}
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var restored reportSnapshot
	if err := json.Unmarshal(data, &restored); err != nil || restored.Identity == nil || *restored.Identity != "" {
		t.Fatal("known empty description did not survive restart", err)
	}
	if got := changeRate("0.29"); got != "29%" {
		t.Fatalf("inexact commission: %s", got)
	}
}
