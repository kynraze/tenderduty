package tenderduty

import (
	"context"
	"encoding/hex"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
	"time"
)

func TestGetAppliedPlanHeight(t *testing.T) {
	const path = "/cosmos.upgrade.v1beta1.Query/AppliedPlan"
	// QueryAppliedPlanRequest{name: 1}.
	request := field(nil, 1, []byte("v2.20.0"))
	for _, tc := range []struct {
		name    string
		value   []byte
		want    int64
		wantErr bool
	}{
		{"applied", varintField(nil, 1, 31_812_210), 31_812_210, false},
		{"absent", nil, 0, false},
		{"unknown field", varintField(nil, 2, 123), 0, false},
		{"malformed", []byte{0x08}, 0, true},
		{"out of range", varintField(nil, 1, 1<<63), 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := govStoreNode(t, map[string][]byte{path + " " + hex.EncodeToString(request): tc.value}, "")
			got, err := getAppliedPlanHeight(context.Background(), client, "v2.20.0")
			if got != tc.want || (err != nil) != tc.wantErr {
				t.Fatalf("height %d, %v; want %d, error %v", got, err, tc.want, tc.wantErr)
			}
		})
	}
	client := govStoreNode(t, nil, "")
	if _, err := getAppliedPlanHeight(context.Background(), client, "v2.20.0"); err == nil {
		t.Fatal("expected an error for a failed ABCI query")
	}
}

func TestUpgradeOutcome(t *testing.T) {
	previous := &upgradePlan{Name: "v2.20.0", Height: 31_812_210}
	for _, tc := range []struct {
		name    string
		height  int64
		applied int64
		want    string
	}{
		{"replayed block", previous.Height - 1, previous.Height, "Upgraded"},
		{"cancelled or rescheduled", previous.Height - 1, 0, "Cancelled"},
		{"reached height", previous.Height, 0, "Upgraded"},
		{"past height, next plan already set", previous.Height + 1, 0, "Upgraded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := upgradeOutcome(previous, tc.height, tc.applied); got != tc.want {
				t.Fatalf("outcome %q, want %q", got, tc.want)
			}
		})
	}
}

func TestParsePlan(t *testing.T) {
	plan := field(nil, 1, []byte("v4.0.0"))
	plan = varintField(plan, 3, 12_345_678)
	got, err := parsePlan(field(nil, 1, plan))
	if err != nil || got == nil || got.Name != "v4.0.0" || got.Height != 12_345_678 {
		t.Fatalf("plan %+v, %v", got, err)
	}
	if got, err := parsePlan(nil); got != nil || err != nil {
		t.Fatalf("no plan gave %+v, %v", got, err)
	}
}

func TestUpgradeMessages(t *testing.T) {
	plan := &upgradePlan{Name: "v4.0.0", Height: 12_345_678}
	upgradeETAs.Store("AtomOne@12345678", time.Now().Add(2*24*time.Hour+4*time.Hour+30*time.Second).UTC())
	defer upgradeETAs.Delete("AtomOne@12345678")

	scheduled := &alertMsg{chain: "AtomOne", severity: "warning", message: upgradeMessage(plan)}
	if icon, label := scheduled.header(); icon != "⬆️" || label != "Upgrade scheduled" {
		t.Fatalf("scheduled header %s %s", icon, label)
	}
	got := buildTelegramMessage(scheduled)
	if !strings.Contains(got, "<b>v4.0.0</b>\n📏 <b>Height:</b> 12,345,678\n🕒 <b>Expected:</b> ") || !strings.Contains(got, " · <b>2d 4h left</b>") {
		t.Fatalf("scheduled message %q", got)
	}
	soon := &alertMsg{chain: "AtomOne", severity: "critical", message: upgradeSoonMessage(plan)}
	if icon, label := soon.header(); icon != "⏰" || label != "Upgrade soon" || strings.Contains(buildTelegramMessage(soon), "is close") {
		t.Fatalf("soon %s %s %q", icon, label, buildTelegramMessage(soon))
	}

	scheduled.resolved = true
	if _, label := scheduled.header(); label != "Upgraded" {
		t.Fatalf("done is %q", label)
	}
	upgradeOutcomes.Store("AtomOne@12345678", "Cancelled")
	defer upgradeOutcomes.Delete("AtomOne@12345678")
	if icon, label := scheduled.header(); icon != "🚫" || label != "Cancelled" {
		t.Fatalf("cancelled is %s %s", icon, label)
	}
}

func TestParsePlanRejectsABogusHeight(t *testing.T) {
	plan := protowire.AppendVarint(protowire.AppendTag(protowire.AppendBytes(protowire.AppendTag(nil, 1, protowire.BytesType), []byte("v4")), 3, protowire.VarintType), 1<<63)
	got, err := parsePlan(protowire.AppendBytes(protowire.AppendTag(nil, 1, protowire.BytesType), plan))
	if err == nil || got != nil {
		t.Fatalf("a height past int64 gave %+v, %v", got, err)
	}
}

func TestSignalUpgradeHidesItsOwnPlan(t *testing.T) {
	s := signalState{Version: 11, UpgradeVersion: 11, UpgradeHeight: 394}
	if !s.coversPlan("v11") || s.coversPlan("v12") || s.coversPlan("v4.0.0") {
		t.Fatal("only the signalled version's plan is covered")
	}
	if (signalState{}).coversPlan("v0") {
		t.Fatal("chains without x/signal keep their plans")
	}
}

func TestStalledAtUpgradeHeightSaysSo(t *testing.T) {
	old := td
	t.Cleanup(func() { td = old })
	cc := &ChainConfig{name: "Chain", ChainId: "chain-1"}
	td = &Config{Chains: map[string]*ChainConfig{"Chain": cc}}
	stalled := &alertMsg{chain: "Chain", message: "stalled: have not seen a new block on chain-1 in 2 minutes"}
	for _, tc := range []struct {
		name   string
		plan   *upgradePlan
		signal signalState
		last   signalUpgrade
		height int64
		want   string
	}{
		{"gov", &upgradePlan{Name: "v4.0.0", Height: 1000}, signalState{}, signalUpgrade{}, 999, "halted at the v4.0.0 upgrade height 1,000"},
		{"signal", &upgradePlan{Name: "v11", Height: 395}, signalState{Version: 11, UpgradeVersion: 11, UpgradeHeight: 394}, signalUpgrade{}, 394, "halted at the version 11 upgrade height 394"},
		{"signal without plan", nil, signalState{Version: 11, UpgradeVersion: 11, UpgradeHeight: 394}, signalUpgrade{}, 394, "halted at the version 11 upgrade height 394"},
		// devnet 17 Oct: the signal query no longer had the upgrade, the card said 524
		{"signal gone at height", &upgradePlan{Name: "v11", Height: 524}, signalState{Version: 11}, signalUpgrade{Version: 11, Height: 523}, 523, "halted at the version 11 upgrade height 523"},
		{"plain stall", &upgradePlan{Name: "v4.0.0", Height: 1000}, signalState{}, signalUpgrade{}, 900, ""},
	} {
		cc.upgrade, cc.signal, cc.signalLast, cc.lastBlockNum = tc.plan, tc.signal, tc.last, tc.height
		lines := strings.SplitN(stalled.summary(), "\n", 2)
		got := ""
		if len(lines) == 2 {
			got = lines[1]
		}
		if tc.want == "" && got != "" || tc.want != "" && !strings.HasPrefix(got, tc.want+", resumes once 2/3") {
			t.Fatalf("%s: %q", tc.name, got)
		}
	}
}
