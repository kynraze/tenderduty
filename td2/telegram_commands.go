package tenderduty

import (
	"fmt"
	"html"
	"net/http"
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
	if strconv.FormatInt(chatId, 10) != strings.TrimSpace(c.Telegram.Channel) {
		return false
	}
	if chatType == "private" {
		return true
	}
	for _, admin := range c.Telegram.Admins {
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

// telegramCommand answers one command with telegram HTML. Commands only read, nothing can be changed from telegram.
func (c *Config) telegramCommand(command, args string, now time.Time) string {
	if command != "status" {
		return "Commands: /status for every chain, /status &lt;chain&gt; for one"
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

// statusDetail is everything about one chain.
func (cc *ChainConfig) statusDetail(name string, now time.Time) string {
	info, _ := cc.validatorState()
	cc.stateMux.RLock()
	height, lastBlock := cc.lastBlockNum, cc.lastBlockTime
	cc.stateMux.RUnlock()
	dot, label := cc.chainState(name, info)
	lines := []string{
		fmt.Sprintf("%s <b>%s</b> · %s", dot, html.EscapeString(name), label),
		"",
		fmt.Sprintf("%s on %s", html.EscapeString(info.Moniker), html.EscapeString(cc.ChainId)),
	}
	if height > 0 {
		block := "Height " + thousands(height)
		if !lastBlock.IsZero() {
			block += " · last block " + shortDuration(now.Sub(lastBlock)) + " ago"
		}
		lines = append(lines, block)
	}
	if info.Window > 0 {
		missed := fmt.Sprintf("Missed %s / %s", thousands(info.Missed), thousands(info.Window))
		if info.MaxMissed > 0 {
			missed += " · " + thousands(info.missesBeforeJail()) + " left before jail"
		}
		lines = append(lines, missed)
	}
	if info.VotingPower > 0 {
		power := "Voting power " + thousands(info.VotingPower)
		if info.TotalPower > 0 {
			power += fmt.Sprintf(" · %.2f%%", 100*float64(info.VotingPower)/float64(info.TotalPower))
		}
		lines = append(lines, power+fmt.Sprintf(" · rank %d/%d", info.Rank, info.ActiveSet))
	}
	healthyNodes := 0
	for _, node := range cc.Nodes {
		if !node.snapshot().down {
			healthyNodes++
		}
	}
	lines = append(lines, fmt.Sprintf("RPC %d/%d healthy", healthyNodes, len(cc.Nodes)))

	alarms.notifyMux.RLock()
	active := make([]string, 0, len(alarms.AllAlarms[name]))
	for message := range alarms.AllAlarms[name] {
		first, _, _ := strings.Cut((&alertMsg{message: message}).summary(), "\n")
		active = append(active, "• "+html.EscapeString(first))
	}
	alarms.notifyMux.RUnlock()
	if len(active) > 0 {
		sort.Strings(active)
		lines = append(lines, "", "<b>Active alerts</b>")
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
	client := notificationClient{ctx: c.ctx, client: &http.Client{Timeout: 60 * time.Second}}
	var bot *tgbotapi.BotAPI
	for bot == nil {
		var err error
		if bot, err = tgbotapi.NewBotAPIWithClient(c.Telegram.ApiKey, tgbotapi.APIEndpoint, client); err != nil {
			l("telegram commands:", err)
			if !waitContext(c.ctx, time.Minute) {
				return
			}
		}
	}
	l("📨 reading commands sent to the telegram bot")
	offset := 0
	for c.ctx.Err() == nil {
		updates, err := bot.GetUpdates(tgbotapi.UpdateConfig{Offset: offset, Timeout: 30, AllowedUpdates: []string{"message", "channel_post"}})
		if err != nil {
			if !waitContext(c.ctx, 10*time.Second) {
				return
			}
			continue
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
			fromId := int64(0)
			if message.From != nil {
				fromId = message.From.ID
			}
			if !c.commandAllowed(message.Chat.ID, message.Chat.Type, fromId) {
				continue
			}
			for _, part := range splitMessage(c.telegramCommand(message.Command(), message.CommandArguments(), time.Now())) {
				reply := tgbotapi.NewMessage(message.Chat.ID, part)
				reply.ParseMode = tgbotapi.ModeHTML
				if _, err := bot.Send(reply); err != nil {
					l("telegram command reply:", err)
				}
			}
		}
	}
}
