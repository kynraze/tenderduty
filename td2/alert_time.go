package tenderduty

import "time"

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
