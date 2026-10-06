package tenderduty

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/PagerDuty/go-pagerduty"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	dash "github.com/kynraze/tenderduty/v2/td2/dashboard"
)

type alertMsg struct {
	ctx       context.Context
	source    *ChainConfig
	ordered   bool
	transient bool // an informational notice, not an alarm
	pd        bool
	disc      bool
	tg        bool
	slk       bool

	severity string
	resolved bool
	chain    string
	message  string
	extra    string
	uniqueId string
	key      string
	// how long a resolved alarm was active, zero if unknown
	lasted time.Duration
	// chat recovery announced by another alarm; PagerDuty still resolves
	quiet bool
	// several nodes of one server, see group_alerts.go
	group *groupCard

	tgChannel  string
	tgKey      string
	tgMentions string

	discHook     string
	discMentions string
	discStyle    string

	slkHook     string
	slkMentions string
}

// heading is the chain, or the server for a group.
func (msg *alertMsg) heading() string {
	if msg.group != nil {
		return msg.group.heading()
	}
	return msg.chain
}

// summary removes the severity line already shown in the header.
func (msg *alertMsg) summary() string {
	if msg.group != nil {
		return msg.group.summary(msg)
	}
	if msg.resolved {
		if r, ok := storedResolution(msg.chain, msg.message); ok {
			return r.text
		}
		if text := resolvedText(msg.chain, msg.message); text != "" {
			return text
		}
	}
	if strings.HasPrefix(msg.message, "Severity: ") {
		if _, rest, ok := strings.Cut(msg.message, "\n"); ok {
			if !msg.resolved {
				if detail := nodeDownDetail(msg.chain, msg.message); detail != "" {
					return rest + "\n" + detail
				}
			}
			return rest
		}
	}
	if (strings.HasPrefix(msg.message, "Proposal #") || strings.Contains(msg.message, "\nCan unjail after ")) && !msg.resolved {
		return govTimeLeft(msg.message)
	}
	if !msg.resolved && isSignalUpgrade(msg.message) {
		body := msg.message
		if line := upgradeTimeLeft(msg.chain, msg.message); line != "" {
			body += "\n" + line
		}
		if line := signalUpgradeDetail(msg.chain, msg.message); line != "" {
			body += "\n" + line
		}
		return body
	}
	if !msg.resolved && strings.HasPrefix(msg.message, "Node stopped at the version ") {
		return msg.message + fmt.Sprintf("\nupgrade celestia-appd to v%d or later", signalMessageVersion(msg.message))
	}
	if !msg.resolved && strings.Contains(msg.message, " is tombstoned on ") {
		first, rest, _ := strings.Cut(msg.message, "\n")
		if detail := jailDetail(msg.chain, first); detail != "" {
			first += "\n" + detail
		}
		if rest == "" {
			rest = "Permanent, the validator can't be unjailed"
		}
		return first + "\n" + rest
	}
	if !msg.resolved && isJailAlarm(msg.message) && !strings.Contains(msg.message, "\n") {
		if line := jailDetail(msg.chain, msg.message); line != "" {
			return msg.message + "\n" + line
		}
	}
	if (strings.HasPrefix(msg.message, "Upgrade ") || isSignalUpgrade(msg.message)) && !msg.resolved {
		if line := upgradeTimeLeft(msg.chain, msg.message); line != "" {
			return msg.message + "\n" + line
		}
	}
	if isSignalMessage(msg.message) && !msg.resolved {
		if line := signalDetail(msg.chain, msg.message); line != "" {
			return msg.message + "\n" + line
		}
	}
	if strings.HasPrefix(msg.message, "stalled: have not seen a new block") && !msg.resolved {
		if line := stalledUpgradeDetail(msg.chain); line != "" {
			return msg.message + "\n" + line
		}
	}
	if strings.HasPrefix(msg.message, fibreEndorsementPrefix) && !msg.resolved {
		if cc := td.Chains[msg.chain]; cc != nil {
			return msg.message + "\n" + cc.endorsementAlertDetail()
		}
	}
	if isFibreMessage(msg.message) && !msg.resolved {
		if line := fibreDetail(msg.chain, msg.message); line != "" {
			return msg.message + "\n" + line
		}
	}
	if isFibreMetricsMessage(msg.message) && !msg.resolved {
		if line := fibreMetricsDetail(msg.chain, msg.message); line != "" {
			return msg.message + "\n" + line
		}
	}
	return msg.message
}

func (msg *alertMsg) text() string {
	if msg.extra == "" {
		return msg.summary()
	}
	return msg.summary() + "\n" + msg.extra
}

// header is the icon and severity (or Resolved) of the first line.
func (msg *alertMsg) header() (icon, state string) {
	if msg.resolved {
		if r, ok := storedResolution(msg.chain, msg.message); ok {
			return r.icon, r.label
		}
	}
	if strings.HasPrefix(msg.message, "Config reload failed\n") {
		return "ℹ️", "Config reload failed"
	}
	if strings.HasPrefix(msg.message, "Proposal #") {
		if msg.resolved {
			return govResolvedLabel(msg.chain, msg.message)
		}
		if strings.Contains(msg.message, " has not voted") {
			return "⏰", "Vote reminder"
		}
		return "🗳", "New proposal"
	}
	switch first, _, _ := strings.Cut(msg.message, "\n"); {
	case strings.Contains(first, " is missing blocks on "):
		if msg.resolved {
			return "✅", "Recovered"
		}
		return "⚠️", "Missing blocks"
	case strings.Contains(first, " is close to jail on "):
		if msg.resolved {
			return "✅", "Recovered"
		}
		return "🚨", "Close to jail"
	case strings.Contains(first, " is tombstoned on "):
		if msg.resolved {
			return "✅", "Resolved"
		}
		return "☠️", "Tombstoned"
	case strings.Contains(first, " is jailed on "):
		if msg.resolved {
			return "✅", "Active again"
		}
		return "🚨", "Jailed"
	case strings.Contains(first, " is no longer in the active set on "):
		if msg.resolved {
			return "✅", "Active again"
		}
		return "🚨", "Not in the active set"
	case strings.Contains(first, " can unjail on "):
		if msg.resolved {
			return "✅", "Unjailed"
		}
		return "🔓", "Can unjail now"
	}
	if isSignalMessage(msg.message) {
		return signalLabel(msg)
	}
	if strings.HasPrefix(msg.message, fibreEndorsementPrefix) {
		if msg.resolved {
			return "✅", "Fibre endorsing"
		}
		return "✍️", "Fibre stopped"
	}
	if isFibreMessage(msg.message) {
		return fibreLabel(msg)
	}
	if isFibreMetricsMessage(msg.message) {
		return fibreMetricsLabel(msg)
	}
	if strings.HasPrefix(msg.message, "Upgrade ") && strings.Contains(msg.message, " at height ") {
		if msg.resolved {
			return upgradeResolvedLabel(msg.chain, msg.message)
		}
		if strings.HasSuffix(msg.message, " is close") {
			return "⏰", "Upgrade soon"
		}
		return "⬆️", "Upgrade scheduled"
	}
	if msg.resolved {
		return "✅", "Resolved"
	}
	state = msg.severity
	if state != "" {
		state = strings.ToUpper(state[:1]) + state[1:]
	}
	switch msg.severity {
	case "critical", "error", "alert":
		return "🚨", state
	case "warning":
		return "⚠️", state
	default:
		return "ℹ️", state
	}
}

type notifyDest uint8

const (
	pd notifyDest = iota
	tg
	di
	slk
)

type alarmCache struct {
	AlarmSeverities   map[string]string               `json:"alarm_severities,omitempty"`
	AlarmIDs          map[string]string               `json:"alarm_ids,omitempty"`
	SentPdAlarms      map[string]time.Time            `json:"sent_pd_alarms"`
	SentTgAlarms      map[string]time.Time            `json:"sent_tg_alarms"`
	SentDiAlarms      map[string]time.Time            `json:"sent_di_alarms"`
	SentSlkAlarms     map[string]time.Time            `json:"sent_slk_alarms"`
	AllAlarms         map[string]map[string]time.Time `json:"sent_all_alarms"`
	PendingRecoveries map[string]*pendingRecovery     `json:"pending_recoveries,omitempty"`
	Outbox            []queuedNotification            `json:"outbox,omitempty"`
	NextNotification  uint64                          `json:"next_notification,omitempty"`
	Groups            map[string]*notifyGroup         `json:"groups,omitempty"`
	flappingAlarms    map[string]map[string]time.Time
	inFlight          map[notifyDest]map[string]bool
	notifyMux         sync.RWMutex
}

type pendingRecovery struct {
	Chain    string `json:"chain"`
	Message  string `json:"message"`
	Severity string `json:"severity"`
	ID       string `json:"id"`
	// Keep the duration after the alarm is removed.
	Lasted time.Duration `json:"lasted,omitempty"`
	Quiet  bool          `json:"quiet,omitempty"`
}

var errNotificationBusy = errors.New("notification delivery is in progress")

func (a *alarmCache) getCount(chain string) int {
	a.notifyMux.RLock()
	defer a.notifyMux.RUnlock()
	if a.AllAlarms == nil || a.AllAlarms[chain] == nil {
		return 0
	}
	return len(a.AllAlarms[chain])
}

func newAlarmCache() *alarmCache {
	return &alarmCache{
		SentPdAlarms:      make(map[string]time.Time),
		SentTgAlarms:      make(map[string]time.Time),
		SentDiAlarms:      make(map[string]time.Time),
		SentSlkAlarms:     make(map[string]time.Time),
		AllAlarms:         make(map[string]map[string]time.Time),
		PendingRecoveries: make(map[string]*pendingRecovery),
		flappingAlarms:    make(map[string]map[string]time.Time),
		inFlight:          make(map[notifyDest]map[string]bool),
	}
}

// alarms is used to prevent double notifications.
var alarms = newAlarmCache()

func shouldNotify(msg *alertMsg, dest notifyDest) (bool, error) {
	if msg.transient {
		return true, nil
	}
	alarms.notifyMux.Lock()
	defer alarms.notifyMux.Unlock()
	var whichMap map[string]time.Time
	switch dest {
	case pd:
		whichMap = alarms.SentPdAlarms
	case tg:
		whichMap = alarms.SentTgAlarms
	case di:
		whichMap = alarms.SentDiAlarms
	case slk:
		whichMap = alarms.SentSlkAlarms
	}
	key := msg.chain + "\x00" + msg.message
	if alarms.inFlight == nil {
		alarms.inFlight = make(map[notifyDest]map[string]bool)
	}
	if alarms.inFlight[dest] == nil {
		alarms.inFlight[dest] = make(map[string]bool)
	}
	if alarms.inFlight[dest][key] {
		return false, errNotificationBusy
	}
	// Old state files used the message alone as the key.
	sent := !whichMap[key].IsZero() || !whichMap[msg.message].IsZero()
	if !msg.ordered && !msg.resolved && sent && alarms.PendingRecoveries[key] != nil {
		return false, errNotificationBusy
	}
	if sent != msg.resolved {
		if msg.resolved && !alarms.sentAnywhere(key, msg.message) && !alarms.inFlightAnywhere(key) {
			delete(alarms.PendingRecoveries, key)
		}
		return false, nil
	}
	// for pagerduty we perform some basic flap detection
	if dest == pd && !msg.resolved && alarms.flappingAlarms[msg.chain][msg.message].After(time.Now().Add(-5*time.Minute)) {
		l("🛑 flapping detected - suppressing pagerduty notification:", msg.chain, msg.message)
		return false, errNotificationBusy
	}
	alarms.inFlight[dest][key] = true
	return true, nil
}

func completeNotify(msg *alertMsg, dest notifyDest, err error) {
	if msg.transient {
		return
	}
	alarms.notifyMux.Lock()
	defer func() {
		alarms.notifyMux.Unlock()
		if err == nil {
			state := "🚨 ALERT delivered"
			if msg.resolved {
				state = "💜 Resolved delivered"
			}
			l(state, msg.chain, "to", []string{"pagerduty", "telegram", "discord", "slack"}[dest])
		}
	}()
	key := msg.chain + "\x00" + msg.message
	delete(alarms.inFlight[dest], key)
	if err != nil {
		return
	}
	var whichMap map[string]time.Time
	switch dest {
	case pd:
		whichMap = alarms.SentPdAlarms
	case tg:
		whichMap = alarms.SentTgAlarms
	case di:
		whichMap = alarms.SentDiAlarms
	case slk:
		whichMap = alarms.SentSlkAlarms
	}
	if msg.resolved {
		delete(whichMap, key)
		delete(whichMap, msg.message)
		if !alarms.sentAnywhere(key, msg.message) {
			delete(alarms.PendingRecoveries, key)
		}
		return
	}
	whichMap[key] = time.Now()
	// re-sent alarm leaves its old group
	alarms.forgetMember(dest, key)
	if dest == pd {
		if alarms.flappingAlarms[msg.chain] == nil {
			alarms.flappingAlarms[msg.chain] = make(map[string]time.Time)
		}
		alarms.flappingAlarms[msg.chain][msg.message] = time.Now()
	}
}

// sentAnywhere is called while notifyMux is held.
func (a *alarmCache) sentAnywhere(key, legacy string) bool {
	for _, sent := range []map[string]time.Time{a.SentPdAlarms, a.SentTgAlarms, a.SentDiAlarms, a.SentSlkAlarms} {
		if !sent[key].IsZero() || !sent[legacy].IsZero() {
			return true
		}
	}
	return false
}

func (a *alarmCache) inFlightAnywhere(key string) bool {
	for _, pending := range a.inFlight {
		if pending[key] {
			return true
		}
	}
	return false
}

func alarmActive(msg *alertMsg) bool {
	alarms.notifyMux.RLock()
	defer alarms.notifyMux.RUnlock()
	return !alarms.AllAlarms[msg.chain][msg.message].IsZero()
}

func alarmIsActive(chain, message string) bool {
	alarms.notifyMux.RLock()
	defer alarms.notifyMux.RUnlock()
	return !alarms.AllAlarms[chain][message].IsZero()
}

func activeMessage(chain, contains string) string {
	alarms.notifyMux.RLock()
	defer alarms.notifyMux.RUnlock()
	for message := range alarms.AllAlarms[chain] {
		if strings.Contains(message, contains) {
			return message
		}
	}
	return ""
}

func hasPendingRecovery(key string) bool {
	alarms.notifyMux.RLock()
	defer alarms.notifyMux.RUnlock()
	if alarms.PendingRecoveries[key] != nil {
		return true
	}
	for _, recovery := range alarms.PendingRecoveries {
		if recovery != nil && recovery.Message == key {
			return true
		}
	}
	return false
}

func notifySlack(msg *alertMsg) (err error) {
	if !msg.slk {
		return
	}
	if send, e := shouldNotify(msg, slk); !send {
		return e
	}
	defer func() { completeNotify(msg, slk, err) }()
	// another alarm announces this recovery
	if msg.quiet {
		return nil
	}
	return postSlack(msg)
}

// postSlack skips sent tracking for grouped messages.
func postSlack(msg *alertMsg) (err error) {
	data, err := json.Marshal(buildSlackMessage(msg))
	if err != nil {
		return
	}

	req, err := http.NewRequestWithContext(msg.context(), "POST", msg.slkHook, bytes.NewBuffer(data))
	if err != nil {
		return scrubURLError(err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := newNotificationClient(msg.context(), 10*time.Second).Do(req)
	if err != nil {
		return
	}
	_ = resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("could not notify slack for %s got %d response", msg.chain, resp.StatusCode)
	}

	return
}

type SlackMessage struct {
	Text        string       `json:"text"`
	Attachments []Attachment `json:"attachments"`
}

type Attachment struct {
	Text      string       `json:"text"`
	Color     string       `json:"color"`
	Title     string       `json:"title"`
	TitleLink string       `json:"title_link"`
	Fields    []SlackField `json:"fields,omitempty"`
	MrkdwnIn  []string     `json:"mrkdwn_in,omitempty"`
}

// tone picks the discord and slack colour, "plain" has none.
func (msg *alertMsg) tone() string {
	switch {
	case msg.resolved:
		return "good"
	case msg.severity == "info":
		return "plain"
	case msg.severity == "warning":
		return "warning"
	default:
		return "danger"
	}
}

func buildSlackMessage(msg *alertMsg) *SlackMessage {
	card := msg.card()
	icon, state := card.Icon, card.State
	message, _, _ := strings.Cut(msg.summary(), "\n")
	if card.Note != "" {
		message = msg.text()
	} else if msg.extra != "" {
		message += "\n" + msg.extra
	}
	if msg.resolved {
		message = "OK: " + message
	}
	color := map[string]string{"good": "good", "warning": "warning", "danger": "danger"}[msg.tone()]
	return &SlackMessage{
		Text: slackText(message),
		Attachments: []Attachment{
			{
				Title:    strings.TrimSpace(fmt.Sprintf("%s %s · %s %s", icon, msg.heading(), state, msg.slkMentions)),
				Color:    color,
				Text:     slackTitle(card),
				Fields:   card.slackFields(),
				MrkdwnIn: []string{"text", "fields"},
			},
		},
	}
}

func notifyDiscord(msg *alertMsg) (err error) {
	if !msg.disc {
		return nil
	}
	if send, e := shouldNotify(msg, di); !send {
		return e
	}
	defer func() { completeNotify(msg, di, err) }()
	// another alarm announces this recovery
	if msg.quiet {
		return nil
	}
	return postDiscord(msg)
}

func postDiscord(msg *alertMsg) (err error) {
	discPost := buildDiscordMessage(msg)
	client := newNotificationClient(msg.context(), 10*time.Second)
	data, err := json.MarshalIndent(discPost, "", "  ")
	if err != nil {
		l("⚠️ Could not notify discord!", err)
		return err
	}

	req, err := http.NewRequestWithContext(msg.context(), "POST", msg.discHook, bytes.NewBuffer(data))
	if err != nil {
		err = scrubURLError(err)
		l("⚠️ Could not notify discord!", err)
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		l("⚠️ Could not notify discord!", err)
		return err
	}
	_ = resp.Body.Close()

	if resp.StatusCode != 204 {
		l("⚠️ Could not notify discord! Returned", resp.StatusCode)
		return fmt.Errorf("could not notify discord for %s: status %d", msg.chain, resp.StatusCode)
	}
	return nil
}

type DiscordMessage struct {
	Username        string                  `json:"username,omitempty"`
	AvatarUrl       string                  `json:"avatar_url,omitempty"`
	Content         string                  `json:"content"`
	Embeds          []DiscordEmbed          `json:"embeds,omitempty"`
	AllowedMentions *DiscordAllowedMentions `json:"allowed_mentions,omitempty"`
}

// DiscordAllowedMentions lists the users and roles a message may ping.
type DiscordAllowedMentions struct {
	Parse []string `json:"parse"`
	Users []string `json:"users,omitempty"`
	Roles []string `json:"roles,omitempty"`
}

var (
	discordUserMention = regexp.MustCompile(`<@!?(\d+)>`)
	discordRoleMention = regexp.MustCompile(`<@&(\d+)>`)
)

// discordMentionLimit is how many users or roles discord accepts in allowed_mentions.
const discordMentionLimit = 100

// discordAllowedMentions permits configured mentions only.
func discordAllowedMentions(configured string) *DiscordAllowedMentions {
	allowed := &DiscordAllowedMentions{Parse: []string{}}
	// "everyone" covers @here as well
	if strings.Contains(configured, "@everyone") || strings.Contains(configured, "@here") {
		allowed.Parse = append(allowed.Parse, "everyone")
	}
	ids := func(pattern *regexp.Regexp) []string {
		var found []string
		seen := make(map[string]bool)
		for _, m := range pattern.FindAllStringSubmatch(configured, -1) {
			if !seen[m[1]] && len(found) < discordMentionLimit {
				seen[m[1]] = true
				found = append(found, m[1])
			}
		}
		return found
	}
	allowed.Users, allowed.Roles = ids(discordUserMention), ids(discordRoleMention)
	return allowed
}

type DiscordEmbed struct {
	Title       string         `json:"title,omitempty"`
	Url         string         `json:"url,omitempty"`
	Description string         `json:"description,omitempty"`
	Color       uint           `json:"color"`
	Fields      []DiscordField `json:"fields,omitempty"`
	Footer      *DiscordFooter `json:"footer,omitempty"`
}

func buildDiscordMessage(msg *alertMsg) *DiscordMessage {
	card := msg.card()
	header := strings.TrimSpace(fmt.Sprintf("%s **%s** · %s %s", card.Icon, msg.heading(), card.State, msg.discMentions))
	// plain is one message with no embed, the blank line separates header and alert
	if msg.discStyle == "plain" {
		lines := card.markdownLines()
		if card.Extra != "" {
			lines = append(lines, card.Extra)
		}
		return &DiscordMessage{Username: "Tenderduty", Content: clipRunes(header+"\n\n"+strings.Join(lines, "\n"), discordContentLimit), AllowedMentions: discordAllowedMentions(msg.discMentions)}
	}
	// dashboard colours, plain gets no bar
	color := map[string]uint{"good": 0x63d6c4, "warning": 0xf1b663, "danger": 0xf27c76}[msg.tone()]
	return &DiscordMessage{
		Username:        "Tenderduty",
		Content:         header,
		Embeds:          []DiscordEmbed{card.discordEmbed(color)},
		AllowedMentions: discordAllowedMentions(msg.discMentions),
	}
}

func notifyTg(msg *alertMsg) (err error) {
	if !msg.tg {
		return nil
	}
	if send, e := shouldNotify(msg, tg); !send {
		return e
	}
	defer func() { completeNotify(msg, tg, err) }()
	// another alarm announces this recovery
	if msg.quiet {
		return nil
	}
	return postTg(msg)
}

func postTg(msg *alertMsg) (err error) {
	bot, err := tgbotapi.NewBotAPIWithClient(msg.tgKey, tgbotapi.APIEndpoint, newNotificationClient(msg.context(), 10*time.Second))
	if err != nil {
		l("notify telegram:", err)
		return
	}

	mc := tgbotapi.NewMessageToChannel(msg.tgChannel, buildTelegramMessage(msg))
	mc.ParseMode = tgbotapi.ModeHTML
	mc.DisableWebPagePreview = true
	_, err = bot.Send(mc)
	if err != nil {
		l("telegram send:", err)
	}
	return err
}

// timeLeft matches the time left shown in governance and upgrade messages.
var timeLeft = regexp.MustCompile(`· ((?:\d+[dhm] )*\d+[dhms] left)`)

// buildTelegramMessage escapes text for Telegram HTML.
func buildTelegramMessage(msg *alertMsg) string {
	card := msg.card()
	text := fmt.Sprintf("%s <b>%s</b> · %s\n\n%s", card.Icon, html.EscapeString(msg.heading()), html.EscapeString(card.State), strings.Join(card.telegramLines(), "\n"))
	if msg.extra != "" {
		text += "\n<i>" + html.EscapeString(msg.extra) + "</i>"
	}
	if msg.tgMentions != "" {
		text += "\n" + msg.tgMentions
	}
	return text
}

func notifyPagerduty(msg *alertMsg) (err error) {
	if !msg.pd {
		return nil
	}
	if send, e := shouldNotify(msg, pd); !send {
		return e
	}
	defer func() { completeNotify(msg, pd, err) }()
	// key from the example, don't spam their api
	if msg.key == "aaaaaaaaaaaabbbbbbbbbbbbbcccccccccccc" {
		l("invalid pagerduty key")
		return fmt.Errorf("invalid pagerduty key for %s", msg.chain)
	}
	action := "trigger"
	if msg.resolved {
		action = "resolve"
	}
	payload := &pagerduty.V2Payload{
		Summary:  strings.ReplaceAll(msg.summary(), "\n", " · "), // pagerduty shows one line
		Source:   msg.uniqueId,
		Severity: msg.severity,
	}
	if msg.extra != "" {
		payload.Details = map[string]string{"extra_info": msg.extra}
	}
	ctx, cancel := context.WithTimeout(msg.context(), 10*time.Second)
	defer cancel()
	_, err = pagerduty.ManageEventWithContext(ctx, pagerduty.V2Event{
		RoutingKey: msg.key,
		Action:     action,
		DedupKey:   msg.uniqueId,
		Payload:    payload,
	})
	return
}

func getAlarms(chain string) string {
	alarms.notifyMux.RLock()
	defer alarms.notifyMux.RUnlock()
	// don't show this info if the logs are disabled on the dashboard, potentially sensitive info could be leaked.
	if td.HideLogs || alarms.AllAlarms[chain] == nil {
		return ""
	}
	result := ""
	for k := range alarms.AllAlarms[chain] {
		// the "is close" reminder repeats the scheduled upgrade, show it once
		if _, ok := alarms.AllAlarms[chain][k+" is close"]; ok {
			continue
		}
		result += "🚨 " + k + "\n"
	}
	return result
}

// makeAlert resolves notification credentials from the current configuration.
func (c *Config) makeAlert(chainName, message, severity string, resolved bool, id *string) *alertMsg {
	c.chainsMux.RLock()
	defer c.chainsMux.RUnlock()
	if c.Chains[chainName] == nil {
		return nil
	}
	uniq := c.Chains[chainName].ValAddress
	if id != nil && *id != "" {
		uniq = *id
	}
	alerts := c.Chains[chainName].alertSettings()
	routed := func(min string) bool { return resolved || severityRank(severity) >= severityRank(min) }
	a := &alertMsg{
		source:       c.Chains[chainName],
		pd:           enabledFor(c.settings().Pagerduty.Enabled, alerts.Pagerduty.Enabled) && routed(alerts.Pagerduty.MinSeverity),
		disc:         enabledFor(c.settings().Discord.Enabled, alerts.Discord.Enabled) && routed(alerts.Discord.MinSeverity),
		tg:           enabledFor(c.settings().Telegram.Enabled, alerts.Telegram.Enabled) && routed(alerts.Telegram.MinSeverity),
		slk:          enabledFor(c.settings().Slack.Enabled, alerts.Slack.Enabled) && routed(alerts.Slack.MinSeverity),
		severity:     severity,
		resolved:     resolved,
		chain:        chainName,
		message:      message,
		extra:        c.Chains[chainName].extraInfo(),
		uniqueId:     uniq,
		key:          alerts.Pagerduty.ApiKey,
		tgChannel:    alerts.Telegram.Channel,
		tgKey:        alerts.Telegram.ApiKey,
		tgMentions:   telegramMentions(alerts.Telegram.Mentions),
		discHook:     alerts.Discord.Webhook,
		discMentions: strings.Join(alerts.Discord.Mentions, " "),
		discStyle:    alerts.Discord.Style,
		slkHook:      alerts.Slack.Webhook,
		slkMentions:  strings.Join(alerts.Slack.Mentions, " "),
	}
	return a
}

// severityRank ranks unknown severities as critical to retain their alerts.
func severityRank(severity string) int {
	switch severity {
	case "":
		return 0
	case "info":
		return 1
	case "warning":
		return 2
	case "error":
		return 3
	default:
		return 4
	}
}

// alert records the incident and its ordered notification queue before waking delivery workers.
func (c *Config) alert(chainName, message, severity string, resolved bool, id *string) {
	c.record(chainName, message, severity, resolved, false, id)
}

// resolveQuietly skips chat recovery but resolves PagerDuty.
func (c *Config) resolveQuietly(chainName, message string, id *string) {
	c.record(chainName, message, "info", true, true, id)
}

func (c *Config) record(chainName, message, severity string, resolved, quiet bool, id *string) {
	a := c.makeAlert(chainName, message, severity, resolved, id)
	if a == nil {
		return
	}
	c.chainsMux.RLock()
	cc := c.Chains[chainName]
	if cc == nil || cc != a.source {
		c.chainsMux.RUnlock()
		return
	}
	cc.stateMux.RLock()
	if cc.retiring && !resolved {
		cc.stateMux.RUnlock()
		c.chainsMux.RUnlock()
		return
	}
	// Keep retirement from passing an alert that is still being recorded.
	if !resolved {
		resolutions.Delete(chainName + "\x00" + message)
	}
	uniq := a.uniqueId
	alarms.notifyMux.Lock()
	if original := alarms.AlarmIDs[chainName+"\x00"+message]; original != "" && !alarms.AllAlarms[chainName][message].IsZero() {
		uniq, a.uniqueId = original, original
	}
	if alarms.AlarmIDs == nil {
		alarms.AlarmIDs = make(map[string]string)
	}
	if !resolved {
		alarms.AlarmIDs[chainName+"\x00"+message] = uniq
	} else {
		delete(alarms.AlarmIDs, chainName+"\x00"+message)
	}
	key := chainName + "\x00" + message
	if resolved {
		delete(alarms.AlarmSeverities, key)
	} else if alarms.AllAlarms[chainName][message].IsZero() {
		if alarms.AlarmSeverities == nil {
			alarms.AlarmSeverities = make(map[string]string)
		}
		alarms.AlarmSeverities[key] = severity
	}
	if alarms.AllAlarms[chainName] == nil {
		alarms.AllAlarms[chainName] = make(map[string]time.Time)
	}
	var lasted time.Duration
	if resolved && !alarms.AllAlarms[chainName][message].IsZero() {
		c.recordReportAlarm(chainName, message, alarms.AllAlarms[chainName][message], time.Now())
		lasted = time.Since(alarms.AllAlarms[chainName][message])
		delete(alarms.AllAlarms[chainName], message)
	} else if !resolved && alarms.AllAlarms[chainName][message].IsZero() {
		alarms.AllAlarms[chainName][message] = time.Now()
		c.recordReportAlarm(chainName, message, alarms.AllAlarms[chainName][message], time.Time{})
	}
	if resolved && (alarms.sentAnywhere(key, message) || alarms.inFlightAnywhere(key)) {
		alarms.PendingRecoveries[key] = &pendingRecovery{Chain: chainName, Message: message, Severity: severity, ID: uniq, Lasted: lasted, Quiet: quiet}
	} else if !resolved {
		// the alarm fired again, recoveries already in the outbox still go out from there. Keeping this around
		// would send a resolve for the new alarm after a restart.
		delete(alarms.PendingRecoveries, key)
	}
	heldMux.Lock()
	_, wasHeld := held[key]
	if resolved {
		// never sent, nothing to recover
		delete(held, key)
	} else if c.holdsAlert(chainName, severity, resolved, time.Now()) {
		held[key] = heldAlert{chain: chainName, message: message, severity: severity, id: uniq}
		wasHeld = true
	}
	heldMux.Unlock()
	a.lasted, a.quiet = lasted, quiet && resolved
	if !wasHeld || resolved {
		alarms.enqueue(a)
	}
	alarms.notifyMux.Unlock()
	cc.stateMux.RUnlock()
	c.chainsMux.RUnlock()
	if err := c.persistState(); err != nil {
		l("could not persist alert:", err)
	}
	select {
	case c.alertChan <- a:
	default:
	}
}

// watch handles monitoring for missed blocks, stalled chain, node downtime
// and also updates a few prometheus stats
// FIXME: not watching for nodes that are lagging the head block!
func (cc *ChainConfig) watch() {
	started := time.Now()
	// valcons isn't known until we connect, and the stalled alarm already uses it as the pagerduty dedup key
	noRpcId := cc.ValAddress + "norpc"
	valInfo, _ := cc.validatorState()
	var proposalAlarm, noNodes bool
	var lastStatus *dash.ChainStatus
	nodeAlarms := make(map[string]bool)

	// wait until we have a moniker:
	noNodesSec := 0 // delay a no-nodes alarm for 30 seconds, too noisy.
	for {
		select {
		case <-cc.context().Done():
			return
		default:
		}
		valInfo, _ = cc.validatorState()
		if valInfo == nil || valInfo.Moniker == "not connected" {
			if !waitContext(cc.context(), time.Second) {
				return
			}
			if cc.alertSettings().AlertIfNoServers && !noNodes && cc.hasNoNodes() && noNodesSec >= 60*td.settings().NodeDownMin {
				noNodes = true
				td.alert(
					cc.name,
					fmt.Sprintf("no RPC endpoints are working for %s", cc.ChainId),
					"critical",
					false,
					&noRpcId,
				)
			}
			noNodesSec += 1
			continue
		}
		noNodesSec = 0
		break
	}
	proposalAlarm = alarmIsActive(cc.name, cc.proposalMessage(valInfo.Moniker))
	cc.lastBlockAlarm = alarmIsActive(cc.name, fmt.Sprintf("stalled: have not seen a new block on %s in %d minutes", cc.ChainId, cc.alertSettings().Stalled))
	noNodes = alarmIsActive(cc.name, fmt.Sprintf("no RPC endpoints are working for %s", cc.ChainId))
	for _, node := range cc.Nodes {
		message := nodeDownMessage(node.Url, cc.ChainId)
		nodeAlarms[node.Url] = alarmIsActive(cc.name, message)
	}
	cc.clearOrphanedAlarms(valInfo, noRpcId)
	reconcile := true
	version := cc.settingsRevision()
	// initial stat creation for nodes, we only update again if the node is positive
	if td.Prom {
		for _, node := range cc.Nodes {
			td.sendStat(cc.mkUpdate(metricNodeDownSeconds, 0, node.Url))
		}
	}

	for {
		if !waitContext(cc.context(), 2*time.Second) {
			return
		}
		select {
		case <-cc.context().Done():
			return
		default:
		}
		if revision := cc.settingsRevision(); revision != version {
			version, reconcile = revision, true
			info, _ := cc.validatorState()
			cc.clearOrphanedAlarms(info, noRpcId)
			alerts := cc.alertSettings()
			proposalAlarm = alarmIsActive(cc.name, cc.proposalMessage(info.Moniker))
			cc.lastBlockAlarm = alarmIsActive(cc.name, fmt.Sprintf("stalled: have not seen a new block on %s in %d minutes", cc.ChainId, alerts.Stalled))
			noNodes = alarmIsActive(cc.name, fmt.Sprintf("no RPC endpoints are working for %s", cc.ChainId))
			for _, node := range cc.Nodes {
				nodeAlarms[node.Url] = alarmIsActive(cc.name, nodeDownMessage(node.Url, cc.ChainId))
			}
		}
		valInfo, _ = cc.validatorState()
		lastBlockTime, _, consecutiveMiss := cc.blockState()
		unavailable := cc.hasNoNodes()

		// alert if we can't monitor
		if reconcile && noNodes && unavailable {
			td.alert(cc.name, fmt.Sprintf("no RPC endpoints are working for %s", cc.ChainId), "critical", false, &noRpcId)
		}
		switch {
		case cc.alertSettings().AlertIfNoServers && !noNodes && unavailable:
			noNodesSec += 2
			if noNodesSec < 60*td.settings().NodeDownMin {
				if noNodesSec%20 == 0 {
					l(fmt.Sprintf("no nodes available on %s for %d seconds, deferring alarm", cc.ChainId, noNodesSec))
				}
				noNodes = false
			} else {
				noNodesSec = 0
				noNodes = true
				td.alert(
					cc.name,
					fmt.Sprintf("no RPC endpoints are working for %s", cc.ChainId),
					"critical",
					false,
					&noRpcId,
				)
			}
		case cc.alertSettings().AlertIfNoServers && noNodes && !unavailable:
			noNodes = false
			td.alert(
				cc.name,
				fmt.Sprintf("no RPC endpoints are working for %s", cc.ChainId),
				"critical",
				true,
				&noRpcId,
			)
		default:
			noNodesSec = 0
		}

		// stalled chain detection, the saved block time is from before we restarted so give the chain the full
		// stall period after startup before alerting
		stallSince := lastBlockTime
		if stallSince.Before(started) {
			stallSince = started
		}
		if cc.alertSettings().StalledAlerts && (!cc.lastBlockAlarm || reconcile) && !lastBlockTime.IsZero() &&
			stallSince.Before(time.Now().Add(time.Duration(-cc.alertSettings().Stalled)*time.Minute)) {

			// chain is stalled send an alert!
			cc.lastBlockAlarm = true
			td.alert(
				cc.name,
				fmt.Sprintf("stalled: have not seen a new block on %s in %d minutes", cc.ChainId, cc.alertSettings().Stalled),
				"critical",
				false,
				&valInfo.Valcons,
			)
		} else if cc.alertSettings().StalledAlerts && cc.lastBlockAlarm && !lastBlockTime.IsZero() &&
			lastBlockTime.After(time.Now().Add(time.Duration(-cc.alertSettings().Stalled)*time.Minute)) {
			cc.lastBlockAlarm = false
			td.alert(
				cc.name,
				fmt.Sprintf("stalled: have not seen a new block on %s in %d minutes", cc.ChainId, cc.alertSettings().Stalled),
				"info",
				true,
				&valInfo.Valcons,
			)
		}

		cc.jailAlarms(valInfo, consecutiveMiss, reconcile)

		if cc.alertSettings().ProposalAlerts {
			misses := cc.recentProposalMisses(time.Duration(cc.alertSettings().ProposalMinutes) * time.Minute)
			id := valInfo.Valcons + "proposal"
			if (!proposalAlarm || reconcile) && misses >= cc.alertSettings().ProposalMissed {
				proposalAlarm = true
				td.alert(cc.name, cc.proposalMessage(valInfo.Moniker), cc.alertSettings().ProposalSeverity, false, &id)
			} else if proposalAlarm && misses < cc.alertSettings().ProposalMissed {
				proposalAlarm = false
				td.alert(cc.name, cc.proposalMessage(valInfo.Moniker), "info", true, &id)
			}
		}

		if cc.alertSettings().GovAlerts && valInfo.Valcons != "" {
			cc.govAlarms(valInfo, reconcile)
		}
		if cc.alertSettings().UpgradeAlerts && valInfo.Valcons != "" {
			cc.upgradeAlarms(valInfo, reconcile)
		}
		if cc.alertSettings().UpgradeAlerts && valInfo.Valcons != "" {
			cc.signalAlarms(valInfo, reconcile)
		}
		if cc.alertSettings().FibreAlerts && valInfo.Valcons != "" {
			cc.fibreAlarms(valInfo, reconcile)
		}
		if cc.alertSettings().FibreAlerts && valInfo.Valcons != "" && td.FibreMetrics.Listen != "" {
			cc.fibreMetricsAlarms(valInfo, reconcile)
		}

		// node down alarms
		for _, node := range cc.Nodes {
			status := node.snapshot()
			// window percentage missed block alarms
			// a node restored as down from the state file has to be checked again before it can raise an alarm
			if node.AlertIfDown && status.down && status.checked && !status.downSince.IsZero() &&
				time.Since(status.downSince) > time.Duration(td.settings().NodeDownMin)*time.Minute {
				// alert on dead node
				if nodeAlarms[node.Url] && !reconcile {
					continue
				}
				nodeAlarms[node.Url] = true // used to keep active alert count correct
				td.alert(
					cc.name,
					nodeDownMessage(node.Url, cc.ChainId),
					td.settings().NodeDownSeverity,
					false,
					&node.Url,
				)
			} else if node.AlertIfDown && !status.down && nodeAlarms[node.Url] {
				// clear the alert
				nodeAlarms[node.Url] = false
				node.clearRecovery()
				td.alert(
					cc.name,
					nodeDownMessage(node.Url, cc.ChainId),
					"info",
					true,
					&node.Url,
				)
			}
		}

		if td.Prom {
			unhealthy := 0
			for _, node := range cc.Nodes {
				if node.snapshot().down {
					unhealthy++
				}
			}
			td.sendStat(cc.mkUpdate(metricUnealthyNodes, float64(unhealthy), ""))
			td.sendStat(cc.mkUpdate(metricTotalNodes, float64(len(cc.Nodes)), ""))
			// raw block timer, ignoring finalized state
			if !lastBlockTime.IsZero() {
				td.sendStat(cc.mkUpdate(metricLastBlockSecondsNotFinal, time.Since(lastBlockTime).Seconds(), ""))
			}
			// update node-down times for prometheus
			for _, node := range cc.Nodes {
				status := node.snapshot()
				if status.down && !status.downSince.IsZero() {
					td.sendStat(cc.mkUpdate(metricNodeDownSeconds, time.Since(status.downSince).Seconds(), node.Url))
				}
			}
		}
		if cc.alertSettings().FibreAlerts && valInfo.Valcons != "" {
			cc.fibreEndorsementAlarms(valInfo, reconcile)
		}
		if td.EnableDash {
			// blocks are pushed as they arrive, only send node and alarm changes here
			status := cc.dashboardStatus()
			if !reflect.DeepEqual(status, lastStatus) {
				lastStatus = status
				select {
				case td.updateChan <- status:
				case <-cc.context().Done():
					return
				}
			}
		}
		reconcile = false
	}
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// jailMessage says why the validator is out, the time left is added when sent.
func jailMessage(valInfo *ValInfo, chainId string) string {
	switch {
	case valInfo.Tombstoned:
		return fmt.Sprintf("%s is tombstoned on %s\nPermanent, the validator can't be unjailed", valInfo.Moniker, chainId)
	case valInfo.Jailed && valInfo.JailedUntil.After(time.Unix(0, 0)):
		return fmt.Sprintf("%s is jailed on %s\nCan unjail after %s", valInfo.Moniker, chainId, valInfo.JailedUntil.UTC().Format(govEndFormat))
	case valInfo.Jailed:
		return fmt.Sprintf("%s is jailed on %s", valInfo.Moniker, chainId)
	default:
		return fmt.Sprintf("%s is no longer in the active set on %s", valInfo.Moniker, chainId)
	}
}

func nodeDownMessage(url, chainId string) string {
	return fmt.Sprintf("Severity: %s\nRPC node %s has been down for > %s on %s", td.settings().NodeDownSeverity, url, plural(td.settings().NodeDownMin, "minute"), chainId)
}

func (cc *ChainConfig) proposalMessage(moniker string) string {
	return fmt.Sprintf("%s missed %s in %s on %s", moniker, plural(cc.alertSettings().ProposalMissed, "block proposal"), plural(cc.alertSettings().ProposalMinutes, "minute"), cc.ChainId)
}

func (cc *ChainConfig) jailRiskMessage(moniker string) string {
	return fmt.Sprintf("%s has used %d%% of the misses allowed before jailing on %s", moniker, cc.alertSettings().JailRiskPercent, cc.ChainId)
}

func (cc *ChainConfig) jailSoonMessage(moniker string) string {
	return fmt.Sprintf("%s will be jailed in under %s on %s if it keeps missing blocks", moniker, plural(cc.alertSettings().JailRiskMinutes, "minute"), cc.ChainId)
}

// clearOrphanedAlarms resolves alarms restored from the state file that would never clear on their own, because the
// alert was disabled or a config change (thresholds, moniker etc.) changed the message.
func (cc *ChainConfig) clearOrphanedAlarms(valInfo *ValInfo, noRpcId string) {
	expected := make(map[string]bool)
	if cc.alertSettings().ProposalAlerts {
		expected[cc.proposalMessage(valInfo.Moniker)] = true
	}
	if cc.alertSettings().StalledAlerts {
		expected[fmt.Sprintf("stalled: have not seen a new block on %s in %d minutes", cc.ChainId, cc.alertSettings().Stalled)] = true
	}
	if cc.alertSettings().AlertIfNoServers {
		expected[fmt.Sprintf("no RPC endpoints are working for %s", cc.ChainId)] = true
	}
	for _, node := range cc.Nodes {
		if node.AlertIfDown {
			expected[nodeDownMessage(node.Url, cc.ChainId)] = true
		}
	}
	if cc.alertSettings().FibreAlerts && cc.alertSettings().FibreEndorsementAlert {
		expected[fibreEndorsementPrefix+cc.ChainId] = true
	}
	if cc.alertSettings().FibreAlerts && td.FibreMetrics.Listen != "" {
		for _, kind := range cc.fibreMetricsKindsOn() {
			expected[cc.fibreMetricsMessage(kind)] = true
		}
	}

	alarms.notifyMux.RLock()
	restored := make([]string, 0, len(alarms.AllAlarms[cc.name]))
	for message := range alarms.AllAlarms[cc.name] {
		restored = append(restored, message)
	}
	alarms.notifyMux.RUnlock()

	for _, message := range restored {
		var id string
		switch {
		case legacySigningAlarm(message):
			id = td.alarmID(cc.name, message)
			if id == "" {
				id = jailAlarmID(valInfo, message)
			}
			td.resolveQuietly(cc.name, message, &id)
			continue
		case isJailAlarm(message):
			continue // the staged alarm handles old and disabled alarms
		case expected[fibreMetricsCurrentMessage(message)]:
			continue
		case strings.Contains(message, " is no longer active: validator is "), strings.Contains(message, " is no longer in the active set on "),
			strings.Contains(message, " is jailed on "):
			if cc.alertSettings().AlertIfInactive && strings.HasPrefix(message, valInfo.Moniker+" is ") && strings.Contains(message, " on "+cc.ChainId) {
				continue // the inactive check picks these up
			}
			id = valInfo.Valcons + "jailed"
		case strings.Contains(message, " is tombstoned on "):
			if cc.alertSettings().AlertIfInactive && strings.Contains(message, valInfo.Moniker) && strings.Contains(message, " on "+cc.ChainId) {
				continue
			}
			id = valInfo.Valcons + "tombstoned"
		case strings.Contains(message, " can unjail on "):
			if cc.alertSettings().AlertIfInactive && strings.Contains(message, valInfo.Moniker) && strings.Contains(message, " on "+cc.ChainId) {
				continue
			}
			id = valInfo.Valcons + "unjail"
		case strings.Contains(message, " of the slashing window's blocks on "):
			id = valInfo.Valcons + "percent"
		case strings.HasPrefix(message, "Proposal #"):
			if cc.alertSettings().GovAlerts {
				continue // the governance check works these out itself
			}
			id = govAlarmId(valInfo, message)
		case strings.HasPrefix(message, "Upgrade ") && strings.Contains(message, " at height "):
			if cc.alertSettings().UpgradeAlerts {
				continue
			}
			id = upgradeAlarmId(valInfo, message)
		case isSignalMessage(message):
			if cc.alertSettings().UpgradeAlerts {
				continue
			}
			id = signalAlarmId(valInfo, message)
		case strings.HasPrefix(message, fibreEndorsementPrefix):
			id = valInfo.Valcons + "fibreendorsement"
		case isFibreMessage(message):
			if cc.alertSettings().FibreAlerts {
				continue
			}
			id = fibreAlarmId(valInfo, message)
		case isFibreMetricsMessage(message):
			// expected ones are skipped above, so this is off or its threshold changed
			id = fibreMetricsAlarmId(valInfo, message)
		case strings.Contains(message, " block proposal") && strings.Contains(message, " in "):
			id = valInfo.Valcons + "proposal"
		case strings.Contains(message, " of the misses allowed before jailing on "):
			id = valInfo.Valcons + "jail"
		case strings.Contains(message, " if it keeps missing blocks"):
			id = valInfo.Valcons + "jailsoon"
		case strings.Contains(message, " has missed ") && strings.Contains(message, " blocks on "):
			id = valInfo.Valcons + "consecutive"
		case strings.HasPrefix(message, "stalled: have not seen a new block"):
			id = valInfo.Valcons
		case strings.HasPrefix(message, "no RPC endpoints are working"):
			id = noRpcId
		case strings.Contains(message, "RPC node ") && strings.Contains(message, " has been down for "):
			id = strings.SplitN(strings.SplitN(message, "RPC node ", 2)[1], " has been down for ", 2)[0]
		default:
			continue // no idea where this came from, it gets purged once it's stale
		}
		l("📂 clearing restored alarm that is no longer monitored -", message)
		resolve(cc.name, message, orphanResolution(message))
		if savedID := td.alarmID(cc.name, message); savedID != "" {
			id = savedID
		}
		td.alert(cc.name, message, "info", true, &id)
	}
}
