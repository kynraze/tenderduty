package tenderduty

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/PagerDuty/go-pagerduty"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

type alertMsg struct {
	pd   bool
	disc bool
	tg   bool
	slk  bool

	severity string
	resolved bool
	chain    string
	message  string
	uniqueId string
	key      string

	tgChannel  string
	tgKey      string
	tgMentions string

	discHook     string
	discMentions string

	slkHook     string
	slkMentions string
}

type notifyDest uint8

const (
	pd notifyDest = iota
	tg
	di
	slk
)

type alarmCache struct {
	SentPdAlarms      map[string]time.Time            `json:"sent_pd_alarms"`
	SentTgAlarms      map[string]time.Time            `json:"sent_tg_alarms"`
	SentDiAlarms      map[string]time.Time            `json:"sent_di_alarms"`
	SentSlkAlarms     map[string]time.Time            `json:"sent_slk_alarms"`
	AllAlarms         map[string]map[string]time.Time `json:"sent_all_alarms"`
	PendingRecoveries map[string]*pendingRecovery     `json:"pending_recoveries,omitempty"`
	flappingAlarms    map[string]map[string]time.Time
	inFlight          map[notifyDest]map[string]bool
	notifyMux         sync.RWMutex
}

type pendingRecovery struct {
	Chain    string `json:"chain"`
	Message  string `json:"message"`
	Severity string `json:"severity"`
	ID       string `json:"id"`
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
	if !msg.resolved && sent && alarms.PendingRecoveries[key] != nil {
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
	data, err := json.Marshal(buildSlackMessage(msg))
	if err != nil {
		return
	}

	req, err := http.NewRequest("POST", msg.slkHook, bytes.NewBuffer(data))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
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
	Text      string `json:"text"`
	Color     string `json:"color"`
	Title     string `json:"title"`
	TitleLink string `json:"title_link"`
}

func buildSlackMessage(msg *alertMsg) *SlackMessage {
	prefix := "🚨 ALERT: "
	color := "danger"
	message := msg.message
	if msg.resolved {
		message = "OK: " + message
		prefix = "💜 Resolved: "
		color = "good"
	}
	return &SlackMessage{
		Text: message,
		Attachments: []Attachment{
			{
				Title: fmt.Sprintf("TenderDuty %s %s %s", prefix, msg.chain, msg.slkMentions),
				Color: color,
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
	discPost := buildDiscordMessage(msg)
	client := &http.Client{Timeout: 10 * time.Second}
	data, err := json.MarshalIndent(discPost, "", "  ")
	if err != nil {
		l("⚠️ Could not notify discord!", err)
		return err
	}

	req, err := http.NewRequest("POST", msg.discHook, bytes.NewBuffer(data))
	if err != nil {
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
	Username  string         `json:"username,omitempty"`
	AvatarUrl string         `json:"avatar_url,omitempty"`
	Content   string         `json:"content"`
	Embeds    []DiscordEmbed `json:"embeds,omitempty"`
}

type DiscordEmbed struct {
	Title       string `json:"title,omitempty"`
	Url         string `json:"url,omitempty"`
	Description string `json:"description"`
	Color       uint   `json:"color"`
}

func buildDiscordMessage(msg *alertMsg) *DiscordMessage {
	prefix := "🚨 ALERT: "
	if msg.resolved {
		prefix = "💜 Resolved: "
	}
	return &DiscordMessage{
		Username: "Tenderduty",
		Content:  prefix + msg.chain,
		Embeds: []DiscordEmbed{{
			Description: msg.message,
		}},
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
	bot, err := tgbotapi.NewBotAPIWithClient(msg.tgKey, tgbotapi.APIEndpoint, &http.Client{Timeout: 10 * time.Second})
	if err != nil {
		l("notify telegram:", err)
		return
	}

	prefix := "🚨 ALERT: "
	if msg.resolved {
		prefix = "💜 Resolved: "
	}

	mc := tgbotapi.NewMessageToChannel(msg.tgChannel, fmt.Sprintf("%s: %s - %s", msg.chain, prefix, msg.message))
	_, err = bot.Send(mc)
	if err != nil {
		l("telegram send:", err)
	}
	return err
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
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err = pagerduty.ManageEventWithContext(ctx, pagerduty.V2Event{
		RoutingKey: msg.key,
		Action:     action,
		DedupKey:   msg.uniqueId,
		Payload: &pagerduty.V2Payload{
			Summary:  msg.message,
			Source:   msg.uniqueId,
			Severity: msg.severity,
		},
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
		result += "🚨 " + k + "\n"
	}
	return result
}

// alert creates a universal alert and pushes it to the alertChan to be delivered to appropriate services
func (c *Config) alert(chainName, message, severity string, resolved bool, id *string) {
	uniq := c.Chains[chainName].ValAddress
	if id != nil && *id != "" {
		uniq = *id
	}
	c.chainsMux.RLock()
	a := &alertMsg{
		pd:           c.Pagerduty.Enabled && c.Chains[chainName].Alerts.Pagerduty.Enabled,
		disc:         c.Discord.Enabled && c.Chains[chainName].Alerts.Discord.Enabled,
		tg:           c.Telegram.Enabled && c.Chains[chainName].Alerts.Telegram.Enabled,
		slk:          c.Slack.Enabled && c.Chains[chainName].Alerts.Slack.Enabled,
		severity:     severity,
		resolved:     resolved,
		chain:        chainName,
		message:      message,
		uniqueId:     uniq,
		key:          c.Chains[chainName].Alerts.Pagerduty.ApiKey,
		tgChannel:    c.Chains[chainName].Alerts.Telegram.Channel,
		tgKey:        c.Chains[chainName].Alerts.Telegram.ApiKey,
		tgMentions:   strings.Join(c.Chains[chainName].Alerts.Telegram.Mentions, " "),
		discHook:     c.Chains[chainName].Alerts.Discord.Webhook,
		discMentions: strings.Join(c.Chains[chainName].Alerts.Discord.Mentions, " "),
		slkHook:      c.Chains[chainName].Alerts.Slack.Webhook,
	}
	alarms.notifyMux.Lock()
	key := chainName + "\x00" + message
	if alarms.AllAlarms[chainName] == nil {
		alarms.AllAlarms[chainName] = make(map[string]time.Time)
	}
	if resolved && !alarms.AllAlarms[chainName][message].IsZero() {
		delete(alarms.AllAlarms[chainName], message)
	} else if !resolved && alarms.AllAlarms[chainName][message].IsZero() {
		alarms.AllAlarms[chainName][message] = time.Now()
	}
	if resolved && (alarms.sentAnywhere(key, message) || alarms.inFlightAnywhere(key)) {
		alarms.PendingRecoveries[key] = &pendingRecovery{Chain: chainName, Message: message, Severity: severity, ID: uniq}
	}
	alarms.notifyMux.Unlock()
	c.alertChan <- a
	c.chainsMux.RUnlock()
}

// watch handles monitoring for missed blocks, stalled chain, node downtime
// and also updates a few prometheus stats
// FIXME: not watching for nodes that are lagging the head block!
func (cc *ChainConfig) watch() {
	valInfo, _ := cc.validatorState()
	var missedAlarm, pctAlarm, noNodes bool
	nodeAlarms := make(map[string]bool)

	// wait until we have a moniker:
	noNodesSec := 0 // delay a no-nodes alarm for 30 seconds, too noisy.
	for {
		select {
		case <-td.ctx.Done():
			return
		default:
		}
		valInfo, _ = cc.validatorState()
		if valInfo == nil || valInfo.Moniker == "not connected" {
			time.Sleep(time.Second)
			if cc.Alerts.AlertIfNoServers && !noNodes && cc.hasNoNodes() && noNodesSec >= 60*td.NodeDownMin {
				noNodes = true
				td.alert(
					cc.name,
					fmt.Sprintf("no RPC endpoints are working for %s", cc.ChainId),
					"critical",
					false,
					&valInfo.Valcons,
				)
			}
			noNodesSec += 1
			continue
		}
		noNodesSec = 0
		break
	}
	missedAlarm = alarmIsActive(cc.name, fmt.Sprintf("%s has missed %d blocks on %s", valInfo.Moniker, cc.Alerts.ConsecutiveMissed, cc.ChainId))
	pctAlarm = alarmIsActive(cc.name, fmt.Sprintf("%s has missed > %d%% of the slashing window's blocks on %s", valInfo.Moniker, cc.Alerts.Window, cc.ChainId))
	cc.lastBlockAlarm = alarmIsActive(cc.name, fmt.Sprintf("stalled: have not seen a new block on %s in %d minutes", cc.ChainId, cc.Alerts.Stalled))
	noNodes = alarmIsActive(cc.name, fmt.Sprintf("no RPC endpoints are working for %s", cc.ChainId))
	inactiveMessage := activeMessage(cc.name, " is no longer active: validator is ")
	for _, node := range cc.Nodes {
		message := fmt.Sprintf("Severity: %s\nRPC node %s has been down for > %d minutes on %s", td.NodeDownSeverity, node.Url, td.NodeDownMin, cc.ChainId)
		nodeAlarms[node.Url] = alarmIsActive(cc.name, message)
	}
	reconcile := true
	// initial stat creation for nodes, we only update again if the node is positive
	if td.Prom {
		for _, node := range cc.Nodes {
			td.statsChan <- cc.mkUpdate(metricNodeDownSeconds, 0, node.Url)
		}
	}

	for {
		time.Sleep(2 * time.Second)
		select {
		case <-td.ctx.Done():
			return
		default:
		}
		valInfo, _ = cc.validatorState()
		lastBlockTime, observedBlock, consecutiveMiss := cc.blockState()
		unavailable := cc.hasNoNodes()

		// alert if we can't monitor
		if reconcile && noNodes && unavailable {
			td.alert(cc.name, fmt.Sprintf("no RPC endpoints are working for %s", cc.ChainId), "critical", false, &valInfo.Valcons)
		}
		switch {
		case cc.Alerts.AlertIfNoServers && !noNodes && unavailable:
			noNodesSec += 2
			if noNodesSec < 60*td.NodeDownMin {
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
					&valInfo.Valcons,
				)
			}
		case cc.Alerts.AlertIfNoServers && noNodes && !unavailable:
			noNodes = false
			td.alert(
				cc.name,
				fmt.Sprintf("no RPC endpoints are working for %s", cc.ChainId),
				"critical",
				true,
				&valInfo.Valcons,
			)
		default:
			noNodesSec = 0
		}

		// stalled chain detection
		if cc.Alerts.StalledAlerts && (!cc.lastBlockAlarm || reconcile) && !lastBlockTime.IsZero() &&
			lastBlockTime.Before(time.Now().Add(time.Duration(-cc.Alerts.Stalled)*time.Minute)) {

			// chain is stalled send an alert!
			cc.lastBlockAlarm = true
			td.alert(
				cc.name,
				fmt.Sprintf("stalled: have not seen a new block on %s in %d minutes", cc.ChainId, cc.Alerts.Stalled),
				"critical",
				false,
				&valInfo.Valcons,
			)
		} else if cc.Alerts.StalledAlerts && cc.lastBlockAlarm && !lastBlockTime.IsZero() &&
			lastBlockTime.After(time.Now().Add(time.Duration(-cc.Alerts.Stalled)*time.Minute)) {
			cc.lastBlockAlarm = false
			td.alert(
				cc.name,
				fmt.Sprintf("stalled: have not seen a new block on %s in %d minutes", cc.ChainId, cc.Alerts.Stalled),
				"info",
				true,
				&valInfo.Valcons,
			)
		}

		// inactive status must also be reconciled after a restart.
		if cc.Alerts.AlertIfInactive {
			id := valInfo.Valcons + "jailed"
			if !valInfo.Bonded {
				reason := "inactive"
				if valInfo.Tombstoned {
					reason = "☠️ tombstoned 🪦"
				} else if valInfo.Jailed {
					reason = "jailed"
				}
				message := fmt.Sprintf("%s is no longer active: validator is %s", valInfo.Moniker, reason)
				if inactiveMessage != "" {
					message = inactiveMessage
				}
				if inactiveMessage == "" || reconcile {
					td.alert(cc.name, message, "critical", false, &id)
				}
				inactiveMessage = message
			} else if inactiveMessage != "" {
				td.alert(cc.name, inactiveMessage, "info", true, &id)
				inactiveMessage = ""
			}
		}

		// consecutive missed block alarms:
		if (!missedAlarm || reconcile) && cc.Alerts.ConsecutiveAlerts && int(consecutiveMiss) >= cc.Alerts.ConsecutiveMissed {
			// alert on missed block counter!
			missedAlarm = true
			id := valInfo.Valcons + "consecutive"
			td.alert(
				cc.name,
				fmt.Sprintf("%s has missed %d blocks on %s", valInfo.Moniker, cc.Alerts.ConsecutiveMissed, cc.ChainId),
				cc.Alerts.ConsecutivePriority,
				false,
				&id,
			)
		} else if missedAlarm && observedBlock && int(consecutiveMiss) < cc.Alerts.ConsecutiveMissed {
			// clear the alert
			missedAlarm = false
			id := valInfo.Valcons + "consecutive"
			td.alert(
				cc.name,
				fmt.Sprintf("%s has missed %d blocks on %s", valInfo.Moniker, cc.Alerts.ConsecutiveMissed, cc.ChainId),
				"info",
				true,
				&id,
			)
		}

		// window percentage missed block alarms
		if cc.Alerts.PercentageAlerts && valInfo.Window > 0 && (!pctAlarm || reconcile) &&
			100*float64(valInfo.Missed)/float64(valInfo.Window) > float64(cc.Alerts.Window) {
			// alert on missed block counter!
			pctAlarm = true
			id := valInfo.Valcons + "percent"
			td.alert(
				cc.name,
				fmt.Sprintf("%s has missed > %d%% of the slashing window's blocks on %s", valInfo.Moniker, cc.Alerts.Window, cc.ChainId),
				cc.Alerts.PercentagePriority,
				false,
				&id,
			)
		} else if cc.Alerts.PercentageAlerts && valInfo.Window > 0 && pctAlarm &&
			100*float64(valInfo.Missed)/float64(valInfo.Window) <= float64(cc.Alerts.Window) {
			// clear the alert
			pctAlarm = false
			id := valInfo.Valcons + "percent"
			td.alert(
				cc.name,
				fmt.Sprintf("%s has missed > %d%% of the slashing window's blocks on %s", valInfo.Moniker, cc.Alerts.Window, cc.ChainId),
				"info",
				true,
				&id,
			)
		}

		// node down alarms
		for _, node := range cc.Nodes {
			status := node.snapshot()
			// window percentage missed block alarms
			if node.AlertIfDown && status.down && !status.downSince.IsZero() &&
				time.Since(status.downSince) > time.Duration(td.NodeDownMin)*time.Minute {
				// alert on dead node
				if nodeAlarms[node.Url] && !reconcile {
					continue
				}
				nodeAlarms[node.Url] = true // used to keep active alert count correct
				td.alert(
					cc.name,
					fmt.Sprintf("Severity: %s\nRPC node %s has been down for > %d minutes on %s", td.NodeDownSeverity, node.Url, td.NodeDownMin, cc.ChainId),
					td.NodeDownSeverity,
					false,
					&node.Url,
				)
			} else if node.AlertIfDown && !status.down && nodeAlarms[node.Url] {
				// clear the alert
				nodeAlarms[node.Url] = false
				node.clearRecovery()
				td.alert(
					cc.name,
					fmt.Sprintf("Severity: %s\nRPC node %s has been down for > %d minutes on %s", td.NodeDownSeverity, node.Url, td.NodeDownMin, cc.ChainId),
					"info",
					true,
					&node.Url,
				)
			}
		}

		if td.Prom {
			// raw block timer, ignoring finalized state
			if !lastBlockTime.IsZero() {
				td.statsChan <- cc.mkUpdate(metricLastBlockSecondsNotFinal, time.Since(lastBlockTime).Seconds(), "")
			}
			// update node-down times for prometheus
			for _, node := range cc.Nodes {
				status := node.snapshot()
				if status.down && !status.downSince.IsZero() {
					td.statsChan <- cc.mkUpdate(metricNodeDownSeconds, time.Since(status.downSince).Seconds(), node.Url)
				}
			}
		}
		if td.EnableDash {
			select {
			case td.updateChan <- cc.dashboardStatus():
			case <-td.ctx.Done():
				return
			}
		}
		reconcile = false
	}
}
