package tenderduty

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// cardCase shares its message; use resets the stored resolution.
type cardCase struct {
	name  string
	msg   *alertMsg
	setup func()
}

func (c cardCase) use() *alertMsg {
	resolutions.Delete(c.msg.chain + "\x00" + c.msg.message)
	if c.setup != nil {
		c.setup()
	}
	return c.msg
}

const cardHost = "localhost:7980"

func cardChain(t *testing.T) *ChainConfig {
	t.Helper()
	c := auditConfig(t)
	c.NodeDownMin, c.NodeDownSeverity = 3, "critical"
	cc := &ChainConfig{name: "Devnet1", ChainId: "devnet-1", valInfo: &ValInfo{Moniker: "Kynraze", Valcons: "valcons1"}}
	cc.Alerts.ConsecutiveMissed, cc.Alerts.Window = 10, 5
	cc.Alerts.ProposalMissed, cc.Alerts.ProposalMinutes = 2, 60
	cc.Alerts.JailRiskPercent, cc.Alerts.JailRiskMinutes = 80, 10
	cc.Alerts.FibreStoragePercent, cc.Alerts.FibreIdleMinutes, cc.Alerts.SignalPercent = 90, 15, 33
	cc.signal = signalState{Version: 10, Power: 7100, Threshold: 8333, Total: 10000}
	cc.fibre = fibreState{Registered: true, Host: cardHost, DownSince: time.Now().Add(-2*time.Minute - 5*time.Second), DownReason: "connection refused: Fibre is not listening on that port"}
	c.Chains["Devnet1"] = cc
	upgradeETAs.Store("Devnet1@12345678", time.Now().Add(52*time.Hour).UTC())
	t.Cleanup(func() {
		govOutcomes.Delete("Devnet1#39")
		upgradeOutcomes.Delete("Devnet1@12345678")
		upgradeETAs.Delete("Devnet1@12345678")
	})
	return cc
}

func cardCases(t *testing.T) []cardCase {
	t.Helper()
	cc := cardChain(t)
	const chain = "Devnet1"
	var cases []cardCase
	raised := func(name, message, severity string) {
		cases = append(cases, cardCase{name: "raised " + name, msg: &alertMsg{chain: chain, message: message, severity: severity}})
	}
	cleared := func(name, message string, setup func()) {
		cases = append(cases, cardCase{name: "resolved " + name, msg: &alertMsg{chain: chain, message: message, severity: "info", resolved: true, lasted: 12 * time.Minute}, setup: setup})
	}
	stored := func(name, message string, r resolution) {
		cleared(name, message, func() { resolve(chain, message, r) })
	}

	until := time.Now().UTC().Truncate(time.Minute).Add(10 * time.Minute)
	monitor := &ValInfo{Moniker: "Kynraze"}
	plain := map[string]string{
		"node down":          nodeDownMessage("http://10.0.0.1:26657", cc.ChainId),
		"no RPC":             "no RPC endpoints are working for devnet-1",
		"stalled":            "stalled: have not seen a new block on devnet-1 in 10 minutes",
		"missed blocks":      "Kynraze has missed 10 blocks on devnet-1",
		"missed window":      "Kynraze has missed > 5% of the slashing window's blocks on devnet-1",
		"missed proposals":   cc.proposalMessage("Kynraze"),
		"jail risk":          cc.jailRiskMessage("Kynraze"),
		"jail stage":         "Kynraze is close to jail on devnet-1",
		"signal stopped":     signalStoppedMessage(11),
		"jail soon":          cc.jailSoonMessage("Kynraze"),
		"jailed":             jailMessage(&ValInfo{Moniker: "Kynraze", Jailed: true, JailedUntil: until}, "devnet-1"),
		"jailed no end":      jailMessage(&ValInfo{Moniker: "Kynraze", Jailed: true}, "devnet-1"),
		"tombstoned":         jailMessage(&ValInfo{Moniker: "Kynraze", Jailed: true, Tombstoned: true}, "devnet-1"),
		"inactive":           jailMessage(monitor, "devnet-1"),
		"can unjail":         "Kynraze can unjail on devnet-1",
		"proposal":           cc.govOpenMessage(govProposal{ID: 39, Title: "Community Pool", VotingEnd: until.Add(48 * time.Hour)}),
		"proposal no title":  cc.govOpenMessage(govProposal{ID: 40, VotingEnd: until}),
		"vote reminder":      cc.govRemindMessage(govProposal{ID: 39, Title: "Community Pool", VotingEnd: until.Add(48 * time.Hour)}, "Kynraze"),
		"upgrade":            upgradeMessage(&upgradePlan{Name: "v4.0.0", Height: 12_345_678}),
		"upgrade soon":       upgradeSoonMessage(&upgradePlan{Name: "v4.0.0", Height: 12_345_678}),
		"signal started":     signalStartMessage(10),
		"signal missing":     signalMissingMessage(10),
		"signal reminder":    signalRemindMessage(10),
		"signal threshold":   signalThresholdMessage(10),
		"signal upgrade":     signalUpgradeMessage(10, 12_345_678),
		"signal upgrade end": signalUpgradeSoonMessage(10, 12_345_678),
		"fibre unregistered": fibreNotRegisteredMessage("devnet-1"),
		"fibre down":         fibreDownMessage(cardHost, "devnet-1"),
		"fibre identity":     fibreIdentityMessage(cardHost, "devnet-1"),
	}
	severities := map[string]string{"proposal": "info", "proposal no title": "info", "can unjail": "info", "jail risk": "warning",
		"signal started": "warning", "upgrade": "warning"}
	order := []string{"node down", "no RPC", "stalled", "missed blocks", "missed window", "missed proposals", "jail risk", "jail stage", "signal stopped", "jail soon",
		"jailed", "jailed no end", "tombstoned", "inactive", "can unjail", "proposal", "proposal no title", "vote reminder", "upgrade",
		"upgrade soon", "signal started", "signal missing", "signal reminder", "signal threshold", "signal upgrade", "signal upgrade end",
		"fibre unregistered", "fibre down", "fibre identity"}
	for _, kind := range fibreMetricsKinds {
		plain["fibre metrics "+kind.kind] = cc.fibreMetricsMessage(kind.kind)
		severities["fibre metrics "+kind.kind] = "warning"
		order = append(order, "fibre metrics "+kind.kind)
	}
	for _, name := range order {
		severity := severities[name]
		if severity == "" {
			severity = "critical"
		}
		raised(name, plain[name], severity)
	}

	for _, name := range []string{"node down", "no RPC", "stalled", "missed blocks", "missed window", "missed proposals", "jail risk",
		"jail soon", "jailed", "jailed no end", "tombstoned", "inactive", "can unjail"} {
		cleared(name, plain[name], nil)
	}
	open, remind, past := plain["proposal"], plain["vote reminder"], cc.govOpenMessage(govProposal{ID: 39, Title: "Community Pool", VotingEnd: until.Add(-48 * time.Hour)})
	pastRemind := cc.govRemindMessage(govProposal{ID: 39, Title: "Community Pool", VotingEnd: until.Add(-48 * time.Hour)}, "Kynraze")
	outcome := func(result string) func() {
		return func() { govOutcomes.Store("Devnet1#39", result) }
	}
	cleared("proposal voted", open, func() { govOutcomes.Delete("Devnet1#39") })
	cleared("proposal voted, reminder", remind, func() { govOutcomes.Delete("Devnet1#39") })
	cleared("proposal ended", past, func() { govOutcomes.Delete("Devnet1#39") })
	cleared("proposal ended without a vote", pastRemind, func() { govOutcomes.Delete("Devnet1#39") })
	cleared("proposal passed", open, outcome("Passed"))
	cleared("proposal passed, ended", past, outcome("Passed"))
	cleared("proposal rejected without a vote", remind, outcome("Rejected"))
	cleared("proposal failed", open, outcome("Failed"))
	cleared("proposal no title", plain["proposal no title"], func() { govOutcomes.Delete("Devnet1#40") })
	for _, name := range []string{"upgrade", "upgrade soon", "signal upgrade", "signal upgrade end"} {
		cleared(name+" done", plain[name], func() { upgradeOutcomes.Delete("Devnet1@12345678") })
		cleared(name+" cancelled", plain[name], func() { upgradeOutcomes.Store("Devnet1@12345678", "Cancelled") })
	}

	down, identity, unregistered := plain["fibre down"], plain["fibre identity"], plain["fibre unregistered"]
	fibre := func(name, message string, state fibreState) {
		stored(name, message, cc.fibreResolution(message, state, "Kynraze"))
	}
	fibre("fibre registered", unregistered, fibreState{Registered: true, Host: cardHost})
	fibre("fibre up", down, fibreState{Registered: true, Host: cardHost, Reachable: true})
	fibre("fibre moved", down, fibreState{Registered: true, Host: "new.example.com:7980", Reachable: true})
	fibre("fibre dropped", down, fibreState{})
	fibre("fibre verified", identity, fibreState{Registered: true, Host: cardHost, Reachable: true, KeyKnown: true, IdentityOK: true})
	fibre("fibre not checked", identity, fibreState{Registered: true, Host: cardHost, Reachable: true})
	for _, kind := range fibreMetricsKinds {
		message := plain["fibre metrics "+kind.kind]
		stored("fibre metrics "+kind.kind, message, cc.fibreMetricsResolution(message, fibreSnapshot{}))
	}
	signal := func(name, message string, state signalState) {
		r, ok := cc.signalResolution(message, state, "Kynraze")
		if !ok {
			t.Fatalf("%s has no resolution", name)
		}
		stored(name, message, r)
	}
	signal("signal upgraded", plain["signal started"], signalState{Version: 11})
	signal("signal signalled", plain["signal missing"], signalState{Version: 10, Signalled: true})
	signal("signal reminder signalled", plain["signal reminder"], signalState{Version: 10, Signalled: true})
	signal("signal reminder dropped", plain["signal reminder"], signalState{Version: 10})
	signal("signal reminder scheduled", plain["signal reminder"], signalState{Version: 10, UpgradeHeight: 12_345_678})
	signal("signal reminder threshold", plain["signal reminder"], signalState{Version: 10, Power: 84, Threshold: 84})
	signal("signal scheduled", plain["signal threshold"], signalState{Version: 10, UpgradeHeight: 12_345_678})
	signal("signal fell back", plain["signal threshold"], signalState{Version: 10})
	signal("signal dropped", plain["signal started"], signalState{Version: 10})

	for _, name := range []string{"fibre unregistered", "fibre down", "fibre identity", "signal started", "signal missing", "signal reminder", "signal threshold"} {
		cleared("restored "+name, plain[name], nil)
	}
	for _, kind := range fibreMetricsKinds {
		cleared("restored fibre metrics "+kind.kind, plain["fibre metrics "+kind.kind], nil)
	}
	for _, name := range order {
		message := plain[name]
		stored("no longer watched "+name, message, orphanResolution(message))
		cases[len(cases)-1].msg.lasted = 0
	}
	return cases
}

// detailForms are the live detail lines a message gets when sent.
var detailForms = []struct{ message, detail string }{
	{signalUpgradeMessage(11, 1365), "localhost:26657 · celestia-app 10.4.0 · needs v11\nrpc.kynraze.com · celestia-app 11.0.0 · ready"},
	{signalUpgradeSoonMessage(11, 1365), "localhost:26657 · version unknown"},
	{signalUpgradeSoonMessage(11, 1365), "all nodes ready"},
	{signalUpgradeMessage(11, 1365), "upgrade height reached · not upgraded: localhost:26657 (10.4.0)"},
	{"RPC node http://localhost:26657 has been down for > 3 minutes on celestia", "stopped at the version 11 upgrade height 1,365: runs celestia-app 10.4.0, upgrade to v11"},
	{signalRemindMessage(10), "71% signalled, the upgrade unlocks at 83.33%: upgrade celestia-appd, then send the signal tx"},
	{signalMissingMessage(10), "upgrade celestia-appd, then send the signal tx"},
	{signalUpgradeMessage(11, 1000), "≈ Wed 7 Oct 14:20 WIB, in 2d 3h\nRPC node runs celestia-app 10.4.0-corto, version 11 needs v11 or later"},
	{signalUpgradeSoonMessage(11, 1000), "≈ Wed 7 Oct 14:20 +0700, in 2h\nRPC node ready: celestia-app 11.0.0 · own node unknown (none with alert_if_down)"},
	{signalStoppedMessage(11), "upgrade celestia-appd to v11 or later"},
	{"Kynraze is close to jail on devnet-1", "9,000 of 9,990 misses used · jailed in ~45 min at this rate"},
	{"Kynraze is jailed on devnet-1", "jailed since Tue 6 Oct 02:00 WIB · can unjail from 02:10"},
	{"Kynraze is jailed on devnet-1", "jailed since Tue 6 Oct 02:00 WIB · can unjail from 7 Oct 02:10"},
	{"Kynraze is jailed on devnet-1", "jailed since Tue 6 Oct 02:00 WIB · can unjail now"},
	{"Kynraze is jailed on devnet-1", "jailed since Tue 6 Oct 02:00 WIB"},
	{"Kynraze is jailed on devnet-1", "can unjail from 02:10"},
	{"Kynraze is jailed on devnet-1", "can unjail now"},
	{"Kynraze is jailed on devnet-1", "jailed until Tue 6 Oct 02:10 WIB"},
	{"Kynraze can unjail on devnet-1", "send the unjail tx"},
	{"Kynraze is no longer in the active set on devnet-1", "tombstoned"},
	{"Kynraze is no longer in the active set on devnet-1", "unbonding"},
	{"Kynraze is no longer in the active set on devnet-1", "not in the top 100 by stake"},
	{"Fibre at localhost:7980 is unreachable on devnet-1", "connection refused: Fibre is not listening on that port"},
	{"Fibre at localhost:7980 does not prove it belongs to this validator on devnet-1", "the certificate is not valid at this time"},
	{"Fibre has sent no metrics for over 3 minutes on devnet-1", "last metrics 1h 3m ago"},
	{"Fibre has sent no metrics for over 3 minutes on devnet-1", "no metrics since tenderduty started"},
	{"Fibre shard budget is nearly full on devnet-1, the budget the chain gives this validator is almost used up", "70 GB of 74 GB, 3 uploads turned away in the last 10 minutes. The budget is set by the chain from your stake, the whole network fills together"},
	{"Fibre shard budget is nearly full on devnet-1, the budget the chain gives this validator is almost used up", "900 MB used, 1 upload turned away in the last 10 minutes. The budget is set by the chain from your stake, the whole network fills together"},
	{"Fibre shard budget is nearly full on devnet-1, the budget the chain gives this validator is almost used up", "0 uploads turned away in the last 10 minutes. The budget is set by the chain from your stake, the whole network fills together"},
	{"Fibre could not sign payment promises on devnet-1", "1 of 23 signatures failed over the last 9h 30m"},
	{"Fibre signing is slower than 10ms on devnet-1", "5 of 9 signatures took over 10ms in the last 10 minutes"},
	{"Fibre storage operations are failing on devnet-1", "put 1, prune 2 failed in the last 10 minutes"},
	{"Fibre storage backend requests are failing on devnet-1", "object: 6 of 100 requests failed in the last 10 minutes (timeout 1, throttled 2, error 3)"},
	{"Fibre storage backend requests are failing on devnet-1", "local: 3 of 120 requests failed in the last 10 minutes (timeout 2, error 1)\nCheck Fibre's data disk: space, health and permissions."},
	{"Fibre storage backend requests are failing on devnet-1", "s3: 3 of 120 requests failed in the last 10 minutes (timeout 2, error 1)\nCheck the bucket's credentials, rate limits, network, and that it is in the same region as Fibre."},
	{"Fibre has had no successful upload for over 15 minutes on devnet-1", "last successful upload 1h 3m ago"},
	{"Kynraze is jailed on devnet-1", "Can unjail after 3 Oct 2026 18:28 UTC · 9m left"},
	{"Kynraze is tombstoned on devnet-1", "Permanent, the validator can't be unjailed"},
	{"Upgrade v4.0.0 at height 12,345,678", "Around 5 Oct 2026 22:18 UTC · 2d 4h left"},
	{"Version 10 upgrade at height 12,345,678 is close", "Around 5 Oct 2026 22:18 UTC · 5h 3m left"},
	{"Proposal #39 Community Pool\nEnds 10 Oct 2026 17:48 UTC", ""},
}

// checkCard checks labelled lines and Telegram and Discord limits.
func checkCard(t *testing.T, name string, card alertCard) {
	t.Helper()
	if !card.known() {
		t.Errorf("%s fell back to a note: %q", name, card.Note)
		return
	}
	if card.Icon == "" || card.State == "" || len(card.Fields) == 0 {
		t.Errorf("%s: icon %q, state %q, %d fields", name, card.Icon, card.State, len(card.Fields))
	}
	if len(card.Fields) > 7 {
		t.Errorf("%s: %d fields", name, len(card.Fields))
	}
	for _, f := range card.Fields {
		if f.Icon == "" || f.Label == "" || f.Value == "" || strings.Contains(f.Value, "\n") || utf8.RuneCountInString(f.Value) > cardValueLimit {
			t.Errorf("%s: bad field %+v", name, f)
		}
		if f.Label == "Detail" || f.Label == "Note" && !strings.HasPrefix(f.Value, "The budget is") && !strings.HasPrefix(f.Value, "Permanent,") {
			t.Errorf("%s: a line was not understood: %+v", name, f)
		}
	}
	telegramHTML(t, strings.Join(card.telegramLines(), "\n"))
	embed := card.discordEmbed(1)
	if len(embed.Fields) != len(card.Fields) {
		t.Errorf("%s: %d discord fields for %d", name, len(embed.Fields), len(card.Fields))
	}
	for _, f := range embed.Fields {
		if f.Name == "" || f.Value == "" {
			t.Errorf("%s: empty discord field %+v", name, f)
		}
	}
}

// checkSent checks HTML, JSON and Discord limits.
func checkSent(t *testing.T, name string, msg *alertMsg) {
	t.Helper()
	telegram := buildTelegramMessage(msg)
	telegramHTML(t, telegram)
	if strings.Contains(telegram, "\n\n\n") {
		t.Errorf("%s: blank lines in %q", name, telegram)
	}
	for _, style := range []string{"embed", "plain"} {
		msg.discStyle = style
		data, err := json.Marshal(buildDiscordMessage(msg))
		if err != nil {
			t.Fatal(err)
		}
		var sent struct {
			Content string
			Embeds  []struct{ Description string }
		}
		if err := json.Unmarshal(data, &sent); err != nil {
			t.Fatal(err)
		}
		if utf8.RuneCountInString(sent.Content) > discordContentLimit || (style == "plain") != (len(sent.Embeds) == 0) {
			t.Errorf("%s: discord %s content is %d characters, %d embeds", name, style, utf8.RuneCountInString(sent.Content), len(sent.Embeds))
		}
	}
	msg.discStyle = ""
	if _, err := json.Marshal(buildSlackMessage(msg)); err != nil {
		t.Fatal(err)
	}
}

func TestEveryNotificationIsACard(t *testing.T) {
	cases := cardCases(t)
	if len(cases) < 100 {
		t.Fatalf("only %d cases", len(cases))
	}
	for _, c := range cases {
		msg := c.use()
		checkCard(t, c.name, msg.card())
		checkSent(t, c.name, msg)
	}
}

func TestCardDetailLinesAreUnderstood(t *testing.T) {
	cardChain(t)
	for _, tc := range detailForms {
		body := tc.message
		if tc.detail != "" {
			body += "\n" + tc.detail
		}
		msg := &alertMsg{chain: "Devnet1", message: tc.message, severity: "warning"}
		checkCard(t, tc.message+" / "+tc.detail, msg.cardFrom(body))
	}
}

// TestTheOldInactiveMessageIsACard: saved state from earlier versions carries the old message
func TestTheOldInactiveMessageIsACard(t *testing.T) {
	cardChain(t)
	for _, resolved := range []bool{false, true} {
		msg := &alertMsg{chain: "Devnet1", severity: "critical", message: "Kynraze is no longer active: validator is jailed", resolved: resolved}
		card := msg.card()
		checkCard(t, "old inactive message", card)
		if card.Fields[0].Value != "Kynraze" {
			t.Fatalf("resolved %v: %+v", resolved, card.Fields)
		}
	}
}

func TestUnknownMessageStaysAsNote(t *testing.T) {
	cardChain(t)
	msg := &alertMsg{chain: "Devnet1", severity: "critical", message: "something new <&> happened", extra: "monitor-eu", tgMentions: "@ops"}
	card := msg.card()
	if card.known() || card.Note != msg.message || len(card.Fields) != 0 {
		t.Fatalf("card is %+v", card)
	}
	if got, want := buildTelegramMessage(msg), "🚨 <b>Devnet1</b> · Critical\n\nsomething new &lt;&amp;&gt; happened\n<i>monitor-eu</i>\n@ops"; got != want {
		t.Fatalf("telegram is %q, want %q", got, want)
	}
	embed := buildDiscordMessage(msg).Embeds[0]
	if embed.Description != msg.message || len(embed.Fields) != 0 || embed.Footer == nil || embed.Footer.Text != "monitor-eu" {
		t.Fatalf("discord embed is %+v", embed)
	}
	slack := buildSlackMessage(msg)
	if slack.Text != "something new &lt;&amp;&gt; happened\nmonitor-eu" || len(slack.Attachments[0].Fields) != 0 {
		t.Fatalf("slack is %+v", slack)
	}
	msg = &alertMsg{chain: "Devnet1", severity: "critical", message: "Severity: critical\nsomething else"}
	if card := msg.card(); card.known() || card.Note != "something else" {
		t.Fatalf("card is %+v", card)
	}
	msg = &alertMsg{chain: "Devnet1", resolved: true, message: "something else"}
	if card := msg.card(); card.known() || card.Note != "something else" || card.State != "Resolved" {
		t.Fatalf("card is %+v", card)
	}
}

func TestTelegramTombstonedSlashCard(t *testing.T) {
	c := auditConfig(t)
	c.Reports.Timezone = "Asia/Jakarta"
	block, val := atomoneSlashFixture(t)
	val.Moniker = "POSTHUMAN"
	var d slashDetails
	d.read(block, val, 10584954)
	d.Denom, d.Symbol, d.Exponent, d.DisplayKnown, d.Fraction = "uatone", "ATONE", 6, true, "0.05"
	cc := &ChainConfig{name: "AtomOne", ChainId: "atomone-1", valInfo: val, slash: d}
	c.Chains[cc.name] = cc
	for _, message := range []string{jailMessage(val, cc.ChainId), "POSTHUMAN is tombstoned on atomone-1"} {
		msg := &alertMsg{chain: cc.name, severity: "critical", message: message}
		want := "\u2620\ufe0f <b>AtomOne</b> \u00b7 Tombstoned\n\n" +
			"\U0001f464 <b>Validator:</b> POSTHUMAN\n\u26d3 <b>Chain:</b> <code>atomone-1</code>\n" +
			"\u270d\ufe0f <b>Double-signed:</b> 10,584,953 \u00b7 round 0 \u00b7 prevote \u00b7 30 Sep 07:53 WIB\n" +
			"\U0001f525 <b>Slashed:</b> 89,868 ATONE (5%)\nPermanent, the validator can&#39;t be unjailed"
		if got := buildTelegramMessage(msg); got != want {
			t.Fatalf("got\n%s\nwant\n%s", got, want)
		}
	}
	val.Tombstoned = false
	msg := &alertMsg{chain: cc.name, severity: "critical", message: "POSTHUMAN is jailed on atomone-1"}
	if got := buildTelegramMessage(msg); !strings.Contains(got, "\U0001f525 <b>Slashed:</b>") || !strings.Contains(got, "\U0001f512 <b>Status:</b> \U0001f534 jailed") {
		t.Fatalf("jailed card: %s", got)
	}
}

func TestTelegramCards(t *testing.T) {
	cc := cardChain(t)
	up := fibreDownMessage(cardHost, "devnet-1")
	resolve("Devnet1", up, cc.fibreResolution(up, fibreState{Registered: true, Host: cardHost, Reachable: true}, "Kynraze"))
	for _, tc := range []struct {
		name string
		msg  *alertMsg
		want string
	}{
		{"fibre down", &alertMsg{chain: "Devnet1", severity: "critical", message: fibreDownMessage(cardHost, "devnet-1")},
			"🚨 <b>Devnet1</b> · Fibre down\n\n🌐 <b>Host:</b> <code>localhost:7980</code>\n⛓ <b>Chain:</b> <code>devnet-1</code>\n📡 <b>Reachable:</b> 🔴 no, for 2m\n" +
				"❓ <b>Why:</b> connection refused: Fibre is not listening on that port\n⚠️ <b>Severity:</b> critical"},
		{"missed blocks", &alertMsg{chain: "Devnet1", severity: "critical", message: "Kynraze has missed 10 blocks on devnet-1", extra: "monitor-eu", tgMentions: "@ops"},
			"🚨 <b>Devnet1</b> · Critical\n\n👤 <b>Validator:</b> Kynraze\n⛓ <b>Chain:</b> <code>devnet-1</code>\n🎯 <b>Missed:</b> 10 blocks in a row\n<i>monitor-eu</i>\n@ops"},
		{"node down", &alertMsg{chain: "Devnet1", severity: "warning", message: nodeDownMessage("http://10.0.0.1:26657", "devnet-1")},
			"⚠️ <b>Devnet1</b> · Warning\n\n🔌 <b>Node:</b> <code>10.0.0.1:26657</code>\n⛓ <b>Chain:</b> <code>devnet-1</code>\n📡 <b>Status:</b> 🔴 down for &gt; 3 minutes"},
		{"signing slow", &alertMsg{chain: "Devnet1", severity: "warning", message: cc.fibreMetricsMessage(kindSigning)},
			"🐢 <b>Devnet1</b> · Signing slow\n\n⛓ <b>Chain:</b> <code>devnet-1</code>\n🐢 <b>Signing:</b> 🟡 slower than 10 ms\n⚠️ <b>Severity:</b> warning"},
		{"proposal", &alertMsg{chain: "Devnet1", severity: "info", message: "Proposal #39 Community Pool\nEnds 10 Oct 2020 17:48 UTC"},
			"🗳 <b>Devnet1</b> · New proposal\n\n<b>#39 Community Pool</b>\n🗓 <b>Ends:</b> 10 Oct 2020 17:48 UTC"},
		{"vote reminder", &alertMsg{chain: "Devnet1", severity: "critical", message: "Proposal #39 Community Pool\nEnds 10 Oct 2020 17:48 UTC\nKynraze has not voted"},
			"⏰ <b>Devnet1</b> · Vote reminder\n\n<b>#39 Community Pool</b>\n🗓 <b>Ends:</b> 10 Oct 2020 17:48 UTC\n👤 <b>Validator:</b> Kynraze\n🗳 <b>Vote:</b> 🔴 not voted yet\n⚠️ <b>Severity:</b> critical"},
		{"upgrade", &alertMsg{chain: "Devnet1", severity: "warning", message: "Upgrade v4.0.0 at height 99,999"},
			"⬆️ <b>Devnet1</b> · Upgrade scheduled\n\n<b>v4.0.0</b>\n📏 <b>Height:</b> 99,999\n⚠️ <b>Severity:</b> warning"},
		{"jailed", &alertMsg{chain: "Devnet1", severity: "critical", message: "Kynraze is jailed on devnet-1"},
			"🚨 <b>Devnet1</b> · Jailed\n\n👤 <b>Validator:</b> Kynraze\n⛓ <b>Chain:</b> <code>devnet-1</code>\n🔒 <b>Status:</b> 🔴 jailed\n⚠️ <b>Severity:</b> critical"},
		{"fibre up", &alertMsg{chain: "Devnet1", resolved: true, severity: "info", lasted: 12 * time.Minute, message: fibreDownMessage(cardHost, "devnet-1")},
			"✅ <b>Devnet1</b> · Fibre up\n\n🌐 <b>Host:</b> <code>localhost:7980</code>\n⛓ <b>Chain:</b> <code>devnet-1</code>\n📡 <b>Reachable:</b> 🟢 yes\n⏱ <b>Lasted:</b> 12m"},
		{"signing", &alertMsg{chain: "Devnet1", resolved: true, severity: "info", lasted: 3*time.Hour + 5*time.Minute, message: "Kynraze has missed 10 blocks on devnet-1"},
			"✅ <b>Devnet1</b> · Resolved\n\n👤 <b>Validator:</b> Kynraze\n⛓ <b>Chain:</b> <code>devnet-1</code>\n✍️ <b>Signing:</b> 🟢 signing blocks again\n⏱ <b>Lasted:</b> 3h 5m"},
		{"active again", &alertMsg{chain: "Devnet1", resolved: true, severity: "info", message: "Kynraze is jailed on devnet-1"},
			"✅ <b>Devnet1</b> · Active again\n\n👤 <b>Validator:</b> Kynraze\n⛓ <b>Chain:</b> <code>devnet-1</code>\n🔒 <b>Status:</b> 🟢 back in the active set"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := buildTelegramMessage(tc.msg); got != tc.want {
				t.Fatalf("got\n%s\nwant\n%s", got, tc.want)
			}
		})
	}

	govOutcomes.Store("Devnet1#39", "Passed")
	passed := &alertMsg{chain: "Devnet1", resolved: true, severity: "info", message: "Proposal #39 Community Pool\nEnds 10 Oct 2020 17:48 UTC"}
	if got, want := buildTelegramMessage(passed), "✅ <b>Devnet1</b> · Passed\n\n<b>#39 Community Pool</b>\n📜 <b>Result:</b> 🟢 passed\n🏁 <b>Voting:</b> ended 10 Oct 2020 17:48 UTC"; got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
	orphan := &alertMsg{chain: "Devnet1", resolved: true, severity: "info", lasted: time.Hour, message: fibreDownMessage(cardHost, "devnet-1")}
	resolve("Devnet1", orphan.message, orphanResolution(orphan.message))
	if got, want := buildTelegramMessage(orphan), "ℹ️ <b>Devnet1</b> · Alert stopped\n\n🌐 <b>Host:</b> <code>localhost:7980</code>\n⛓ <b>Chain:</b> <code>devnet-1</code>\n👁 <b>Watching:</b> ⚪ stopped, the alert was turned off or its settings changed"; got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
	resolve("Devnet1", orphan.message, removedResolution(orphan.message))
	if got, want := buildTelegramMessage(orphan), "ℹ️ <b>Devnet1</b> · No longer watched\n\n🌐 <b>Host:</b> <code>localhost:7980</code>\n⛓ <b>Chain:</b> <code>devnet-1</code>\n👁 <b>Watching:</b> ⚪ stopped, the chain was removed from the config"; got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}

func TestHostileTextIsEscaped(t *testing.T) {
	cardChain(t)
	const hostile = `<b>&"x"</b> @everyone [a](http://x) *y* ` + "`z`"
	for _, message := range []string{
		hostile + " has missed 10 blocks on devnet-1",
		"Fibre at " + hostile[:3] + "&host is unreachable on devnet-1",
		"Proposal #1 " + hostile + "\nEnds 10 Oct 2020 17:48 UTC",
		"RPC node http://a<b>&c has been down for > 3 minutes on devnet-1",
	} {
		msg := &alertMsg{chain: "Dev<net>&1", severity: "critical", message: message, extra: "<i>extra</i>&", discMentions: "@here", tgMentions: "<b>@ops</b>"}
		card := msg.card()
		if !card.known() {
			t.Fatalf("%q fell back", message)
		}
		telegram := buildTelegramMessage(msg)
		// mentions are already HTML, the rest must be escaped
		telegramHTML(t, strings.Replace(telegram, "<b>@ops</b>", "", 1))
		if strings.Contains(telegram, "<b>&") || strings.Contains(telegram, "<i>extra") || strings.Contains(telegram, "Dev<net>") {
			t.Fatalf("unescaped text in %q", telegram)
		}
		// discord only pings from the content
		hook := buildDiscordMessage(msg)
		if strings.Contains(hook.Content, "everyone") || !strings.HasSuffix(hook.Content, "@here") {
			t.Fatalf("discord content is %q", hook.Content)
		}
		data, _ := json.Marshal(hook)
		if !strings.Contains(string(data), "everyone") && strings.Contains(message, "everyone") {
			t.Fatalf("the text was lost: %s", data)
		}
		msg.discStyle = "plain"
		if buildDiscordMessage(msg).Embeds != nil {
			t.Fatal("plain style has an embed")
		}
		// slack reads < and & as markup
		for _, a := range buildSlackMessage(msg).Attachments {
			for _, f := range a.Fields {
				if strings.ContainsAny(strings.NewReplacer("&amp;", "", "&lt;", "", "&gt;", "").Replace(f.Value), "<>&") {
					t.Fatalf("slack field %+v has markup", f)
				}
			}
			if strings.ContainsAny(strings.NewReplacer("&amp;", "", "&lt;", "", "&gt;", "").Replace(a.Text), "<>&") {
				t.Fatalf("slack text %q has markup", a.Text)
			}
		}
	}
}

func TestDiscordCards(t *testing.T) {
	cardChain(t)
	msg := &alertMsg{chain: "Devnet1", severity: "critical", message: fibreDownMessage(cardHost, "devnet-1"), extra: "monitor-eu", discMentions: "<@1234>"}
	hook := buildDiscordMessage(msg)
	if hook.Content != "🚨 **Devnet1** · Fibre down <@1234>" || len(hook.Embeds) != 1 {
		t.Fatalf("content %q, %d embeds", hook.Content, len(hook.Embeds))
	}
	embed := hook.Embeds[0]
	if embed.Color != 0xf27c76 || embed.Description != "" || embed.Footer == nil || embed.Footer.Text != "monitor-eu" {
		t.Fatalf("embed %+v", embed)
	}
	want := []DiscordField{
		{"🌐 Host", "`localhost:7980`", true},
		{"⛓ Chain", "`devnet-1`", true},
		{"📡 Reachable", "🔴 no, for 2m", true},
		{"❓ Why", "connection refused: Fibre is not listening on that port", false},
		{"⚠️ Severity", "critical", true},
	}
	if len(embed.Fields) != len(want) {
		t.Fatalf("fields %+v", embed.Fields)
	}
	for i, f := range want {
		if embed.Fields[i] != f {
			t.Errorf("field %d is %+v, want %+v", i, embed.Fields[i], f)
		}
	}

	msg = &alertMsg{chain: "Devnet1", severity: "info", message: "Proposal #39 Community Pool\nEnds 10 Oct 2020 17:48 UTC"}
	embed = buildDiscordMessage(msg).Embeds[0]
	if embed.Description != "**#39 Community Pool**" || len(embed.Fields) != 1 || embed.Fields[0].Name != "🗓 Ends" {
		t.Fatalf("proposal embed %+v", embed)
	}
	msg = &alertMsg{chain: "Devnet1", severity: "critical", message: "Kynraze is jailed on devnet-1\nCan unjail after 3 Oct 2030 18:28 UTC"}
	if got := (alertCard{Fields: []alertField{{Icon: "🔓", Label: "Unjail after", Value: "3 Oct 2030 18:28 UTC · 9m left"}}}).discordEmbed(0).Fields[0].Value; got != "3 Oct 2030 18:28 UTC · **9m left**" {
		t.Fatalf("time left is %q", got)
	}
	msg = &alertMsg{chain: "Devnet1", resolved: true, severity: "info", lasted: time.Hour, message: "Kynraze is jailed on devnet-1"}
	if embed = buildDiscordMessage(msg).Embeds[0]; embed.Color != 0x63d6c4 || embed.Fields[len(embed.Fields)-1].Name != "⏱ Lasted" {
		t.Fatalf("resolved embed %+v", embed)
	}
}

func TestDiscordLimits(t *testing.T) {
	long := strings.Repeat("🔴", 3000)
	card := alertCard{Title: long, Extra: long}
	for i := 0; i < 40; i++ {
		card.Fields = append(card.Fields, alertField{Icon: "🎯", Label: strings.Repeat("é", 400), Value: long})
	}
	embed := card.discordEmbed(1)
	size := utf8.RuneCountInString(embed.Description) + utf8.RuneCountInString(embed.Footer.Text)
	for _, f := range embed.Fields {
		size += utf8.RuneCountInString(f.Name) + utf8.RuneCountInString(f.Value)
		if utf8.RuneCountInString(f.Name) > discordNameLimit || utf8.RuneCountInString(f.Value) > discordValueLimit || f.Name == "" || f.Value == "" {
			t.Fatalf("field over the limit: %d, %d", utf8.RuneCountInString(f.Name), utf8.RuneCountInString(f.Value))
		}
	}
	if len(embed.Fields) > discordFieldsLimit || size > discordEmbedLimit || utf8.RuneCountInString(embed.Description) > discordDescriptionLimit ||
		utf8.RuneCountInString(embed.Footer.Text) > discordFooterLimit {
		t.Fatalf("%d fields, %d characters", len(embed.Fields), size)
	}
	data, err := json.Marshal(embed)
	if err != nil || !utf8.Valid(data) {
		t.Fatalf("not valid json: %v", err)
	}

	// discord allows 25 fields
	card = alertCard{}
	for i := 0; i < 30; i++ {
		card.Fields = append(card.Fields, alertField{Icon: "🎯", Label: "Label", Value: "value"})
	}
	if got := len(card.discordEmbed(0).Fields); got != discordFieldsLimit {
		t.Fatalf("%d fields", got)
	}

	msg := &alertMsg{chain: "Devnet1", severity: "critical", message: strings.Repeat("x", 5000), discStyle: "plain"}
	if got := utf8.RuneCountInString(buildDiscordMessage(msg).Content); got > discordContentLimit {
		t.Fatalf("plain content is %d characters", got)
	}
}

func TestSlackCards(t *testing.T) {
	cardChain(t)
	msg := &alertMsg{chain: "Devnet1", severity: "critical", message: fibreDownMessage(cardHost, "devnet-1"), slkMentions: "<!here>"}
	slack := buildSlackMessage(msg)
	if slack.Text != fibreDownMessage(cardHost, "devnet-1") {
		t.Fatalf("text is %q", slack.Text)
	}
	a := slack.Attachments[0]
	if a.Title != "🚨 Devnet1 · Fibre down <!here>" || a.Color != "danger" || a.Text != "" {
		t.Fatalf("attachment %+v", a)
	}
	want := []SlackField{
		{"🌐 Host", "`localhost:7980`", true},
		{"⛓ Chain", "`devnet-1`", true},
		{"📡 Reachable", "🔴 no, for 2m", true},
		{"❓ Why", "connection refused: Fibre is not listening on that port", false},
		{"⚠️ Severity", "critical", true},
	}
	if len(a.Fields) != len(want) {
		t.Fatalf("fields %+v", a.Fields)
	}
	for i, f := range want {
		if a.Fields[i] != f {
			t.Errorf("field %d is %+v, want %+v", i, a.Fields[i], f)
		}
	}
	data, _ := json.Marshal(slack)
	if !strings.Contains(string(data), `"fields":[{"title":"🌐 Host","value":"`) || !strings.Contains(string(data), `"mrkdwn_in":["text","fields"]`) {
		t.Fatalf("json is %s", data)
	}

	msg = &alertMsg{chain: "Devnet1", resolved: true, severity: "info", message: "Proposal #39 Community Pool\nEnds 10 Oct 2020 17:48 UTC"}
	a = buildSlackMessage(msg).Attachments[0]
	if a.Color != "good" || a.Text != "*#39 Community Pool*" || len(a.Fields) == 0 {
		t.Fatalf("resolved attachment %+v", a)
	}
}

func TestPagerdutyTextIsNotACard(t *testing.T) {
	cardChain(t)
	msg := &alertMsg{chain: "Devnet1", severity: "critical", message: fibreDownMessage(cardHost, "devnet-1")}
	if got := strings.ReplaceAll(msg.summary(), "\n", " · "); got != fibreDownMessage(cardHost, "devnet-1")+" · connection refused: Fibre is not listening on that port" {
		t.Fatalf("pagerduty summary is %q", got)
	}
}

func TestLastedFollowsTheAlarm(t *testing.T) {
	c := auditConfig(t)
	cc := &ChainConfig{name: "Devnet1", ChainId: "devnet-1", valInfo: &ValInfo{Moniker: "Kynraze"}}
	c.Chains["Devnet1"] = cc
	chain := c.Chains["Devnet1"]
	chain.Alerts.Telegram.Enabled = yes(true)
	c.Telegram.Enabled = yes(true)
	const message = "Kynraze has missed 10 blocks on devnet-1"
	id := "valcons1consecutive"

	c.alert("Devnet1", message, "critical", false, &id)
	<-c.alertChan
	alarms.notifyMux.Lock()
	alarms.AllAlarms["Devnet1"][message] = time.Now().Add(-12 * time.Minute)
	alarms.notifyMux.Unlock()
	alarms.notifyMux.Lock()
	alarms.SentTgAlarms["Devnet1\x00"+message] = time.Now()
	alarms.notifyMux.Unlock()
	c.alert("Devnet1", message, "info", true, &id)
	got := <-c.alertChan
	if got.lasted < 12*time.Minute || got.lasted > 13*time.Minute {
		t.Fatalf("lasted %v", got.lasted)
	}
	if fields := got.card().Fields; fields[len(fields)-1] != (alertField{Icon: "⏱", Label: "Lasted", Value: "12m"}) {
		t.Fatalf("fields %+v", fields)
	}
	alarms.notifyMux.RLock()
	var queued *queuedNotification
	for i := range alarms.Outbox {
		if alarms.Outbox[i].Resolved {
			queued = &alarms.Outbox[i]
		}
	}
	recovery := alarms.PendingRecoveries["Devnet1\x00"+message]
	alarms.notifyMux.RUnlock()
	if queued == nil || queued.Lasted != got.lasted || recovery == nil || recovery.Lasted != got.lasted {
		t.Fatalf("queued %+v, recovery %+v", queued, recovery)
	}
	data, err := json.Marshal(queued)
	if err != nil || !strings.Contains(string(data), `"lasted":`) {
		t.Fatalf("not saved: %s %v", data, err)
	}

	c.alert("Devnet1", "Kynraze has missed 11 blocks on devnet-1", "info", true, &id)
	if never := <-c.alertChan; never.lasted != 0 {
		t.Fatalf("lasted %v", never.lasted)
	} else if card := never.card(); card.Fields[len(card.Fields)-1].Label == "Lasted" {
		t.Fatalf("fields %+v", card.Fields)
	}
}

func TestValidatorChangeCardLabels(t *testing.T) {
	for _, title := range []string{"Stake changed", "Validator changed"} {
		msg := &alertMsg{transient: true, message: title + "\nStake +71.6% in 1h · 58 → 100 saga\nSelf-bond a → b saga\nCommission x → y\nMin self delegation a → b\nMoniker a → b\nDescription: website\nUnknown change"}
		got := msg.card().Fields
		want := []alertField{
			row("", "Stake", "+71.6% in 1h · 58 → 100 saga"),
			row("", "Self-bond", "a → b saga"),
			row("", "Commission", "x → y"),
			row("", "Min self delegation", "a → b"),
			row("", "Moniker", "a → b"),
			row("", "Description", "website"),
			row("ℹ️", "Note", "Unknown change"),
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s fields: %#v, want %#v", title, got, want)
		}
	}
	msg := &alertMsg{transient: true, message: "Stake changed\nStake +71.6% in 1h · 58 → 100 saga"}
	if text := strings.Join(msg.card().telegramLines(), "\n"); strings.Contains(text, "Note") || !strings.Contains(text, "Stake") {
		t.Fatalf("stake card: %s", text)
	}
}

func TestReloadFailureCardLabels(t *testing.T) {
	msg := &alertMsg{transient: true, message: "Config reload failed\nerror: invalid config\nwarning: missing endpoint\nwarn: deprecated option\nyaml: line 5: invalid value\n"}
	card := msg.card()
	want := []alertField{
		row("❌", "Error", "invalid config"),
		row("⚠️", "Warning", "missing endpoint"),
		row("⚠️", "Warning", "deprecated option"),
		row("❌", "Error", "yaml: line 5: invalid value"),
	}
	if card.Title != "Previous configuration is still running" || !reflect.DeepEqual(card.Fields, want) {
		t.Fatalf("reload card: %#v", card)
	}
}

func TestAlertDisplayTimezone(t *testing.T) {
	cc := cardChain(t)
	for _, zone := range []string{"Asia/Jakarta", ""} {
		t.Run(zone, func(t *testing.T) {
			td.Reports.Timezone = zone
			at := time.Now().UTC().Truncate(time.Minute).Add(48 * time.Hour)
			stamp := at.Format(govEndFormat)
			want := at.In(alertLocation()).Format("2 Jan 2006 15:04 MST")
			proposal := cc.govOpenMessage(govProposal{ID: 99, VotingEnd: at})
			jail := jailMessage(&ValInfo{Moniker: "Kynraze", Jailed: true, JailedUntil: at}, cc.ChainId)
			upgrade := upgradeMessage(&upgradePlan{Name: "v4", Height: 12_345_678})
			upgradeETAs.Store("Devnet1@12345678", at)
			for _, tc := range []struct {
				message, label string
				resolved       bool
			}{
				{proposal, "Ends", false},
				{jail, "Unjail after", false},
				{upgrade, "Expected", false},
				{proposal, "Voting ends", true},
			} {
				msg := &alertMsg{chain: "Devnet1", message: tc.message, resolved: tc.resolved}
				found := false
				for _, field := range msg.card().Fields {
					if field.Label == tc.label && strings.HasPrefix(field.Value, want) {
						found = true
					}
				}
				if !found || !strings.Contains(msg.summary(), want) {
					t.Fatalf("%s: summary %q, fields %#v", tc.label, msg.summary(), msg.card().Fields)
				}
				if msg.message != tc.message {
					t.Fatal("stored message changed")
				}
			}
			if !strings.Contains(proposal, stamp) || !strings.Contains(jail, stamp) {
				t.Fatal("alarm keys must keep UTC timestamps")
			}
			for _, prefix := range []string{"Voting ended ", "Voted, voting ends "} {
				msg := &alertMsg{chain: "Devnet1", message: proposal, resolved: true}
				r := resolution{"✅", "Voted", "Proposal #99\n" + prefix + stamp}
				resolve(msg.chain, msg.message, r)
				if !strings.Contains(msg.summary(), prefix+want) || !strings.Contains(strings.Join(msg.card().plainLines(), "\n"), want) {
					t.Fatalf("stored resolution display: %q", msg.summary())
				}
				if stored, _ := storedResolution(msg.chain, msg.message); stored.text != r.text {
					t.Fatal("stored resolution changed")
				}
				resolutions.Delete(msg.chain + "\x00" + msg.message)
			}
		})
	}
}
