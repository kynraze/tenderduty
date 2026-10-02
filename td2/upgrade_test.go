package tenderduty

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
	"time"
)

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
	if !strings.Contains(got, "<b>v4.0.0</b> at height 12,345,678\nAround ") || !strings.Contains(got, " · <b>2d 4h left</b>") {
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
