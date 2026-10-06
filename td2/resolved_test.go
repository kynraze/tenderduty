package tenderduty

import (
	"fmt"
	"testing"
	"time"
)

func TestResolvedNotificationsSayWhatIsTrueNow(t *testing.T) {
	c := auditConfig(t)
	c.NodeDownMin, c.NodeDownSeverity = 3, "critical"
	cc := &ChainConfig{name: "AtomOne", ChainId: "atomone-1"}
	cc.Alerts.ProposalMissed, cc.Alerts.ProposalMinutes, cc.Alerts.JailRiskPercent, cc.Alerts.JailRiskMinutes = 3, 60, 50, 60
	c.Chains["AtomOne"] = cc
	jailed := &ValInfo{Moniker: "Kynraze", Jailed: true, JailedUntil: time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)}
	for _, tc := range []struct {
		name, message, icon, label, text string
	}{
		{"consecutive", "Kynraze has missed 5 blocks on atomone-1", "✅", "Resolved", "Kynraze is signing blocks again on atomone-1"},
		{"percentage", "Kynraze has missed > 10% of the slashing window's blocks on atomone-1", "✅", "Resolved", "Kynraze is back under 10% of the slashing window missed on atomone-1"},
		{"stalled", "stalled: have not seen a new block on atomone-1 in 10 minutes", "✅", "Resolved", "atomone-1 is producing blocks again"},
		{"no rpc", "no RPC endpoints are working for atomone-1", "✅", "Resolved", "RPC endpoints are working again for atomone-1"},
		{"node down", nodeDownMessage("https://rpc.example.com:443", "atomone-1"), "✅", "Resolved", "RPC node https://rpc.example.com:443 is back up on atomone-1"},
		{"jailed", jailMessage(jailed, "atomone-1"), "✅", "Active again", "Kynraze is back in the active set on atomone-1"},
		{"inactive", jailMessage(&ValInfo{Moniker: "Kynraze"}, "atomone-1"), "✅", "Active again", "Kynraze is back in the active set on atomone-1"},
		{"tombstoned", jailMessage(&ValInfo{Moniker: "Kynraze", Jailed: true, Tombstoned: true}, "atomone-1"), "✅", "Resolved", "Kynraze is no longer tombstoned on atomone-1"},
		{"can unjail", "Kynraze can unjail on atomone-1", "✅", "Unjailed", "Kynraze was unjailed on atomone-1"},
		{"missed proposal", cc.proposalMessage("Kynraze"), "✅", "Resolved", "Kynraze missed fewer than 3 block proposals in the last 60 minutes on atomone-1"},
		{"jail risk", cc.jailRiskMessage("Kynraze"), "✅", "Resolved", "Kynraze is back under 50% of the misses allowed before jailing on atomone-1"},
		{"jail soon", cc.jailSoonMessage("Kynraze"), "✅", "Resolved", "Kynraze is no longer close to being jailed on atomone-1"},
		{"upgrade", upgradeMessage(&upgradePlan{Name: "v4", Height: 9_550_000}), "✅", "Upgraded", "Upgrade v4 at height 9,550,000"},
		{"other", "something nobody formats", "✅", "Resolved", "something nobody formats"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg := &alertMsg{chain: "AtomOne", message: tc.message, resolved: true}
			if icon, label := msg.header(); icon != tc.icon || label != tc.label {
				t.Errorf("header %s %s, want %s %s", icon, label, tc.icon, tc.label)
			}
			if got := msg.summary(); got != tc.text {
				t.Errorf("text %q, want %q", got, tc.text)
			}
		})
	}
}

func TestResolvedGovernance(t *testing.T) {
	cc := &ChainConfig{name: "Gov", ChainId: "gov-1"}
	future := govProposal{ID: 21, Title: "Raise the block size", VotingEnd: time.Now().Add(48 * time.Hour).UTC()}
	past := govProposal{ID: 22, Title: "Lower fees", VotingEnd: time.Now().Add(-time.Hour).UTC()}
	early := govProposal{ID: 23, Title: "Expedited", VotingEnd: time.Now().Add(48 * time.Hour).UTC()}
	govOutcomes.Store("Gov#22", "Rejected")
	govOutcomes.Store("Gov#23", "Passed")
	end := func(p govProposal) string { return p.VotingEnd.Format(govEndFormat) }
	for _, tc := range []struct {
		name, message, label, text string
	}{
		{"voted", cc.govOpenMessage(future), "Voted", fmt.Sprintf("Proposal #21 Raise the block size\nVoted, voting ends %s", end(future))},
		{"reminder answered", cc.govRemindMessage(future, "Kynraze"), "Voted", "Proposal #21 Raise the block size\nKynraze voted"},
		{"ended", cc.govOpenMessage(past), "Rejected", fmt.Sprintf("Proposal #22 Lower fees\nVoting ended %s", end(past))},
		{"ended without a vote", cc.govRemindMessage(past, "Kynraze"), "Rejected", "Proposal #22 Lower fees\nVoting ended without a vote from Kynraze"},
		{"closed early", cc.govOpenMessage(early), "Passed", "Proposal #23 Expedited\nVoting closed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg := &alertMsg{chain: "Gov", message: tc.message, resolved: true}
			if _, label := msg.header(); label != tc.label {
				t.Errorf("label %s, want %s", label, tc.label)
			}
			if got := msg.summary(); got != tc.text {
				t.Errorf("text %q, want %q", got, tc.text)
			}
		})
	}
}

func TestResolvedSignalling(t *testing.T) {
	cc := &ChainConfig{name: "Celestia", ChainId: "celestia"}
	cc.Alerts.SignalPercent = 10
	for _, tc := range []struct {
		name, message string
		state         signalState
		icon, label   string
		text          string
	}{
		{"chain upgraded", signalMissingMessage(10), signalState{Version: 11}, "✅", "Upgraded", "celestia runs version 10 now"},
		{"validator signalled", signalMissingMessage(10), signalState{Version: 10, Signalled: true}, "✅", "Signalled", "Kynraze signalled version 10 on celestia"},
		{"signalling dropped, not signalled", signalMissingMessage(10), signalState{Version: 10}, "✅", "Resolved", "Version 10 signalling dropped below 10%"},
		{"start dropped", signalStartMessage(10), signalState{Version: 10}, "✅", "Resolved", "Version 10 signalling dropped below 10%"},
		{"threshold, upgrade scheduled", signalThresholdMessage(10), signalState{Version: 10, UpgradeHeight: 15_000_000}, "⬆️", "Upgrade scheduled", "Version 10 upgrade is scheduled at height 15,000,000"},
		{"threshold fell back", signalThresholdMessage(10), signalState{Version: 10}, "✅", "Resolved", "Version 10 fell back under the upgrade threshold"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, ok := cc.signalResolution(tc.message, tc.state, "Kynraze")
			if !ok || r.icon != tc.icon || r.label != tc.label || r.text != tc.text {
				t.Errorf("got %+v, want %s %s %q", r, tc.icon, tc.label, tc.text)
			}
		})
	}
	if r, ok := cc.signalResolution(signalUpgradeMessage(10, 15_000_000), signalState{}, "Kynraze"); !ok || r.text != "upgrade cancelled or rescheduled" {
		t.Errorf("scheduled resolution: %+v", r)
	}
}

// TestSignalAlarmsRecordWhyTheyCleared: a chain that upgraded must not read "Signalled"
func TestSignalAlarmsRecordWhyTheyCleared(t *testing.T) {
	c := auditConfig(t)
	cc := &ChainConfig{name: "Celestia", ChainId: "celestia", signalLoaded: true, signal: signalState{Version: 11}}
	cc.Alerts.SignalPercent = 10
	cc.Alerts.UpgradeAlerts = true
	c.Chains["Celestia"] = cc
	alarms.AllAlarms["Celestia"] = map[string]time.Time{signalMissingMessage(10): time.Now()}
	cc.signalAlarms(&ValInfo{Valcons: "cons", Moniker: "Kynraze"}, false)
	alert := <-c.alertChan
	if icon, label := alert.header(); !alert.resolved || label != "Upgraded" || alert.summary() != "celestia runs version 10 now" {
		t.Fatalf("cleared as %s %s %q", icon, label, alert.summary())
	}
}

// TestRaisingAgainForgetsTheLastResolution: or its next recovery would repeat the old reason
func TestRaisingAgainForgetsTheLastResolution(t *testing.T) {
	c := auditConfig(t)
	c.Chains["Chain"] = &ChainConfig{name: "Chain", ChainId: "chain-1"}
	message := "Kynraze has missed 5 blocks on chain-1"
	resolve("Chain", message, resolution{"ℹ️", "No longer watched", "off"})
	id := "x"
	c.alert("Chain", message, "warning", false, &id)
	if _, ok := storedResolution("Chain", message); ok {
		t.Fatal("the old resolution survived the alarm being raised again")
	}
}

func TestOrphanedAlarmIsNotCalledFixed(t *testing.T) {
	r := orphanResolution("Severity: critical\nRPC node https://rpc.example.com:443 has been down for > 3 minutes on atomone-1")
	if r.label != "No longer watched" || r.text != "RPC node https://rpc.example.com:443 has been down for > 3 minutes on atomone-1\nThe alert was turned off or its settings changed" {
		t.Fatalf("orphan resolution %+v", r)
	}
}

// TestFibreSendsWhatClearedFirst: "registered" reads before the problem found on the new host
func TestFibreSendsWhatClearedFirst(t *testing.T) {
	c := auditConfig(t)
	cc := &ChainConfig{name: "Mocha", ChainId: "mocha-5", fibreLoaded: true,
		fibre: fibreState{Registered: true, Host: "1.2.3.4:7980", Reachable: true, KeyKnown: true, IdentityError: "x"}}
	cc.Alerts.FibreAlerts, cc.Alerts.FibreSeverity = true, "warning"
	c.Chains["Mocha"] = cc
	alarms.AllAlarms["Mocha"] = map[string]time.Time{fibreNotRegisteredMessage("mocha-5"): time.Now()}
	cc.fibreAlarms(&ValInfo{Valcons: "cons", Moniker: "Kynraze"}, false)
	first, second := <-c.alertChan, <-c.alertChan
	if !first.resolved || second.resolved {
		t.Fatalf("sent %q (resolved %v) before %q (resolved %v)", first.message, first.resolved, second.message, second.resolved)
	}
	if got := first.summary(); got != "Kynraze is a registered Fibre provider on mocha-5 at 1.2.3.4:7980" {
		t.Errorf("registered reads %q", got)
	}
}
