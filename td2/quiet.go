package tenderduty

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// QuietHoursConfig holds back alerts below MinSeverity during part of the day, they go out when it ends if still active.
type QuietHoursConfig struct {
	Enabled     bool   `yaml:"enabled"`
	Start       string `yaml:"start"`    // "23:00"
	End         string `yaml:"end"`      // "07:00"
	Timezone    string `yaml:"timezone"` // IANA name like Europe/Berlin, or a fixed offset like UTC+7, UTC when empty
	MinSeverity string `yaml:"min_severity"`

	location   *time.Location
	start, end int // minutes after midnight
}

type heldAlert struct {
	chain, message, severity, id string
}

var (
	heldMux sync.Mutex
	held    = make(map[string]heldAlert) // by chain and message
)

// offsetZone matches a fixed offset like UTC+7.
var offsetZone = regexp.MustCompile(`^(?i:UTC|GMT)([+-])(\d{1,2})(?::?(\d{2}))?$`)

// loadTimezone takes an IANA name (follows daylight saving) or a fixed offset like UTC+7. Empty is UTC.
func loadTimezone(name string) (*time.Location, error) {
	name = strings.TrimSpace(name)
	if location, err := time.LoadLocation(name); err == nil {
		return location, nil
	}
	parts := offsetZone.FindStringSubmatch(name)
	if parts == nil {
		return nil, fmt.Errorf("quiet_hours timezone %q is not an IANA name like Europe/Berlin or an offset like UTC+7", name)
	}
	hours, _ := strconv.Atoi(parts[2])
	minutes := 0
	if parts[3] != "" {
		minutes, _ = strconv.Atoi(parts[3])
	}
	if hours > 14 || minutes > 59 {
		return nil, fmt.Errorf("quiet_hours timezone %q is not a real offset", name)
	}
	seconds := hours*3600 + minutes*60
	if parts[1] == "-" {
		seconds = -seconds
	}
	return time.FixedZone(strings.ToUpper(name), seconds), nil
}

func (q *QuietHoursConfig) parse() error {
	if !q.Enabled {
		return nil
	}
	var err error
	if q.location, err = loadTimezone(q.Timezone); err != nil {
		return err
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

// active handles a window that wraps past midnight, like 23:00 to 07:00.
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

// holds is false for a recovery, it is never held.
func (q *QuietHoursConfig) holds(severity string, resolved bool, now time.Time) bool {
	return !resolved && q.active(now) && severityRank(severity) < severityRank(q.MinSeverity)
}

func (c *Config) holdsAlert(chain, severity string, resolved bool, now time.Time) bool {
	quiet := c.settings().QuietHours
	return quiet.holds(severity, resolved, now)
}

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
