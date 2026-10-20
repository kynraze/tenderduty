package tenderduty

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	rpchttp "github.com/tendermint/tendermint/rpc/client/http"
	"google.golang.org/protobuf/encoding/protowire"
)

func containsMessage(messages []string, wanted string) bool {
	for _, message := range messages {
		if message == wanted {
			return true
		}
	}
	return false
}

func TestSignalBinaryMajor(t *testing.T) {
	for _, tc := range []struct {
		binary string
		major  uint64
		known  bool
	}{
		{"10.4.0-corto", 10, true}, {"v11.0.0-mocha", 11, true}, {"11.0.0-rc1", 11, true}, {"abc", 0, false}, {"", 0, false},
	} {
		major, known := parseBinaryMajor(tc.binary)
		if major != tc.major || known != tc.known {
			t.Fatalf("%q: %d %v", tc.binary, major, known)
		}
	}
	if signalBinaryDetail("abc", 11) != "" || signalBinaryDetail("v11.0.0-mocha", 11) != "RPC node ready: celestia-app 11.0.0-mocha · own node unknown (none with alert_if_down)" || signalBinaryDetail("10.4.0-corto", 11) != "RPC node runs celestia-app 10.4.0-corto, version 11 needs v11 or later" {
		t.Fatal("wrong binary detail")
	}
}

func TestSignalETAFixedZone(t *testing.T) {
	eta := time.Date(2026, 10, 7, 7, 20, 0, 0, time.UTC)
	for _, tc := range []struct {
		loc  *time.Location
		zone string
	}{{time.FixedZone("WIB", 7*3600), "WIB"}, {time.FixedZone("", 7*3600), "+0700"}} {
		want := "≈ Wed 7 Oct 14:20 " + tc.zone + ", in 2d 3h"
		if got := formatUpgradeETA(eta, eta.Add(-51*time.Hour), tc.loc); got != want {
			t.Fatalf("%q, want %q", got, want)
		}
	}
	c := auditConfig(t)
	c.QuietHours.Timezone = "UTC+7"
	if alertLocation().String() != "UTC+7" {
		t.Fatal("quiet timezone ignored")
	}
	c.Reports.Timezone = "UTC-5"
	if alertLocation().String() != "UTC-5" {
		t.Fatal("report timezone ignored")
	}
}

func TestSignalScheduledBinaryReadiness(t *testing.T) {
	for _, binary := range []string{"11.0.0", "v12.0.0-mocha", "10.4.0-corto", "abc"} {
		t.Run(binary, func(t *testing.T) {
			c := auditConfig(t)
			cc := &ChainConfig{name: "Chain", lastBlockNum: 500, signalLoaded: true, signal: signalState{Version: 11, UpgradeVersion: 11, UpgradeHeight: 1000, BinaryVersion: binary}}
			cc.Alerts.SignalRemindMinutes = 60
			cc.Alerts.UpgradeAlerts = true
			c.Chains[cc.name] = cc
			upgradeETAs.Store("Chain@1000", time.Now().Add(30*time.Minute))
			defer upgradeETAs.Delete("Chain@1000")
			val := &ValInfo{Valcons: "cons"}
			cc.signalAlarms(val, false)
			soon := signalUpgradeSoonMessage(11, 1000)
			if alarmIsActive(cc.name, soon) == signalBinaryReady(binary, 11) {
				t.Fatal("wrong close reminder")
			}
			if !alarmIsActive(cc.name, signalUpgradeMessage(11, 1000)) {
				t.Fatal("missing scheduled alarm")
			}
			cc.signal.BinaryVersion = "11.0.0"
			cc.signalAlarms(val, false)
			if alarmIsActive(cc.name, soon) {
				t.Fatal("ready reminder active")
			}
			if !signalBinaryReady(binary, 11) {
				r, ok := storedResolution(cc.name, soon)
				if !ok || r.text != "RPC node ready: celestia-app 11.0.0 · own node unknown (none with alert_if_down)" {
					t.Fatalf("resolution %+v", r)
				}
				checkCard(t, "binary ready", (&alertMsg{chain: cc.name, message: soon, resolved: true}).card())
			}
		})
	}
}

// TestSignalCloseReminderSurvivesETAJitter: block-time changes move the ETA around the reminder threshold.
func TestSignalCloseReminderSurvivesETAJitter(t *testing.T) {
	c := auditConfig(t)
	cc := &ChainConfig{name: "Chain", signalLoaded: true, lastBlockNum: 500, signal: signalState{Version: 11, UpgradeVersion: 11, UpgradeHeight: 1000, BinaryVersion: "10.4.0-corto"}}
	cc.Alerts.UpgradeAlerts = true
	cc.Alerts.SignalRemindMinutes = 10
	c.Chains[cc.name] = cc
	defer upgradeETAs.Delete("Chain@1000")
	val := &ValInfo{Valcons: "cons"}
	soon := signalUpgradeSoonMessage(11, 1000)
	closeMessages := func() (sent, resolved int) {
		for {
			select {
			case msg := <-c.alertChan:
				if strings.HasSuffix(msg.message, " is close") {
					sent++
					if msg.resolved {
						resolved++
					}
				}
			default:
				return
			}
		}
	}
	total := 0
	for _, eta := range []time.Duration{9*time.Minute + 59*time.Second, 10*time.Minute + time.Second, 9*time.Minute + 58*time.Second} {
		upgradeETAs.Store("Chain@1000", time.Now().Add(eta))
		cc.signalAlarms(val, false)
		sent, resolved := closeMessages()
		total += sent
		if resolved != 0 || !alarmIsActive(cc.name, soon) {
			t.Fatalf("ETA %s: reminder resolved", eta)
		}
	}
	if total != 1 {
		t.Fatalf("%d reminder messages, want 1", total)
	}
	// a failed read says nothing about the upgrade
	cc.signalReadFailed = true
	cc.signalAlarms(val, false)
	if sent, _ := closeMessages(); sent != 0 || !alarmIsActive(cc.name, soon) {
		t.Fatal("reminder resolved on a failed read")
	}
	cc.signalReadFailed = false
	cc.signal = signalState{Version: 11, UpgradeVersion: 11, UpgradeHeight: 2000, BinaryVersion: "10.4.0-corto"}
	cc.signalAlarms(val, false)
	if alarmIsActive(cc.name, soon) {
		t.Fatal("rescheduled upgrade kept the old reminder")
	}
	if r, ok := storedResolution(cc.name, soon); !ok || r.text != "upgrade cancelled or rescheduled" {
		t.Fatalf("rescheduled resolution %+v", r)
	}
}

func TestSignalStoppedAtHeight(t *testing.T) {
	for _, rpcFailed := range []bool{false, true} {
		c := auditConfig(t)
		state := signalState{Version: 11, UpgradeVersion: 11, UpgradeHeight: 1000, BinaryVersion: "10.4.0-corto"}
		cc := &ChainConfig{name: "Chain", signalLoaded: true, signal: state, lastBlockNum: 999}
		cc.Alerts.UpgradeAlerts = true
		c.Chains[cc.name] = cc
		val := &ValInfo{Valcons: "cons"}
		cc.signalAlarms(val, false)
		cc.lastBlockNum, cc.lastBlockTime = 1000, time.Now().Add(-3*time.Minute)
		cc.signalAlarms(val, false)
		cc.signalWatchSince = time.Now().Add(-3 * time.Minute)
		cc.signalRPCFailed = rpcFailed
		if rpcFailed {
			cc.lastBlockTime = time.Now()
		}
		cc.signalAlarms(val, false)
		message := signalStoppedMessage(11)
		if !alarmIsActive(cc.name, message) {
			t.Fatal("no stopped alarm")
		}
		if got := (&alertMsg{chain: cc.name, message: message}).summary(); !strings.Contains(got, "upgrade celestia-appd to v11 or later") {
			t.Fatal(got)
		}
		for _, alarm := range []string{signalUpgradeMessage(11, 1000), signalUpgradeSoonMessage(11, 1000)} {
			if r, ok := storedResolution(cc.name, alarm); ok && r.text != "upgrade height reached" {
				t.Fatalf("height resolution %+v", r)
			}
		}
		cc.lastBlockNum, cc.lastBlockTime = 1001, time.Now()
		cc.signalAlarms(val, false)
		if alarmIsActive(cc.name, message) {
			t.Fatal("stopped alarm did not recover")
		}
		checkCard(t, "blocks advancing", (&alertMsg{chain: cc.name, message: message, resolved: true}).card())
	}
}

func TestLegacySignalNewsClearSilently(t *testing.T) {
	c := auditConfig(t)
	cc := &ChainConfig{name: "Chain", signalLoaded: true, signal: signalState{Version: 11, Power: 90, Threshold: 84, Total: 100}}
	cc.Alerts.SignalPercent = 5
	cc.Alerts.UpgradeAlerts = true
	c.Chains[cc.name] = cc
	alarms.AllAlarms[cc.name] = map[string]time.Time{signalStartMessage(11): time.Now(), signalThresholdMessage(11): time.Now()}
	cc.signalAlarms(&ValInfo{Valcons: "cons"}, false)
	announced, quiet := recoveries(t, c)
	if len(announced) != 0 || len(quiet) != 2 || !alarmIsActive(cc.name, signalMissingMessage(11)) {
		t.Fatalf("announced %v quiet %v", announced, quiet)
	}
}

func TestScheduledSignalRefreshReadsSoftwareVersion(t *testing.T) {
	binary := "10.4.0-corto"
	infoReads := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			ID     interface{}                `json:"id"`
			Method string                     `json:"method"`
			Params map[string]json.RawMessage `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		response := map[string]interface{}{"app_version": "10", "version": binary}
		if request.Method == "abci_info" {
			infoReads++
		} else {
			var path string
			_ = json.Unmarshal(request.Params["path"], &path)
			var value []byte
			if path == "/celestia.signal.v1.Query/GetUpgrade" {
				inner := protowire.AppendVarint(protowire.AppendTag(nil, 1, protowire.VarintType), 11)
				inner = protowire.AppendVarint(protowire.AppendTag(inner, 2, protowire.VarintType), 1000)
				value = protowire.AppendBytes(protowire.AppendTag(nil, 1, protowire.BytesType), inner)
			}
			response = map[string]interface{}{"code": 0, "value": value}
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"jsonrpc": "2.0", "id": request.ID, "result": map[string]interface{}{"response": response}})
	}))
	defer server.Close()
	client, err := rpchttp.New(server.URL, "/websocket")
	if err != nil {
		t.Fatal(err)
	}
	cc := &ChainConfig{ValAddress: "celestiavaloper1q92rn4rnvjygajsjf6dzm4fsc92a60c3m6x9ur"}
	cc.refreshSignal(context.Background(), client)
	if !cc.signalLoaded || cc.signal.BinaryVersion != binary || cc.signal.UpgradeHeight != 1000 {
		t.Fatalf("signal %+v", cc.signal)
	}
	binary = "v11.0.0-mocha"
	cc.signalReadFailed = true
	cc.refreshSignal(context.Background(), client)
	if infoReads != 2 || cc.signalReadFailed || !signalBinaryReady(cc.signal.BinaryVersion, 11) {
		t.Fatalf("signal %+v, reads %d", cc.signal, infoReads)
	}
}

func TestReadySignalNodeNeverRaisesStoppedAlarm(t *testing.T) {
	c := auditConfig(t)
	cc := &ChainConfig{name: "Chain", signalLoaded: true, lastBlockNum: 999, signal: signalState{Version: 11, UpgradeVersion: 11, UpgradeHeight: 1000, BinaryVersion: "11.0.0"}}
	cc.Alerts.UpgradeAlerts = true
	c.Chains[cc.name] = cc
	val := &ValInfo{Valcons: "cons"}
	cc.signalAlarms(val, false)
	cc.lastBlockNum, cc.signalRPCFailed = 1000, true
	cc.signalAlarms(val, false)
	if alarmIsActive(cc.name, signalStoppedMessage(11)) {
		t.Fatal("ready node raised stopped alarm")
	}
}
