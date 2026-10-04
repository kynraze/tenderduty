package tenderduty

import (
	"strings"
	"testing"
	"time"
)

func TestCommandAllowed(t *testing.T) {
	c := &Config{}
	c.Telegram.Channel, c.Telegram.Admins = "-1001234", []int64{42}
	for _, tc := range []struct {
		chat     int64
		chatType string
		from     int64
		want     bool
	}{
		{-1001234, "supergroup", 42, true},
		{-1001234, "supergroup", 7, false},
		{-1001234, "channel", 0, false},
		{-999, "supergroup", 42, false},
	} {
		if got := c.commandAllowed(tc.chat, tc.chatType, tc.from); got != tc.want {
			t.Fatalf("%+v allowed %v", tc, got)
		}
	}
	c.Telegram.Channel = "555"
	if !c.commandAllowed(555, "private", 555) || c.commandAllowed(556, "private", 556) {
		t.Fatal("a private chat is allowed only when it is the configured one")
	}
}

func TestStatusCommand(t *testing.T) {
	c := auditConfig(t)
	healthy := &ChainConfig{ChainId: "atomone-1", lastBlockNum: 10_622_456, lastBlockTime: time.Now().Add(-4 * time.Second), monitoring: true,
		valInfo: &ValInfo{Moniker: "Validator", Bonded: true, Missed: 12, Window: 10_000, MaxMissed: 5_000, VotingPower: 169_960, TotalPower: 61_441_660, Rank: 72, ActiveSet: 100}}
	healthy.Nodes = []*NodeConfig{{Url: "http://a"}}
	jailed := &ChainConfig{ChainId: "realionetwork_3301-1", monitoring: true, valInfo: &ValInfo{Moniker: "Validator", Jailed: true}}
	c.Chains["AtomOne"], c.Chains["Realio"] = healthy, jailed
	alarms.AllAlarms["AtomOne"] = map[string]time.Time{"Severity: critical\nRPC node http://b has been down for > 3 minutes on atomone-1": time.Now()}

	summary := c.telegramCommand("status", "", time.Now(), tgMention{})
	if !strings.HasPrefix(summary, "📊 <b>Status</b> · 0 of 2 chains healthy") || strings.Index(summary, "Realio") > strings.Index(summary, "AtomOne") && !strings.Contains(summary, "🔴 <b>Realio</b> · jailed") {
		t.Fatalf("summary %q", summary)
	}
	detail := c.telegramCommand("status", "atomone-1", time.Now(), tgMention{})
	for _, want := range []string{"🔴 <b>AtomOne</b> · 1 active alert", "⛓ <b>Validator</b>", "📦 <b>Height:</b> 10,622,456 · last block 4s ago",
		"🎯 <b>Missed:</b> 12 / 10,000 (4,988 to jail)", "#72/100", "🔌 <b>RPC:</b> 🟢 1/1 healthy", "🚨 <b>Active alerts</b>",
		"• RPC node http://b has been down"} {
		if !strings.Contains(detail, want) {
			t.Fatalf("detail is missing %q:\n%s", want, detail)
		}
	}
	telegramHTML(t, detail)
	if reply := c.telegramCommand("status", "nope", time.Now(), tgMention{}); !strings.Contains(reply, "No chain named nope") {
		t.Fatalf("unknown chain %q", reply)
	}
	if reply := c.telegramCommand("mute", "AtomOne 2h", time.Now(), tgMention{}); strings.Contains(reply, "muted") {
		t.Fatal("mute is still answered")
	}
}

func TestSplitMessage(t *testing.T) {
	line := strings.Repeat("x", 1000)
	parts := splitMessage(strings.Join([]string{line, line, line, line, line}, "\n"))
	if len(parts) != 2 || len(parts[0]) > telegramMessageLimit {
		t.Fatalf("%d parts, first %d long", len(parts), len(parts[0]))
	}
}

func TestMentionMeRegistersAndPings(t *testing.T) {
	c := auditConfig(t)
	restoreMentions(nil)
	t.Cleanup(func() { restoreMentions(nil) })
	c.Telegram.Enabled = yes(true)
	chain := &ChainConfig{}
	chain.Alerts.Telegram.Enabled = yes(true)
	chain.Alerts.Telegram.Mentions = []string{"@ops <team>"}
	c.Chains["Chain"] = chain
	someone := tgMention{ID: 123456789, Name: "Ops"}

	if reply := c.telegramCommand("mentionme", "", time.Now(), someone); !strings.Contains(reply, `<a href="tg://user?id=123456789">Ops</a>`) {
		t.Fatalf("mentionme reply %q", reply)
	}
	// the config name is escaped, the registered one stays a link
	msg := c.makeAlert("Chain", "something happened", "critical", false, nil)
	got := buildTelegramMessage(msg)
	if !strings.HasSuffix(got, "\n@ops &lt;team&gt; <a href=\"tg://user?id=123456789\">Ops</a>") {
		t.Fatalf("alert mentions %q", got)
	}
	// asking twice doesn't duplicate, and the name is kept up to date
	c.telegramCommand("mentionme", "", time.Now(), tgMention{ID: someone.ID, Name: "Ops Team"})
	if list := mentionSnapshot(); len(list) != 1 || list[0].Name != "Ops Team" {
		t.Fatalf("ping list is %+v", list)
	}
	restored := mentionSnapshot()
	restoreMentions(nil)
	restoreMentions(restored)
	if len(mentionSnapshot()) != 1 {
		t.Fatal("the ping list was lost")
	}
	if reply := c.telegramCommand("mentionme", "off", time.Now(), someone); !strings.Contains(reply, "won't be pinged") || len(mentionSnapshot()) != 0 {
		t.Fatalf("off reply %q, list %+v", reply, mentionSnapshot())
	}
	if got := buildTelegramMessage(c.makeAlert("Chain", "something happened", "critical", false, nil)); strings.Contains(got, "tg://user") {
		t.Fatalf("still pinging after off: %q", got)
	}
}
