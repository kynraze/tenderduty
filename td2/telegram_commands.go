package tenderduty

import (
	"context"
	"fmt"
	"html"
	"sort"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// split replies on lines to fit Telegram's 4096-character limit
const telegramMessageLimit = 4000

// commandAllowed: the chat must be the configured one, and in a group the sender an admin.
// Only a channel's admins can post in it, and Telegram doesn't say which one.
func (c *Config) commandAllowed(chatId int64, chatType string, fromId int64) bool {
	telegram := c.settings().Telegram
	if strconv.FormatInt(chatId, 10) != strings.TrimSpace(telegram.Channel) {
		return false
	}
	if chatType == "private" || chatType == "channel" {
		return true
	}
	for _, admin := range telegram.Admins {
		if admin == fromId && fromId != 0 {
			return true
		}
	}
	return false
}

func (c *Config) chainNamed(name string) (string, *ChainConfig) {
	c.chainsMux.RLock()
	defer c.chainsMux.RUnlock()
	if chain := c.Chains[name]; chain != nil {
		return name, chain
	}
	names := sortedReportNames(c.Chains)
	for _, chainName := range names {
		if strings.EqualFold(chainName, name) {
			return chainName, c.Chains[chainName]
		}
	}
	matches := make([]string, 0)
	for _, chainName := range names {
		if strings.EqualFold(c.Chains[chainName].ChainId, name) {
			matches = append(matches, chainName)
		}
	}
	if len(matches) == 0 {
		for _, chainName := range names {
			if strings.HasPrefix(strings.ToLower(chainName), strings.ToLower(name)) {
				matches = append(matches, chainName)
			}
		}
	}
	if len(matches) == 1 {
		return matches[0], c.Chains[matches[0]]
	}
	if len(matches) > 1 {
		return fmt.Sprintf("%q matches %s; be more specific", name, strings.Join(matches, ", ")), nil
	}
	return "", nil
}

// telegramCommand answers HTML commands and updates mentions only.
func (c *Config) telegramCommand(command, args string, now time.Time, from tgMention) string {
	switch strings.ToLower(command) {
	case "mentionme":
		if strings.EqualFold(strings.TrimSpace(args), "off") {
			if !removeMention(from.ID) {
				return "You were not on the ping list"
			}
			c.saveMentions()
			return "🔕 You won't be pinged in alerts any more"
		}
		if from.ID == 0 {
			return "Telegram didn't say who sent this, so there is nobody to ping"
		}
		addMention(from.ID, from.Name)
		c.saveMentions()
		return fmt.Sprintf("🔔 %s will be pinged in alerts. Send /mentionme off to stop.", from.link())
	case "report":
		weekly := strings.EqualFold(strings.TrimSpace(args), "weekly")
		if strings.TrimSpace(args) != "" && !weekly {
			return "Use /report or /report weekly"
		}
		return c.buildReport(now, weekly, false, nil).telegram()
	case "status":
	default:
		return "Commands: /report or /report weekly, /status for every chain, /status &lt;chain&gt; for one, /mentionme to be pinged in alerts"
	}
	if name := strings.TrimSpace(args); name != "" {
		chainName, cc := c.chainNamed(name)
		if cc == nil {
			if chainName != "" {
				return html.EscapeString(chainName)
			}
			return "No chain named " + html.EscapeString(name)
		}
		return cc.statusDetail(chainName, now)
	}
	return c.statusSummary()
}

// chainState returns a status dot and label.
func (cc *ChainConfig) chainState(name string, info *ValInfo) (dot, label string) {
	switch {
	case info.Tombstoned:
		return "🔴", "tombstoned"
	case info.Jailed:
		return "🔴", "jailed"
	case cc.urgentAlerts() > 0:
		return "🔴", plural(cc.urgentAlerts(), "active alert")
	case cc.hasNoNodes():
		return "🟡", "no RPC node working"
	case !cc.isMonitoring():
		return "🟡", "not watching blocks"
	case info.Moniker == "not connected":
		return "🟡", "no validator data"
	case !info.Bonded:
		return "⚪", "not in the active set"
	}
	if _, notices, label := cc.dashboardAlerts(); notices > 0 {
		return "🔵", strings.ToLower(label)
	}
	return "🟢", "healthy"
}

func (c *Config) statusSummary() string {
	c.chainsMux.RLock()
	names := make([]string, 0, len(c.Chains))
	for name := range c.Chains {
		names = append(names, name)
	}
	c.chainsMux.RUnlock()
	type row struct {
		rank int
		line string
	}
	rows := make([]row, 0, len(names))
	healthy, inactive := 0, 0
	for _, name := range names {
		c.chainsMux.RLock()
		cc := c.Chains[name]
		c.chainsMux.RUnlock()
		if cc == nil {
			continue
		}
		info, _ := cc.validatorState()
		dot, label := cc.chainState(name, info)
		rank := map[string]int{"🔴": 0, "🟡": 1, "🔵": 2, "⚪": 3, "🟢": 4}[dot]
		// as in the report: notices are healthy, out of the active set is counted apart
		switch dot {
		case "🟢", "🔵":
			healthy++
		case "⚪":
			inactive++
		}
		line := fmt.Sprintf("%s <b>%s</b> · %s", dot, html.EscapeString(name), label)
		if info.Window > 0 {
			line += fmt.Sprintf(" · missed %s/%s", thousands(info.Missed), thousands(info.Window))
		}
		line += cc.bridgeSummary()
		rows = append(rows, row{rank, line})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		return rows[i].rank < rows[j].rank || (rows[i].rank == rows[j].rank && rows[i].line < rows[j].line)
	})
	header := fmt.Sprintf("📊 <b>Status</b> · %d of %s healthy", healthy, plural(len(names), "chain"))
	if inactive > 0 {
		header += fmt.Sprintf(" · ⚪ %d inactive", inactive)
	}
	if rest := len(rows) - healthy - inactive; rest > 0 {
		header += fmt.Sprintf(" · 🔴 %d needs attention", rest)
	}
	lines := []string{header, ""}
	for _, r := range rows {
		lines = append(lines, r.line)
	}
	return strings.Join(lines, "\n")
}

func statusField(icon, label, value string) string {
	return icon + " <b>" + label + ":</b> " + value
}

// With every own node down but blocks still arriving, a public node is serving the chain.
func (cc *ChainConfig) fallbackStatus(healthyNodes int) string {
	if !cc.publicFallbackEnabled() {
		return "⚪ off"
	}
	cc.stateMux.RLock()
	noNodes, public := cc.noNodes, cc.publicNode
	cc.stateMux.RUnlock()
	switch {
	case healthyNodes > 0 && len(cc.Nodes) > 0:
		return "🟢 on, standby"
	case noNodes:
		return "🔴 on, no public node working"
	case public != "" && !td.HideLogs:
		return "🟡 in use · <code>" + html.EscapeString(nodeEndpoint(public)) + "</code>"
	default:
		return "🟡 in use"
	}
}

// statusDetail shows validator, bridge, Fibre and active alerts.
func (cc *ChainConfig) statusDetail(name string, now time.Time) string {
	info, _ := cc.validatorState()
	cc.stateMux.RLock()
	height, lastBlock := cc.lastBlockNum, cc.lastBlockTime
	cc.stateMux.RUnlock()
	dot, label := cc.chainState(name, info)
	lines := []string{
		fmt.Sprintf("%s <b>%s</b> · %s", dot, html.EscapeString(name), label),
		fmt.Sprintf("%s on <code>%s</code>", html.EscapeString(info.Moniker), html.EscapeString(cc.ChainId)),
		"",
		"⛓ <b>Validator</b>",
	}
	if height > 0 {
		block := thousands(height)
		if !lastBlock.IsZero() {
			block += " · last block " + shortDuration(now.Sub(lastBlock)) + " ago"
		}
		lines = append(lines, statusField("📦", "Height", block))
	}
	if info.Window > 0 {
		missed := fmt.Sprintf("%s / %s", thousands(info.Missed), thousands(info.Window))
		if info.MaxMissed > 0 {
			missed += " (" + thousands(info.missesBeforeJail()) + " to jail)"
		}
		lines = append(lines, statusField("🎯", "Missed", missed))
	}
	if info.VotingPower > 0 || info.stakeDisplay() != "" {
		power := thousands(info.VotingPower)
		if stake := info.stakeDisplay(); stake != "" {
			power = html.EscapeString(stake)
		}
		if info.TotalPower > 0 {
			power += " (" + html.EscapeString(percent(info.VotingPower, info.TotalPower)) + ")"
		}
		if info.Rank > 0 {
			power += fmt.Sprintf(" · #%d/%d", info.Rank, info.ActiveSet)
		}
		lines = append(lines, statusField("⚖️", "Voting power", power))
	}
	lines = append(lines, cc.statusReportLines(name, now)...)
	healthyNodes := 0
	for _, node := range cc.Nodes {
		if !node.snapshot().down {
			healthyNodes++
		}
	}
	rpc := "🟢"
	switch {
	case healthyNodes == 0:
		rpc = "🔴"
	case healthyNodes < len(cc.Nodes):
		rpc = "🟡"
	}
	lines = append(lines, statusField("🔌", "RPC", fmt.Sprintf("%s %d/%d healthy", rpc, healthyNodes, len(cc.Nodes))))
	lines = append(lines, statusField("🛟", "Public fallback", cc.fallbackStatus(healthyNodes)))
	for i, node := range cc.Nodes {
		status := node.snapshot()
		text := nodeEndpoint(node.Url)
		label := "Node"
		if td.HideLogs {
			text = ""
			label = fmt.Sprintf("Node %d", i+1)
		}
		// hide_logs also hides versions: a public channel shouldn't learn which node is unpatched
		if node.AlertIfDown && !td.HideLogs {
			if text != "" {
				text += " · "
			}
			text += nodeBinaryLabel(status)
		}
		if status.down {
			if text != "" {
				text += " · "
			}
			text += "down"
			if td.HideLogs && !status.downSince.IsZero() {
				text += " " + shortDuration(now.Sub(status.downSince))
			}
		} else {
			if text != "" {
				text += " · "
			}
			text += "healthy"
		}
		lines = append(lines, statusField("🔌", label, html.EscapeString(text)))
	}
	if title, facts := cc.bridgeFacts(now); title != "" {
		head := "\U0001f309 <b>Bridge</b>"
		if version := strings.TrimPrefix(title, "Bridge"); version != "" {
			head += " <code>" + html.EscapeString(strings.TrimSpace(version)) + "</code>"
		}
		lines = append(lines, "", head)
		for _, fact := range facts {
			value := html.EscapeString(fact.Value)
			switch fact.Tone {
			case "critical":
				value = "\U0001f534 " + value
			case "warning":
				value = "\U0001f7e1 " + value
			}
			lines = append(lines, statusField(fact.Icon, fact.Label, value))
		}
	}
	if fibre := cc.fibreReport(now); fibre != nil {
		lines = append(append(lines, ""), fibre.telegramLines()...)
	}

	alarms.notifyMux.RLock()
	active := make([]string, 0, len(alarms.AllAlarms[name]))
	for message := range alarms.AllAlarms[name] {
		msg := &alertMsg{chain: name, message: message}
		summary := msg.summary()
		// hide_logs: name nodes as the Nodes lines above do
		if nodeURL, _, ok := nodeDownParts(message); ok && td.HideLogs {
			label := "node"
			for i, node := range cc.Nodes {
				if node.Url == nodeURL {
					label = fmt.Sprintf("Node %d", i+1)
				}
			}
			summary = strings.ReplaceAll(msg.rawSummary(), nodeURL, label)
		}
		first, _, _ := strings.Cut(summary, "\n")
		active = append(active, "• "+html.EscapeString(first))
	}
	alarms.notifyMux.RUnlock()
	if len(active) > 0 {
		sort.Strings(active)
		lines = append(lines, "", "🚨 <b>Active alerts</b>")
		lines = append(lines, active...)
	}
	return strings.Join(lines, "\n")
}

func splitMessage(text string) []string {
	parts := make([]string, 0, 1)
	current := ""
	for _, line := range strings.Split(text, "\n") {
		if current != "" && len(current)+len(line)+1 > telegramMessageLimit {
			parts = append(parts, current)
			current = ""
		}
		if current != "" {
			current += "\n"
		}
		current += line
	}
	return append(parts, current)
}

// telegramCommandsContext long polls for commands, so no port needs to be open. Telegram allows one reader at a time.
func (c *Config) telegramCommandsContext(ctx context.Context) {
	client := newNotificationClient(ctx, 60*time.Second)
	var bot *tgbotapi.BotAPI
	for bot == nil {
		var err error
		if bot, err = tgbotapi.NewBotAPIWithClient(c.settings().Telegram.ApiKey, tgbotapi.APIEndpoint, client); err != nil {
			l("telegram commands:", err)
			if !waitContext(ctx, time.Minute) {
				return
			}
		}
	}
	l("📨 reading commands sent to the telegram bot")
	offset := 0
	for ctx.Err() == nil {
		updates, err := bot.GetUpdates(tgbotapi.UpdateConfig{Offset: offset, Timeout: 30, AllowedUpdates: []string{"message", "channel_post"}})
		if err != nil {
			if !waitContext(ctx, 10*time.Second) {
				return
			}
			continue
		}
		if ctx.Err() != nil {
			return
		}
		for _, update := range updates {
			offset = update.UpdateID + 1
			message := update.Message
			if message == nil {
				message = update.ChannelPost
			}
			if message == nil || !message.IsCommand() || message.Chat == nil {
				continue
			}
			from := tgMention{}
			if message.From != nil {
				from.ID, from.Name = message.From.ID, strings.TrimSpace(message.From.FirstName+" "+message.From.LastName)
				if from.Name == "" {
					from.Name = message.From.UserName
				}
			}
			if !c.commandAllowed(message.Chat.ID, message.Chat.Type, from.ID) {
				if strconv.FormatInt(message.Chat.ID, 10) == strings.TrimSpace(c.settings().Telegram.Channel) {
					l(fmt.Sprintf("telegram: /%s from user id %d ignored, not in telegram.admins", message.Command(), from.ID))
				}
				continue
			}
			for _, part := range splitMessage(c.telegramCommand(message.Command(), message.CommandArguments(), time.Now(), from)) {
				reply := tgbotapi.NewMessage(message.Chat.ID, part)
				reply.ParseMode = tgbotapi.ModeHTML
				if _, err := bot.Send(reply); err != nil {
					l("telegram command reply:", err)
				}
			}
		}
	}
}

func (cc *ChainConfig) statusReportLines(name string, now time.Time) []string {
	lines := make([]string, 0)
	history := td.savedReports(now)
	var snapshot *reportSnapshot
	if snapshots := history.Snapshots[name]; len(snapshots) > 0 {
		snapshot = &snapshots[len(snapshots)-1]
	}
	if snapshot != nil && snapshot.Delegators != nil {
		value := strconv.FormatUint(*snapshot.Delegators, 10)
		if snapshot.Self != nil {
			value += " · self " + compactReportNumber(*snapshot.Self)
			if snapshot.Symbol != "" {
				value += " " + snapshot.Symbol
			}
		}
		lines = append(lines, statusField("🤝", "Delegators", html.EscapeString(value)))
	}
	total := reportTotals(history.Hours[name], now.Add(-24*time.Hour), now)
	if signed, known := reportSigned(total); known {
		value := fmt.Sprintf("%.2f%% · proposed %d/%d", signed, total.Proposed, total.Proposed+total.MissedProps)
		lines = append(lines, statusField("✍️", "Signing 24h", value))
	}
	if cc.alertSettings().GovAlerts {
		proposals, loaded := cc.govSnapshot()
		open, unvoted := 0, 0
		pending := make([]string, 0, 2)
		sort.Slice(proposals, func(i, j int) bool { return proposals[i].VotingEnd.Before(proposals[j].VotingEnd) })
		if loaded {
			for _, p := range proposals {
				if !p.VotingEnd.After(now) {
					continue
				}
				open++
				if !p.Voted {
					unvoted++
					if len(pending) < 2 {
						pending = append(pending, fmt.Sprintf("#%d, ends %s", p.ID, reportDuration(p.VotingEnd.Sub(now))))
					}
				}
			}
		}
		if open > 0 {
			value := fmt.Sprintf("%d open · %d not voted", open, unvoted)
			if len(pending) > 0 {
				value += " (" + strings.Join(pending, "; ") + ")"
			}
			if unvoted > len(pending) {
				value += fmt.Sprintf(" +%d more", unvoted-len(pending))
			}
			lines = append(lines, statusField("🗳", "Governance", html.EscapeString(value)))
		}
	}
	return lines
}
