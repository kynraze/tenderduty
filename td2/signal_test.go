package tenderduty

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	rpchttp "github.com/tendermint/tendermint/rpc/client/http"
	"google.golang.org/protobuf/encoding/protowire"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestParseTally(t *testing.T) {
	// Captured from Celestia mainnet, 3 Oct 2026: version 10, nobody had signalled.
	absent, err := hex.DecodeString("109ced96bc0118d49ce8e101")
	if err != nil {
		t.Fatal(err)
	}
	var signalled []byte
	for num, value := range map[protowire.Number]uint64{1: 52000000, 2: 394639004, 3: 473566804} {
		signalled = protowire.AppendVarint(protowire.AppendTag(signalled, num, protowire.VarintType), value)
	}
	for _, tc := range []struct {
		name  string
		data  []byte
		power uint64
	}{
		{"mainnet absent voting power", absent, 0},
		{"with voting power", signalled, 52000000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			power, threshold, total, err := parseTally(tc.data)
			if err != nil {
				t.Fatal(err)
			}
			if power != tc.power || threshold != 394639004 || total != 473566804 {
				t.Fatalf("power %d threshold %d total %d", power, threshold, total)
			}
		})
	}
}

func TestParseSignalUpgrade(t *testing.T) {
	// the empty value from GetUpgrade is no upgrade
	upgrade, err := parseSignalUpgrade(nil)
	if upgrade != nil || err != nil {
		t.Fatalf("empty response gave %+v, %v", upgrade, err)
	}
	inner := protowire.AppendVarint(protowire.AppendTag(nil, 1, protowire.VarintType), 10)
	inner = protowire.AppendVarint(protowire.AppendTag(inner, 2, protowire.VarintType), 12345678)
	upgrade, err = parseSignalUpgrade(protowire.AppendBytes(protowire.AppendTag(nil, 1, protowire.BytesType), inner))
	if err != nil || upgrade == nil || upgrade.Version != 10 || upgrade.Height != 12345678 {
		t.Fatalf("got %+v, %v", upgrade, err)
	}
}

func TestParseSignalUpgradeRejectsABogusHeight(t *testing.T) {
	inner := protowire.AppendVarint(protowire.AppendTag(nil, 1, protowire.VarintType), 10)
	inner = protowire.AppendVarint(protowire.AppendTag(inner, 2, protowire.VarintType), 1<<63)
	upgrade, err := parseSignalUpgrade(protowire.AppendBytes(protowire.AppendTag(nil, 1, protowire.BytesType), inner))
	if err == nil || upgrade != nil {
		t.Fatalf("an out of range height was accepted: %+v, %v", upgrade, err)
	}
}

func TestParseSignalledVersion(t *testing.T) {
	version, signalled, err := parseSignalledVersion(nil)
	if version != 0 || signalled || err != nil {
		t.Fatalf("empty value gave %d, %v, %v", version, signalled, err)
	}
	b := binary.BigEndian.AppendUint64(nil, 10)
	if string(b) != "\x00\x00\x00\x00\x00\x00\x00\x0a" {
		t.Fatalf("test bytes are not big-endian: %x", b)
	}
	version, signalled, err = parseSignalledVersion(b)
	if version != 10 || !signalled || err != nil {
		t.Fatalf("got %d, %v, %v", version, signalled, err)
	}
	if _, _, err = parseSignalledVersion([]byte{1, 2, 3}); err == nil {
		t.Fatal("a short value was accepted")
	}
}

func TestSignalMessagesAreStable(t *testing.T) {
	for message, want := range map[string]string{
		signalStartMessage(10):                 "Version 10 signalling has started",
		signalMissingMessage(10):               "Validator has not signalled version 10",
		signalThresholdMessage(10):             "Version 10 reached the upgrade threshold",
		signalUpgradeMessage(10, 12345678):     "Version 10 upgrade at height 12,345,678",
		signalUpgradeSoonMessage(10, 12345678): "Version 10 upgrade at height 12,345,678 is close",
	} {
		if message != want {
			t.Errorf("got %q, want %q", message, want)
		}
		if !isSignalMessage(message) {
			t.Errorf("%q is not recognised as a signal message", message)
		}
	}
	if isSignalMessage("Upgrade v5 at height 12") {
		t.Error("an x/upgrade message was taken for a signal one")
	}
}

func TestSignalAlarms(t *testing.T) {
	const (
		started   = "Version 10 signalling has started"
		missing   = "Validator has not signalled version 10"
		threshold = "Version 10 reached the upgrade threshold"
		scheduled = "Version 10 upgrade at height 1,000"
		soon      = "Version 10 upgrade at height 1,000 is close"
	)
	base := signalState{Version: 10, Threshold: 670, Total: 1000}
	for _, tc := range []struct {
		name         string
		before       signalState
		after        signalState
		eta          time.Duration
		active, gone []string
	}{
		{"signalling starts", base, signalState{Version: 10, Power: 100, Threshold: 670, Total: 1000}, 0, []string{missing}, []string{started, threshold}},
		{"below the percentage", base, signalState{Version: 10, Power: 99, Threshold: 670, Total: 1000}, 0, nil, []string{started, missing}},
		{"our validator signals", signalState{Version: 10, Power: 300, Threshold: 670, Total: 1000}, signalState{Version: 10, Power: 300, Threshold: 670, Total: 1000, Signalled: true}, 0, nil, []string{started, missing}},
		{"threshold reached", signalState{Version: 10, Power: 300, Threshold: 670, Total: 1000, Signalled: true}, signalState{Version: 10, Power: 700, Threshold: 670, Total: 1000, Signalled: true}, 0, nil, []string{started, threshold}},
		{"upgrade scheduled", signalState{Version: 10, Power: 700, Threshold: 670, Total: 1000, Signalled: true}, signalState{Version: 10, Power: 700, Threshold: 670, Total: 1000, Signalled: true, UpgradeVersion: 10, UpgradeHeight: 1000}, 0, []string{scheduled}, []string{threshold}},
		{"upgrade is close", signalState{Version: 10, Power: 700, Threshold: 670, Total: 1000, Signalled: true, UpgradeVersion: 10, UpgradeHeight: 1000}, signalState{Version: 10, Power: 700, Threshold: 670, Total: 1000, Signalled: true, UpgradeVersion: 10, UpgradeHeight: 1000}, 30 * time.Minute, []string{scheduled, soon}, []string{threshold}},
		{"upgrade cancelled", signalState{Version: 10, Power: 700, Threshold: 670, Total: 1000, Signalled: true, UpgradeVersion: 10, UpgradeHeight: 1000}, signalState{Version: 10, Power: 700, Threshold: 670, Total: 1000, Signalled: true}, 0, nil, []string{threshold, scheduled, soon}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := auditConfig(t)
			chain := &ChainConfig{name: "Chain", ChainId: "celestia", lastBlockNum: 500, signal: tc.before, signalLoaded: true}
			chain.Alerts.SignalPercent, chain.Alerts.SignalRemindMinutes = 10, 60
			chain.Alerts.UpgradeAlerts = true
			c.Chains["Chain"] = chain
			valInfo := &ValInfo{Valcons: "cons"}
			upgradeETAs.Delete("Chain@1000")
			if tc.eta > 0 {
				upgradeETAs.Store("Chain@1000", time.Now().Add(tc.eta))
				defer upgradeETAs.Delete("Chain@1000")
			}
			chain.signalAlarms(valInfo, false)
			chain.stateMux.Lock()
			chain.signal = tc.after
			chain.stateMux.Unlock()
			chain.signalAlarms(valInfo, false)
			for _, message := range tc.active {
				if !alarmIsActive("Chain", message) {
					t.Errorf("%q should be active", message)
				}
			}
			for _, message := range tc.gone {
				if alarmIsActive("Chain", message) {
					t.Errorf("%q should have resolved", message)
				}
			}
			chain.stateMux.Lock()
			chain.signal = signalState{Version: 11, Threshold: 670, Total: 1000}
			chain.stateMux.Unlock()
			chain.signalAlarms(valInfo, false)
			if left := activeMessages("Chain", "Version "); len(left) != 0 {
				t.Errorf("alarms did not resolve: %v", left)
			}
			if left := activeMessages("Chain", signalMissingPrefix); len(left) != 0 {
				t.Errorf("alarms did not resolve: %v", left)
			}
		})
	}
}

func TestSignalAlarmIds(t *testing.T) {
	valInfo := &ValInfo{Valcons: "cons"}
	seen := make(map[string]string)
	for _, message := range []string{
		signalStartMessage(10), signalMissingMessage(10), signalRemindMessage(10), signalThresholdMessage(10),
		signalUpgradeMessage(10, 1000), signalUpgradeSoonMessage(10, 1000), signalStartMessage(11),
		// same version rescheduled
		signalUpgradeMessage(10, 2000), signalUpgradeSoonMessage(10, 2000),
	} {
		id := signalAlarmId(valInfo, message)
		if other, ok := seen[id]; ok {
			t.Errorf("%q and %q share the id %s", message, other, id)
		}
		seen[id] = message
	}
	if got := fmt.Sprint(signalAlarmId(valInfo, signalMissingMessage(10))); got != "conssignalmissing10" {
		t.Errorf("id is %s", got)
	}
}

// Answer abci_info with version 9 and count abci_query requests returning the given code.
func signalNode(t *testing.T, codespace string, code int, queries *int) *rpchttp.HTTP {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			ID     interface{} `json:"id"`
			Method string      `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		response := map[string]interface{}{"app_version": "9"}
		if request.Method == "abci_query" {
			*queries++
			response = map[string]interface{}{"code": code, "codespace": codespace, "log": "unknown query path: unknown request"}
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"jsonrpc": "2.0", "id": request.ID, "result": map[string]interface{}{"response": response}})
	}))
	t.Cleanup(server.Close)
	client, err := rpchttp.New(server.URL, "/websocket")
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestChainWithoutSignalIsAskedOnce(t *testing.T) {
	queries := 0
	client := signalNode(t, "sdk", 6, &queries)
	chain := &ChainConfig{ChainId: "atomone-1", ValAddress: "celestiavaloper1q92rn4rnvjygajsjf6dzm4fsc92a60c3m6x9ur"}
	chain.refreshSignal(context.Background(), client)
	chain.refreshSignal(context.Background(), client)
	if queries != 1 || !chain.signalUnsupported || chain.signalLoaded {
		t.Fatalf("%d queries, unsupported %v, loaded %v", queries, chain.signalUnsupported, chain.signalLoaded)
	}
}

func TestSignalFailureIsRetried(t *testing.T) {
	for _, tc := range []struct {
		codespace string
		code      int
	}{{"sdk", 1}, {"signal", 6}} {
		queries := 0
		client := signalNode(t, tc.codespace, tc.code, &queries)
		chain := &ChainConfig{ChainId: "celestia", ValAddress: "celestiavaloper1q92rn4rnvjygajsjf6dzm4fsc92a60c3m6x9ur"}
		chain.refreshSignal(context.Background(), client)
		chain.refreshSignal(context.Background(), client)
		if queries != 2 || chain.signalUnsupported {
			t.Fatalf("%s %d: %d queries, unsupported %v", tc.codespace, tc.code, queries, chain.signalUnsupported)
		}
	}
}

// TestRescheduledSignalUpgradeKeepsItsIncident: the new upgrade is raised before the old one resolves, they must not share a dedup key
func TestRescheduledSignalUpgradeKeepsItsIncident(t *testing.T) {
	c := auditConfig(t)
	chain := &ChainConfig{name: "Celestia", ChainId: "celestia", lastBlockNum: 100}
	chain.Alerts.SignalPercent, chain.Alerts.SignalRemindMinutes = 10, 60
	chain.Alerts.UpgradeAlerts = true
	c.Chains["Celestia"] = chain
	valInfo := &ValInfo{Valcons: "cons"}
	alarms.AllAlarms["Celestia"] = map[string]time.Time{signalUpgradeMessage(10, 1_000_000): time.Now()}
	chain.signal = signalState{Version: 10, Power: 90, Threshold: 84, Total: 100, Signalled: true, UpgradeVersion: 10, UpgradeHeight: 2_000_000}
	chain.signalLoaded = true
	chain.signalAlarms(valInfo, false)

	triggered, resolved := make(map[string]string), make(map[string]string)
	for len(c.alertChan) > 0 {
		alert := <-c.alertChan
		if alert.resolved {
			resolved[alert.uniqueId] = alert.message
		} else {
			triggered[alert.uniqueId] = alert.message
		}
	}
	if _, ok := resolved[signalAlarmId(valInfo, signalUpgradeMessage(10, 1_000_000))]; !ok {
		t.Fatalf("the cancelled upgrade was not resolved: %v", resolved)
	}
	for id, message := range triggered {
		if old, ok := resolved[id]; ok {
			t.Fatalf("%q and the resolved %q share the dedup key %s", message, old, id)
		}
	}
}

func TestSignalReminder(t *testing.T) {
	for _, tc := range []struct {
		name    string
		power   uint64
		percent int
		enabled bool
		want    bool
	}{
		{"below reminder", 659, 66, true, false},
		{"at reminder", 660, 66, true, true},
		{"above reminder", 710, 66, true, true},
		{"at threshold", 834, 66, true, false},
		{"equal to signal percent", 710, 5, true, false},
		{"below signal percent", 710, 4, true, false},
		{"disabled", 710, 66, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := auditConfig(t)
			cc := &ChainConfig{name: "Celestia", ChainId: "celestia", signalLoaded: true,
				signal: signalState{Version: 10, Power: tc.power, Threshold: 834, Total: 1000}}
			cc.Alerts.SignalPercent, cc.Alerts.SignalRemindPercent = 5, tc.percent
			cc.Alerts.UpgradeAlerts = tc.enabled
			c.Chains[cc.name] = cc
			cc.signalAlarms(&ValInfo{Valcons: "cons"}, false)
			message := signalRemindMessage(10)
			if alarmIsActive(cc.name, message) != tc.want {
				t.Fatalf("reminder active = %v, want %v", alarmIsActive(cc.name, message), tc.want)
			}
			count := 0
			for len(c.alertChan) > 0 {
				a := <-c.alertChan
				if a.message == message {
					count++
					if a.severity != "critical" || a.resolved {
						t.Fatalf("reminder severity %s, resolved %v", a.severity, a.resolved)
					}
				}
			}
			if tc.want && count != 1 {
				t.Fatalf("got %d reminders, want one", count)
			}
			cc.signalAlarms(&ValInfo{Valcons: "cons"}, false)
			if len(c.alertChan) != 0 {
				t.Fatal("unchanged signalling repeated a notification")
			}
		})
	}
}

func TestSignalReminderRecovery(t *testing.T) {
	for _, tc := range []struct {
		name   string
		state  signalState
		quiet  bool
		active string
	}{
		{"validator signalled", signalState{Version: 10, Power: 710, Threshold: 834, Total: 1000, Signalled: true}, false, ""},
		{"threshold takes over", signalState{Version: 10, Power: 834, Threshold: 834, Total: 1000}, true, ""},
		{"scheduled upgrade takes over", signalState{Version: 10, Power: 710, Threshold: 834, Total: 1000, UpgradeHeight: 1000}, true, signalUpgradeMessage(10, 1000)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := auditConfig(t)
			cc := &ChainConfig{name: "Celestia", ChainId: "celestia", signalLoaded: true, lastBlockNum: 100,
				signal: signalState{Version: 10, Power: 710, Threshold: 834, Total: 1000}}
			cc.Alerts.SignalPercent, cc.Alerts.SignalRemindPercent = 5, 66
			cc.Alerts.UpgradeAlerts = true
			c.Chains[cc.name] = cc
			val := &ValInfo{Valcons: "cons", Moniker: "Kynraze"}
			cc.signalAlarms(val, false)
			for len(c.alertChan) > 0 {
				<-c.alertChan
			}
			cc.signal = tc.state
			cc.signalAlarms(val, false)
			reminder, missing := signalRemindMessage(10), signalMissingMessage(10)
			if alarmIsActive(cc.name, reminder) {
				t.Fatal("reminder did not clear")
			}
			if tc.active != "" && !alarmIsActive(cc.name, tc.active) {
				t.Fatalf("takeover alarm %q is not active", tc.active)
			}
			announced, quiet := recoveries(t, c)
			if tc.quiet {
				if len(announced) != 0 || len(quiet) != map[bool]int{true: 2, false: 1}[tc.state.UpgradeHeight > 0] || !containsMessage(quiet, reminder) {
					t.Fatalf("announced %q, quiet %q", announced, quiet)
				}
			} else {
				if alarmIsActive(cc.name, missing) || len(announced) != 1 || announced[0] != reminder || len(quiet) != 1 || quiet[0] != missing {
					t.Fatalf("announced %q, quiet %q, missing active %v", announced, quiet, alarmIsActive(cc.name, missing))
				}
				r, ok := storedResolution(cc.name, reminder)
				if !ok || r.label != "Signalled" || r.text != "Kynraze signalled version 10 on celestia" {
					t.Fatalf("resolution %+v, found %v", r, ok)
				}
			}
		})
	}
}

func TestSignalReminderDetailsAndId(t *testing.T) {
	c := auditConfig(t)
	c.Chains["Celestia"] = &ChainConfig{signal: signalState{Version: 10, Power: 7100, Threshold: 8333, Total: 10000}}
	message := signalRemindMessage(10)
	if message != "Validator has not signalled version 10, upgrade soon" {
		t.Fatalf("message %q", message)
	}
	if got := signalAlarmId(&ValInfo{Valcons: "cons"}, message); got != "conssignalremind10" {
		t.Fatalf("id %q", got)
	}
	if got := signalDetail("Celestia", message); got != "71% signalled, the upgrade unlocks at 83.33%: upgrade celestia-appd, then send the signal tx" {
		t.Fatalf("detail %q", got)
	}
	if got := signalDetail("Celestia", signalMissingMessage(10)); got != "71% signalled, the upgrade unlocks at 83.33%: upgrade celestia-appd, then send the signal tx" {
		t.Fatalf("missing detail %q", got)
	}
	icon, label := signalLabel(&alertMsg{message: message})
	if icon != "🔔" || label != "Signal reminder" {
		t.Fatalf("label %q %q", icon, label)
	}
}

func TestSignalRemindPercentValidation(t *testing.T) {
	for _, tc := range []struct {
		percent, want int
		warning       bool
	}{{0, 66, false}, {-1, 66, true}, {101, 66, true}, {100, 100, false}, {5, 5, false}, {71, 71, false}} {
		c := auditConfig(t)
		cc := &ChainConfig{ChainId: "celestia", ValAddress: "celestiavaloper1example", PublicFallback: true}
		cc.Alerts.SignalPercent, cc.Alerts.SignalRemindPercent = 5, tc.percent
		cc.Alerts.UpgradeAlerts = true
		c.Chains["Celestia"] = cc
		_, problems := validateConfig(c)
		warned := false
		for _, problem := range problems {
			warned = warned || strings.Contains(problem, "signal_remind_percent")
		}
		if cc.Alerts.SignalRemindPercent != tc.want || warned != tc.warning {
			t.Fatalf("input %d: percent %d, warned %v, problems %q", tc.percent, cc.Alerts.SignalRemindPercent, warned, problems)
		}
	}
}

func TestSignalReminderPassesQuietHours(t *testing.T) {
	c := auditConfig(t)
	heldMux.Lock()
	previous := held
	held = make(map[string]heldAlert)
	heldMux.Unlock()
	t.Cleanup(func() {
		heldMux.Lock()
		held = previous
		heldMux.Unlock()
	})
	now := time.Now().UTC()
	c.QuietHours = QuietHoursConfig{Enabled: true, Start: now.Add(-time.Hour).Format("15:04"), End: now.Add(time.Hour).Format("15:04"), Timezone: "UTC"}
	if err := c.QuietHours.parse(); err != nil {
		t.Fatal(err)
	}
	c.Telegram.Enabled = yes(true)
	cc := &ChainConfig{name: "Celestia", ChainId: "celestia", signalLoaded: true,
		signal: signalState{Version: 10, Power: 71, Threshold: 84, Total: 100}}
	cc.Alerts.SignalPercent, cc.Alerts.SignalRemindPercent = 5, 66
	cc.Alerts.UpgradeAlerts = true
	c.Chains[cc.name] = cc
	cc.signalAlarms(&ValInfo{Valcons: "cons"}, false)
	heldMux.Lock()
	_, reminderHeld := held[cc.name+"\x00"+signalRemindMessage(10)]
	_, missingHeld := held[cc.name+"\x00"+signalMissingMessage(10)]
	heldMux.Unlock()
	alarms.notifyMux.RLock()
	queued := len(alarms.Outbox)
	alarms.notifyMux.RUnlock()
	if reminderHeld || !missingHeld || queued != 1 {
		t.Fatalf("reminder held %v, warning held %v, queued %d", reminderHeld, missingHeld, queued)
	}
}

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
