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

// telegram's limit is 4096 characters a message, longer replies are split on lines
const telegramMessageLimit = 4000

// commandAllowed says if a message may command tenderduty: it has to come from the configured chat, and in a group or
// channel from one of the admins.
func (c *Config) commandAllowed(chatId int64, chatType string, fromId int64) bool {
	telegram := c.settings().Telegram
	if strconv.FormatInt(chatId, 10) != strings.TrimSpace(telegram.Channel) {
		return false
	}
	if chatType == "private" {
		return true
	}
	for _, admin := range telegram.Admins {
		if admin == fromId && fromId != 0 {
			return true
		}
	}
	return false
}

// chainNamed finds a chain by its name or chain id, ignoring case.
func (c *Config) chainNamed(name string) (string, *ChainConfig) {
	c.chainsMux.RLock()
	defer c.chainsMux.RUnlock()
	for chainName, chain := range c.Chains {
		if strings.EqualFold(chainName, name) || strings.EqualFold(chain.ChainId, name) {
			return chainName, chain
		}
	}
	return "", nil
}

// telegramCommand answers one command with telegram HTML. The only thing it changes is who gets pinged, by the person
// asking for themselves, so nothing about the monitoring can be touched from telegram.
func (c *Config) telegramCommand(command, args string, now time.Time, from tgMention) string {
	switch command {
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
	case "status":
	default:
		return "Commands: /status for every chain, /status &lt;chain&gt; for one, /mentionme to be pinged in alerts"
	}
	if name := strings.TrimSpace(args); name != "" {
		chainName, cc := c.chainNamed(name)
		if cc == nil {
			return "No chain named " + html.EscapeString(name)
		}
		return cc.statusDetail(chainName, now)
	}
	return c.statusSummary()
}

// chainState is the dot in front of a chain: red with active alerts or jailed, yellow when it can't be watched,
// white when not in the active set, green otherwise.
func (cc *ChainConfig) chainState(name string, info *ValInfo) (dot, label string) {
	switch {
	case info.Tombstoned:
		return "🔴", "tombstoned"
	case info.Jailed:
		return "🔴", "jailed"
	case alarms.getCount(name) > 0:
		return "🔴", plural(alarms.getCount(name), "active alert")
	case cc.hasNoNodes():
		return "🟡", "no RPC node working"
	case !cc.isMonitoring():
		return "🟡", "not watching blocks"
	case !info.Bonded:
		return "⚪", "not in the active set"
	default:
		return "🟢", "healthy"
	}
}

// statusSummary is a line per chain, problems first.
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
	healthy := 0
	for _, name := range names {
		c.chainsMux.RLock()
		cc := c.Chains[name]
		c.chainsMux.RUnlock()
		if cc == nil {
			continue
		}
		info, _ := cc.validatorState()
		dot, label := cc.chainState(name, info)
		rank := map[string]int{"🔴": 0, "🟡": 1, "⚪": 2, "🟢": 3}[dot]
		if dot == "🟢" {
			healthy++
		}
		line := fmt.Sprintf("%s <b>%s</b> · %s", dot, html.EscapeString(name), label)
		if info.Window > 0 {
			line += fmt.Sprintf(" · missed %s/%s", thousands(info.Missed), thousands(info.Window))
		}
		rows = append(rows, row{rank, line})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		return rows[i].rank < rows[j].rank || (rows[i].rank == rows[j].rank && rows[i].line < rows[j].line)
	})
	lines := []string{fmt.Sprintf("📊 <b>Status</b> · %d of %s healthy", healthy, plural(len(names), "chain")), ""}
	for _, r := range rows {
		lines = append(lines, r.line)
	}
	return strings.Join(lines, "\n")
}

// statusField is a labelled line of /status <chain>: an icon, the label in bold, and the value.
func statusField(icon, label, value string) string {
	return icon + " <b>" + label + ":</b> " + value
}

// statusDetail is everything about one chain, in sections: the validator, its Fibre when watched, the active alerts.
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
	if info.VotingPower > 0 {
		power := thousands(info.VotingPower)
		if info.TotalPower > 0 {
			power += " (" + html.EscapeString(percent(info.VotingPower, info.TotalPower)) + ")"
		}
		lines = append(lines, statusField("⚖️", "Voting power", power+fmt.Sprintf(" · #%d/%d", info.Rank, info.ActiveSet)))
	}
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
	if fibre := cc.fibreReport(now); fibre != nil {
		lines = append(append(lines, ""), fibre.telegramLines()...)
	}

	alarms.notifyMux.RLock()
	active := make([]string, 0, len(alarms.AllAlarms[name]))
	for message := range alarms.AllAlarms[name] {
		first, _, _ := strings.Cut((&alertMsg{message: message}).summary(), "\n")
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

// splitMessage cuts a reply into telegram sized messages on line breaks.
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

// telegramCommands reads commands sent to the bot by long polling, no port needs to be open. The bot can't be read by
// anything else at the same time, telegram only allows one reader.
func (c *Config) telegramCommands() {
	c.telegramCommandsContext(c.context())
}

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
