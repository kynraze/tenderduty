package tenderduty

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
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
