package tenderduty

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Small slips an operator makes: Gotify's full endpoint as the url, "Gotify" capitalized, 0 meaning off.
func TestOperatorSlips(t *testing.T) {
	auditConfig(t)
	var path string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { path = r.URL.Path }))
	defer server.Close()
	msg := &alertMsg{chain: "Chain", message: "stalled", severity: "critical", gotifyURL: server.URL + "/message/", gotifyToken: "t"}
	if err := postGotify(msg); err != nil || path != "/message" {
		t.Fatalf("posted to %q: %v", path, err)
	}

	c := &Config{Gotify: GotifyConfig{Enabled: yes(true), URL: server.URL, Token: "t"}, Escalation: EscalationConfig{Enabled: true, To: " Gotify ", MinSeverity: "Critical"}}
	if problems := validateEscalation(c); len(problems) != 0 || !c.Escalation.Enabled || c.Escalation.To != "gotify" {
		t.Fatalf("capitalized escalation settings: %v %+v", problems, c.Escalation)
	}

	stake := func(v float64) *float64 { return &v }
	at := time.Now()
	old, now := reportSnapshot{At: at, Stake: stake(1000), Symbol: "TIA"}, reportSnapshot{At: at.Add(time.Hour), Stake: stake(999), Symbol: "TIA"}
	if got := validatorChanges(&old, now, AlertConfig{}); got != "" {
		t.Fatalf("drop and rise percents at 0 still reported: %q", got)
	}
}

// min_severity: escalation keeps a destination for escalations only; it means nothing elsewhere.
func TestMinSeverityEscalationIsForDestinations(t *testing.T) {
	text := reflect.TypeOf("")
	keys := map[string]reflect.Type{"pagerduty.min_severity": text, "alerts.gotify.min_severity": text, "quiet_hours.min_severity": text, "escalation.min_severity": text}
	for _, ok := range []string{"pagerduty.min_severity=escalation", "alerts.gotify.min_severity=escalation"} {
		if _, err := parseSetting(keys, ok); err != nil {
			t.Fatalf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"quiet_hours.min_severity=escalation", "escalation.min_severity=escalation"} {
		if _, err := parseSetting(keys, bad); err == nil {
			t.Fatalf("%s was accepted", bad)
		}
	}
}

// An operator with Telegram only sets escalation to telegram: alerts still arrive, the escalation repeats them.
func TestEscalationToTheMainChannelRepeatsAlerts(t *testing.T) {
	h := newEscalationHarness(t)
	cc := h.chain("Celestia")
	h.c.Gotify.Enabled = nil
	h.c.Telegram = TeleConfig{Enabled: yes(true), ApiKey: "bot", Channel: "1"}
	cc.Alerts.Telegram = TeleConfig{ApiKey: "bot", Channel: "1"}
	h.c.Escalation.To = "telegram"
	now := time.Now()
	h.raise("Celestia", h.nodeDown("Celestia"), "critical", now.Add(-31*time.Minute))
	if out := h.outbox(); len(out) != 1 || out[0].Destination != tg || out[0].Escalated {
		t.Fatalf("the raise did not reach Telegram: %+v", out)
	}
	alarms.SentTgAlarms["Celestia\x00"+h.nodeDown("Celestia")] = now // delivered
	alarms.Outbox = nil
	h.c.checkEscalations(now)
	h.c.checkEscalations(now)
	if out := h.outbox(); len(out) != 1 || !out[0].Escalated {
		t.Fatalf("want one escalation on top of the delivered raise: %+v", out)
	}
}

// A testnet with a small slashing window jails 10 minutes after the misses start.
func TestCloseToJailEscalatesBeforeTheJail(t *testing.T) {
	h := newEscalationHarness(t)
	cc := h.chain("Testnet")
	cc.blockTime = 6 * time.Second
	cc.valInfo = &ValInfo{Moniker: "Kynraze", Bonded: true, Window: 200, MaxMissed: 100, Missed: 50} // 50 misses × 6s = 5 min left
	message := "Kynraze is close to jail on testnet-1"
	now := time.Now()
	h.raise("Testnet", message, "critical", now.Add(-4*time.Minute))
	h.c.checkEscalations(now)
	if len(h.outbox()) != 0 {
		t.Fatal("escalated after 4 minutes with 5 left")
	}
	h.c.checkEscalations(now.Add(time.Minute))
	if len(h.outbox()) != 1 {
		t.Fatal("5 minutes in with 5 minutes left to jail, nothing was escalated")
	}
}

// The RPC was down for the hourly snapshot while a big delegator left.
func TestStakeDropAcrossAnRPCOutageIsReported(t *testing.T) {
	stake := func(v float64) *float64 { return &v }
	at := time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)
	snapshots := []reportSnapshot{{At: at, Stake: stake(1000), Symbol: "TIA"}, {At: at.Add(time.Hour)}}
	after := reportSnapshot{At: at.Add(2 * time.Hour), Stake: stake(500), Symbol: "TIA"}
	got := validatorChanges(lastReadSnapshot(snapshots), after, AlertConfig{ChangesDropPercent: 10, ChangesRisePercent: 10})
	if !strings.Contains(got, "Stake -50.0% in 2h") {
		t.Fatalf("stake 1000 -> 500 TIA across an empty snapshot: %q", got)
	}
	if lastReadSnapshot([]reportSnapshot{{At: at}}) != nil {
		t.Fatal("an empty snapshot was used")
	}
}

// A signal upgrade scheduled days ahead with the node not ready: only "is close" escalates.
func TestScheduledUpgradeEscalatesWhenClose(t *testing.T) {
	h := newEscalationHarness(t)
	h.chain("Celestia") // its own node has no readable version: not ready
	now := time.Now()
	h.raise("Celestia", signalUpgradeMessage(10, 15116658), "critical", now.Add(-time.Hour))
	alarms.Outbox = nil
	h.c.checkEscalations(now)
	if out := h.outbox(); len(out) != 0 {
		t.Fatalf("a scheduled upgrade was escalated: %+v", out)
	}
	h.raise("Celestia", signalUpgradeSoonMessage(10, 15116658), "critical", now.Add(-31*time.Minute))
	alarms.Outbox = nil
	h.c.checkEscalations(now)
	if out := h.outbox(); len(out) != 1 || !out[0].Escalated {
		t.Fatalf("want the close reminder escalated: %+v", out)
	}
}

// tenderduty restarts with a node-down alarm saved 40 minutes ago; the monitor hasn't checked it yet.
func TestRestoredAlarmIsCheckedBeforeEscalating(t *testing.T) {
	h := newEscalationHarness(t)
	cc := h.chain("Celestia")
	cc.restoredChecked = false
	now := time.Now()
	h.raise("Celestia", h.nodeDown("Celestia"), "critical", processStarted.Add(-40*time.Minute))
	alarms.Outbox = nil
	h.c.checkEscalations(now)
	if len(h.outbox()) != 0 {
		t.Fatal("a restored alarm was escalated before the monitor checked it")
	}
	h.c.checkEscalations(processStarted.Add(10 * time.Minute))
	if len(h.outbox()) != 1 {
		t.Fatal("a restored alarm the monitor never checked was not escalated after 10 minutes")
	}
	alarms.Outbox, alarms.Escalated = nil, nil
	cc.restoredChecked = true
	h.c.checkEscalations(now)
	if len(h.outbox()) != 1 {
		t.Fatal("a checked restored alarm was not escalated")
	}
}
