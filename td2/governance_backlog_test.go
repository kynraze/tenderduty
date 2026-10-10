package tenderduty

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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
	if got := <-received; !strings.Contains(got, "Proposals already in voting") {
		t.Fatal("missing summary title")
	}
	msg := &alertMsg{message: body, severity: "info", transient: true, discStyle: "plain"}
	card := msg.card()
	if card.Title != "Proposals already in voting" || card.Icon != "🗳" || len(card.Fields) != 2 {
		t.Fatalf("unexpected card: %+v", card)
	}
	if text := buildDiscordMessage(msg).Content; !strings.Contains(text, "Alpha #7 ") || !strings.Contains(text, "Chain #7 ") {
		t.Fatalf("plain summary lost proposal lines: %q", text)
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
