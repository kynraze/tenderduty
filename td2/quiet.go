package tenderduty

import (
	"fmt"
	"sync"
	"time"
)

// QuietHoursConfig holds back alerts below a severity during part of the day, they go out when it ends if they are
// still active.
type QuietHoursConfig struct {
	Enabled     bool   `yaml:"enabled"`
	Start       string `yaml:"start"`    // "23:00"
	End         string `yaml:"end"`      // "07:00"
	Timezone    string `yaml:"timezone"` // IANA name, UTC when empty
	MinSeverity string `yaml:"min_severity"`

	location   *time.Location
	start, end int // minutes after midnight
}

// heldAlert is an alarm raised during quiet hours that wasn't sent yet.
type heldAlert struct {
	chain, message, severity, id string
}

var (
	heldMux sync.Mutex
	held    = make(map[string]heldAlert) // by chain and message
)

// parse checks the settings and readies them, quiet hours stay off when they don't make sense.
func (q *QuietHoursConfig) parse() error {
	if !q.Enabled {
		return nil
	}
	var err error
	if q.location, err = time.LoadLocation(q.Timezone); err != nil {
		return fmt.Errorf("quiet_hours timezone %q: %w", q.Timezone, err)
	}
	for _, t := range []struct {
		value string
		into  *int
	}{{q.Start, &q.start}, {q.End, &q.end}} {
		parsed, err := time.Parse("15:04", t.value)
		if err != nil {
			return fmt.Errorf("quiet_hours time %q is not HH:MM", t.value)
		}
		*t.into = parsed.Hour()*60 + parsed.Minute()
	}
	if q.start == q.end {
		return fmt.Errorf("quiet_hours start and end are the same")
	}
	if q.MinSeverity == "" {
		q.MinSeverity = "critical"
	}
	return nil
}

// active says if now is in quiet hours, a window past midnight like 23:00 to 07:00 wraps around.
func (q *QuietHoursConfig) active(now time.Time) bool {
	if !q.Enabled || q.location == nil {
		return false
	}
	local := now.In(q.location)
	minute := local.Hour()*60 + local.Minute()
	if q.start < q.end {
		return minute >= q.start && minute < q.end
	}
	return minute >= q.start || minute < q.end
}

// holds says if an alert waits for quiet hours to end, a recovery is never held.
func (q *QuietHoursConfig) holds(severity string, resolved bool, now time.Time) bool {
	return !resolved && q.active(now) && severityRank(severity) < severityRank(q.MinSeverity)
}

// holdsAlert says if an alert waits for quiet hours to end.
func (c *Config) holdsAlert(chain, severity string, resolved bool, now time.Time) bool {
	return c.QuietHours.holds(severity, resolved, now)
}

// releaseHeld sends the held alerts that no longer wait and are still active.
func (c *Config) releaseHeld() {
	now := time.Now()
	heldMux.Lock()
	pending := make([]heldAlert, 0)
	for key, alert := range held {
		if !c.holdsAlert(alert.chain, alert.severity, false, now) {
			pending = append(pending, alert)
			delete(held, key)
		}
	}
	heldMux.Unlock()
	sent := 0
	for _, alert := range pending {
		if alarmIsActive(alert.chain, alert.message) {
			id := alert.id
			c.alert(alert.chain, alert.message, alert.severity, false, &id)
			sent++
		}
	}
	if len(pending) > 0 {
		l(fmt.Sprintf("🌅 sent %d held alerts that are still active, %d cleared while held", sent, len(pending)-sent))
	}
}
