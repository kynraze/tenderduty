package tenderduty

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDiscordFailureCanBeRetriedAndResolved(t *testing.T) {
	old := alarms
	alarms = newAlarmCache()
	t.Cleanup(func() { alarms = old })

	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	msg := &alertMsg{disc: true, chain: "Osmosis", message: "missed blocks", discHook: server.URL}
	if err := notifyDiscord(msg); err == nil {
		t.Fatal("failed delivery was reported as successful")
	}
	if err := notifyDiscord(msg); err != nil {
		t.Fatal(err)
	}
	if err := notifyDiscord(msg); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("duplicate alert was delivered %d times", calls)
	}

	msg.resolved = true
	if err := notifyDiscord(msg); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatalf("recovery was not delivered: %d calls", calls)
	}
}

func TestRestoredAlertsRecover(t *testing.T) {
	fastWatch(t)
	for _, kind := range []string{"percentage", "stalled", "consecutive"} {
		t.Run(kind, func(t *testing.T) {
			oldConfig, oldAlarms := td, alarms
			ctx, cancel := context.WithCancel(context.Background())
			chain := &ChainConfig{name: "Juno", ChainId: "juno-1", ValAddress: "junovaloper1example", observedBlock: true, lastBlockTime: time.Now(),
				valInfo: &ValInfo{Moniker: "validator", Bonded: true, Window: 100, Missed: 10, Valcons: "junovalcons1example"}}
			var message string
			switch kind {
			case "percentage":
				chain.Alerts.PercentageAlerts, chain.Alerts.Window = true, 10
				message = "validator has missed > 10% of the slashing window's blocks on juno-1"
			case "stalled":
				chain.Alerts.StalledAlerts, chain.Alerts.Stalled = true, 1
				message = "stalled: have not seen a new block on juno-1 in 1 minutes"
			case "consecutive":
				chain.Alerts.ConsecutiveAlerts, chain.Alerts.ConsecutiveMissed = true, 5
				message = "validator has missed 5 blocks on juno-1"
			}
			alarms = newAlarmCache()
			alarms.AllAlarms["Juno"] = map[string]time.Time{message: time.Now()}
			td = &Config{ctx: ctx, alertChan: make(chan *alertMsg, 4), Chains: map[string]*ChainConfig{"Juno": chain}}
			done := make(chan struct{})
			go func() { chain.watch(); close(done) }()
			defer func() {
				cancel()
				<-done
				td, alarms = oldConfig, oldAlarms
			}()
			select {
			case alert := <-td.alertChan:
				if !alert.resolved || alert.message != message {
					t.Fatalf("restored alert did not recover: %+v", alert)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("restored alert did not produce a recovery")
			}
		})
	}
}

func TestRecoveryWaitsForAnInFlightAlert(t *testing.T) {
	old := alarms
	alarms = newAlarmCache()
	t.Cleanup(func() { alarms = old })
	msg := &alertMsg{chain: "Juno", message: "missed blocks"}
	if send, err := shouldNotify(msg, tg); !send || err != nil {
		t.Fatal("initial delivery was not reserved")
	}
	recovery := *msg
	recovery.resolved = true
	if send, err := shouldNotify(&recovery, tg); send || !errors.Is(err, errNotificationBusy) {
		t.Fatal("recovery did not wait for the alert delivery")
	}
	completeNotify(msg, tg, nil)
	if send, err := shouldNotify(&recovery, tg); !send || err != nil {
		t.Fatal("recovery could not be delivered after the alert")
	}
	completeNotify(&recovery, tg, nil)
	if alarms.sentAnywhere("Juno\x00missed blocks", "missed blocks") {
		t.Fatal("recovery did not clear delivery state")
	}
}

func TestFailedRecoveryKeepsPendingState(t *testing.T) {
	old := alarms
	alarms = newAlarmCache()
	t.Cleanup(func() { alarms = old })
	msg := &alertMsg{chain: "Juno", message: "missed blocks"}
	_, _ = shouldNotify(msg, tg)
	completeNotify(msg, tg, nil)
	key := "Juno\x00missed blocks"
	alarms.PendingRecoveries[key] = &pendingRecovery{Chain: "Juno", Message: msg.message}
	msg.resolved = true
	_, _ = shouldNotify(msg, tg)
	completeNotify(msg, tg, errors.New("temporary delivery failure"))
	if alarms.PendingRecoveries[key] == nil || !alarms.sentAnywhere(key, msg.message) {
		t.Fatal("failed recovery lost the state needed for a retry")
	}
	_, _ = shouldNotify(msg, tg)
	completeNotify(msg, tg, nil)
	if alarms.PendingRecoveries[key] != nil {
		t.Fatal("delivered recovery is still pending")
	}
}

func TestSlackRecoveryDoesNotChangeAlertIdentity(t *testing.T) {
	msg := &alertMsg{chain: "Juno", message: "RPC unavailable", resolved: true}
	message := buildSlackMessage(msg)
	if msg.message != "RPC unavailable" || message.Text != "OK: RPC unavailable" {
		t.Fatalf("alert identity changed during formatting: %#v", msg)
	}
}

func TestExtraInfoIsAddedToAlerts(t *testing.T) {
	msg := &alertMsg{chain: "Osmosis", message: "missed blocks", extra: "sent from monitor-eu"}
	// extra_info is the embed's footer, the body stays the alert
	embed := buildDiscordMessage(msg).Embeds[0]
	if embed.Description != "missed blocks" || embed.Footer == nil || embed.Footer.Text != "sent from monitor-eu" {
		t.Fatalf("discord message is %+v", embed)
	}
	if got := buildSlackMessage(msg).Text; got != "missed blocks\nsent from monitor-eu" {
		t.Fatalf("slack message is %q", got)
	}

	msg.extra = ""
	if embed = buildDiscordMessage(msg).Embeds[0]; embed.Description != "missed blocks" || embed.Footer != nil {
		t.Fatalf("discord message without extra_info is %+v", embed)
	}
}

func TestTelegramMessageIsReadable(t *testing.T) {
	msg := &alertMsg{chain: "AtomOne", severity: "critical", message: "Severity: critical\nRPC node http://a<b> has been down", extra: "monitor-eu", tgMentions: "@ops"}
	want := "🚨 <b>AtomOne</b> · Critical\n\nRPC node http://a&lt;b&gt; has been down\n<i>monitor-eu</i>\n@ops"
	if got := buildTelegramMessage(msg); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	msg.resolved = true
	if got := buildTelegramMessage(msg); !strings.HasPrefix(got, "✅ <b>AtomOne</b> · Resolved\n\n") {
		t.Fatalf("resolved message is %q", got)
	}
}

func TestNotificationColorsFollowSeverity(t *testing.T) {
	for _, tc := range []struct {
		severity string
		resolved bool
		discord  uint
		slack    string
	}{
		{"info", false, 0, ""},
		{"warning", false, 0xf1b663, "warning"},
		{"critical", false, 0xf27c76, "danger"},
		{"info", true, 0x63d6c4, "good"},
		{"critical", true, 0x63d6c4, "good"},
	} {
		msg := &alertMsg{chain: "AtomOne", message: "something happened", severity: tc.severity, resolved: tc.resolved}
		if got := buildDiscordMessage(msg).Embeds[0].Color; got != tc.discord {
			t.Fatalf("%+v discord color %#x, want %#x", tc, got, tc.discord)
		}
		if got := buildSlackMessage(msg).Attachments[0].Color; got != tc.slack {
			t.Fatalf("%+v slack color %q, want %q", tc, got, tc.slack)
		}
	}
}

func TestDiscordPlainStyle(t *testing.T) {
	msg := &alertMsg{chain: "AtomOne", severity: "critical", message: "Validator is jailed on atomone-1",
		extra: "monitor-eu", discMentions: "@here", discStyle: "plain"}
	plain := buildDiscordMessage(msg)
	want := "🚨 **AtomOne** · Jailed @here\n\n👤 **Validator:** Validator\n⛓ **Chain:** `atomone-1`\n🔒 **Status:** 🔴 jailed\n⚠️ **Severity:** critical\nmonitor-eu"
	if plain.Content != want || len(plain.Embeds) != 0 {
		t.Fatalf("plain is %q with %d embeds", plain.Content, len(plain.Embeds))
	}
	msg.discStyle = "embed"
	embed := buildDiscordMessage(msg)
	if len(embed.Embeds) != 1 || embed.Content != "🚨 **AtomOne** · Jailed @here" || embed.Embeds[0].Color != 0xf27c76 {
		t.Fatalf("embed is %q / %+v", embed.Content, embed.Embeds)
	}
}

func TestDashboardShowsTheUpgradeReminderOnce(t *testing.T) {
	oldAlarms, oldTd := alarms, td
	alarms, td = newAlarmCache(), &Config{}
	t.Cleanup(func() { alarms, td = oldAlarms, oldTd })
	alarms.AllAlarms["Devnet"] = map[string]time.Time{
		"Version 11 upgrade at height 394":          time.Now(),
		"Version 11 upgrade at height 394 is close": time.Now(),
	}
	if got := getAlarms("Devnet"); got != "🚨 Version 11 upgrade at height 394 is close\n" {
		t.Fatalf("got %q", got)
	}
}

// An operator with three nodes behind one server: one card, not three.
func TestOpThreeNodesOneServerIsOneCard(t *testing.T) {
	h := newGroupHarness(t, map[string]string{"a1": hostAIP, "a2": hostAIP, "a3": hostAIP})
	h.chain("AlphaOne", "http://a1:26657", "@one")
	h.chain("AlphaTwo", "http://a2:26657", "@one")
	h.chain("AlphaThree", "http://a3:26657", "@one")
	h.run()
	h.down("AlphaOne")
	h.down("AlphaTwo")
	h.down("AlphaThree")
	sent := h.waitTelegram(1)
	time.Sleep(4 * groupUnit)
	if got := len(h.telegramSent()); got != 1 {
		t.Fatalf("%d telegram messages after the group wait, want 1", got)
	}
	contains(t, sent[0].text, "3 nodes down", "AlphaOne", "AlphaTwo", "AlphaThree")
}

// A node flaps down, up, down inside the group wait: the operator sees them in order.
func TestOpFlapDownUpDownArrivesInOrder(t *testing.T) {
	h := newGroupHarness(t, map[string]string{"a1": hostAIP})
	h.chain("AlphaOne", "http://a1:26657", "@one")
	h.run()
	h.down("AlphaOne")
	h.up("AlphaOne")
	h.down("AlphaOne")
	h.waitFor("the outbox to drain", h.drained)
	time.Sleep(4 * groupUnit)
	h.mux.Lock()
	sent := append([]sentTelegram(nil), h.telegram...)
	h.mux.Unlock()
	// the first down was never sent, so its recovery and stale raise are dropped
	if len(sent) != 1 {
		t.Fatalf("%d telegram messages for down, up, down: %+v", len(sent), sent)
	}
	if text := strings.ToLower(sent[0].text); strings.Contains(text, "back up") || !strings.Contains(text, "down") {
		t.Errorf("message lacks the current down state: %s", sent[0].text)
	}
}

// A warning node down in quiet hours that recovers before they end sends nothing.
func TestOpWarningDownInQuietHoursIsSilentWhenItRecovers(t *testing.T) {
	h := newGroupHarness(t, map[string]string{"a1": hostAIP})
	h.chain("AlphaOne", "http://a1:26657", "@one")
	h.c.NodeDownSeverity = "warning"
	now := time.Now().UTC()
	h.c.QuietHours = QuietHoursConfig{Enabled: true, Start: now.Add(-time.Hour).Format("15:04"), End: now.Add(time.Hour).Format("15:04"), Timezone: "UTC"}
	if err := h.c.QuietHours.parse(); err != nil {
		t.Fatal(err)
	}
	h.run()
	id := h.c.Chains["AlphaOne"].Nodes[0].Url
	msg := nodeDownMessage(id, "alphaone-1")
	h.c.alert("AlphaOne", msg, "warning", false, &id)
	h.c.alert("AlphaOne", msg, "info", true, &id)
	h.waitFor("the outbox to drain", h.drained)
	time.Sleep(4 * groupUnit)
	if sent := h.telegramSent(); len(sent) != 0 {
		t.Fatalf("quiet hours sent %+v for a warning that recovered inside them", sent)
	}
}

// A critical node down in quiet hours is sent, and so is its recovery.
func TestOpCriticalNodeDownGoesThroughQuietHours(t *testing.T) {
	for _, minSeverity := range []string{"", "critical", "warning"} {
		t.Run("min_severity="+minSeverity, func(t *testing.T) {
			h := newGroupHarness(t, map[string]string{"a1": hostAIP})
			h.chain("AlphaOne", "http://a1:26657", "@one")
			now := time.Now().UTC()
			h.c.QuietHours = QuietHoursConfig{Enabled: true, Start: now.Add(-time.Hour).Format("15:04"), End: now.Add(time.Hour).Format("15:04"), Timezone: "UTC", MinSeverity: minSeverity}
			if err := h.c.QuietHours.parse(); err != nil {
				t.Fatal(err)
			}
			h.run()
			h.down("AlphaOne")
			h.waitTelegram(1)
			h.up("AlphaOne")
			sent := h.waitTelegram(2)
			if !strings.Contains(strings.ToLower(sent[0].text)+strings.ToLower(sent[1].text), "back up") {
				t.Fatalf("no recovery sent: %+v", sent)
			}
		})
	}
}

// quiet_hours.min_severity written with a capital letter should mean warning, not critical.
func TestOpQuietMinSeverityCapitalIsRead(t *testing.T) {
	night := time.Date(2026, 10, 2, 2, 0, 0, 0, time.UTC)
	for _, minSeverity := range []string{"warning", "Warning"} {
		q := QuietHoursConfig{Enabled: true, Start: "23:00", End: "07:00", Timezone: "UTC", MinSeverity: minSeverity}
		if err := q.parse(); err != nil {
			t.Fatal(err)
		}
		if q.holds("warning", false, night) {
			t.Errorf("a warning is held when quiet_hours.min_severity is %q", minSeverity)
		}
	}
}

// A destination with min_severity written as Warning should still get warnings.
func TestOpDestinationMinSeverityCapitalIsRead(t *testing.T) {
	for _, minSeverity := range []string{"warning", "Warning"} {
		t.Run(minSeverity, func(t *testing.T) {
			h := newGroupHarness(t, map[string]string{"a1": hostAIP})
			cc := h.chain("AlphaOne", "http://a1:26657", "@one")
			cc.Alerts.Telegram.MinSeverity = minSeverity
			h.run()
			id := "warn-id"
			h.c.alert("AlphaOne", "validator has missed 5 blocks on alphaone-1", "warning", false, &id)
			h.waitFor("the outbox to drain", h.drained)
			time.Sleep(4 * groupUnit)
			if got := len(h.telegramSent()); got != 1 {
				t.Fatalf("%d telegram messages for a warning with min_severity %q, want 1", got, minSeverity)
			}
		})
	}
}

// A destination with min_severity critical gets no warning, raised or recovered.
func TestOpCriticalOnlyDestinationGetsNoWarning(t *testing.T) {
	h := newGroupHarness(t, map[string]string{"a1": hostAIP})
	cc := h.chain("AlphaOne", "http://a1:26657", "@one")
	cc.Alerts.Telegram.MinSeverity = "critical"
	h.run()
	id := "warn-id"
	message := "validator has missed 5 blocks on alphaone-1"
	h.c.alert("AlphaOne", message, "warning", false, &id)
	h.c.alert("AlphaOne", message, "info", true, &id)
	h.waitFor("the outbox to drain", h.drained)
	time.Sleep(4 * groupUnit)
	if sent := h.telegramSent(); len(sent) != 0 {
		t.Fatalf("a warning reached a critical-only destination: %+v", sent)
	}
	critical := jailMessage(&ValInfo{Moniker: "Kynraze", Jailed: true}, "alphaone-1")
	h.c.alert("AlphaOne", critical, "critical", false, &id)
	sent := h.waitTelegram(1)
	contains(t, sent[0].text, "Kynraze", "Jailed", "alphaone-1")
}

// Down, server restart, recovery, down again: one message each time.
func TestOpRestartKeepsOneRecoveryAndNewRaise(t *testing.T) {
	auditConfig(t)
	dir := t.TempDir()
	configFile, stateFile := filepath.Join(dir, "config.yml"), filepath.Join(dir, "state.json")
	config := "node_down_alert_minutes: 3\ntelegram:\n  enabled: yes\n  api_key: \"" + testToken + "\"\n  channel: \"@one\"\n" +
		"chains:\n  AlphaOne:\n    chain_id: alphaone-1\n    valoper_address: alphaonevaloper\n    nodes:\n      - url: http://a1:26657\n        alert_if_down: yes\n"
	if err := os.WriteFile(configFile, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	h := &groupHarness{t: t}
	h.server = httptest.NewServer(http.HandlerFunc(h.serve))
	target, _ := url.Parse(h.server.URL)
	oldTransport := http.DefaultTransport
	var current *Config
	t.Cleanup(func() {
		if current != nil {
			current.cancel()
			current.deliveryCancel()
			current.workers.Wait()
		}
		http.DefaultTransport = oldTransport
		h.server.Close()
	})
	http.DefaultTransport = redirectTransport{base: oldTransport, target: target}
	start := func() *Config {
		password := ""
		c, err := loadConfig(configFile, stateFile, filepath.Join(dir, "chains.d"), &password)
		if err != nil {
			t.Fatal(err)
		}
		if fatal, problems := validateConfig(c); fatal {
			t.Fatal(problems)
		}
		td, current = c, c
		h.c = c
		c.startWorker(c.runNotifications)
		return c
	}
	stop := func(c *Config) {
		c.cancel()
		c.deliveryCancel()
		c.workers.Wait()
		current = nil
	}

	c := start()
	h.down("AlphaOne")
	h.waitTelegram(1)
	if err := c.persistState(); err != nil {
		t.Fatal(err)
	}
	stop(c)

	c = start()
	if n := alarms.getCount("AlphaOne"); n != 1 {
		t.Fatalf("%d alarms restored, want 1", n)
	}
	h.up("AlphaOne")
	sent := h.waitTelegram(2)
	if !strings.Contains(strings.ToLower(sent[1].text), "back up") && !strings.Contains(strings.ToLower(sent[0].text), "back up") {
		t.Fatalf("no recovery after the restart: %+v", sent)
	}
	h.down("AlphaOne")
	h.waitTelegram(3)
	stop(c)
}

// A validator that is tombstoned stays out and is never announced as resolved.
func TestOpTombstonedValidatorNeverGetsResolved(t *testing.T) {
	c := auditConfig(t)
	val := &ValInfo{Moniker: "Kynraze", Valcons: "cons", Bonded: false, Tombstoned: true}
	cc := &ChainConfig{name: "Chain", ChainId: "chain-1", valInfo: val, blockTime: time.Second}
	cc.Alerts.MissedBlocksAlerts, cc.Alerts.AlertIfInactive = true, true
	c.Chains[cc.name] = cc
	for i := 0; i < 3; i++ {
		cc.jailAlarms(val, 0, false)
	}
	var raised []*alertMsg
	for len(c.alertChan) > 0 {
		a := <-c.alertChan
		if a.resolved {
			t.Fatalf("a tombstoned validator got a recovery: %q", a.message)
		}
		raised = append(raised, a)
	}
	if len(raised) != 1 {
		t.Fatalf("%d alerts for a tombstoned validator, want 1", len(raised))
	}
	if n := alarms.getCount(cc.name); n != 1 {
		t.Fatalf("%d alarms, want 1", n)
	}
	contains(t, buildTelegramMessage(raised[0]), "tombstoned")
}

// Jailed, then back in the active set before the next check: one raise, one recovery, no repeat.
func TestOpJailedThenBackClearsOnce(t *testing.T) {
	c := auditConfig(t)
	val := &ValInfo{Moniker: "Kynraze", Valcons: "cons", Bonded: true, Jailed: true, JailedUntil: time.Now().Add(time.Hour)}
	cc := &ChainConfig{name: "Chain", ChainId: "chain-1", valInfo: val, blockTime: time.Second}
	cc.Alerts.MissedBlocksAlerts, cc.Alerts.AlertIfInactive = true, true
	c.Chains[cc.name] = cc
	cc.jailAlarms(val, 0, false)
	val.Jailed, val.JailedUntil = false, time.Time{}
	cc.jailAlarms(val, 0, false)
	cc.jailAlarms(val, 0, false)
	var raised, recovered int
	for len(c.alertChan) > 0 {
		a := <-c.alertChan
		if a.resolved {
			recovered++
			if a.summary() != "back in the active set" {
				t.Errorf("recovery says %q", a.summary())
			}
		} else {
			raised++
		}
	}
	if raised != 1 || recovered != 1 || alarms.getCount(cc.name) != 0 {
		t.Fatalf("raised %d, recovered %d, alarms %d", raised, recovered, alarms.getCount(cc.name))
	}
}

// A moniker with HTML characters stays escaped in the raise and the recovery.
func TestOpHostileMonikerStaysEscaped(t *testing.T) {
	auditConfig(t)
	moniker := `<Bob & Co>`
	raised := &alertMsg{chain: "Chain", message: jailMessage(&ValInfo{Moniker: moniker, Jailed: true}, "chain-1"), severity: "critical"}
	checkSent(t, "raised", raised)
	contains(t, buildTelegramMessage(raised), "&lt;Bob &amp; Co&gt;")
	recovered := &alertMsg{chain: "Chain", message: raised.message, resolved: true}
	checkSent(t, "recovered", recovered)
	text := buildTelegramMessage(recovered)
	contains(t, text, "&lt;Bob &amp; Co&gt;")
	if strings.Contains(text, "<Bob") {
		t.Fatalf("raw moniker in %q", text)
	}
}
