package tenderduty

import (
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
)

func field(b []byte, num protowire.Number, value []byte) []byte {
	return protowire.AppendBytes(protowire.AppendTag(b, num, protowire.BytesType), value)
}

func varintField(b []byte, num protowire.Number, value uint64) []byte {
	return protowire.AppendVarint(protowire.AppendTag(b, num, protowire.VarintType), value)
}

func TestParseProposals(t *testing.T) {
	end := varintField(nil, 1, 1_790_000_000)
	v1 := varintField(nil, 1, 12)
	v1 = varintField(v1, 3, proposalStatusVoting)
	v1 = field(v1, 9, end)
	v1 = field(v1, 11, []byte("Enable IBC transfers"))
	got, err := parseProposals(field(nil, 1, v1), false)
	if err != nil || len(got) != 1 || got[0].ID != 12 || got[0].Title != "Enable IBC transfers" || !got[0].VotingEnd.Equal(time.Unix(1_790_000_000, 0)) {
		t.Fatalf("v1 proposal: %+v, %v", got, err)
	}

	// v1beta1 keeps the title inside the content Any
	content := field(nil, 1, []byte("/cosmos.gov.v1beta1.TextProposal"))
	content = field(content, 2, field(nil, 1, []byte("Old style")))
	legacy := field(varintField(nil, 1, 7), 2, content)
	legacy = field(legacy, 9, end)
	got, err = parseProposals(field(nil, 1, legacy), true)
	if err != nil || len(got) != 1 || got[0].ID != 7 || got[0].Title != "Old style" {
		t.Fatalf("v1beta1 proposal: %+v, %v", got, err)
	}
}

func TestVoterAddress(t *testing.T) {
	if got := voterAddress("atonevaloper1zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3dqqvw0"); got[:6] != "atone1" {
		t.Fatalf("voter is %q", got)
	}
	if got := voterAddress("atonevalcons1zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3xq0ue9"); got != "" {
		t.Fatalf("a consensus address gave a voter %q", got)
	}
}

func TestGovernanceMessages(t *testing.T) {
	cc := &ChainConfig{name: "MANTRA", ChainId: "mantra-1"}
	p := govProposal{ID: 39, Title: "Community Pool", VotingEnd: time.Now().Add(7*24*time.Hour - 30*time.Minute).UTC().Truncate(time.Minute)}
	open := &alertMsg{chain: "MANTRA", severity: "info", message: cc.govOpenMessage(p)}
	if icon, label := open.header(); icon != "🗳" || label != "New proposal" {
		t.Fatalf("open header %s %s", icon, label)
	}
	if got := buildTelegramMessage(open); !strings.Contains(got, "<b>#39 Community Pool</b>\nEnds ") || !strings.Contains(got, " · <b>6d 23h left</b>") {
		t.Fatalf("open message %q", got)
	}
	remind := &alertMsg{chain: "MANTRA", severity: "critical", message: cc.govRemindMessage(p, "Validator")}
	if icon, label := remind.header(); icon != "⏰" || label != "Vote reminder" || !strings.HasSuffix(remind.summary(), "\nValidator has not voted") {
		t.Fatalf("reminder %s %s %q", icon, label, remind.summary())
	}

	// cleared while voting is open is a vote, after that the result
	open.resolved = true
	if _, label := open.header(); label != "Voted" {
		t.Fatalf("cleared while open is %q", label)
	}
	govOutcomes.Store("MANTRA#39", "Rejected")
	defer govOutcomes.Delete("MANTRA#39")
	if icon, label := open.header(); icon != "❌" || label != "Rejected" {
		t.Fatalf("result is %s %s", icon, label)
	}
	ended := &alertMsg{chain: "Other", resolved: true, message: cc.govOpenMessage(govProposal{ID: 2, VotingEnd: time.Now().Add(-time.Hour).UTC()})}
	if _, label := ended.header(); label != "Voting ended" {
		t.Fatalf("ended without a result is %q", label)
	}
}

func TestMinSeverityRouting(t *testing.T) {
	c := auditConfig(t)
	c.Telegram.Enabled, c.Pagerduty.Enabled = true, true
	chain := &ChainConfig{}
	chain.Alerts.Telegram.Enabled, chain.Alerts.Pagerduty.Enabled = true, true
	chain.Alerts.Pagerduty.MinSeverity = "warning"
	c.Chains["Chain"] = chain
	if a := c.makeAlert("Chain", "new proposal", "info", false, nil); !a.tg || a.pd {
		t.Fatalf("info went to telegram %v, pagerduty %v", a.tg, a.pd)
	}
	if a := c.makeAlert("Chain", "missed blocks", "critical", false, nil); !a.tg || !a.pd {
		t.Fatalf("critical went to telegram %v, pagerduty %v", a.tg, a.pd)
	}
	if a := c.makeAlert("Chain", "missed blocks", "info", true, nil); !a.pd {
		t.Fatal("a recovery was kept from pagerduty")
	}
}
