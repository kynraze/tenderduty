package tenderduty

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf16"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

type reportDestination struct {
	Kind    string
	Key     string
	Token   string
	Address string
	Chains  map[string]bool
}

type reportJob struct {
	Key        string            `json:"key"`
	ReportKind string            `json:"report_kind"`
	Kind       string            `json:"kind"`
	At         time.Time         `json:"at"`
	Parts      []json.RawMessage `json:"parts"`
	Next       int               `json:"next,omitempty"`
	Late       bool              `json:"late,omitempty"`
}

func reportDestinationKey(kind, token, address string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(kind+"\x00"+token+"\x00"+address)))
}

func (c *Config) reportDestinations() []reportDestination {
	settings := c.settings()
	c.chainsMux.RLock()
	defer c.chainsMux.RUnlock()
	byKey := make(map[string]*reportDestination)
	for _, name := range sortedReportNames(c.Chains) {
		cc := c.Chains[name]
		if cc.reportIsTestnet() && settings.Reports.IncludeTestnets != nil && !*settings.Reports.IncludeTestnets {
			continue
		}
		a := cc.alertSettings()
		for _, destination := range []struct {
			kind, token, address string
			enabled              bool
		}{
			{"telegram", a.Telegram.ApiKey, a.Telegram.Channel, enabledFor(settings.Telegram.Enabled, a.Telegram.Enabled)},
			{"discord", "", a.Discord.Webhook, enabledFor(settings.Discord.Enabled, a.Discord.Enabled)},
			{"slack", "", a.Slack.Webhook, enabledFor(settings.Slack.Enabled, a.Slack.Enabled)},
		} {
			if !destination.enabled || destination.address == "" || (destination.kind == "telegram" && destination.token == "") {
				continue
			}
			key := reportDestinationKey(destination.kind, destination.token, destination.address)
			if byKey[key] == nil {
				byKey[key] = &reportDestination{Kind: destination.kind, Key: key, Token: destination.token, Address: destination.address, Chains: make(map[string]bool)}
			}
			byKey[key].Chains[name] = true
		}
	}
	result := make([]reportDestination, 0, len(byKey))
	for _, d := range byKey {
		result = append(result, *d)
	}
	return result
}

func reportMarkdown(s string) string {
	return strings.NewReplacer("\\", "\\\\", "*", "\\*", "_", "\\_", "~", "\\~", "`", "\\`", "|", "\\|", "[", "\\[", "]", "\\]", ">", "\\>", "@", "@\u200b").Replace(s)
}

func reportSlackText(s string) string {
	return strings.NewReplacer("*", "∗", "_", "＿", "~", "～", "`", "'", "@", "@\u200b").Replace(slackText(s))
}

func reportColor(dot string) uint {
	return map[string]uint{dotGreen: 0x63d6c4, dotYellow: 0xf1b663, dotRed: 0xf27c76}[dot]
}

func worstReportDot(rows []reportRow, testnet bool) string {
	dot := dotGreen
	for _, row := range rows {
		if row.Testnet != testnet {
			continue
		}
		if row.Dot == dotRed {
			return dotRed
		}
		if row.Dot == dotYellow {
			dot = dotYellow
		}
	}
	return dot
}

// One group embed per message; fields are split to bound the total.
func (r periodReport) discord() []DiscordMessage {
	messages := make([]DiscordMessage, 0)
	header := reportDiscordTake(reportMarkdown(strings.Join(r.Header, "\n")), discordContentLimit)
	footer := reportDiscordTake(reportMarkdown(r.Footer), discordFooterLimit)
	for _, testnet := range []bool{false, true} {
		title := map[bool]string{false: "Mainnet", true: "Testnet"}[testnet]
		embed := DiscordEmbed{Title: title, Color: reportColor(worstReportDot(r.Rows, testnet)), Footer: &DiscordFooter{Text: footer}}
		total := reportDiscordLen(title) + reportDiscordLen(footer)
		flush := func() {
			if len(embed.Fields) > 0 {
				messages = append(messages, DiscordMessage{Username: "Tenderduty", Content: header, Embeds: []DiscordEmbed{embed}})
			}
			embed.Fields = nil
			total = reportDiscordLen(title) + reportDiscordLen(footer)
		}
		add := func(name, value string) {
			name = reportDiscordTake(reportMarkdown(name), discordNameLimit)
			value = reportMarkdown(value)
			for len([]rune(value)) > 0 {
				part := reportDiscordTake(value, discordValueLimit-1)
				if strings.HasSuffix(part, "\\") {
					part = strings.TrimSuffix(part, "\\")
				}
				value = string([]rune(value)[len([]rune(part)):])
				size := reportDiscordLen(name) + reportDiscordLen(part)
				if len(embed.Fields) >= discordFieldsLimit || total+size > discordEmbedLimit {
					flush()
				}
				embed.Fields = append(embed.Fields, DiscordField{Name: name, Value: part})
				total += size
			}
		}
		for _, row := range r.Rows {
			if row.Testnet != testnet {
				continue
			}
			name := row.Dot + " " + row.Name
			if row.Moniker != "" {
				name += " · " + row.Moniker
			}
			lines := append([]string(nil), row.Lines...)
			if row.Summary != "" {
				lines = append([]string{row.Summary}, lines...)
			}
			add(name, strings.Join(lines, "\n"))
			if len(row.Fibre) > 0 {
				add("🧵 "+row.Name+" Fibre", strings.Join(row.Fibre, "\n"))
			}
		}
		flush()
	}
	for _, part := range splitReportText(reportMarkdown(strings.Join(r.Notes, "\n")), discordContentLimit/2) {
		if part != "" {
			messages = append(messages, DiscordMessage{Username: "Tenderduty", Content: part})
		}
	}
	if len(messages) == 0 {
		messages = append(messages, DiscordMessage{Username: "Tenderduty", Content: header + "\n" + footer})
	}
	return messages
}

func splitReportText(s string, limit int) []string {
	parts := make([]string, 0)
	for len([]rune(s)) > limit {
		runes := []rune(s)
		parts = append(parts, string(runes[:limit]))
		s = string(runes[limit:])
	}
	return append(parts, s)
}

func (r periodReport) slack() SlackMessage {
	message := SlackMessage{Text: reportSlackText(strings.Join(r.Header, "\n"))}
	for _, testnet := range []bool{false, true} {
		attachment := Attachment{Title: map[bool]string{false: "Mainnet", true: "Testnet"}[testnet], Color: fmt.Sprintf("#%06x", reportColor(worstReportDot(r.Rows, testnet))), MrkdwnIn: []string{"fields"}}
		for _, row := range r.Rows {
			if row.Testnet != testnet {
				continue
			}
			name := row.Dot + " " + row.Name
			if row.Moniker != "" {
				name += " · " + row.Moniker
			}
			lines := append([]string(nil), row.Lines...)
			if row.Summary != "" {
				lines = append([]string{row.Summary}, lines...)
			}
			attachment.Fields = append(attachment.Fields, SlackField{Title: reportSlackText(name), Value: reportSlackText(strings.Join(lines, "\n"))})
			if len(row.Fibre) > 0 {
				attachment.Fields = append(attachment.Fields, SlackField{Title: reportSlackText(row.Name + " Fibre"), Value: reportSlackText(strings.Join(row.Fibre, "\n"))})
			}
		}
		if len(attachment.Fields) > 0 {
			message.Attachments = append(message.Attachments, attachment)
		}
	}
	message.Attachments = append(message.Attachments, Attachment{Text: reportSlackText(strings.Join(append(append([]string(nil), r.Notes...), r.Footer), "\n"))})
	return message
}

func reportParts(r periodReport, kind string) []json.RawMessage {
	parts := make([]json.RawMessage, 0)
	add := func(v interface{}) {
		b, _ := json.Marshal(v)
		parts = append(parts, b)
	}
	switch kind {
	case "telegram":
		for _, part := range splitMessage(r.telegram()) {
			add(part)
		}
	case "discord":
		for _, message := range r.discord() {
			// Reports never ping, including @everyone in a chain name.
			add(struct {
				DiscordMessage
				AllowedMentions struct {
					Parse []string `json:"parse"`
				} `json:"allowed_mentions"`
			}{DiscordMessage: message, AllowedMentions: struct {
				Parse []string `json:"parse"`
			}{Parse: []string{}}})
		}
	case "slack":
		add(r.slack())
	}
	return parts
}

func (c *Config) runReports() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		c.reportTick(time.Now())
		select {
		case <-c.context().Done():
			return
		case <-ticker.C:
		}
	}
}

func (c *Config) reportTick(now time.Time) {
	settings := c.settings()
	loc, err := settings.Reports.location(settings.QuietHours.Timezone)
	if err != nil {
		return
	}
	c.collectReportSnapshots(now, false)
	for _, schedule := range []struct {
		kind, value string
		weekly      bool
	}{{"daily", settings.Reports.Daily, false}, {"weekly", settings.Reports.Weekly, true}} {
		c.reportsMux.Lock()
		last := c.reportData.Last[schedule.kind]
		c.reportsMux.Unlock()
		at, due, late := reportDue(now, last, schedule.value, schedule.weekly, loc)
		if at.IsZero() || !at.After(last) {
			continue
		}
		jobs := make([]reportJob, 0)
		if due {
			c.collectReportSnapshots(now, true)
			for _, destination := range c.reportDestinations() {
				report := c.buildReport(now, schedule.weekly, late, destination.Chains)
				jobs = append(jobs, reportJob{Key: destination.Key, Kind: destination.Kind, ReportKind: schedule.kind, At: now, Late: late, Parts: reportParts(report, destination.Kind)})
			}
		}
		c.reportsMux.Lock()
		if c.reportData.Last == nil {
			c.reportData.Last = make(map[string]time.Time)
		}
		// Saved together so a restart cannot skip a queued report.
		c.reportData.Last[schedule.kind] = at
		c.reportData.Pending = append(c.reportData.Pending, jobs...)
		c.reportsMux.Unlock()
	}
	if err := c.persistState(); err != nil {
		l("could not save reports:", err)
		return
	}
	c.deliverReports()
}

func (c *Config) deliverReports() {
	destinations := make(map[string]reportDestination)
	for _, d := range c.reportDestinations() {
		destinations[d.Key] = d
	}
	c.reportsMux.Lock()
	jobs := append([]reportJob(nil), c.reportData.Pending...)
	c.reportsMux.Unlock()
	for _, job := range jobs {
		destination, enabled := destinations[job.Key]
		if !enabled || time.Since(job.At) > reportKeep || job.Next < 0 || job.Next > len(job.Parts) {
			c.finishReportJob(job, true)
			continue
		}
		for job.Next < len(job.Parts) {
			if c.context().Err() != nil {
				return
			}
			if err := c.sendReportPart(destination, job.Parts[job.Next]); err != nil {
				// Errors can contain webhook URLs or bot tokens.
				l("report delivery failed for", job.Kind)
				break
			}
			job.Next++
			c.finishReportJob(job, job.Next == len(job.Parts))
			if job.Next == len(job.Parts) {
				// once per destination, however many parts it took
				note := ""
				if job.Late {
					note = " (late)"
				}
				l("📊 " + job.ReportKind + " report delivered to " + job.Kind + note)
			}
			if err := c.persistState(); err != nil {
				l("could not save report delivery:", err)
				return
			}
		}
	}
}

func (c *Config) finishReportJob(job reportJob, remove bool) {
	c.reportsMux.Lock()
	defer c.reportsMux.Unlock()
	for i := range c.reportData.Pending {
		pending := &c.reportData.Pending[i]
		if pending.Key == job.Key && pending.ReportKind == job.ReportKind && pending.At.Equal(job.At) {
			if remove {
				c.reportData.Pending = append(c.reportData.Pending[:i], c.reportData.Pending[i+1:]...)
			} else {
				pending.Next = job.Next
			}
			return
		}
	}
}

func (c *Config) sendReportPart(destination reportDestination, part json.RawMessage) error {
	if destination.Kind == "telegram" {
		var text string
		if err := json.Unmarshal(part, &text); err != nil {
			return err
		}
		bot, err := tgbotapi.NewBotAPIWithClient(destination.Token, tgbotapi.APIEndpoint, newNotificationClient(c.context(), 10*time.Second))
		if err != nil {
			return err
		}
		message := tgbotapi.NewMessageToChannel(destination.Address, text)
		message.ParseMode = tgbotapi.ModeHTML
		message.DisableWebPagePreview = true
		_, err = bot.Send(message)
		return err
	}
	req, err := http.NewRequestWithContext(c.context(), http.MethodPost, destination.Address, bytes.NewReader(part))
	if err != nil {
		return scrubURLError(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := newNotificationClient(c.context(), 10*time.Second).Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("report HTTP status %d", resp.StatusCode)
	}
	return nil
}

func reportDiscordLen(s string) int {
	return len(utf16.Encode([]rune(s)))
}

func reportDiscordTake(s string, limit int) string {
	n := 0
	for i, r := range s {
		width := 1
		if r > 0xffff {
			width = 2
		}
		if n+width > limit {
			return s[:i]
		}
		n += width
	}
	return s
}
