package tenderduty

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
	"time"
)

// recoveries returns the recoveries chats get, and the quiet ones.
func recoveries(t *testing.T, c *Config) (announced, quiet []string) {
	t.Helper()
	for {
		select {
		case a := <-c.alertChan:
			if !a.resolved {
				continue
			}
			if a.quiet {
				quiet = append(quiet, a.message)
			} else {
				announced = append(announced, a.message)
			}
		default:
			sort.Strings(announced)
			sort.Strings(quiet)
			return
		}
	}
}

func expectOne(t *testing.T, c *Config, announce string, quietly ...string) {
	t.Helper()
	announced, quiet := recoveries(t, c)
	if len(announced) != 1 || announced[0] != announce {
		t.Errorf("announced %q, want only %q", announced, announce)
	}
	sort.Strings(quietly)
	if len(quiet) != len(quietly) {
		t.Fatalf("cleared quietly %q, want %q", quiet, quietly)
	}
	for i := range quiet {
		if quiet[i] != quietly[i] {
			t.Fatalf("cleared quietly %q, want %q", quiet, quietly)
		}
	}
}

func TestAProposalsAlarmAndReminderClearAsOneMessage(t *testing.T) {
	c := auditConfig(t)
	cc := &ChainConfig{name: "Chain", ChainId: "chain-1", proposalsLoaded: true}
	c.Chains["Chain"] = cc
	p := govProposal{ID: 7, Title: "Raise the limit", VotingEnd: time.Now().Add(time.Hour)}
	open, remind := cc.govOpenMessage(p), cc.govRemindMessage(p, "Kynraze")
	alarms.AllAlarms["Chain"] = map[string]time.Time{open: time.Now(), remind: time.Now()}
	cc.govAlarms(&ValInfo{Valcons: "cons", Moniker: "Kynraze"}, false)
	expectOne(t, c, remind, open)

	// the proposal notice announces a vote cast before any reminder
	alarms.AllAlarms["Chain"] = map[string]time.Time{open: time.Now()}
	cc.govAlarms(&ValInfo{Valcons: "cons", Moniker: "Kynraze"}, false)
	expectOne(t, c, open)
}

func TestAnUpgradesAlarmAndReminderClearAsOneMessage(t *testing.T) {
	c := auditConfig(t)
	cc := &ChainConfig{name: "Chain", ChainId: "chain-1", upgradeLoaded: true, lastBlockNum: 1001}
	c.Chains["Chain"] = cc
	plan := &upgradePlan{Name: "v4", Height: 1000}
	alarms.AllAlarms["Chain"] = map[string]time.Time{upgradeMessage(plan): time.Now(), upgradeSoonMessage(plan): time.Now()}
	cc.upgradeAlarms(&ValInfo{Valcons: "cons"}, false)
	expectOne(t, c, upgradeSoonMessage(plan), upgradeMessage(plan))
}

func TestAVersionsSignalAlarmsClearAsOneMessage(t *testing.T) {
	c := auditConfig(t)
	// state.Version is the next version, 11
	cc := &ChainConfig{name: "Celestia", ChainId: "celestia", signalLoaded: true, signal: signalState{Version: 11}, lastBlockNum: 2000}
	cc.Alerts.SignalPercent = 10
	cc.Alerts.UpgradeAlerts = true
	c.Chains["Celestia"] = cc
	started, missing := signalStartMessage(10), signalMissingMessage(10)
	upgrade, soon := signalUpgradeMessage(10, 1500), signalUpgradeSoonMessage(10, 1500)
	alarms.AllAlarms["Celestia"] = map[string]time.Time{started: time.Now(), missing: time.Now(), upgrade: time.Now(), soon: time.Now()}
	cc.signalAlarms(&ValInfo{Valcons: "cons", Moniker: "Kynraze"}, false)
	expectOne(t, c, soon, started, missing, upgrade)

	// signalling dropped: the missing signal goes with it
	cc.signal = signalState{Version: 10, Power: 1, Total: 100}
	alarms.AllAlarms["Celestia"] = map[string]time.Time{started: time.Now(), missing: time.Now()}
	cc.signalAlarms(&ValInfo{Valcons: "cons", Moniker: "Kynraze"}, false)
	expectOne(t, c, missing, started)

	// the missing-signal recovery announces that the validator signalled
	cc.signal = signalState{Version: 10, Power: 50, Total: 100, Signalled: true}
	alarms.AllAlarms["Celestia"] = map[string]time.Time{started: time.Now(), missing: time.Now()}
	cc.signalAlarms(&ValInfo{Valcons: "cons", Moniker: "Kynraze"}, false)
	expectOne(t, c, missing, started)
}

func TestAThresholdClearedByItsScheduledUpgradeIsQuiet(t *testing.T) {
	c := auditConfig(t)
	cc := &ChainConfig{name: "Celestia", ChainId: "celestia", signalLoaded: true, lastBlockNum: 1000,
		signal: signalState{Version: 10, Power: 90, Total: 100, Threshold: 84, Signalled: true, UpgradeHeight: 1500}}
	cc.Alerts.SignalPercent = 10
	cc.Alerts.UpgradeAlerts = true
	c.Chains["Celestia"] = cc
	threshold := signalThresholdMessage(10)
	alarms.AllAlarms["Celestia"] = map[string]time.Time{signalStartMessage(10): time.Now(), threshold: time.Now()}
	cc.signalAlarms(&ValInfo{Valcons: "cons", Moniker: "Kynraze"}, false)
	announced, quiet := recoveries(t, c)
	// the scheduled-upgrade alarm announces this recovery
	if len(announced) != 0 || len(quiet) != 2 || !containsMessage(quiet, threshold) || !containsMessage(quiet, signalStartMessage(10)) {
		t.Fatalf("announced %q, quiet %q", announced, quiet)
	}
	if !alarmIsActive("Celestia", signalUpgradeMessage(10, 1500)) {
		t.Fatal("the upgrade alarm was not raised")
	}
}

// TestAnUnjailIsOneMessage: recovery announces the active set once; legacy "can unjail" alarms clear quietly.
func TestAnUnjailIsOneMessage(t *testing.T) {
	fastWatch(t)
	oldConfig, oldAlarms := td, alarms
	ctx, cancel := context.WithCancel(context.Background())
	chain := &ChainConfig{name: "Juno", ChainId: "juno-1", ValAddress: "junovaloper1example", observedBlock: true, lastBlockTime: time.Now(),
		valInfo: &ValInfo{Moniker: "Kynraze", Bonded: true, Window: 100, Valcons: "junovalcons1example"}}
	chain.Alerts.MissedBlocksAlerts, chain.Alerts.AlertIfInactive = true, true
	jailed, unjail := "Kynraze is jailed on juno-1", "Kynraze can unjail on juno-1"
	alarms = newAlarmCache()
	alarms.AllAlarms["Juno"] = map[string]time.Time{jailed: time.Now(), unjail: time.Now()}
	td = &Config{ctx: ctx, alertChan: make(chan *alertMsg, 8), Chains: map[string]*ChainConfig{"Juno": chain}}
	done := make(chan struct{})
	go func() { chain.watch(); close(done) }()
	defer func() {
		cancel()
		<-done
		td, alarms = oldConfig, oldAlarms
	}()
	got := make(map[string]bool) // message to quiet
	deadline := time.After(5 * time.Second)
	for len(got) < 2 {
		select {
		case a := <-td.alertChan:
			if a.resolved {
				got[a.message] = a.quiet
			}
		case <-deadline:
			t.Fatalf("recoveries %v", got)
		}
	}
	if quiet, ok := got[jailed]; !ok || quiet {
		t.Errorf("back in the active set: sent %v, quiet %v", ok, quiet)
	}
	if quiet, ok := got[unjail]; !ok || !quiet {
		t.Errorf("can unjail: sent %v, quiet %v", ok, quiet)
	}
}

func TestAMovedFibreHostIsOneMessage(t *testing.T) {
	c := auditConfig(t)
	c.NodeDownMin = 5
	const old, moved = "old.example:7980", "new.example:7980"
	cc := &ChainConfig{name: "Chain", ChainId: "chain-1", fibreLoaded: true,
		fibre: fibreState{Registered: true, Host: moved, Reachable: true, KeyKnown: true, IdentityOK: true}}
	cc.Alerts.FibreAlerts = true
	c.Chains["Chain"] = cc
	down, identity := fibreDownMessage(old, "chain-1"), fibreIdentityMessage(old, "chain-1")
	alarms.AllAlarms["Chain"] = map[string]time.Time{down: time.Now(), identity: time.Now()}
	cc.fibreAlarms(&ValInfo{Valcons: "cons", Moniker: "Kynraze"}, false)
	announced, quiet := recoveries(t, c)
	if len(announced) != 1 || len(quiet) != 1 {
		t.Fatalf("announced %q, quiet %q", announced, quiet)
	}
}

// TestAQuietRecoverySendsNothingButClears: clearing lets the alarm be sent again later
func TestAQuietRecoverySendsNothingButClears(t *testing.T) {
	auditConfig(t)
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	msg := &alertMsg{disc: true, chain: "Chain", message: "Kynraze is tombstoned on chain-1", discHook: server.URL}
	if err := notifyDiscord(msg); err != nil || calls != 1 {
		t.Fatalf("the alert was not sent: %v, %d calls", err, calls)
	}
	msg.resolved, msg.quiet = true, true
	if err := notifyDiscord(msg); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("a quiet recovery was sent: %d calls", calls)
	}
	if alarms.sentAnywhere("Chain\x00"+msg.message, msg.message) {
		t.Fatal("the alert is still recorded as sent")
	}
	msg.resolved, msg.quiet = false, false
	if err := notifyDiscord(msg); err != nil || calls != 2 {
		t.Fatalf("the alert could not be sent again: %v, %d calls", err, calls)
	}
}

func TestQuietSurvivesTheQueue(t *testing.T) {
	c := auditConfig(t)
	c.Chains["Chain"] = &ChainConfig{name: "Chain", ChainId: "chain-1"}
	c.Discord.Enabled = yes(true)
	c.Chains["Chain"].Alerts.Discord.Webhook = "http://127.0.0.1:1/hook"
	message := "Kynraze is tombstoned on chain-1"
	alarms.AllAlarms["Chain"] = map[string]time.Time{message: time.Now()}
	alarms.SentDiAlarms["Chain\x00"+message] = time.Now()
	id := "cons-tombstoned"
	c.resolveQuietly("Chain", message, &id)
	alarms.notifyMux.RLock()
	defer alarms.notifyMux.RUnlock()
	if r := alarms.PendingRecoveries["Chain\x00"+message]; r == nil || !r.Quiet {
		t.Fatalf("pending recovery %+v", r)
	}
	found := false
	for _, item := range alarms.Outbox {
		if item.Message == message && item.Resolved {
			found = true
			if !item.Quiet {
				t.Fatalf("queued recovery %+v is not quiet", item)
			}
		}
	}
	if !found {
		t.Fatal("the recovery was not queued")
	}
}
