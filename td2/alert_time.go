package tenderduty

import (
	"strings"
	"time"
)

// Convert display text only; alarm keys and stored resolutions stay in UTC.
func localAlertTimes(body string) string {
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		for _, prefix := range []string{"Ends ", "Can unjail after ", "Around ", "Voted, voting ends ", "Voting ended "} {
			value, ok := strings.CutPrefix(line, prefix)
			if !ok {
				continue
			}
			stamp, suffix, hasSuffix := strings.Cut(value, " · ")
			at, err := time.Parse(govEndFormat, stamp)
			if err != nil {
				break
			}
			lines[i] = prefix + at.In(alertLocation()).Format("2 Jan 2006 15:04 MST")
			if hasSuffix {
				lines[i] += " · " + suffix
			}
			break
		}
	}
	return strings.Join(lines, "\n")
}

func alertLocation() *time.Location {
	settings := td.settings()
	loc, err := settings.Reports.location(settings.QuietHours.Timezone)
	if err != nil {
		return time.UTC
	}
	return loc
}

func alertDate(at time.Time, loc *time.Location) string {
	local := at.In(loc)
	zone, _ := local.Zone()
	if zone == "" {
		return local.Format("Mon 2 Jan 15:04 -0700")
	}
	return local.Format("Mon 2 Jan 15:04 MST")
}

func formatUpgradeETA(eta, now time.Time, loc *time.Location) string {
	line := "≈ " + alertDate(eta, loc)
	if eta.After(now) {
		line += ", in " + reportDuration(eta.Sub(now))
	}
	return line
}
