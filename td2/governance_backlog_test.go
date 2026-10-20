package tenderduty

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func unseenGovChain(t *testing.T) (*Config, *ChainConfig, *ValInfo, govProposal) {
	t.Helper()
	c, cc, val, p := govEarlyTestChain(t)
	delete(alarms.GovSeen, cc.name)
	c.Discord.Enabled = yes(true)
	return c, cc, val, p
}

func TestFirstSightProposalsAreSummarised(t *testing.T) {
	c, cc, val, p := unseenGovChain(t)
	now := p.VotingStart
	other := p
	other.ID, other.Title = 2, "Second proposal"
	cc.proposals = []govProposal{p, other}
	cc.govAlarmsAt(val, true, now)
	if !alarms.GovSeen[cc.name] || len(activeMessages(cc.name, "Proposal #")) != 2 || len(alarms.Outbox) != 0 || len(drainGovAlerts(c)) != 0 {
		t.Fatal("first sight did not record both proposals silently")
	}
	for _, proposal := range cc.proposals {
		key := cc.name + "\x00" + cc.govOpenMessage(proposal)
		if alarms.AlarmIDs[key] == "" || alarms.AlarmSeverities[key] != "info" || !alarms.SilentAlarms[key] {
			t.Fatalf("missing silent alarm metadata for %q", key)
		}
	}
	if len(c.reportData.Incidents) != 2 || len(c.govSummary) != 2 {
		t.Fatal("missing report incidents or pending summary entries")
	}
	if got := c.flushGovBacklog(now.Add(2*time.Minute - time.Nanosecond)); got != "" {
		t.Fatal("summary flushed early")
	}
	// Round-trip the saved alarm state, then reconcile as a restarted watcher would.
	data, err := json.Marshal(alarms)
	if err != nil {
		t.Fatal(err)
	}
	restored := newAlarmCache()
	if err := json.Unmarshal(data, restored); err != nil {
		t.Fatal(err)
	}
	alarms = restored
	cc.govAlarmsAt(val, true, now.Add(time.Minute))
	if len(alarms.Outbox) != 0 || len(drainGovAlerts(c)) != 0 || len(c.govSummary) != 2 {
		t.Fatal("reconciliation announced or recollected a silent proposal")
	}
	// Disable delivery for this body-only assertion.
	c.Discord.Enabled = yes(false)
	body := c.flushGovBacklog(now.Add(2 * time.Minute))
	if !strings.HasPrefix(body, "Proposals already in voting\nChain #2 Second proposal · ") || strings.Index(body, "Chain #7 ") < strings.Index(body, "Chain #2 ") {
		t.Fatalf("unexpected summary: %q", body)
	}
	if len(c.govSummary) != 0 || c.flushGovBacklog(now.Add(3*time.Minute)) != "" {
		t.Fatal("summary was not drained once")
	}
}

func TestSeenChainAnnouncesNewProposal(t *testing.T) {
	c, cc, val, p := govEarlyTestChain(t)
	c.Discord.Enabled = yes(true)
	cc.govAlarmsAt(val, false, p.VotingStart)
	expectGovRaised(t, c, map[string]string{cc.govOpenMessage(p): "info"})
	if len(alarms.Outbox) != 1 || len(c.govSummary) != 0 {
		t.Fatal("seen chain did not queue a normal announcement")
	}
}

func TestSilentProposalStillReminds(t *testing.T) {
	for _, stage := range []string{"early", "final"} {
		t.Run(stage, func(t *testing.T) {
			c, cc, val, p := unseenGovChain(t)
			now, message, severity := p.VotingEnd.Add(-42*time.Hour), cc.govEarlyRemindMessage(p, val.Moniker), "warning"
			if stage == "final" {
				now, message, severity = p.VotingEnd.Add(-time.Hour), cc.govRemindMessage(p, val.Moniker), "critical"
			}
			cc.govAlarmsAt(val, false, now)
			expectGovRaised(t, c, map[string]string{message: severity})
			if len(alarms.Outbox) != 1 || alarms.Outbox[0].Message != message || len(c.govSummary) != 1 {
				t.Fatal("silent first sight suppressed the due reminder")
			}
		})
	}
}

func TestSilentProposalResolvesWithoutRecovery(t *testing.T) {
	for _, voted := range []bool{false, true} {
		t.Run(map[bool]string{false: "ended", true: "voted"}[voted], func(t *testing.T) {
			c, cc, val, p := unseenGovChain(t)
			cc.govAlarmsAt(val, false, p.VotingStart)
			if voted {
				cc.proposals[0].Voted = true
			} else {
				cc.proposals = nil
			}
			cc.govAlarmsAt(val, false, p.VotingEnd.Add(time.Minute))
			if len(activeMessages(cc.name, "Proposal #")) != 0 || len(alarms.Outbox) != 0 || len(alarms.PendingRecoveries) != 0 || len(alarms.SilentAlarms) != 0 {
				t.Fatal("unsent proposal queued a recovery or stayed active")
			}
			if len(c.reportData.Incidents) != 1 || c.reportData.Incidents[0].End.IsZero() {
				t.Fatal("silent proposal's report incident was not resolved")
			}
		})
	}
}

func TestGovFirstSightLegacyAndEmptyState(t *testing.T) {
	for _, legacy := range []string{"alarm", "empty", "not loaded"} {
		t.Run(legacy, func(t *testing.T) {
			c, cc, val, p := unseenGovChain(t)
			switch legacy {
			case "alarm":
				alarms.AllAlarms[cc.name] = map[string]time.Time{cc.govOpenMessage(p): time.Now()}
			case "empty":
				cc.proposals = nil
			case "not loaded":
				cc.proposalsLoaded = false
			}
			cc.govAlarmsAt(val, false, p.VotingStart)
			if len(c.govSummary) != 0 || alarms.GovSeen[cc.name] != (legacy != "not loaded") {
				t.Fatal("legacy or unloaded chain treated as unseen")
			}
			if legacy == "empty" {
				cc.proposals = []govProposal{p}
				cc.govAlarmsAt(val, false, p.VotingStart)
				expectGovRaised(t, c, map[string]string{cc.govOpenMessage(p): "info"})
			}
		})
	}
}

func TestGovBacklogNoticeDestinationsAndCard(t *testing.T) {
	c, cc, _, p := unseenGovChain(t)
	received := make(chan string, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received <- string(body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	c.Discord = DiscordConfig{Enabled: yes(true), Webhook: server.URL, MinSeverity: "critical"}
	cc.Alerts.Discord = c.Discord
	c.Chains["Alpha"] = &ChainConfig{name: "Alpha", ChainId: "alpha-1", Alerts: AlertConfig{Discord: c.Discord}}
	c.Chains["Unrelated"] = &ChainConfig{name: "Unrelated", ChainId: "other-1", Alerts: AlertConfig{Discord: DiscordConfig{Enabled: yes(true), Webhook: server.URL + "/unrelated"}}}
	now := p.VotingStart
	c.collectGovBacklog(cc.name, p, now)
	c.collectGovBacklog("Alpha", p, now.Add(time.Minute))
	body := c.flushGovBacklog(now.Add(2 * time.Minute))
	c.workers.Wait()
	if len(received) != 1 || len(alarms.Outbox) != 0 || alarms.getCount(cc.name) != 0 {
		t.Fatal("summary was duplicated, sent to an unrelated chain, or recorded as an alarm")
	}
	if !strings.HasPrefix(body, "Proposals already in voting\nAlpha #7 ") {
		t.Fatalf("chains not sorted: %q", body)
	}
	var sent DiscordMessage
	if err := json.Unmarshal([]byte(<-received), &sent); err != nil {
		t.Fatal(err)
	}
	if sent.Content != "🗳 **Proposals already in voting**" {
		t.Fatalf("unexpected shared summary title: %q", sent.Content)
	}
	msg := &alertMsg{message: body, severity: "info", transient: true, discStyle: "plain"}
	card := msg.card()
	if card.Title != "" || card.Icon != "🗳" || len(card.Fields) != 2 {
		t.Fatalf("unexpected card: %+v", card)
	}
	if card.Fields[0].Icon != "" || card.Fields[0].Label != "Alpha" || !strings.HasPrefix(card.Fields[0].Value, "#7 ") {
		t.Fatalf("unexpected proposal row: %+v", card.Fields[0])
	}
	for _, field := range card.Fields {
		if field.Label == "Note" {
			t.Fatalf("proposal rendered as a note: %+v", field)
		}
	}
	if len(sent.Embeds) != 1 || len(sent.Embeds[0].Fields) != 2 || sent.Embeds[0].Fields[0].Name != "Alpha" || !strings.HasPrefix(sent.Embeds[0].Fields[0].Value, "#7 ") {
		t.Fatalf("unexpected summary embed: %+v", sent.Embeds)
	}
	if text := buildDiscordMessage(msg).Content; strings.Contains(text, "Note") || !strings.Contains(text, "**Alpha:** #7 ") || !strings.Contains(text, "**Chain:** #7 ") {
		t.Fatalf("plain summary lost proposal lines: %q", text)
	}
	if title := buildGotifyMessage(msg).Title; title != "🗳 Proposals already in voting" {
		t.Fatalf("unexpected gotify title: %q", title)
	}
	if title := buildSlackMessage(msg).Attachments[0].Title; title != "🗳 Proposals already in voting" {
		t.Fatalf("unexpected slack title: %q", title)
	}
}

func TestGovBacklogCardRows(t *testing.T) {
	msg := &alertMsg{message: "Proposals already in voting\nAlpha #7 Update #8 title · 3d left\nFallback line\n", transient: true}
	card := msg.card()
	if len(card.Fields) != 2 || card.Fields[0] != row("", "Alpha", "#7 Update #8 title · 3d left") || card.Fields[1] != row("ℹ️", "Note", "Fallback line") {
		t.Fatalf("unexpected proposal rows: %+v", card.Fields)
	}
	for _, tc := range []struct {
		name string
		got  string
		want string
	}{
		{"telegram", card.telegramLines()[0], "<b>Alpha:</b> #7 Update #8 title · <b>3d left</b>"},
		{"markdown", card.markdownLines()[0], "**Alpha:** #7 Update #8 title · **3d left**"},
		{"plain", card.plainLines()[0], "Alpha: #7 Update #8 title · 3d left"},
		{"embed", card.discordEmbed(0).Fields[0].Name, "Alpha"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, tc.got, tc.want)
		}
	}
}

func TestGovBacklogClippingAndCancellation(t *testing.T) {
	c, cc, _, p := unseenGovChain(t)
	c.Discord.Enabled = yes(false)
	now := p.VotingStart
	p.Title = strings.Repeat("界", 100)
	c.collectGovBacklog(cc.name, p, now)
	body := c.flushGovBacklog(now.Add(2 * time.Minute))
	if !strings.Contains(body, clipRunes(p.Title, 60)) || strings.Contains(body, strings.Repeat("界", 61)) {
		t.Fatal("title not clipped to 60 runes")
	}
	for id := uint64(1); id <= 30; id++ {
		p.ID = id
		c.collectGovBacklog(cc.name, p, now)
	}
	if body = c.flushGovBacklog(now.Add(2 * time.Minute)); len([]rune(body)) > len([]rune("Proposals already in voting\n"))+900 {
		t.Fatal("summary exceeded body limit")
	}
	c.collectGovBacklog(cc.name, p, now)
	c.cancel()
	if body := c.flushGovBacklog(now.Add(2 * time.Minute)); body != "" {
		t.Fatal("cancelled config flushed a summary")
	}
}

func TestGovBacklogTelegramDestinations(t *testing.T) {
	for _, tc := range []struct {
		name    string
		key     string
		channel string
		enabled bool
		want    int
	}{
		{"shared chat", testToken, "@one", true, 1},
		{"separate chats", testToken, "@two", true, 2},
		{"separate bots", "456:test", "@one", true, 2},
		{"disabled chain", testToken, "@one", false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			type delivery struct{ path, chat, text string }
			received := make(chan delivery, 4)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if strings.HasSuffix(r.URL.Path, "/getMe") {
					io.WriteString(w, `{"ok":true,"result":{"id":123,"is_bot":true,"first_name":"Test"}}`)
					return
				}
				if err := r.ParseForm(); err != nil {
					t.Error(err)
				}
				received <- delivery{r.URL.Path, r.Form.Get("chat_id"), r.Form.Get("text")}
				io.WriteString(w, `{"ok":true,"result":{"message_id":1}}`)
			}))
			target, _ := url.Parse(server.URL)
			oldTransport := http.DefaultTransport
			t.Cleanup(func() {
				http.DefaultTransport = oldTransport
				server.Close()
			})
			http.DefaultTransport = redirectTransport{base: oldTransport, target: target}
			c, cc, _, p := unseenGovChain(t)
			c.Discord.Enabled = yes(false)
			c.Telegram = TeleConfig{Enabled: yes(true), ApiKey: testToken, Channel: "@one", MinSeverity: "critical"}
			cc.Alerts.Telegram = c.Telegram
			alpha := c.Telegram
			alpha.ApiKey, alpha.Channel, alpha.Enabled = tc.key, tc.channel, yes(tc.enabled)
			c.Chains["Alpha"] = &ChainConfig{name: "Alpha", Alerts: AlertConfig{Telegram: alpha}}
			c.Chains["Unrelated"] = &ChainConfig{name: "Unrelated", Alerts: AlertConfig{Telegram: c.Telegram}}
			now := p.VotingStart
			c.collectGovBacklog(cc.name, p, now)
			c.collectGovBacklog("Alpha", p, now)
			p.ID = 2
			c.collectGovBacklog(cc.name, p, now)
			c.flushGovBacklog(now.Add(2 * time.Minute))
			c.workers.Wait()
			if len(received) != tc.want || len(alarms.Outbox) != 0 {
				t.Fatalf("got %d deliveries and %d outbox entries", len(received), len(alarms.Outbox))
			}
			for i := 0; i < tc.want; i++ {
				got := <-received
				header, _, _ := strings.Cut(got.text, "\n")
				if tc.name == "shared chat" {
					if header != "🗳 <b>Proposals already in voting</b>" || !strings.Contains(got.text, "<b>Alpha:</b> #7 ") || !strings.Contains(got.text, "<b>Chain:</b> #7 ") {
						t.Fatalf("unexpected shared summary: %+v", got)
					}
				} else {
					name, other := "Chain", "Alpha"
					if got.chat == "@two" || strings.Contains(got.path, "456:test") {
						name, other = other, name
					}
					if header != "🗳 <b>"+name+"</b> · Proposals already in voting" || !strings.Contains(got.text, "<b>"+name+":</b> #7 ") || strings.Contains(got.text, "<b>"+other+":</b> #") {
						t.Fatalf("unexpected routed summary: %+v", got)
					}
				}
				if strings.Contains(got.text, "Note") || strings.Contains(got.text, "Unrelated") || (strings.Contains(got.text, "<b>Chain:</b> #7 ") && strings.Index(got.text, "<b>Chain:</b> #2 ") > strings.Index(got.text, "<b>Chain:</b> #7 ")) {
					t.Fatalf("unrelated or unsorted proposals: %+v", got)
				}
			}
		})
	}
}
