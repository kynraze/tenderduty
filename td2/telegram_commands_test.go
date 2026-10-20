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
		{-1001234, "channel", 0, true},
		{-999, "channel", 0, false},
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
	healthy := &ChainConfig{name: "AtomOne", ChainId: "atomone-1", lastBlockNum: 10_622_456, lastBlockTime: time.Now().Add(-4 * time.Second), monitoring: true,
		valInfo: &ValInfo{Moniker: "Validator", Bonded: true, Missed: 12, Window: 10_000, MaxMissed: 5_000, VotingPower: 169_960, TotalPower: 61_441_660, Rank: 72, ActiveSet: 100}}
	healthy.Nodes = []*NodeConfig{{Url: "http://a"}}
	jailed := &ChainConfig{name: "Realio", ChainId: "realionetwork_3301-1", monitoring: true, valInfo: &ValInfo{Moniker: "Validator", Jailed: true}}
	c.Chains["AtomOne"], c.Chains["Realio"] = healthy, jailed
	alarms.AllAlarms["AtomOne"] = map[string]time.Time{"Severity: critical\nRPC node http://b has been down for > 3 minutes on atomone-1": time.Now()}

	summary := c.telegramCommand("status", "", time.Now(), tgMention{})
	if !strings.HasPrefix(summary, "📊 <b>Status</b> · 0 of 2 chains healthy") || strings.Index(summary, "Realio") > strings.Index(summary, "AtomOne") && !strings.Contains(summary, "🔴 <b>Realio</b> · jailed") {
		t.Fatalf("summary %q", summary)
	}
	detail := c.telegramCommand("status", "atomone-1", time.Now(), tgMention{})
	for _, want := range []string{"🔴 <b>AtomOne</b> · 1 active alert", "⛓ <b>Validator</b>", "📦 <b>Height:</b> 10,622,456 · last block 4s ago",
		"🎯 <b>Missed:</b> 12 / 10,000 (4,988 to jail)", "#72/100", "🔌 <b>RPC:</b> 🟢 1/1 healthy", "🚨 <b>Active alerts</b>",
		"• RPC node b has been down"} {
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

func TestFallbackStatus(t *testing.T) {
	c := auditConfig(t)
	cc := &ChainConfig{name: "Chain", Nodes: []*NodeConfig{{Url: "http://own:26657"}}}
	if got := cc.fallbackStatus(1); got != "⚪ off" {
		t.Fatalf("off: %q", got)
	}
	cc.PublicFallback = true
	if got := cc.fallbackStatus(1); got != "🟢 on, standby" {
		t.Fatalf("standby: %q", got)
	}
	cc.publicNode = "https://rpc.public.example:443"
	if got := cc.fallbackStatus(0); !strings.HasPrefix(got, "🟡 in use") || !strings.Contains(got, "rpc.public.example") {
		t.Fatalf("in use: %q", got)
	}
	c.HideLogs = true
	if got := cc.fallbackStatus(0); got != "🟡 in use" {
		t.Fatalf("hidden host: %q", got)
	}
	cc.noNodes = true
	if got := cc.fallbackStatus(0); got != "🔴 on, no public node working" {
		t.Fatalf("none working: %q", got)
	}
}

// Operator sends /Status on a phone whose keyboard capitalised the first letter.
func TestOpCommandCapitalsGetHelpNotStatus(t *testing.T) {
	c := auditConfig(t)
	c.Chains["Chain"] = &ChainConfig{name: "Chain", monitoring: true, valInfo: &ValInfo{Moniker: "Kyn", Bonded: true}}
	if reply := c.telegramCommand("Status", "", time.Now(), tgMention{}); !strings.HasPrefix(reply, "📊 <b>Status</b>") {
		t.Fatalf("/Status answered %q", reply)
	}
	if reply := c.telegramCommand("REPORT", "weekly", time.Now(), tgMention{}); !strings.Contains(reply, "Weekly report") {
		t.Fatalf("/REPORT answered %q", reply)
	}
}

// Operator with no chains yet runs /status.
func TestOpStatusWithZeroChains(t *testing.T) {
	c := auditConfig(t)
	if got := strings.TrimSpace(c.telegramCommand("status", "", time.Now(), tgMention{})); got != "📊 <b>Status</b> · 0 of 0 chains healthy" {
		t.Fatalf("empty status %q", got)
	}
	if got := c.telegramCommand("status", "anything", time.Now(), tgMention{}); got != "No chain named anything" {
		t.Fatalf("empty lookup %q", got)
	}
}

// A validator that is out of the active set with stake shows "#0/100", which is not a rank.
func TestOpStatusInactiveValidatorHasNoRank(t *testing.T) {
	c := auditConfig(t)
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	cc := &ChainConfig{name: "Chain", ChainId: "chain-1", monitoring: true, lastBlockNum: 100, lastBlockTime: now,
		valInfo: &ValInfo{Moniker: "Kyn", Tokens: "5000000000", Token: bondToken{DisplayKnown: true, Symbol: "ATOM", Exponent: 6}, Window: 10000, MaxMissed: 500, ActiveSet: 100}}
	c.Chains["Chain"] = cc
	text := cc.statusDetail("Chain", now)
	if strings.Contains(text, "#0/") {
		t.Fatalf("inactive validator shows a rank:\n%s", text)
	}
}

// A node not read yet (RPC unreachable) must not show as "not in the active set".
func TestOpStatusUnreachableIsNotInactive(t *testing.T) {
	c := auditConfig(t)
	cc := &ChainConfig{name: "Chain", ChainId: "chain-1", monitoring: true, monitoringSince: time.Now(), Nodes: []*NodeConfig{{Url: "http://a"}}}
	c.Chains["Chain"] = cc
	dot, label := cc.chainState("Chain", &ValInfo{Moniker: "not connected"})
	if label == "not in the active set" {
		t.Fatalf("unreachable validator reads %s %q", dot, label)
	}
}

// hide_logs: a public channel must not see node hosts in the active alerts list.
func TestOpHideLogsKeepsHostOutOfActiveAlerts(t *testing.T) {
	c := auditConfig(t)
	c.HideLogs = true
	now := time.Now()
	cc := &ChainConfig{name: "Chain", ChainId: "chain-1", monitoring: true, lastBlockNum: 5, lastBlockTime: now, valInfo: &ValInfo{Moniker: "Kyn", Bonded: true}, Nodes: []*NodeConfig{{Url: "http://secret-b"}}}
	c.Chains["Chain"] = cc
	alarms.AllAlarms["Chain"] = map[string]time.Time{"Severity: critical\nRPC node http://secret-b has been down for > 3 minutes on chain-1": now}
	if text := cc.statusDetail("Chain", now); strings.Contains(text, "secret-b") {
		t.Fatalf("host leaked with hide_logs:\n%s", text)
	}
}

// hide_logs: a daily report names the incident by kind only, never by host.
func TestOpHideLogsReportNoHost(t *testing.T) {
	c := auditConfig(t)
	c.HideLogs = true
	now := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	c.Chains["Chain"] = &ChainConfig{name: "Chain", ChainId: "chain-1", valInfo: &ValInfo{Moniker: "Kyn", Bonded: true}}
	c.recordReportAlarm("Chain", "RPC node http://secret-a has been down for > 3 minutes on chain-1", now.Add(-2*time.Hour), now.Add(-time.Hour))
	if text := c.buildReport(now, false, false, nil).telegram(); strings.Contains(text, "secret-a") {
		t.Fatalf("report leaked host:\n%s", text)
	}
}

// A report where every read failed all day renders without panic or fake numbers.
func TestOpReportAllReadsFailed(t *testing.T) {
	c := auditConfig(t)
	now := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	c.Chains["Chain"] = &ChainConfig{name: "Chain", valInfo: &ValInfo{Moniker: "not connected"}}
	c.reportData.Snapshots = map[string][]reportSnapshot{"Chain": {{At: now.Add(-2 * time.Hour)}, {At: now.Add(-time.Hour)}}}
	for _, weekly := range []bool{false, true} {
		r := c.buildReport(now, weekly, false, nil)
		text := r.telegram()
		if !strings.Contains(text, "RPC unreachable") || strings.Contains(text, "+0") {
			t.Fatalf("weekly=%v:\n%s", weekly, text)
		}
	}
}

// Weekly report keeps signing across a restart: saved history is reloaded and summed.
func TestOpWeeklyAcrossRestart(t *testing.T) {
	c := auditConfig(t)
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	c.reportData.Hours = map[string][]reportHour{"Chain": {
		{At: now.Add(-3 * 24 * time.Hour), Signed: 990, Missed: 10, Watched: 3600},
		{At: now.Add(-2 * time.Hour), Signed: 990, Missed: 10, Watched: 3600},
	}}
	saved := c.savedReports(now)

	c2 := auditConfig(t)
	c2.restoreReports(saved, now)
	c2.Chains["Chain"] = &ChainConfig{name: "Chain", valInfo: &ValInfo{Moniker: "Kyn", Bonded: true}}
	r := c2.buildReport(now, true, false, nil)
	if len(r.Rows) != 1 || !strings.HasPrefix(r.Rows[0].Summary, "99.00%") {
		t.Fatalf("restored signing: %+v", r.Rows)
	}
}

// compactReportNumber: 999,999 should read as 1.00M, not "1000.00K".
func TestOpCompactNumberRoundsUpUnit(t *testing.T) {
	if got := compactReportNumber(999_999); got != "1.00M" {
		t.Fatalf("999,999 shows %q", got)
	}
}

// Stake 0 and a huge stake render without panic or broken units.
func TestOpStakeZeroAndHuge(t *testing.T) {
	zero := &ValInfo{Tokens: "0", Token: bondToken{DisplayKnown: true, Symbol: "CHEQ", Exponent: 9}}
	if got := zero.stakeDisplay(); got != "0 CHEQ" {
		t.Fatalf("zero stake %q", got)
	}
	huge := &ValInfo{Tokens: "1000000000000000000000000", Token: bondToken{DisplayKnown: true, Symbol: "ATOM", Exponent: 6}}
	if got := huge.stakeDisplay(); got != "1000000T ATOM" {
		t.Fatalf("huge stake %q", got)
	}
}

// HTML and emoji in a moniker: escaped in /status and the report, still valid Telegram HTML.
func TestOpMonikerHTMLEscaped(t *testing.T) {
	c := auditConfig(t)
	now := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	name := `<b>Evil</b> & "co" 🚀`
	cc := &ChainConfig{name: "Chain", ChainId: "chain-1", monitoring: true, lastBlockNum: 5, lastBlockTime: now, valInfo: &ValInfo{Moniker: name, Jailed: true}}
	c.Chains["Chain"] = cc
	text := cc.statusDetail("Chain", now)
	if strings.Contains(text, "<b>Evil") || !strings.Contains(text, "&lt;b&gt;Evil&lt;/b&gt; &amp;") {
		t.Fatalf("status moniker not escaped:\n%s", text)
	}
	telegramHTML(t, text)
	report := c.buildReport(now, false, false, nil).telegram()
	if strings.Contains(report, "<b>Evil") {
		t.Fatalf("report moniker not escaped:\n%s", report)
	}
	telegramHTML(t, report)
}

// /mentionme from a user with emoji and HTML in the display name.
func TestOpMentionNameEscaped(t *testing.T) {
	c := auditConfig(t)
	restoreMentions(nil)
	t.Cleanup(func() { restoreMentions(nil) })
	reply := c.telegramCommand("mentionme", "", time.Now(), tgMention{ID: 77, Name: "Ana 🚀 <i>x</i>"})
	if !strings.Contains(reply, `<a href="tg://user?id=77">Ana 🚀 &lt;i&gt;x&lt;/i&gt;</a>`) {
		t.Fatalf("mention reply %q", reply)
	}
}

// Two chains share one chain ID: the report keeps both rows, and a lookup by ID is ambiguous.
func TestOpSharedChainIDTwoRows(t *testing.T) {
	c := auditConfig(t)
	now := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	for _, name := range []string{"Realio-Network / RIO", "Realio-Network / RST"} {
		c.Chains[name] = &ChainConfig{name: name, ChainId: "realionetwork_3301-1", monitoring: true, valInfo: &ValInfo{Moniker: "Kyn", Bonded: true}}
	}
	text := c.buildReport(now, false, false, nil).telegram()
	for _, want := range []string{"<b>Realio-Network / RIO</b>", "<b>Realio-Network / RST</b>"} {
		if !strings.Contains(text, want) {
			t.Fatalf("report missing %q:\n%s", want, text)
		}
	}
	if reply := c.telegramCommand("status", "realionetwork_3301-1", now, tgMention{}); !strings.Contains(reply, "be more specific") {
		t.Fatalf("chain id lookup %q", reply)
	}
	if reply := c.telegramCommand("status", "realio-network / rst", now, tgMention{}); !strings.Contains(reply, "<b>Realio-Network / RST</b>") {
		t.Fatalf("name lookup %q", reply)
	}
}

// A renamed chain keeps its signing history under the new name, and the old name is gone.
func TestOpRenamedChainKeepsHistory(t *testing.T) {
	c := auditConfig(t)
	now := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	c.reportData.Hours = map[string][]reportHour{"Old": {{At: now.Add(-2 * time.Hour), Signed: 99, Missed: 1}}}
	c.Chains["New"] = &ChainConfig{name: "New", ChainId: "chain-1", valInfo: &ValInfo{Moniker: "Kyn", Bonded: true}}
	c.renameChainState("Old", "New")
	if _, stale := c.reportData.Hours["Old"]; stale {
		t.Fatal("old name still holds history")
	}
	r := c.buildReport(now, false, false, nil)
	if len(r.Rows) != 1 || len(r.Rows[0].Lines) == 0 || !strings.Contains(r.Rows[0].Lines[0], "99.00%") {
		t.Fatalf("renamed chain lost signing: %+v", r.Rows)
	}
}

// /report takes only "weekly"; anything else gets the usage line, and it is not escaped twice.
func TestOpReportArgs(t *testing.T) {
	c := auditConfig(t)
	if reply := c.telegramCommand("report", "weekly extra", time.Now(), tgMention{}); reply != "Use /report or /report weekly" {
		t.Fatalf("extra arg %q", reply)
	}
	if reply := c.telegramCommand("report", "  Weekly  ", time.Now(), tgMention{}); !strings.Contains(reply, "Weekly report") {
		t.Fatalf("weekly with spaces %q", reply)
	}
}

// A stake that didn't move is left out of the daily report, not shown as "(= AKT)".
func TestOpUnchangedStakeIsQuiet(t *testing.T) {
	c := auditConfig(t)
	now := time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)
	stake := 24300.0
	c.Chains["Akash"] = &ChainConfig{name: "Akash", valInfo: &ValInfo{Moniker: "Kyn", Bonded: true}}
	c.reportData.Snapshots = map[string][]reportSnapshot{"Akash": {{At: now.Add(-24 * time.Hour), Stake: &stake, Symbol: "AKT"}, {At: now, Stake: &stake, Symbol: "AKT"}}}
	if text := c.buildReport(now, false, false, nil).telegram(); strings.Contains(text, "= AKT") || !strings.Contains(text, "24.3K AKT") {
		t.Fatalf("unchanged stake:\n%s", text)
	}
}

// /status counts like the daily report: a proposal open is healthy, inactive is apart.
func TestOpStatusHeaderMatchesReport(t *testing.T) {
	c := auditConfig(t)
	chain := func(name string, info ValInfo) {
		c.Chains[name] = &ChainConfig{name: name, monitoring: true, monitoringSince: time.Now(), Nodes: []*NodeConfig{{Url: "http://a"}}, valInfo: &info}
	}
	chain("Green", ValInfo{Moniker: "Kyn", Bonded: true})
	chain("Blue", ValInfo{Moniker: "Kyn", Bonded: true})
	chain("White", ValInfo{Moniker: "Kyn"})
	chain("Red", ValInfo{Moniker: "Kyn", Jailed: true})
	alarms.AllAlarms["Blue"] = map[string]time.Time{"Proposal #1 Raise the limit": time.Now()}
	header, _, _ := strings.Cut(c.telegramCommand("status", "", time.Now(), tgMention{}), "\n")
	if want := "📊 <b>Status</b> · 2 of 4 chains healthy · ⚪ 1 inactive · 🔴 1 needs attention"; header != want {
		t.Fatalf("header %q, want %q", header, want)
	}
}
