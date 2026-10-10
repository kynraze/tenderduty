package tenderduty

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
)

func reportFixture(weekly bool) periodReport {
	r := periodReport{
		Title:  "Daily report",
		Header: []string{"📊 Daily report · 4 Oct 2026", "🟢 1 healthy · 🔴 1 needs attention"},
		Footer: "Watched 24h of 24h · Asia/Jakarta",
		Rows: []reportRow{
			{Name: "AtomOne", Moniker: "Kynraze", Dot: dotGreen, Lines: []string{
				"✍️ 99.94% signed · 8 missed (3 late)",
				"🧱 Proposed 17 of 17 turns",
				"⚖️ 1.20M (+12.4K · +1.0%) · #72 ▲3",
				"🤝 812 delegators (+5) · self 50K (=)",
				"🔔 No alerts",
				"🗳 #41 Community pool spend · 1d 4h left",
			}},
			{Name: "Mocha <test>", Moniker: "A&B", Dot: dotRed, Testnet: true, Lines: []string{
				"🚨 Jailed since 6 Oct 02:00",
				"✍️ 97.31% signed · 310 missed (25 late, 1 prevote only)",
				"🧱 Proposed 12 of 17 turns · 5 missed",
				"⚖️ 1.10M (=) · #64 ▼2",
				"🤝 3 delegators (=) · self unavailable",
				"🔔 Missed blocks 42m",
				"⬆️ v11 at 6,120,000 · ~2d 3h",
			}, Fibre: []string{
				"🧵 Fibre: 1,284 shards stored · 0 failed",
				"✍️ Signing: 100% under 10 ms · p95 0.9 ms",
				"📡 99.3% reachable · upload p95 24 ms",
				"🗃 Shards 39.6 of 74.0 GiB (53%)",
			}},
		},
	}
	if weekly {
		r.Title, r.Weekly = "Weekly report", true
		r.Header = []string{"📈 Weekly report · 28 Sep – 4 Oct", "3 incidents · 4 votes cast"}
		r.Footer = "Watched 7d of 7d · Asia/Jakarta"
		r.Rows[0].Summary = "99.95% ▲0.02 · #72 ▲3"
		r.Rows[0].Lines = []string{"⚖️ +12.4K · +1.0% · 🤝 +5 · 🔔 0 · 🧱 17/17"}
		r.Rows[1].Summary = "97.60% ▲1.10 · #64 ▼2"
		r.Rows[1].Lines = []string{"🚨 Jailed since 6 Oct 02:00", "⚖️ = · 🤝 = · 🔔 3 · longest 1h 12m · 🧱 12/17"}
		r.Rows[1].Fibre = []string{
			"🧵 Fibre: 8,940 shards stored · 0 failed · 99.6% reachable",
			"✍️ Signing: 100% under 10 ms · p95 0.9 ms · upload p95 26 ms",
		}
		r.Notes = []string{"🗳 4 cast · 1 due (AtomOne #41)", "⬆️ Mocha v11 · Mon ~10:00"}
	}
	return r
}

func TestReportTelegramGolden(t *testing.T) {
	for _, weekly := range []bool{false, true} {
		kind := "daily"
		if weekly {
			kind = "weekly"
		}
		t.Run(kind, func(t *testing.T) {
			want, err := os.ReadFile(filepath.Join("testdata", "report_"+kind+".html"))
			if err != nil {
				t.Fatal(err)
			}
			got := reportFixture(weekly).telegram()
			if got != strings.TrimSuffix(strings.ReplaceAll(string(want), "\r\n", "\n"), "\n") {
				t.Fatalf("report changed:\n%s", got)
			}
			telegramHTML(t, got)
		})
	}
}

func TestReportNumbersTrendsAndDots(t *testing.T) {
	for _, tc := range []struct {
		n               float64
		compact, change string
	}{{1200000, "1.20M", "+1.20M"}, {88100, "88.1K", "+88.1K"}, {-12400, "-12.4K", "−12.4K"}, {0, "0", "="}, {812, "812", "+812"}} {
		if compactReportNumber(tc.n) != tc.compact || reportChange(tc.n) != tc.change {
			t.Fatalf("number %v: %s %s", tc.n, compactReportNumber(tc.n), reportChange(tc.n))
		}
	}
	a, b := 72, 75
	if reportRankTrend(&a, &b) != " ▲3" || reportRankTrend(&b, &a) != " ▼3" || reportRankTrend(&a, &a) != " =" || reportRankTrend(&a, nil) != "" {
		t.Fatal("rank trends")
	}
	if reportSigningTrend(reportHour{Signed: 100}, reportHour{}) != "" || reportSigningTrend(reportHour{Signed: 100}, reportHour{Signed: 99, Missed: 1}) != " ▲1.00" {
		t.Fatal("signing trends")
	}
	for _, tc := range []struct {
		active, jailed, known bool
		signed                float64
		incidents             int
		want                  string
	}{{true, false, true, 100, 0, dotRed}, {false, true, true, 100, 0, dotRed}, {false, false, true, 94, 0, dotRed}, {false, false, true, 99, 0, dotYellow}, {false, false, true, 100, 1, dotYellow}, {false, false, true, 100, 0, dotGreen}, {false, false, false, 0, 0, dotGreen}} {
		if got := reportDot(tc.active, tc.jailed, tc.signed, tc.known, tc.incidents, 95); got != tc.want {
			t.Fatalf("dot %+v: %s", tc, got)
		}
	}
}

func TestReportMissedBreakdown(t *testing.T) {
	c := &Config{}
	now := time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)
	for _, at := range []time.Time{now.Add(-2 * time.Hour), now.Add(-time.Hour)} {
		for _, status := range []StatusType{Statusmissed, StatusPrecommit, StatusPrevote, StatusSigned, StatusProposed, StatusMissedProposal} {
			c.recordReportBlock("Chain", at, status, status == StatusMissedProposal)
		}
	}
	c.recordReportBlock("Chain", now.Add(-25*time.Hour), StatusPrecommit, false)
	c.recordReportBlock("Chain", now, StatusPrevote, false)
	total := reportTotals(c.reportData.Hours["Chain"], now.Add(-24*time.Hour), now)
	if total.Missed != 6 || total.Late != 2 || total.PrevoteOnly != 2 || total.Signed != 6 || total.Proposed != 2 || total.MissedProps != 2 {
		t.Fatalf("block totals: %+v", total)
	}
	if signed, known := reportSigned(total); !known || signed != 50 {
		t.Fatalf("on-chain signing: %v, %v", signed, known)
	}
}

func TestReportSigningAndProposerLines(t *testing.T) {
	for _, tc := range []struct {
		name     string
		hour     reportHour
		signing  string
		proposer string
		weekly   string
	}{
		{"no turns", reportHour{Signed: 9957, Missed: 43}, "✍️ 99.57% signed · 43 missed", "", ""},
		{"late", reportHour{Signed: 9957, Missed: 43, Late: 25, Proposed: 17}, "✍️ 99.57% signed · 43 missed (25 late)", "🧱 Proposed 17 of 17 turns", " · 🧱 17/17"},
		{"late and prevote", reportHour{Signed: 9957, Missed: 43, Late: 25, PrevoteOnly: 1, Proposed: 12, MissedProps: 5}, "✍️ 99.57% signed · 43 missed (25 late, 1 prevote only)", "🧱 Proposed 12 of 17 turns · 5 missed", " · 🧱 12/17"},
		{"prevote only", reportHour{Signed: 9957, Missed: 43, PrevoteOnly: 1}, "✍️ 99.57% signed · 43 missed (1 prevote only)", "", ""},
		{"missed turns", reportHour{Signed: 9957, Missed: 43, MissedProps: 5}, "✍️ 99.57% signed · 43 missed", "🧱 Proposed 0 of 5 turns · 5 missed", " · 🧱 0/5"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := auditConfig(t)
			now := time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)
			c.Chains["Chain"] = &ChainConfig{name: "Chain", valInfo: &ValInfo{Moniker: "Kynraze", Bonded: true}}
			tc.hour.At = now.Add(-time.Hour)
			c.reportData.Hours = map[string][]reportHour{"Chain": {tc.hour}}
			row := c.buildReport(now, false, false, nil).Rows[0]
			if row.Lines[0] != tc.signing {
				t.Fatalf("signing: %s", row.Lines[0])
			}
			if tc.proposer != "" {
				if row.Lines[1] != tc.proposer {
					t.Fatalf("proposer: %s", row.Lines[1])
				}
			} else if strings.Contains(strings.Join(row.Lines, "\n"), "🧱") {
				t.Fatal("proposer line without turns")
			}
			weekly := c.buildReport(now, true, false, nil).Rows[0]
			// a short history adds a "Watched" line after it
			if len(weekly.Lines) == 0 || (tc.weekly != "" && !strings.HasSuffix(weekly.Lines[0], tc.weekly)) || (tc.weekly == "" && strings.Contains(weekly.Lines[0], "🧱")) {
				t.Fatalf("weekly lines: %v", weekly.Lines)
			}
		})
	}
}

func TestReportOldHourState(t *testing.T) {
	now := time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)
	var state savedState
	data := []byte(`{"reports":{"hours":{"Chain":[{"at":"2026-10-04T08:00:00Z","signed":9957,"missed":43,"proposed":12,"missed_props":5}]}}}`)
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	c := &Config{}
	c.restoreReports(state.Reports, now)
	total := reportTotals(c.reportData.Hours["Chain"], now.Add(-24*time.Hour), now)
	if total.Signed != 9957 || total.Missed != 43 || total.Proposed != 12 || total.MissedProps != 5 || total.Late != 0 || total.PrevoteOnly != 0 {
		t.Fatalf("old hour: %+v", total)
	}
	data, err := json.Marshal(c.savedReports(now))
	if err != nil || strings.Contains(string(data), `"late"`) || strings.Contains(string(data), `"prevote_only"`) {
		t.Fatalf("zero counters not omitted: %s, %v", data, err)
	}
	c.recordReportBlock("Chain", now.Add(-time.Hour), StatusPrecommit, false)
	c.recordReportBlock("Chain", now.Add(-time.Hour), StatusPrevote, false)
	data, err = json.Marshal(c.savedReports(now))
	if err != nil || !strings.Contains(string(data), `"late":1`) || !strings.Contains(string(data), `"prevote_only":1`) {
		t.Fatalf("new counters not saved: %s, %v", data, err)
	}
	var history reportHistory
	if err := json.Unmarshal(data, &history); err != nil {
		t.Fatal(err)
	}
	c.restoreReports(&history, now)
	total = reportTotals(c.reportData.Hours["Chain"], now.Add(-24*time.Hour), now)
	if total.Missed != 45 || total.Late != 1 || total.PrevoteOnly != 1 {
		t.Fatalf("new counters not restored: %+v", total)
	}
}

func TestReportSchedule(t *testing.T) {
	loc, err := loadTimezone("UTC+7")
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 5, 9, 0, 0, 0, loc)
	for _, tc := range []struct {
		name      string
		now, last time.Time
		weekly    bool
		schedule  string
		due, late bool
	}{
		{"due", at, time.Time{}, false, "09:00", true, false},
		{"before", at.Add(-time.Minute), at.AddDate(0, 0, -1), false, "09:00", false, false},
		{"restart", at.Add(time.Minute), at, false, "09:00", false, false},
		{"late", at.Add(6 * time.Hour), time.Time{}, false, "09:00", true, true},
		{"skip", at.Add(6*time.Hour + time.Second), time.Time{}, false, "09:00", false, true},
		{"off", at, time.Time{}, false, "", false, false},
		{"weekly", at, time.Time{}, true, "monday 09:00", true, false},
		{"weekly before", at.Add(-time.Minute), at.AddDate(0, 0, -7), true, "monday 09:00", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, due, late := reportDue(tc.now, tc.last, tc.schedule, tc.weekly, loc)
			if due != tc.due || late != tc.late {
				t.Fatalf("due %v late %v", due, late)
			}
		})
	}
	newYork, err := loadTimezone("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	for _, date := range []time.Time{
		time.Date(2026, 3, 8, 3, 30, 0, 0, newYork),
		time.Date(2026, 11, 1, 1, 30, 0, 0, newYork),
	} {
		clock := date.Format("15:04")
		occurrence, due, _ := reportDue(date, time.Time{}, clock, false, newYork)
		if !due || !occurrence.Equal(date) {
			t.Fatalf("DST occurrence %v wanted %v", occurrence, date)
		}
		if _, due, _ := reportDue(date.Add(time.Hour), occurrence, clock, false, newYork); due {
			t.Fatal("DST repeated a report")
		}
	}
	r := ReportsConfig{Daily: "09:00", Weekly: "monday 09:00"}
	if err := r.validate("Asia/Jakarta"); err != nil {
		t.Fatal(err)
	}
	if loc, _ := r.location("UTC+7"); loc.String() != "UTC+7" {
		t.Fatal("quiet timezone fallback")
	}
	for _, r := range []ReportsConfig{{Daily: "25:00"}, {Weekly: "freeday 09:00"}, {Timezone: "bad/zone"}} {
		if r.validate("") == nil {
			t.Fatal("accepted invalid report settings")
		}
	}
}

func TestReportGroupingDestinationsAndCommand(t *testing.T) {
	c := auditConfig(t)
	now := time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)
	c.Reports.Timezone = "UTC"
	c.reportData.Hours = make(map[string][]reportHour)
	c.Telegram.Enabled, c.Discord.Enabled, c.Slack.Enabled = yes(true), yes(true), yes(true)
	for _, name := range []string{"A", "B", "Test"} {
		cc := &ChainConfig{name: name, ChainId: "report-" + name, valInfo: &ValInfo{Moniker: "Kynraze"}}
		cc.Alerts.Telegram = TeleConfig{ApiKey: "secret", Channel: "1"}
		cc.Alerts.Discord = DiscordConfig{Webhook: "https://example.com/discord"}
		cc.Alerts.Slack = SlackConfig{Webhook: "https://example.com/slack"}
		c.Chains[name] = cc
		for i := 0; i < 48; i++ {
			c.reportData.Hours[name] = append(c.reportData.Hours[name], reportHour{At: now.Add(time.Duration(i-48) * time.Hour), Signed: 9999, Missed: 1, Watched: 3600})
		}
	}
	c.Chains["Test"].Testnet = yes(true)
	c.Chains["B"].Alerts.Telegram.Channel = "2"
	if destinations := c.reportDestinations(); len(destinations) != 4 {
		t.Fatalf("dedup: %d destinations", len(destinations))
	}
	r := c.buildReport(now, false, false, nil)
	if len(r.Rows) != 3 || !r.Rows[2].Testnet || r.Footer != "Watched 24h of 24h · UTC" {
		t.Fatalf("grouping and coverage: %+v", r)
	}
	for _, unknown := range []string{"🗳", "⬆️", "🧵"} {
		if strings.Contains(r.telegram(), unknown) {
			t.Fatalf("unknown %s included", unknown)
		}
	}
	c.Reports.IncludeTestnets = yes(false)
	if len(c.buildReport(now, true, false, nil).Rows) != 2 {
		t.Fatal("testnets not excluded")
	}
	if len(c.buildReport(now, false, false, map[string]bool{"B": true}).Rows) != 1 {
		t.Fatal("destination chains not filtered")
	}
	c.Telegram.Channel, c.Telegram.Admins = "42", []int64{7}
	if !c.commandAllowed(42, "supergroup", 7) || c.commandAllowed(42, "supergroup", 8) || c.commandAllowed(43, "private", 7) {
		t.Fatal("report permission gate")
	}
	before, _ := json.Marshal(c.savedReports(now))
	for _, args := range []string{"", "weekly"} {
		if reply := c.telegramCommand("report", args, now, tgMention{ID: 7}); !strings.Contains(reply, "report</b>") {
			t.Fatalf("command: %s", reply)
		}
	}
	after, _ := json.Marshal(c.savedReports(now))
	if string(before) != string(after) || len(c.reportData.Last) != 0 || len(c.reportData.Pending) != 0 {
		t.Fatal("report command changed persisted data or scheduling")
	}
	reportNetworks.Store("registry-test", true)
	defer reportNetworks.Delete("registry-test")
	cc := &ChainConfig{ChainId: "registry-test"}
	if !cc.reportIsTestnet() {
		t.Fatal("registry classification")
	}
	cc.Testnet = yes(false)
	if cc.reportIsTestnet() || (&ChainConfig{ChainId: "unknown"}).reportIsTestnet() {
		t.Fatal("classification override/default")
	}
}

func TestReportBuildTrends(t *testing.T) {
	c := auditConfig(t)
	now := time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)
	cc := &ChainConfig{name: "AtomOne", valInfo: &ValInfo{Moniker: "Kynraze"}}
	c.Chains[cc.name] = cc
	power, oldPower := int64(1200000), int64(1187600)
	rank, oldRank := 72, 75
	delegators, oldDelegators := uint64(812), uint64(807)
	self := float64(50000)
	c.reportData.Snapshots = map[string][]reportSnapshot{cc.name: {
		{At: now.Add(-24 * time.Hour), Power: &oldPower, Rank: &oldRank, Delegators: &oldDelegators, Self: &self},
		{At: now, Power: &power, Rank: &rank, Delegators: &delegators, Self: &self},
	}}
	for i := 0; i < 48; i++ {
		missed := int64(2)
		if i >= 24 {
			missed = 1
		}
		c.recordReportBlock(cc.name, now.Add(time.Duration(i-48)*time.Hour), StatusSigned, false)
		h := &c.reportData.Hours[cc.name][i]
		h.Signed, h.Missed, h.Watched = 10000-missed, missed, 3600
	}
	text := c.buildReport(now, false, false, nil).telegram()
	for _, want := range []string{"1.20M (+12.4K · +1.0%)", "#72 ▲3", "812 delegators (+5)", "self 50K (=)", "99.99% signed"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q:\n%s", want, text)
		}
	}
	if reportSigningTrend(reportTotals(c.reportData.Hours[cc.name], now.Add(-24*time.Hour), now), reportTotals(c.reportData.Hours[cc.name], now.Add(-48*time.Hour), now.Add(-24*time.Hour))) != " ▲0.01" {
		t.Fatal("previous period signing trend")
	}
}

func TestReportDiscordLimitsAndSlack(t *testing.T) {
	for _, weekly := range []bool{false, true} {
		r := reportFixture(weekly)
		for i := 0; i < 80; i++ {
			r.Rows = append(r.Rows, reportRow{Name: fmt.Sprintf("%d @everyone *chain*", i), Dot: dotYellow, Testnet: i%2 == 0, Lines: []string{strings.Repeat("😀*", 900)}, Fibre: []string{"known"}})
		}
		messages := r.discord()
		if len(messages) < 4 {
			t.Fatal("report wasn't split")
		}
		for _, message := range messages {
			if reportDiscordLen(message.Content) > discordContentLimit {
				t.Fatal("content limit")
			}
			total := 0
			for _, embed := range message.Embeds {
				total += reportDiscordLen(embed.Title) + reportDiscordLen(embed.Description)
				if embed.Footer != nil {
					total += reportDiscordLen(embed.Footer.Text)
				}
				if len(embed.Fields) > discordFieldsLimit {
					t.Fatal("field count limit")
				}
				for _, field := range embed.Fields {
					if reportDiscordLen(field.Name) > discordNameLimit || reportDiscordLen(field.Value) > discordValueLimit {
						t.Fatal("field size limit")
					}
					total += reportDiscordLen(field.Name) + reportDiscordLen(field.Value)
				}
			}
			if total > discordEmbedLimit {
				t.Fatalf("message embeds %d", total)
			}
		}
		parts := reportParts(r, "telegram")
		if len(parts) < 2 {
			t.Fatal("Telegram report wasn't split")
		}
		for _, part := range parts {
			var text string
			if err := json.Unmarshal(part, &text); err != nil {
				t.Fatal(err)
			}
			if len([]rune(text)) > telegramMessageLimit {
				t.Fatal("Telegram message limit")
			}
			telegramHTML(t, text)
		}
	}
	parts := reportParts(reportFixture(false), "discord")
	var payload struct {
		Content         string         `json:"content"`
		Embeds          []DiscordEmbed `json:"embeds"`
		AllowedMentions struct {
			Parse []string `json:"parse"`
		} `json:"allowed_mentions"`
	}
	if err := json.Unmarshal(parts[0], &payload); err != nil || len(payload.Embeds) != 1 || payload.Embeds[0].Title != "Mainnet" || payload.AllowedMentions.Parse == nil || len(payload.AllowedMentions.Parse) != 0 {
		t.Fatalf("Discord JSON: %s, %v", parts[0], err)
	}
	if got := reportFixture(false).slack(); len(got.Attachments) != 3 || !strings.Contains(got.Attachments[1].Fields[0].Title, "&lt;test&gt;") {
		t.Fatalf("Slack groups/escaping: %+v", got)
	}
}

func TestReportStakingProtobuf(t *testing.T) {
	page := protowire.AppendVarint(protowire.AppendTag(nil, 2, protowire.VarintType), 812)
	response := protowire.AppendBytes(protowire.AppendTag(nil, 2, protowire.BytesType), page)
	if count, err := parseReportDelegators(response); err != nil || count == nil || *count != 812 {
		t.Fatalf("delegators %v %v", count, err)
	}
	zero := protowire.AppendBytes(protowire.AppendTag(nil, 2, protowire.BytesType), nil)
	if count, err := parseReportDelegators(zero); err != nil || *count != 0 {
		t.Fatal("zero count")
	}
	coin := protowire.AppendString(protowire.AppendTag(nil, 1, protowire.BytesType), "uatone")
	coin = protowire.AppendString(protowire.AppendTag(coin, 2, protowire.BytesType), "50000")
	delegation := protowire.AppendBytes(protowire.AppendTag(nil, 2, protowire.BytesType), coin)
	response = protowire.AppendBytes(protowire.AppendTag(nil, 1, protowire.BytesType), delegation)
	if amount, err := parseReportSelf(response); err != nil || amount == nil || *amount != 50000 {
		t.Fatalf("self %v %v", amount, err)
	}
	for _, bad := range [][]byte{nil, {0xff}, {0x12, 0x03, 0xff}} {
		if value, err := parseReportSelf(bad); value != nil || err == nil {
			t.Fatal("invalid self parsed")
		}
		if value, err := parseReportDelegators(bad); value != nil || err == nil {
			t.Fatal("invalid pagination parsed")
		}
	}
}

func TestReportStateAndWatching(t *testing.T) {
	c := auditConfig(t)
	now := time.Now().UTC().Truncate(time.Hour).Add(30 * time.Minute)
	cc := &ChainConfig{name: "Chain", monitoring: true, monitoringSince: now.Add(-30 * time.Second), reportWatchAt: now.Add(-30 * time.Second)}
	c.Chains[cc.name] = cc
	cc.stateMux.Lock()
	cc.reportWatchingLocked(now)
	cc.reportWatchingLocked(now.Add(2 * time.Hour))
	cc.monitoring = false
	cc.reportWatchingLocked(now.Add(3 * time.Hour))
	cc.stateMux.Unlock()
	hours := c.reportData.Hours[cc.name]
	var watched float64
	for _, h := range hours {
		watched += h.Watched
	}
	if watched != 60 {
		t.Fatalf("silence/downtime counted: %v", watched)
	}
	c.recordReportBlock(cc.name, now, StatusMissedProposal, true)
	c.reportData.Last = map[string]time.Time{"daily": now}
	c.reportData.Pending = []reportJob{{Key: "hash", Kind: "discord", ReportKind: "daily", At: now, Parts: []json.RawMessage{json.RawMessage(`{"content":"report"}`)}}}
	state := savedState{Reports: c.savedReports(now)}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	var loaded savedState
	if err := json.Unmarshal(data, &loaded); err != nil {
		t.Fatal(err)
	}
	copy := &Config{}
	copy.restoreReports(loaded.Reports, now)
	if !copy.reportData.Last["daily"].Equal(now) || len(copy.reportData.Pending) != 1 || reportTotals(copy.reportData.Hours[cc.name], now.Add(-time.Hour), now.Add(time.Hour)).MissedProps != 1 {
		t.Fatal("report state didn't round trip")
	}
	if err := json.Unmarshal([]byte(`{"blocks":{},"alarms":null}`), &loaded); err != nil {
		t.Fatal(err)
	}
	old := &Config{}
	old.restoreReports(nil, now)
	if len(old.reportData.Hours) != 0 {
		t.Fatal("old state needs report data")
	}
	copy.reportData.Hours["old"] = []reportHour{{At: now.Add(-31 * 24 * time.Hour)}}
	copy.savedReports(now)
	if len(copy.reportData.Hours["old"]) != 0 {
		t.Fatal("old hours not trimmed")
	}
}

func TestReportQuietRecoveryAndBound(t *testing.T) {
	c := auditConfig(t)
	cc := &ChainConfig{name: "Chain", ChainId: "chain-1"}
	c.Chains[cc.name] = cc
	c.QuietHours = QuietHoursConfig{Enabled: true, Start: "00:00", End: "23:59", Timezone: "UTC", MinSeverity: "critical"}
	if err := c.QuietHours.parse(); err != nil {
		t.Fatal(err)
	}
	message := "Kynraze has missed 5 blocks on chain-1"
	c.alert(cc.name, message, "warning", false, nil)
	c.alert(cc.name, message, "warning", false, nil)
	c.resolveQuietly(cc.name, message, nil)
	if len(c.reportData.Incidents) != 1 || c.reportData.Incidents[0].End.IsZero() || c.reportData.Incidents[0].Kind != "Missed blocks" {
		t.Fatalf("quiet history: %+v", c.reportData.Incidents)
	}
	c.reportsMux.Lock()
	for i := 0; i < reportIncidentLimit+3; i++ {
		c.reportData.Incidents = append(c.reportData.Incidents, reportIncident{Chain: cc.name, Start: time.Now()})
	}
	c.trimReportsLocked(time.Now())
	c.reportsMux.Unlock()
	if len(c.reportData.Incidents) != reportIncidentLimit {
		t.Fatal("incident history unbounded")
	}
}

func TestReportVotesSurviveRestartAndJobIdentity(t *testing.T) {
	c := &Config{}
	now := time.Now()
	open := govProposal{ID: 41, VotingEnd: now.Add(time.Hour)}
	c.recordReportVotes("Chain", nil, []govProposal{open}, now)
	copy := &Config{}
	copy.restoreReports(c.savedReports(now), now)
	open.Voted = true
	copy.recordReportVotes("Chain", nil, []govProposal{open}, now)
	copy.recordReportVotes("Chain", nil, []govProposal{open}, now)
	if reportTotals(copy.reportData.Hours["Chain"], now.Add(-time.Hour), now.Add(time.Hour)).Votes != 1 {
		t.Fatal("vote missed or counted twice across restart")
	}
	daily := reportJob{Key: "same", Kind: "discord", ReportKind: "daily", At: now}
	weekly := daily
	weekly.ReportKind = "weekly"
	copy.reportData.Pending = []reportJob{daily, weekly}
	copy.finishReportJob(weekly, true)
	if len(copy.reportData.Pending) != 1 || copy.reportData.Pending[0].ReportKind != "daily" {
		t.Fatal("daily/weekly jobs collided")
	}
}

func TestReportFibreUsesPersistedPeriodHistograms(t *testing.T) {
	c := auditConfig(t)
	now := time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)
	st := c.fibreData()
	st.now = func() time.Time { return now }
	st.hours["Chain"] = make(map[int64]*fibreHour)
	for i := 1; i <= 2; i++ {
		at := now.Add(-time.Duration(i) * time.Hour).Unix()
		buckets := []uint64{50, 0, 0}
		if i == 2 {
			buckets = []uint64{0, 50, 0}
		}
		st.hours["Chain"][at] = &fibreHour{
			Time: at, Exports: 1, Checks: 10, Reachable: 9,
			Counts: map[string]float64{"uploads_ok": 10, "uploads_failed": 0, "signatures_ok": 50, "signatures_fast": 50},
			Extra: &fibreExtra{Histograms: map[string]fibreHistogram{
				"sign":   {Bounds: []float64{.001, .01}, Buckets: buckets, Count: 50},
				"upload": {Bounds: []float64{.01, .1}, Buckets: []uint64{0, 50, 0}, Count: 50},
			}},
		}
	}
	lines := strings.Join(c.reportFibre("Chain", now.Add(-24*time.Hour), now, false), "\n")
	for _, want := range []string{"20 shards stored", "0 failed", "100% under 10 ms", "p95 9.1 ms", "90.0% reachable"} {
		if !strings.Contains(lines, want) {
			t.Fatalf("missing %s: %s", want, lines)
		}
	}
	copy := &Config{}
	copy.fibreData().now = func() time.Time { return now }
	copy.fibreData().restoreHours(st.savedHours())
	if got := strings.Join(copy.reportFibre("Chain", now.Add(-24*time.Hour), now, false), "\n"); got != lines {
		t.Fatalf("Fibre changed on restart: %s", got)
	}
}

func TestReportQueuedDeliveryRetry(t *testing.T) {
	c := auditConfig(t)
	var requests atomic.Int64
	var fail atomic.Bool
	fail.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if fail.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		var payload map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	c.Discord.Enabled = yes(true)
	cc := &ChainConfig{name: "Chain"}
	cc.Alerts.Discord.Webhook = server.URL
	c.Chains[cc.name] = cc
	d := c.reportDestinations()[0]
	now := time.Now()
	c.reportData.Pending = []reportJob{{Key: d.Key, Kind: d.Kind, ReportKind: "daily", At: now, Parts: []json.RawMessage{json.RawMessage(`{"content":"report","allowed_mentions":{"parse":[]}}`)}}}
	c.deliverReports()
	if requests.Load() != 1 || len(c.reportData.Pending) != 1 || c.reportData.Pending[0].Next != 0 {
		t.Fatal("failed report not kept for retry")
	}
	copy := &Config{Chains: c.Chains, Discord: c.Discord}
	copy.restoreReports(c.savedReports(now), now)
	fail.Store(false)
	copy.deliverReports()
	if requests.Load() != 2 || len(copy.reportData.Pending) != 0 {
		t.Fatal("queued report lost on restart or not acknowledged")
	}
}

func TestReportReloadAndSkippedOccurrence(t *testing.T) {
	c := auditConfig(t)
	cc := &ChainConfig{name: "Chain", ChainId: "chain-1"}
	c.Chains[cc.name] = cc
	next := &Config{Chains: map[string]*ChainConfig{cc.name: {name: cc.name, ChainId: cc.ChainId, Testnet: yes(true)}}, Reports: ReportsConfig{Daily: "09:00", Weekly: "monday 09:00", Timezone: "UTC+7", IncludeTestnets: yes(false)}}
	diff := c.applyReload(next)
	if len(diff.Updated) != 1 || c.settings().Reports.Daily != "09:00" || !cc.reportIsTestnet() {
		t.Fatal("reports/testnet settings didn't reload")
	}
	now := time.Date(2026, 10, 5, 18, 0, 0, 0, time.FixedZone("UTC+7", 7*3600))
	c.reportTick(now)
	want := time.Date(2026, 10, 5, 9, 0, 0, 0, now.Location())
	if !c.reportData.Last["daily"].Equal(want) || !c.reportData.Last["weekly"].Equal(want) || len(c.reportData.Pending) != 0 {
		t.Fatal("old schedule occurrence wasn't marked skipped")
	}
	copy := &Config{Reports: next.Reports}
	copy.restoreReports(c.savedReports(now), now)
	copy.reportTick(now)
	if len(copy.reportData.Pending) != 0 || !copy.reportData.Last["daily"].Equal(want) {
		t.Fatal("restart changed skipped occurrence")
	}
}

func TestReportHeaderCountsIncidentsApart(t *testing.T) {
	c := auditConfig(t)
	now := time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)
	for _, name := range []string{"Calm", "Eventful", "Scheduled"} {
		c.Chains[name] = &ChainConfig{name: name, valInfo: &ValInfo{Moniker: "Kynraze", Bonded: true}}
	}
	c.reportData.Incidents = []reportIncident{{Chain: "Eventful", Key: "missed blocks", Kind: "missed", Start: now.Add(-2 * time.Hour), End: now.Add(-time.Hour)}}
	// a scheduled upgrade is a notice, it does not make the row red
	alarms.AllAlarms["Scheduled"] = map[string]time.Time{"Upgrade v4.0.0 at height 1000": now}
	report := c.buildReport(now, false, false, nil)
	if got := report.Header[1]; got != "🟢 2 healthy · 🟡 1 had incidents" {
		t.Fatalf("header %q", got)
	}
}

func TestReportSkipsAnnouncements(t *testing.T) {
	now := time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)
	history := []reportIncident{
		{Chain: "Chain", Key: "Proposal #12 is now in voting period\nCommunity pool spend", Kind: "New proposal", Start: now.Add(-4 * time.Hour), End: now.Add(-time.Hour)},
		{Chain: "Chain", Key: "Upgrade v5 at height 1,000", Kind: "Upgrade", Start: now.Add(-3 * time.Hour), End: now.Add(-time.Hour)},
		{Chain: "Chain", Key: "validator is missing blocks on chain-1", Kind: "Missed blocks", Start: now.Add(-2 * time.Hour), End: now.Add(-time.Hour)},
	}
	lines, count, longest := reportIncidents(history, "Chain", now.Add(-24*time.Hour), now)
	if count != 1 || len(lines) != 1 || !strings.HasPrefix(lines[0], "Missed blocks") {
		t.Fatalf("announcements counted: count %d, lines %v", count, lines)
	}
	if longest != time.Hour {
		t.Fatalf("announcements affected longest incident: %s", longest)
	}
}

func TestReportDurationUnderAMinute(t *testing.T) {
	for d, want := range map[time.Duration]string{0: "0m", 30 * time.Second: "<1m", time.Minute: "1m", 90 * time.Minute: "1h 30m"} {
		if got := reportDuration(d); got != want {
			t.Errorf("reportDuration(%s) = %q, want %q", d, got, want)
		}
	}
}
