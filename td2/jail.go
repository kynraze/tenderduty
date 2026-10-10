package tenderduty

import (
	"fmt"
	"strings"
	"time"
)

// a missing blocks alarm resolves after this long without another run of consecutive_missed
var missedRecoverAfter = 2 * time.Minute

func (a AlertConfig) consecutiveEnabled() bool {
	return a.MissedBlocksAlerts && a.ConsecutiveAlerts
}

func (a AlertConfig) jailRiskEnabled() bool {
	return a.MissedBlocksAlerts
}

func (a AlertConfig) jailInactiveEnabled() bool {
	return a.MissedBlocksAlerts && a.AlertIfInactive
}

func (a AlertConfig) signingStageEnabled(stage string) bool {
	switch stage {
	case "missed":
		return a.consecutiveEnabled()
	case "risk":
		return a.jailRiskEnabled()
	case "unjail":
		return false
	default:
		return a.jailInactiveEnabled()
	}
}

func isJailAlarm(message string) bool {
	first, _, _ := strings.Cut(message, "\n")
	return strings.Contains(first, " is missing blocks on ") || strings.Contains(first, " is close to jail on ") || cardJailed.MatchString(first) ||
		cardInactive.MatchString(first) || cardLegacyInactive.MatchString(first) || canUnjail.MatchString(first) ||
		strings.Contains(first, " is tombstoned on ") || jailRisk.MatchString(first) || cardJailSoon.MatchString(first)
}

func jailStage(message string) string {
	switch {
	case strings.Contains(message, " is missing blocks on "):
		return "missed"
	case strings.Contains(message, " is close to jail on "), jailRisk.MatchString(message), cardJailSoon.MatchString(message):
		return "risk"
	case strings.Contains(message, " can unjail on "):
		return "unjail"
	case strings.Contains(message, " is jailed on "):
		return "jailed"
	default:
		return "inactive"
	}
}

func jailAlarmID(val *ValInfo, message string) string {
	switch {
	case cardMissed.MatchString(message):
		return val.Valcons + "consecutive"
	case percentageMissed.MatchString(message):
		return val.Valcons + "percent"
	case strings.Contains(message, " is missing blocks on "):
		return val.Valcons + "jail"
	case jailRisk.MatchString(message):
		return val.Valcons + "jail"
	case cardJailSoon.MatchString(message):
		return val.Valcons + "jailsoon"
	case strings.Contains(message, " is tombstoned on "):
		return val.Valcons + "tombstoned"
	case strings.Contains(message, " can unjail on "):
		return val.Valcons + "unjail"
	case strings.Contains(message, " is close to jail on "):
		return val.Valcons + "jail"
	default:
		return val.Valcons + "jailed"
	}
}

func (cc *ChainConfig) jailAlarms(val *ValInfo, consecutive float64, reconcile bool) {
	settings := cc.alertSettings()
	for message := range activeMessages(cc.name, "") {
		if legacySigningAlarm(message) || isJailAlarm(message) && !settings.signingStageEnabled(jailStage(message)) {
			id := td.alarmID(cc.name, message)
			if id == "" {
				id = jailAlarmID(val, message)
			}
			td.resolveQuietly(cc.name, message, &id)
		}
	}
	if val.ValidatorStale {
		return
	}
	cc.stateMux.Lock()
	if val.Jailed {
		if cc.jailedSince.IsZero() {
			cc.jailedSince = time.Now()
		}
	} else {
		cc.jailedSince = time.Time{}
	}
	cc.stateMux.Unlock()
	blockTime := cc.averageBlockTime()
	percent, minutes := settings.JailRiskPercent, settings.JailRiskMinutes
	if percent == 0 {
		percent = 90
	}
	if minutes == 0 {
		minutes = 60
	}
	threshold := settings.ConsecutiveMissed
	if threshold < 1 {
		threshold = 5
	}
	wanted, severity := "", "critical"
	out := !val.Bonded || val.Jailed || val.Tombstoned
	waitForSigning := val.Jailed && !val.Tombstoned && val.SigningStale && !val.SigningUpdatedAt.IsZero() && time.Since(val.SigningUpdatedAt) < 3*time.Minute
	if waitForSigning {
		return
	}
	if out && settings.jailInactiveEnabled() {
		switch {
		case val.Tombstoned:
			wanted = fmt.Sprintf("%s is no longer in the active set on %s", val.Moniker, cc.ChainId)
		case val.Jailed:
			wanted = fmt.Sprintf("%s is jailed on %s", val.Moniker, cc.ChainId)
		default:
			wanted = fmt.Sprintf("%s is no longer in the active set on %s", val.Moniker, cc.ChainId)
		}
	} else if !out && settings.jailRiskEnabled() && !val.SigningStale && val.MaxMissed > 0 {
		atRisk := 100*val.Missed >= int64(percent)*val.MaxMissed
		// a single miss counts once half the window is used, before that only a run of consecutive_missed
		soon := (consecutive >= float64(threshold) || consecutive > 0 && 2*val.Missed >= val.MaxMissed) && blockTime > 0 && time.Duration(val.missesBeforeJail())*blockTime <= time.Duration(minutes)*time.Minute
		if atRisk || soon {
			wanted = fmt.Sprintf("%s is close to jail on %s", val.Moniker, cc.ChainId)
			severity = "critical"
		}
	}
	if wanted == "" && !out && settings.consecutiveEnabled() && consecutive >= float64(threshold) {
		wanted = fmt.Sprintf("%s is missing blocks on %s", val.Moniker, cc.ChainId)
		severity = "warning"
	}
	cc.stateMux.Lock()
	if consecutive >= float64(threshold) {
		cc.signedSince = time.Time{}
	} else if cc.signedSince.IsZero() {
		cc.signedSince = time.Now()
	}
	clearFor := time.Since(cc.signedSince)
	cc.stateMux.Unlock()
	active := make(map[string]bool)
	for message := range activeMessages(cc.name, "") {
		if isJailAlarm(message) {
			active[message] = true
		}
	}
	if wanted == "" && val.SigningStale && (settings.consecutiveEnabled() || settings.jailRiskEnabled() || settings.jailInactiveEnabled()) {
		return
	}
	// keep close-to-jail state while missing, despite signing-info refreshes
	if !out && consecutive > 0 && (wanted == "" || jailStage(wanted) == "missed") {
		for message := range active {
			if strings.Contains(message, " is close to jail on ") {
				wanted, severity = message, "critical"
			}
		}
	}
	// Keep the same incident id across stages.
	id := val.Valcons + "jail"
	for message := range active {
		if !legacySigningAlarm(message) {
			if saved := td.alarmID(cc.name, message); saved != "" {
				id = saved
				break
			}
		}
	}
	best := ""
	for message := range active {
		if message == wanted {
			continue
		}
		if best == "" || jailStage(best) == "risk" && jailStage(message) != "risk" || jailStage(best) == "unjail" && jailStage(message) == "jailed" {
			best = message
		}
	}
	for message := range active {
		if message == wanted {
			continue
		}
		oldID := td.alarmID(cc.name, message)
		if oldID == "" {
			oldID = jailAlarmID(val, message)
		}
		if wanted != "" || message != best || out || !settings.signingStageEnabled(jailStage(message)) {
			td.resolveQuietly(cc.name, message, &oldID)
			continue
		}
		if wanted == "" && jailStage(message) == "missed" && settings.consecutiveEnabled() {
			if _, observed, _ := cc.blockState(); !observed {
				continue
			}
			if clearFor < missedRecoverAfter {
				continue
			}
		}
		text := "back in the active set"
		if jailStage(message) == "risk" || jailStage(message) == "missed" {
			text = fmt.Sprintf("signing again · %s misses left", thousands(val.missesBeforeJail()))
		}
		resolve(cc.name, message, resolution{"✅", "Recovered", text})
		td.alert(cc.name, message, "info", true, &oldID)
	}
	if wanted != "" && (!active[wanted] || reconcile) {
		td.alert(cc.name, wanted, severity, false, &id)
	}
}

func jailDetail(chain, message string) string {
	td.chainsMux.RLock()
	cc := td.Chains[chain]
	td.chainsMux.RUnlock()
	if cc == nil {
		return ""
	}
	val, _ := cc.validatorState()
	if val == nil {
		return ""
	}
	if strings.Contains(message, " is tombstoned on ") {
		return cc.slashSnapshot().detail(true, alertLocation())
	}
	switch jailStage(message) {
	case "missed":
		_, _, consecutive := cc.blockState()
		return fmt.Sprintf("%s in a row · %s misses left before jail", thousands(int64(consecutive)), thousands(val.missesBeforeJail()))
	case "risk":
		// the window isn't known until signing info is read
		if val.MaxMissed <= 0 {
			return ""
		}
		line := fmt.Sprintf("%s of %s misses used", thousands(val.Missed), thousands(val.MaxMissed))
		_, _, consecutive := cc.blockState()
		if blockTime := cc.averageBlockTime(); blockTime > 0 && consecutive > 0 {
			line += fmt.Sprintf(" · jailed in ~%d min at this rate", time.Duration(val.missesBeforeJail())*blockTime/time.Minute)
		}
		return line
	case "unjail":
		return "send the unjail tx"
	case "jailed":
		line := formatJailDetail(cc.jailSince(), val.JailedUntil, time.Now(), alertLocation())
		if detail := cc.slashSnapshot().detail(false, alertLocation()); detail != "" {
			line = strings.TrimSpace(line + "\n" + detail)
		}
		return line
	case "inactive":
		if val.Tombstoned {
			line := "tombstoned"
			if detail := cc.slashSnapshot().detail(true, alertLocation()); detail != "" {
				line += "\n" + detail
			}
			return line
		}
		if val.Unbonding {
			return "unbonding"
		}
		if val.MaxValidators > 0 {
			return fmt.Sprintf("not in the top %d by stake", val.MaxValidators)
		}
		if val.ActiveSet > 0 {
			return fmt.Sprintf("not in the top %d by stake", val.ActiveSet)
		}
		return "not in the active set"
	}
	return ""
}

func (cc *ChainConfig) jailSince() time.Time {
	cc.stateMux.RLock()
	defer cc.stateMux.RUnlock()
	return cc.jailedSince
}

func formatJailDetail(since, until, now time.Time, loc *time.Location) string {
	var parts []string
	if !since.IsZero() {
		parts = append(parts, "jailed since "+alertDate(since, loc))
	}
	if until.After(time.Unix(0, 0)) {
		unjail := "can unjail now"
		if until.After(now) {
			format := "15:04"
			if until.In(loc).Format("2006-01-02") != now.In(loc).Format("2006-01-02") {
				format = "2 Jan 15:04"
			}
			unjail = "can unjail from " + until.In(loc).Format(format)
		}
		parts = append(parts, unjail)
	}
	return strings.Join(parts, " · ")
}

func legacySigningAlarm(message string) bool {
	first, rest, _ := strings.Cut(message, "\n")
	if strings.HasPrefix(first, "Severity: ") {
		first, _, _ = strings.Cut(rest, "\n")
	}
	return cardMissed.MatchString(first) || percentageMissed.MatchString(first) || jailRisk.MatchString(first) || cardJailSoon.MatchString(first) || canUnjail.MatchString(first) || cardLegacyInactive.MatchString(first) || strings.Contains(message, "\nCan unjail after ")
}

func (c *Config) clearLegacySigningAlarms() {
	alarms.notifyMux.Lock()
	queue := alarms.Outbox[:0]
	for _, item := range alarms.Outbox {
		if legacySigningAlarm(item.Message) {
			if !item.Resolved {
				continue
			}
			item.Quiet = true
		}
		queue = append(queue, item)
	}
	alarms.Outbox = queue
	for _, recovery := range alarms.PendingRecoveries {
		if recovery != nil && legacySigningAlarm(recovery.Message) {
			recovery.Quiet = true
		}
	}
	alarms.notifyMux.Unlock()
	for name, cc := range c.Chains {
		for message := range activeMessages(name, "") {
			if legacySigningAlarm(message) {
				id := c.alarmID(name, message)
				if id == "" {
					id = jailAlarmID(&ValInfo{Valcons: cc.ValAddress}, message)
				}
				c.resolveQuietly(name, message, &id)
			}
		}
	}
}
