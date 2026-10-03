package tenderduty

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	rpchttp "github.com/tendermint/tendermint/rpc/client/http"
	"google.golang.org/protobuf/encoding/protowire"
)

// captured from Celestia mainnet, 3 Oct 2026: VersionTally for version 10, nobody had signalled yet
func TestParseTallyAbsentVotingPowerIsZero(t *testing.T) {
	b, err := hex.DecodeString("109ced96bc0118d49ce8e101")
	if err != nil {
		t.Fatal(err)
	}
	power, threshold, total, err := parseTally(b)
	if err != nil {
		t.Fatal(err)
	}
	if power != 0 || threshold != 394639004 || total != 473566804 {
		t.Fatalf("power %d threshold %d total %d", power, threshold, total)
	}
}

func TestParseTallyWithVotingPower(t *testing.T) {
	var b []byte
	for num, value := range map[protowire.Number]uint64{1: 52000000, 2: 394639004, 3: 473566804} {
		b = protowire.AppendVarint(protowire.AppendTag(b, num, protowire.VarintType), value)
	}
	power, threshold, total, err := parseTally(b)
	if err != nil {
		t.Fatal(err)
	}
	if power != 52000000 || threshold != 394639004 || total != 473566804 {
		t.Fatalf("power %d threshold %d total %d", power, threshold, total)
	}
}

func TestParseSignalUpgrade(t *testing.T) {
	// the empty value from GetUpgrade is no upgrade, not an error
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
		{"signalling starts", base, signalState{Version: 10, Power: 100, Threshold: 670, Total: 1000}, 0, []string{started, missing}, nil},
		{"below the percentage", base, signalState{Version: 10, Power: 99, Threshold: 670, Total: 1000}, 0, nil, []string{started, missing}},
		{"our validator signals", signalState{Version: 10, Power: 300, Threshold: 670, Total: 1000}, signalState{Version: 10, Power: 300, Threshold: 670, Total: 1000, Signalled: true}, 0, []string{started}, []string{missing}},
		{"threshold reached", signalState{Version: 10, Power: 300, Threshold: 670, Total: 1000, Signalled: true}, signalState{Version: 10, Power: 700, Threshold: 670, Total: 1000, Signalled: true}, 0, []string{started, threshold}, nil},
		{"upgrade scheduled", signalState{Version: 10, Power: 700, Threshold: 670, Total: 1000, Signalled: true}, signalState{Version: 10, Power: 700, Threshold: 670, Total: 1000, Signalled: true, UpgradeVersion: 10, UpgradeHeight: 1000}, 0, []string{scheduled}, []string{threshold}},
		{"upgrade is close", signalState{Version: 10, Power: 700, Threshold: 670, Total: 1000, Signalled: true, UpgradeVersion: 10, UpgradeHeight: 1000}, signalState{Version: 10, Power: 700, Threshold: 670, Total: 1000, Signalled: true, UpgradeVersion: 10, UpgradeHeight: 1000}, 30 * time.Minute, []string{scheduled, soon}, []string{threshold}},
		{"upgrade cancelled", signalState{Version: 10, Power: 700, Threshold: 670, Total: 1000, Signalled: true, UpgradeVersion: 10, UpgradeHeight: 1000}, signalState{Version: 10, Power: 700, Threshold: 670, Total: 1000, Signalled: true}, 0, []string{threshold}, []string{scheduled, soon}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := auditConfig(t)
			chain := &ChainConfig{name: "Chain", ChainId: "celestia", lastBlockNum: 500, signal: tc.before, signalLoaded: true}
			chain.Alerts.SignalAlerts, chain.Alerts.SignalPercent, chain.Alerts.SignalRemindMinutes, chain.Alerts.SignalPriority = true, 10, 60, "warning"
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
			// nothing is left once the condition stops holding
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
		signalStartMessage(10), signalMissingMessage(10), signalThresholdMessage(10),
		signalUpgradeMessage(10, 1000), signalUpgradeSoonMessage(10, 1000), signalStartMessage(11),
		// the same version rescheduled to another height
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

// a fake node answering abci_info with app version 9 and every abci_query with the given code, counting the queries
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

// a chain without x/signal is asked once, so signal_enabled can sit in alert_defaults for every chain
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

// any other failure can pass, the chain is asked again next time
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

// rescheduling raises the new upgrade before resolving the old one, they must not go out under one dedup key
func TestRescheduledSignalUpgradeKeepsItsIncident(t *testing.T) {
	c := auditConfig(t)
	chain := &ChainConfig{name: "Celestia", ChainId: "celestia", lastBlockNum: 100}
	chain.Alerts.SignalAlerts, chain.Alerts.SignalPercent, chain.Alerts.SignalRemindMinutes = true, 10, 60
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
