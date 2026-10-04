package tenderduty

import (
	"fmt"
	"html"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type reportRow struct {
	Name    string   `json:"name"`
	Moniker string   `json:"moniker,omitempty"`
	Dot     string   `json:"dot"`
	Testnet bool     `json:"testnet,omitempty"`
	Summary string   `json:"summary,omitempty"`
	Lines   []string `json:"lines,omitempty"`
	Fibre   []string `json:"fibre,omitempty"`
}

type periodReport struct {
	Title  string      `json:"title"`
	Header []string    `json:"header"`
	Rows   []reportRow `json:"rows"`
	Notes  []string    `json:"notes,omitempty"`
	Footer string      `json:"footer"`
	Weekly bool        `json:"weekly,omitempty"`
}

func compactReportNumber(n float64) string {
	if math.IsNaN(n) || math.IsInf(n, 0) {
		return "unavailable"
	}
	a := math.Abs(n)
	for _, unit := range []struct {
		scale  float64
		suffix string
	}{{1e12, "T"}, {1e9, "B"}, {1e6, "M"}, {1e3, "K"}} {
		if a >= unit.scale {
			v := n / unit.scale
			if a < 10*unit.scale {
				return fmt.Sprintf("%.2f%s", v, unit.suffix)
			}
			return strings.TrimSuffix(fmt.Sprintf("%.1f", v), ".0") + unit.suffix
		}
	}
	return strconv.FormatFloat(n, 'f', 0, 64)
}

func reportChange(n float64) string {
	if n == 0 {
		return "="
	}
	if n < 0 {
		return "−" + compactReportNumber(-n)
	}
	return "+" + compactReportNumber(n)
}

func reportPowerChange(current, previous *int64) string {
	if current == nil || previous == nil {
		return ""
	}
	delta := float64(*current) - float64(*previous)
	if delta == 0 {
		return "="
	}
	change := reportChange(delta)
	if *previous > 0 {
		change += fmt.Sprintf(" · %s%.1f%%", map[bool]string{true: "+", false: "−"}[delta > 0], math.Abs(100*delta/float64(*previous)))
	}
	return change
}

func reportRankTrend(current, previous *int) string {
	if current == nil || previous == nil || *current <= 0 || *previous <= 0 {
		return ""
	}
	if *current == *previous {
		return " ="
	}
	if *current < *previous {
		return fmt.Sprintf(" ▲%d", *previous-*current)
	}
	return fmt.Sprintf(" ▼%d", *current-*previous)
}

func reportSigningTrend(current, previous reportHour) string {
	value, have := reportSigned(current)
	prior, had := reportSigned(previous)
	if !have || !had {
		return ""
	}
	delta := value - prior
	if math.Abs(delta) < .005 {
		return " ="
	}
	if delta > 0 {
		return fmt.Sprintf(" ▲%.2f", delta)
	}
	return fmt.Sprintf(" ▼%.2f", -delta)
}

func reportDot(active, jailed bool, signed float64, known bool, incidents int, threshold float64) string {
	if active || jailed || (known && signed < threshold) {
		return dotRed
	}
	if incidents > 0 || (known && signed < reportYellowSigned) {
		return dotYellow
	}
	return dotGreen
}

func reportDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	minutes := int64(d / time.Minute)
	days, hours, mins := minutes/1440, minutes%1440/60, minutes%60
	parts := make([]string, 0, 3)
	if days > 0 {
		parts = append(parts, fmt.Sprintf("%dd", days))
	}
	if hours > 0 {
		parts = append(parts, fmt.Sprintf("%dh", hours))
	}
	if mins > 0 || len(parts) == 0 {
		parts = append(parts, fmt.Sprintf("%dm", mins))
	}
	return strings.Join(parts, " ")
}

func reportIncidents(history []reportIncident, chain string, start, end time.Time) ([]string, int, time.Duration) {
	lines := make([]string, 0)
	count := 0
	longest := time.Duration(0)
	for _, incident := range history {
		if incident.Chain != chain || !incident.Start.Before(end) || (!incident.End.IsZero() && !incident.End.After(start)) {
			continue
		}
		count++
		from, to := incident.Start, incident.End
		if from.Before(start) {
			from = start
		}
		if to.IsZero() || to.After(end) {
			to = end
		}
		length := to.Sub(from)
		if length > longest {
			longest = length
		}
		lines = append(lines, clipRunes(incident.Kind, 60)+" "+reportDuration(length))
	}
	return lines, count, longest
}

func reportDelegatorChange(current, previous *reportSnapshot) string {
	if current == nil || previous == nil || current.Delegators == nil || previous.Delegators == nil {
		return ""
	}
	return reportChange(float64(*current.Delegators) - float64(*previous.Delegators))
}

func (c *Config) buildReport(now time.Time, weekly, late bool, include map[string]bool) periodReport {
	settings := c.settings()
	loc, err := settings.Reports.location(settings.QuietHours.Timezone)
	if err != nil {
		loc = time.UTC
	}
	span := 24 * time.Hour
	icon, title := "📊", "Daily report"
	date := now.In(loc).Format("2 Jan 2006")
	if weekly {
		span = 7 * 24 * time.Hour
		icon, title = "📈", "Weekly report"
		date = now.In(loc).AddDate(0, 0, -6).Format("2 Jan") + " – " + now.In(loc).Format("2 Jan")
	}
	start := now.Add(-span)
	result := periodReport{Title: title, Weekly: weekly}
	result.Header = []string{icon + " " + title + " · " + date}
	if late {
		result.Header[0] += " (late)"
	}
	history := c.savedReports(now)
	c.chainsMux.RLock()
	chains := make(map[string]*ChainConfig, len(c.Chains))
	for name, cc := range c.Chains {
		chains[name] = cc
	}
	c.chainsMux.RUnlock()
	watched := span.Seconds()
	healthy, incidents, votes, due := 0, 0, 0, 0
	dueNames := make([]string, 0)
	for _, name := range sortedReportNames(chains) {
		cc := chains[name]
		testnet := cc.reportIsTestnet()
		if (include != nil && !include[name]) || (testnet && settings.Reports.IncludeTestnets != nil && !*settings.Reports.IncludeTestnets) {
			continue
		}
		total := reportTotals(history.Hours[name], start, now)
		previousTotal := reportTotals(history.Hours[name], start.Add(-span), start)
		current := reportSnapshotAt(history.Snapshots[name], now)
		previous := reportSnapshotAt(history.Snapshots[name], start)
		info, _ := cc.validatorState()
		alerts := cc.alertSettings()
		threshold := reportDefaultRedSigned
		if alerts.PercentageAlerts && alerts.Window > 0 {
			threshold = 100 - float64(alerts.Window)
		}
		alertLines, count, longest := reportIncidents(history.Incidents, name, start, now)
		signed, known := reportSigned(total)
		dot := reportDot(alarms.getCount(name) > 0, info.Jailed || info.Tombstoned, signed, known, count, threshold)
		if dot == dotGreen {
			healthy++
		}
		incidents += count
		votes += total.Votes
		watched = math.Min(watched, total.Watched)
		row := reportRow{Name: clipRunes(name, 120), Moniker: clipRunes(info.Moniker, 100), Dot: dot, Testnet: testnet}
		signing := "unavailable"
		if known {
			signing = fmt.Sprintf("%.2f%%", signed)
		}
		power, rank, powerChange := "unavailable", "", ""
		if current != nil {
			if current.Power != nil {
				power = compactReportNumber(float64(*current.Power))
			}
			if previous != nil {
				powerChange = reportPowerChange(current.Power, previous.Power)
			}
			if current.Rank != nil && *current.Rank > 0 {
				rank = fmt.Sprintf("#%d", *current.Rank)
				if previous != nil {
					rank += reportRankTrend(current.Rank, previous.Rank)
				}
			}
		}
		delegators, self := "unavailable", "unavailable"
		delegatorChange := reportDelegatorChange(current, previous)
		if current != nil {
			if current.Delegators != nil {
				delegators = compactReportNumber(float64(*current.Delegators))
			}
			if current.Self != nil {
				self = compactReportNumber(*current.Self)
				if previous != nil && previous.Self != nil {
					self += " (" + reportChange(*current.Self-*previous.Self) + ")"
				}
			}
		}
		if weekly {
			row.Summary = signing + reportSigningTrend(total, previousTotal)
			if rank != "" {
				row.Summary += " · " + rank
			}
			if powerChange == "" {
				powerChange = power
			}
			if delegatorChange == "" {
				delegatorChange = delegators
			}
			line := fmt.Sprintf("⚖️ %s · 🤝 %s · 🔔 %d", powerChange, delegatorChange, count)
			if longest > 0 {
				line += " · longest " + reportDuration(longest)
			}
			row.Lines = []string{line}
		} else {
			// blocks proposed are routine, a missed proposal is what the operator acts on
			line := fmt.Sprintf("✍️ %s signed · %s missed", signing, compactReportNumber(float64(total.Missed)))
			if total.MissedProps > 0 {
				line += " · " + plural(int(total.MissedProps), "missed proposal")
			}
			row.Lines = append(row.Lines, line)
			line = "⚖️ " + power
			if powerChange != "" {
				line += " (" + powerChange + ")"
			}
			if rank != "" {
				line += " · " + rank
			}
			row.Lines = append(row.Lines, line)
			delegatorsLine := "🤝 " + delegators + " delegators"
			if delegatorChange != "" {
				delegatorsLine += " (" + delegatorChange + ")"
			}
			row.Lines = append(row.Lines, delegatorsLine+" · self "+self)
			if len(alertLines) == 0 {
				row.Lines = append(row.Lines, "🔔 No alerts")
			} else {
				for _, line := range alertLines {
					row.Lines = append(row.Lines, "🔔 "+line)
				}
			}
		}
		proposals, loaded := cc.govSnapshot()
		if loaded {
			for _, p := range proposals {
				if p.Voted || !p.VotingEnd.After(now) {
					continue
				}
				due++
				dueNames = append(dueNames, fmt.Sprintf("%s #%d", name, p.ID))
				if !weekly {
					row.Lines = append(row.Lines, fmt.Sprintf("🗳 #%d %s · %s left", p.ID, clipRunes(p.Title, 80), reportDuration(p.VotingEnd.Sub(now))))
				}
			}
		}
		cc.stateMux.RLock()
		var plan *upgradePlan
		if cc.upgrade != nil {
			copy := *cc.upgrade
			plan = &copy
		} else if cc.signal.UpgradeHeight > 0 {
			plan = &upgradePlan{Name: fmt.Sprintf("v%d", cc.signal.UpgradeVersion), Height: cc.signal.UpgradeHeight}
		}
		height, blockTime := cc.lastBlockNum, cc.blockTime
		cc.stateMux.RUnlock()
		if plan != nil && plan.Height > height {
			line := "⬆️ " + clipRunes(plan.Name, 80) + " at " + thousands(plan.Height)
			if blockTime > 0 && plan.Height-height <= int64(math.MaxInt64)/int64(blockTime) {
				left := time.Duration(plan.Height-height) * blockTime
				if weekly {
					line = "⬆️ " + name + " " + clipRunes(plan.Name, 80) + " · " + now.Add(left).In(loc).Format("Mon ~15:04")
				} else {
					line += " · ~" + reportDuration(left)
				}
			}
			if weekly {
				result.Notes = append(result.Notes, line)
			} else {
				row.Lines = append(row.Lines, line)
			}
		}
		if total.Watched < span.Seconds()-60 {
			row.Lines = append(row.Lines, "Watched "+reportWatchedDuration(time.Duration(total.Watched)*time.Second)+" of "+reportWatchedDuration(span))
		}
		result.Rows = append(result.Rows, row)
	}
	if len(result.Rows) == 0 {
		watched = 0
	}
	if weekly {
		result.Header = append(result.Header, fmt.Sprintf("%d incidents · %d votes cast", incidents, votes))
		note := fmt.Sprintf("🗳 %d cast · %d due", votes, due)
		if len(dueNames) > 0 {
			note += " (" + strings.Join(dueNames, ", ") + ")"
		}
		result.Notes = append(splitReportText(note, 900), result.Notes...)
	} else {
		result.Header = append(result.Header, fmt.Sprintf("🟢 %d healthy · 🔴 %d needs attention", healthy, len(result.Rows)-healthy))
	}
	result.Footer = "Watched " + reportWatchedDuration(time.Duration(watched)*time.Second) + " of " + reportWatchedDuration(span) + " · " + loc.String()
	if watched < span.Seconds()-60 {
		result.Header[0] += " · Watched " + reportWatchedDuration(time.Duration(watched)*time.Second) + " of " + reportWatchedDuration(span)
	}
	return result
}

var reportBoldTime = regexp.MustCompile(`((?:\d+d )?(?:\d+h )?\d+[dhm]) left|~((?:\d+d )?(?:\d+h )?\d+[dhm])$`)

func reportHTMLLine(s string) string {
	s = html.EscapeString(s)
	return reportBoldTime.ReplaceAllStringFunc(s, func(value string) string {
		if strings.HasPrefix(value, "~") {
			return "~<b>" + strings.TrimPrefix(value, "~") + "</b>"
		}
		return "<b>" + strings.TrimSuffix(value, " left") + "</b> left"
	})
}

func (r periodReport) telegram() string {
	lines := make([]string, 0)
	for _, header := range r.Header {
		line := html.EscapeString(header)
		line = strings.Replace(line, r.Title, "<b>"+r.Title+"</b>", 1)
		lines = append(lines, line)
	}
	for _, testnet := range []bool{false, true} {
		group := false
		for _, row := range r.Rows {
			if row.Testnet != testnet {
				continue
			}
			if !group {
				lines = append(lines, "", "<b>"+map[bool]string{false: "MAINNET", true: "TESTNET"}[testnet]+"</b>")
				group = true
			}
			if !r.Weekly {
				lines = append(lines, "")
			}
			line := row.Dot + " <b>" + html.EscapeString(row.Name) + "</b>"
			if r.Weekly {
				line += " " + html.EscapeString(row.Summary)
			} else if row.Moniker != "" {
				line += " · " + html.EscapeString(row.Moniker)
			}
			lines = append(lines, line)
			for _, value := range append(append([]string(nil), row.Lines...), row.Fibre...) {
				prefix := ""
				if r.Weekly {
					prefix = "   "
				}
				lines = append(lines, prefix+reportHTMLLine(value))
			}
		}
	}
	if len(r.Notes) > 0 {
		lines = append(lines, "")
		for _, note := range r.Notes {
			lines = append(lines, reportHTMLLine(note))
		}
	}
	lines = append(lines, "", "<i>"+html.EscapeString(r.Footer)+"</i>")
	return strings.Join(lines, "\n")
}

func reportWatchedDuration(d time.Duration) string {
	if d == 24*time.Hour {
		return "24h"
	}
	return reportDuration(d)
}
