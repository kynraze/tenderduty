package tenderduty

import (
	"fmt"
	"strings"
	"time"
)

// escalationCheckInterval is how often unresolved alarms are checked.
const escalationCheckInterval = 30 * time.Second

// EscalationConfig sends an alarm that needs action and is still active to one more destination.
type EscalationConfig struct {
	Enabled      bool   `yaml:"enabled"`
	AfterMinutes int    `yaml:"after_minutes"`
	To           string `yaml:"to"`
	MinSeverity  string `yaml:"min_severity"`
}

var escalationDests = map[string]notifyDest{"pagerduty": pd, "telegram": tg, "discord": di, "slack": slk, "gotify": gt}

// validateEscalation switches escalation off, with a warning, when it has nowhere to send.
func validateEscalation(c *Config) (problems []string) {
	e := &c.Escalation
	if !e.Enabled {
		return nil
	}
	e.To, e.MinSeverity = strings.ToLower(strings.TrimSpace(e.To)), strings.ToLower(strings.TrimSpace(e.MinSeverity))
	if e.AfterMinutes == 0 {
		e.AfterMinutes = 30
	}
	if e.AfterMinutes < 1 || e.AfterMinutes > 1440 {
		e.Enabled = false
		return append(problems, "warning: escalation after_minutes must be between 1 and 1440, escalation is off")
	}
	if e.MinSeverity == "" {
		e.MinSeverity = "critical"
	}
	if severityRank(e.MinSeverity) >= 4 && e.MinSeverity != "critical" {
		problems = append(problems, fmt.Sprintf("warning: escalation min_severity %q is not info, warning, error or critical, using critical", e.MinSeverity))
		e.MinSeverity = "critical"
	}
	dest, known := escalationDests[e.To]
	switch {
	case !known:
		e.Enabled = false
		problems = append(problems, fmt.Sprintf("warning: escalation to %q is not pagerduty, telegram, discord, slack or gotify, escalation is off", e.To))
	case !escalationReady(c, dest):
		e.Enabled = false
		problems = append(problems, fmt.Sprintf("warning: escalation to %s needs that destination enabled with its credentials, escalation is off", e.To))
	}
	return problems
}

// escalationReady tells if a destination is enabled with its credentials in the top level settings.
func escalationReady(c *Config, dest notifyDest) bool {
	switch dest {
	case pd:
		return on(c.Pagerduty.Enabled) && c.Pagerduty.ApiKey != ""
	case tg:
		return on(c.Telegram.Enabled) && c.Telegram.ApiKey != "" && c.Telegram.Channel != ""
	case di:
		return on(c.Discord.Enabled) && validHTTPURL(c.Discord.Webhook)
	case slk:
		return on(c.Slack.Enabled) && validHTTPURL(c.Slack.Webhook)
	case gt:
		return on(c.Gotify.Enabled) && validHTTPURL(c.Gotify.URL) && c.Gotify.Token != ""
	}
	return false
}

func (c *Config) runEscalations() {
	for waitContext(c.context(), escalationCheckInterval) {
		c.checkEscalations(time.Now())
	}
}

// checkEscalations queues each unresolved alarm that is due. The sent maps and the outbox keep it to one send.
func (c *Config) checkEscalations(now time.Time) {
	esc := c.settings().Escalation
	dest, known := escalationDests[esc.To]
	if !esc.Enabled || !known {
		return
	}
	type unresolved struct {
		chain, message, severity string
		started                  time.Time
	}
	var candidates []unresolved
	alarms.notifyMux.Lock()
	for key, started := range alarms.Escalated {
		chain, message, _ := strings.Cut(key, "\x00")
		if !alarms.AllAlarms[chain][message].Equal(started) {
			delete(alarms.Escalated, key) // resolved, or raised again since
			delete(alarms.EscalationsSent, key)
		}
	}
	for chain, active := range alarms.AllAlarms {
		for message, started := range active {
			key := chain + "\x00" + message
			severity := alarms.AlarmSeverities[key]
			if started.IsZero() || severityRank(severity) < severityRank(esc.MinSeverity) || !alarms.Escalated[key].IsZero() {
				continue
			}
			candidates = append(candidates, unresolved{chain, message, severity, started})
		}
	}
	alarms.notifyMux.Unlock()
	for _, item := range candidates {
		c.chainsMux.RLock()
		cc := c.Chains[item.chain]
		c.chainsMux.RUnlock()
		// a scheduled signal upgrade can be days away; its "is close" reminder escalates
		if cc == nil || !cc.urgentAlarm(item.message, item.severity) || isSignalUpgrade(item.message) && !strings.HasSuffix(item.message, " is close") {
			continue
		}
		cc.stateMux.RLock()
		retiring := cc.retiring
		cc.stateMux.RUnlock()
		if retiring {
			continue
		}
		// an alarm restored from before the start waits for the monitor to check it, at most 10 minutes
		if item.started.Before(processStarted) && !cc.alarmsChecked() && now.Before(processStarted.Add(10*time.Minute)) {
			continue
		}
		after := time.Duration(esc.AfterMinutes) * time.Minute
		// a close to jail alarm escalates before the validator is jailed: at half the jail risk minutes,
		// or once it has lasted as long as the time left to jail
		if strings.Contains(item.message, " is close to jail on ") {
			if half := time.Duration(cc.alertSettings().JailRiskMinutes) * time.Minute / 2; half < after {
				after = half
			}
			if left, known := cc.timeToJail(); known && left < after {
				after = max(left, 0)
			}
		}
		if now.Sub(item.started) < after {
			continue
		}
		id := c.alarmID(item.chain, item.message)
		msg := c.escalationAlert(dest, item.chain, item.message, item.severity, &id)
		if msg == nil {
			continue
		}
		alarms.notifyMux.Lock()
		// the alarm may have resolved while the message was built
		if alarms.AllAlarms[item.chain][item.message].Equal(item.started) {
			alarms.enqueue(msg)
			if alarms.Escalated == nil {
				alarms.Escalated = make(map[string]time.Time)
			}
			alarms.Escalated[item.chain+"\x00"+item.message] = item.started
		}
		alarms.notifyMux.Unlock()
		if err := c.persistState(); err != nil {
			l("could not persist alert:", err)
		}
		select {
		case c.alertChan <- msg:
		default:
		}
	}
}

// escalationAlert is a raise for one destination only. Its own min_severity doesn't apply, the alarm already passed the escalation's.
func (c *Config) escalationAlert(dest notifyDest, chain, message, severity string, id *string) *alertMsg {
	msg := c.makeAlert(chain, message, severity, false, id)
	if msg == nil {
		return nil
	}
	c.chainsMux.RLock()
	cc := c.Chains[chain]
	c.chainsMux.RUnlock()
	if cc == nil {
		return nil
	}
	alerts, settings := cc.alertSettings(), c.settings()
	msg.pd, msg.tg, msg.disc, msg.slk, msg.gotify = false, false, false, false, false
	switch dest {
	case pd:
		msg.pd = enabledFor(settings.Pagerduty.Enabled, alerts.Pagerduty.Enabled)
	case tg:
		msg.tg = enabledFor(settings.Telegram.Enabled, alerts.Telegram.Enabled)
	case di:
		msg.disc = enabledFor(settings.Discord.Enabled, alerts.Discord.Enabled)
	case slk:
		msg.slk = enabledFor(settings.Slack.Enabled, alerts.Slack.Enabled)
	case gt:
		msg.gotify = enabledFor(settings.Gotify.Enabled, alerts.Gotify.Enabled)
	}
	msg.escalated, msg.escalatedAt = true, alarmStarted(chain, message)
	return msg
}

// queuedAlert rebuilds the alert for an outbox item, an escalation keeps its one destination.
func (c *Config) queuedAlert(item queuedNotification) *alertMsg {
	if item.Escalated && !item.Resolved {
		return c.escalationAlert(item.Destination, item.Chain, item.Message, item.Severity, &item.ID)
	}
	return c.makeAlert(item.Chain, item.Message, item.Severity, item.Resolved, &item.ID)
}

// processStarted tells restored alarms from new ones.
var processStarted = time.Now()

// timeToJail is how long missing every block takes to jail, when the window and block time are known.
func (cc *ChainConfig) timeToJail() (time.Duration, bool) {
	info, _ := cc.validatorState()
	cc.stateMux.RLock()
	blockTime := cc.blockTime
	cc.stateMux.RUnlock()
	if info == nil || info.MaxMissed <= 0 || blockTime <= 0 {
		return 0, false
	}
	return time.Duration(info.missesBeforeJail()) * blockTime, true
}

func (cc *ChainConfig) alarmsChecked() bool {
	cc.stateMux.RLock()
	defer cc.stateMux.RUnlock()
	return cc.restoredChecked
}

func alarmStarted(chain, message string) time.Time {
	alarms.notifyMux.RLock()
	defer alarms.notifyMux.RUnlock()
	return alarms.AllAlarms[chain][message]
}
