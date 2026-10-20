package tenderduty

import (
	"encoding/json"
	"fmt"
	dash "github.com/kynraze/tenderduty/v2/td2/dashboard"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// escalationHarness sends escalations to a fake gotify server, and records what it receives.
type escalationHarness struct {
	t      *testing.T
	c      *Config
	server *httptest.Server

	mux      sync.Mutex
	status   int
	attempts int
	got      []GotifyMessage
}

func newEscalationHarness(t *testing.T) *escalationHarness {
	t.Helper()
	h := &escalationHarness{t: t, status: http.StatusOK}
	h.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m GotifyMessage
		_ = json.NewDecoder(r.Body).Decode(&m)
		h.mux.Lock()
		defer h.mux.Unlock()
		h.attempts++
		if h.status == http.StatusOK {
			h.got = append(h.got, m)
		}
		w.WriteHeader(h.status)
	}))
	t.Cleanup(h.server.Close)
	h.c = auditConfig(t)
	h.c.NodeDownMin, h.c.NodeDownSeverity = 3, "critical"
	h.c.Gotify = GotifyConfig{Enabled: yes(true), URL: h.server.URL, Token: gotifyTestToken}
	h.c.Escalation = EscalationConfig{Enabled: true, AfterMinutes: 30, To: "gotify", MinSeverity: "critical"}
	return h
}

// chain adds a validator with one node. Its gotify settings are the top level ones, as validateConfig fills them in.
func (h *escalationHarness) chain(name string) *ChainConfig {
	cc := &ChainConfig{name: name, ChainId: strings.ToLower(name) + "-1", ValAddress: strings.ToLower(name) + "valoper", observedBlock: true,
		Nodes: []*NodeConfig{{Url: "http://" + strings.ToLower(name) + ":26657", AlertIfDown: true}}, restoredChecked: true}
	cc.Alerts.JailRiskMinutes = 90
	cc.Alerts.Gotify = GotifyConfig{URL: h.server.URL, Token: gotifyTestToken, MinSeverity: "escalation"}
	h.c.Chains[name] = cc
	return cc
}

func (h *escalationHarness) nodeDown(chain string) string {
	cc := h.c.Chains[chain]
	return nodeDownMessage(cc.Nodes[0].Url, cc.ChainId)
}

// raise records an alarm that started at started, the way the monitor does.
func (h *escalationHarness) raise(chain, message, severity string, started time.Time) {
	id := chain + "|" + message
	h.c.recordSince(chain, message, severity, false, false, &id, started)
}

func (h *escalationHarness) resolve(chain, message string) {
	id := chain + "|" + message
	h.c.alert(chain, message, "info", true, &id)
}

func (h *escalationHarness) outbox() []queuedNotification {
	alarms.notifyMux.RLock()
	defer alarms.notifyMux.RUnlock()
	return append([]queuedNotification(nil), alarms.Outbox...)
}

func (h *escalationHarness) received() []GotifyMessage {
	h.mux.Lock()
	defer h.mux.Unlock()
	return append([]GotifyMessage(nil), h.got...)
}

func (h *escalationHarness) setStatus(status int) {
	h.mux.Lock()
	defer h.mux.Unlock()
	h.status = status
}

func (h *escalationHarness) attemptCount() int {
	h.mux.Lock()
	defer h.mux.Unlock()
	return h.attempts
}

func (h *escalationHarness) run() { h.c.startWorker(h.c.runNotifications) }

func (h *escalationHarness) waitFor(what string, ok func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			h.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitReceived waits until the outbox is empty and n messages arrived.
func (h *escalationHarness) waitReceived(n int) []GotifyMessage {
	h.t.Helper()
	h.waitFor("the outbox to drain", func() bool { return len(h.outbox()) == 0 && len(h.received()) >= n })
	return h.received()
}

func TestEscalationSendsOnceAndResolvesToTheSameDestination(t *testing.T) {
	h := newEscalationHarness(t)
	h.chain("Devnet1")
	msg := h.nodeDown("Devnet1")
	now := time.Now()
	h.raise("Devnet1", msg, "critical", now.Add(-31*time.Minute))
	if n := len(h.outbox()); n != 0 {
		t.Fatalf("a raise went to the escalation destination normally: %d queued", n)
	}
	h.c.checkEscalations(now)
	h.c.checkEscalations(now)
	if out := h.outbox(); len(out) != 1 || out[0].Destination != gt || out[0].Resolved {
		t.Fatalf("outbox %+v, want one escalation to gotify", out)
	}
	h.run()
	got := h.waitReceived(1)
	if got[0].Priority != 10 || !strings.Contains(got[0].Title, "Escalated") || !strings.Contains(got[0].Message, "Unresolved for: 31 min") {
		t.Fatalf("escalation %+v", got[0])
	}
	h.c.checkEscalations(now.Add(time.Minute))
	if n := len(h.outbox()); n != 0 {
		t.Fatalf("escalated twice: %d queued after the send", n)
	}
	h.resolve("Devnet1", msg)
	got = h.waitReceived(2)
	if len(got) != 2 || got[1].Priority != 2 || !strings.Contains(got[1].Title, "Resolved") {
		t.Fatalf("resolve did not reach gotify: %+v", got)
	}
}

func TestEscalationWaitsForAfterMinutes(t *testing.T) {
	h := newEscalationHarness(t)
	h.chain("Devnet1")
	msg := h.nodeDown("Devnet1")
	now := time.Now()
	h.raise("Devnet1", msg, "critical", now.Add(-29*time.Minute))
	h.c.checkEscalations(now)
	if n := len(h.outbox()); n != 0 {
		t.Fatalf("escalated at 29 minutes: %d queued", n)
	}
	h.c.checkEscalations(now.Add(2 * time.Minute))
	if n := len(h.outbox()); n != 1 {
		t.Fatalf("not escalated at 31 minutes: %d queued", n)
	}
}

func TestEscalationClockRestartsWhenAnAlarmIsRaisedAgain(t *testing.T) {
	h := newEscalationHarness(t)
	h.chain("Devnet1")
	msg := h.nodeDown("Devnet1")
	first := time.Now().Add(-51 * time.Minute)
	h.raise("Devnet1", msg, "critical", first)
	h.resolve("Devnet1", msg)
	h.raise("Devnet1", msg, "critical", first.Add(20*time.Minute))
	h.c.checkEscalations(first.Add(31 * time.Minute))
	if n := len(h.outbox()); n != 0 {
		t.Fatalf("escalated 31 minutes after the first start, 11 after the second: %d queued", n)
	}
	h.c.checkEscalations(first.Add(50 * time.Minute))
	if n := len(h.outbox()); n != 1 {
		t.Fatalf("not escalated 30 minutes after the second start: %d queued", n)
	}
}

func TestEscalationSkipsAValidatorNotInTheActiveSetWhileNotBonded(t *testing.T) {
	h := newEscalationHarness(t)
	cc := h.chain("Devnet1")
	cc.valInfo = &ValInfo{Moniker: "val", Bonded: false}
	msg := "val is no longer in the active set on devnet1-1"
	now := time.Now()
	h.raise("Devnet1", msg, "critical", now.Add(-60*time.Minute))
	h.c.checkEscalations(now)
	if n := len(h.outbox()); n != 0 {
		t.Fatalf("an unbonded validator out of the set was escalated: %d queued", n)
	}
	cc.valInfo.Bonded = true
	h.c.checkEscalations(now)
	if n := len(h.outbox()); n != 1 {
		t.Fatalf("a bonded validator out of the set was not escalated: %d queued", n)
	}
}

func TestEscalationSignalUpgradeNoticesAndReminders(t *testing.T) {
	scheduled := "Version 10 upgrade at height 12,345,678"
	t.Run("scheduled upgrade with own nodes ready", func(t *testing.T) {
		h := newEscalationHarness(t)
		cc := h.chain("Devnet1")
		cc.Nodes[0].binaryReadable, cc.Nodes[0].binaryVersion = true, "v10.0.0"
		now := time.Now()
		h.raise("Devnet1", scheduled, "critical", now.Add(-40*time.Minute))
		h.c.checkEscalations(now)
		if n := len(h.outbox()); n != 0 {
			t.Fatalf("a scheduled upgrade with nodes ready was escalated: %d queued", n)
		}
	})
	t.Run("reminder with a node not ready", func(t *testing.T) {
		h := newEscalationHarness(t)
		cc := h.chain("Devnet1")
		cc.Nodes[0].binaryReadable, cc.Nodes[0].binaryVersion = true, "v9.0.0"
		reminder := scheduled + " is close"
		now := time.Now()
		h.raise("Devnet1", reminder, "critical", now.Add(-40*time.Minute))
		h.c.checkEscalations(now)
		if out := h.outbox(); len(out) != 1 || out[0].Message != reminder {
			t.Fatalf("reminder not escalated: %+v", out)
		}
	})
}

func TestEscalationMinSeverityAndCloseToJail(t *testing.T) {
	h := newEscalationHarness(t)
	h.chain("Devnet1")
	now := time.Now()
	h.raise("Devnet1", "val is missing blocks on devnet1-1", "warning", now.Add(-40*time.Minute))
	h.c.checkEscalations(now)
	if n := len(h.outbox()); n != 0 {
		t.Fatalf("a warning escalated under min_severity critical: %d queued", n)
	}
	closeToJail := "val is close to jail on devnet1-1"
	h.raise("Devnet1", closeToJail, "critical", now.Add(-40*time.Minute))
	h.c.checkEscalations(now)
	if out := h.outbox(); len(out) != 1 || out[0].Message != closeToJail {
		t.Fatalf("close to jail not escalated: %+v", out)
	}
}

func TestEscalationIsNotHeldByQuietHours(t *testing.T) {
	h := newEscalationHarness(t)
	h.chain("Devnet1")
	h.c.Escalation.MinSeverity = "warning"
	now := time.Now()
	h.c.QuietHours = QuietHoursConfig{Enabled: true, Start: now.Add(-time.Hour).UTC().Format("15:04"), End: now.Add(time.Hour).UTC().Format("15:04"), Timezone: "UTC", MinSeverity: "critical"}
	if err := h.c.QuietHours.parse(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		heldMux.Lock()
		held = make(map[string]heldAlert)
		heldMux.Unlock()
	})
	message := "val is missing blocks on devnet1-1"
	h.raise("Devnet1", message, "warning", now.Add(-31*time.Minute))
	heldMux.Lock()
	heldCount := len(held)
	heldMux.Unlock()
	if heldCount != 1 {
		t.Fatalf("the warning was not held by quiet hours: %d held", heldCount)
	}
	h.c.checkEscalations(now)
	h.run()
	if got := h.waitReceived(1); !strings.Contains(got[0].Title, "Escalated") {
		t.Fatalf("held escalation %+v", got[0])
	}
}

func TestEscalationToPagerdutyIsTheOnlyRaise(t *testing.T) {
	h := newEscalationHarness(t)
	h.chain("Devnet1")
	h.c.Gotify.Enabled = nil
	h.c.Pagerduty = PDConfig{Enabled: yes(true), ApiKey: "escalationkey"}
	h.c.Chains["Devnet1"].Alerts.Pagerduty = PDConfig{ApiKey: "escalationkey", MinSeverity: "escalation"}
	h.c.Escalation.To = "pagerduty"
	now := time.Now()
	h.raise("Devnet1", h.nodeDown("Devnet1"), "critical", now.Add(-31*time.Minute))
	if n := len(h.outbox()); n != 0 {
		t.Fatalf("a raise went to pagerduty normally: %d queued", n)
	}
	h.c.checkEscalations(now)
	h.c.checkEscalations(now)
	if out := h.outbox(); len(out) != 1 || out[0].Destination != pd {
		t.Fatalf("outbox %+v, want one pagerduty escalation", out)
	}
}

func TestEscalationSurvivesARestart(t *testing.T) {
	h := newEscalationHarness(t)
	dir := t.TempDir()
	configFile, stateFile := filepath.Join(dir, "config.yml"), filepath.Join(dir, "state.json")
	config := `node_down_alert_minutes: 3
gotify:
  enabled: yes
  url: ` + h.server.URL + `
  token: "` + gotifyTestToken + `"
  min_severity: escalation
escalation:
  enabled: yes
  after_minutes: 30
  to: gotify
chains:
  Devnet1:
    chain_id: devnet1-1
    valoper_address: devnet1valoper
    nodes:
      - url: http://devnet1:26657
        alert_if_down: yes
`
	if err := os.WriteFile(configFile, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	start := func() *Config {
		password := ""
		c, err := loadConfig(configFile, stateFile, filepath.Join(dir, "chains.d"), &password)
		if err != nil {
			t.Fatal(err)
		}
		if fatal, problems := validateConfig(c); fatal {
			t.Fatal(problems)
		}
		for _, cc := range c.Chains {
			cc.restoredChecked = true // as the monitor does once it is watching
		}
		td, h.c = c, c
		return c
	}
	stop := func(c *Config) {
		c.cancel()
		c.deliveryCancel()
		c.workers.Wait()
	}

	now := time.Now()
	c := start()
	msg := h.nodeDown("Devnet1")
	h.raise("Devnet1", msg, "critical", now.Add(-10*time.Minute))
	c.checkEscalations(now)
	if n := len(h.outbox()); n != 0 {
		t.Fatalf("escalated 10 minutes in: %d queued", n)
	}
	if err := c.persistState(); err != nil {
		t.Fatal(err)
	}
	stop(c)

	c = start()
	c.checkEscalations(now.Add(20 * time.Minute))
	if out := h.outbox(); len(out) != 1 {
		t.Fatalf("not escalated 30 minutes after the original start, outbox %+v", out)
	}
	c.startWorker(c.runNotifications)
	h.waitReceived(1)
	stop(c)

	c = start()
	c.checkEscalations(now.Add(40 * time.Minute))
	if n := len(h.outbox()); n != 0 {
		t.Fatalf("escalated again after a restart: %d queued", n)
	}
	if got := h.received(); len(got) != 1 {
		t.Fatalf("%d messages received, want 1", len(got))
	}
	stop(c)
}

func TestEscalationRetriesUntilTheDestinationAnswers(t *testing.T) {
	old := notificationRetryDelay
	notificationRetryDelay = 10 * time.Millisecond
	t.Cleanup(func() { notificationRetryDelay = old })
	h := newEscalationHarness(t)
	h.chain("Devnet1")
	h.setStatus(http.StatusInternalServerError)
	now := time.Now()
	h.raise("Devnet1", h.nodeDown("Devnet1"), "critical", now.Add(-31*time.Minute))
	h.c.checkEscalations(now)
	h.run()
	h.waitFor("retries", func() bool { return h.attemptCount() >= 3 })
	if out := h.outbox(); len(out) != 1 {
		t.Fatalf("escalation dropped after a 500: %+v", out)
	}
	h.setStatus(http.StatusOK)
	h.waitReceived(1)
}

func TestEscalationGroupsNodesOfOneServer(t *testing.T) {
	old := groupWaitUnit
	groupWaitUnit = groupUnit
	t.Cleanup(func() { groupWaitUnit = old })
	h := newEscalationHarness(t)
	cc := h.chain("Devnet1")
	cc.Nodes = nil
	for i := 1; i <= 5; i++ {
		cc.Nodes = append(cc.Nodes, &NodeConfig{Url: fmt.Sprintf("http://n%d:26657", i), AlertIfDown: true, Server: "srv-1"})
	}
	h.c.GroupNodeAlerts, h.c.GroupWaitSeconds = true, 2
	now := time.Now()
	for _, node := range cc.Nodes {
		h.raise("Devnet1", nodeDownMessage(node.Url, cc.ChainId), "critical", now.Add(-31*time.Minute))
	}
	h.c.checkEscalations(now)
	if n := len(h.outbox()); n != 5 {
		t.Fatalf("%d escalations queued, want 5", n)
	}
	h.run()
	got := h.waitReceived(1)
	time.Sleep(4 * groupUnit)
	if len(h.received()) != 1 {
		t.Fatalf("%d messages, want one grouped card: %+v", len(h.received()), h.received())
	}
	if !strings.Contains(got[0].Title, "Server srv-1") || !strings.Contains(got[0].Title, "Escalated") || !strings.Contains(got[0].Title, "5 nodes down") {
		t.Fatalf("grouped card %+v", got[0])
	}
}

func TestEscalationResolveReachesDestinationAfterChainRemoval(t *testing.T) {
	h := newEscalationHarness(t)
	h.chain("Devnet1")
	h.c.updateChan = make(chan *dash.ChainStatus, 8)
	h.c.statsChan = make(chan *promUpdate, 8)
	msg := h.nodeDown("Devnet1")
	now := time.Now()
	h.raise("Devnet1", msg, "critical", now.Add(-31*time.Minute))
	h.c.checkEscalations(now)
	h.run()
	h.waitReceived(1)
	h.c.applyReload(&Config{Chains: map[string]*ChainConfig{}})
	got := h.waitReceived(2)
	if len(got) != 2 || got[1].Priority != 2 || !strings.Contains(got[1].Title, "No longer watched") {
		t.Fatalf("resolve did not reach gotify after the chain was removed: %+v", got)
	}
}

func TestEscalationNeverEscalatesProposals(t *testing.T) {
	h := newEscalationHarness(t)
	h.chain("Devnet1")
	now := time.Now()
	h.raise("Devnet1", "Proposal #7 Burn\nEnds 2026-10-20 12:00 UTC\nval has not voted yet", "critical", now.Add(-40*time.Minute))
	h.c.checkEscalations(now)
	if n := len(h.outbox()); n != 0 {
		t.Fatalf("a proposal vote reminder was escalated: %d queued", n)
	}
}

func TestEscalationCloseToJailUsesHalfTheJailRiskMinutes(t *testing.T) {
	h := newEscalationHarness(t)
	cc := h.chain("Devnet1")
	cc.Alerts.JailRiskMinutes = 20
	message := "val is close to jail on devnet1-1"
	now := time.Now()
	h.raise("Devnet1", message, "critical", now.Add(-9*time.Minute))
	h.c.checkEscalations(now)
	if n := len(h.outbox()); n != 0 {
		t.Fatalf("close to jail escalated at 9 minutes with jail_risk_minutes 20: %d queued", n)
	}
	h.c.checkEscalations(now.Add(time.Minute))
	if n := len(h.outbox()); n != 1 {
		t.Fatalf("close to jail not escalated at 10 minutes: %d queued", n)
	}
}

func TestEscalationConfigValidation(t *testing.T) {
	for _, tc := range []struct {
		name     string
		esc      EscalationConfig
		jailRisk int
		warning  string
		enabled  bool
	}{
		{"valid", EscalationConfig{Enabled: true, To: "gotify"}, 90, "", true},
		{"unknown destination", EscalationConfig{Enabled: true, To: "email"}, 90, `escalation to "email" is not`, false},
		{"destination not enabled", EscalationConfig{Enabled: true, To: "pagerduty"}, 90, "escalation to pagerduty needs", false},
		{"after_minutes out of range", EscalationConfig{Enabled: true, To: "gotify", AfterMinutes: 1441}, 90, "after_minutes must be between 1 and 1440", false},
		{"jail risk below after_minutes", EscalationConfig{Enabled: true, To: "gotify"}, 20, "jail_risk_minutes is below escalation after_minutes", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := auditConfig(t)
			c.Gotify = GotifyConfig{Enabled: yes(true), URL: "https://gotify.example", Token: gotifyTestToken}
			c.Escalation = tc.esc
			cc := &ChainConfig{name: "Devnet1", ChainId: "devnet1-1", ValAddress: "devnet1valoper", Nodes: []*NodeConfig{{Url: "http://devnet1:26657", AlertIfDown: true}}}
			cc.Alerts.MissedBlocksAlerts, cc.Alerts.JailRiskMinutes = true, tc.jailRisk
			c.Chains["Devnet1"] = cc
			fatal, problems := validateConfig(c)
			joined := strings.Join(problems, "\n")
			if fatal {
				t.Fatalf("fatal: %v", problems)
			}
			if tc.warning == "" && strings.Contains(joined, "escalation") {
				t.Fatalf("valid escalation warned: %v", problems)
			}
			if tc.warning != "" && !strings.Contains(joined, tc.warning) {
				t.Fatalf("no warning %q: %v", tc.warning, problems)
			}
			if c.Escalation.Enabled != tc.enabled {
				t.Fatalf("enabled %v, want %v: %v", c.Escalation.Enabled, tc.enabled, problems)
			}
		})
	}
	c := auditConfig(t)
	c.Gotify = GotifyConfig{Enabled: yes(true), URL: "https://gotify.example", Token: gotifyTestToken}
	c.Escalation = EscalationConfig{Enabled: true, To: "gotify"}
	c.Chains["Devnet1"] = &ChainConfig{name: "Devnet1", ChainId: "devnet1-1", ValAddress: "devnet1valoper", Nodes: []*NodeConfig{{Url: "http://devnet1:26657", AlertIfDown: true}}}
	validateConfig(c)
	if c.Escalation.AfterMinutes != 30 || c.Escalation.MinSeverity != "critical" {
		t.Fatalf("defaults: %+v", c.Escalation)
	}
}

func TestEscalatedGotifyPriorityIsTen(t *testing.T) {
	msg := &alertMsg{chain: "Devnet1", severity: "critical", message: "missed blocks", escalated: true}
	if got := buildGotifyMessage(msg).Priority; got != 10 {
		t.Fatalf("escalated priority %d, want 10", got)
	}
	msg.resolved = true
	if got := buildGotifyMessage(msg).Priority; got != 2 {
		t.Fatalf("resolved escalation priority %d, want 2", got)
	}
}

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
