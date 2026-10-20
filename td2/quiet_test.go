package tenderduty

import (
	"testing"
	"time"
)

func TestQuietHoursWrapMidnight(t *testing.T) {
	q := QuietHoursConfig{Enabled: true, Start: "23:00", End: "07:00", Timezone: "Asia/Jakarta"}
	if err := q.parse(); err != nil {
		t.Fatal(err)
	}
	jakarta, _ := time.LoadLocation("Asia/Jakarta")
	for hour, want := range map[int]bool{22: false, 23: true, 2: true, 6: true, 7: false, 12: false} {
		if got := q.active(time.Date(2026, 10, 2, hour, 30, 0, 0, jakarta)); got != want {
			t.Fatalf("%02d:30 quiet %v, want %v", hour, got, want)
		}
	}
	night := time.Date(2026, 10, 2, 2, 0, 0, 0, jakarta)
	if !q.holds("warning", false, night) || q.holds("critical", false, night) || q.holds("warning", true, night) {
		t.Fatal("only less than critical alerts are held, never recoveries")
	}
}

func TestTimezoneTakesNamesAndOffsets(t *testing.T) {
	for _, tc := range []struct {
		name   string
		offset int // seconds east of UTC
	}{
		{"", 0},
		{"UTC", 0},
		{"Asia/Jakarta", 7 * 3600},
		{"UTC+7", 7 * 3600},
		{"utc+07", 7 * 3600},
		{"UTC-5", -5 * 3600},
		{"GMT+05:30", 5*3600 + 30*60},
		{"UTC+0530", 5*3600 + 30*60},
	} {
		location, err := loadTimezone(tc.name)
		if err != nil {
			t.Fatalf("%q: %v", tc.name, err)
		}
		if _, got := time.Date(2026, 10, 2, 12, 0, 0, 0, location).Zone(); got != tc.offset {
			t.Fatalf("%q is %d seconds from UTC, want %d", tc.name, got, tc.offset)
		}
	}
	for _, bad := range []string{"WIB", "UTC+15", "UTC+7:70", "+7", "Mars/Olympus"} {
		if _, err := loadTimezone(bad); err == nil {
			t.Fatalf("%q was accepted as a timezone", bad)
		}
	}
}

func TestQuietHoursHoldAndRelease(t *testing.T) {
	c := auditConfig(t)
	heldMux.Lock()
	held = make(map[string]heldAlert)
	heldMux.Unlock()
	c.Telegram.Enabled = yes(true)
	chain := &ChainConfig{}
	chain.Alerts.Telegram.Enabled = yes(true)
	c.Chains["Chain"] = chain
	now := time.Now().UTC()
	window := func(from, to time.Time) {
		c.QuietHours = QuietHoursConfig{Enabled: true, Start: from.Format("15:04"), End: to.Format("15:04"), Timezone: "UTC"}
		if err := c.QuietHours.parse(); err != nil {
			t.Fatal(err)
		}
	}
	outbox := func() int {
		alarms.notifyMux.RLock()
		defer alarms.notifyMux.RUnlock()
		return len(alarms.Outbox)
	}

	window(now.Add(-time.Hour), now.Add(time.Hour))
	id := "id"
	c.alert("Chain", "validator has missed 5 blocks on chain", "warning", false, &id)
	if outbox() != 0 || !alarmIsActive("Chain", "validator has missed 5 blocks on chain") {
		t.Fatal("a warning was sent during quiet hours, or its alarm wasn't recorded")
	}
	c.alert("Chain", "validator is no longer active: validator is jailed", "critical", false, &id)
	if outbox() != 1 {
		t.Fatal("a critical alert was held")
	}
	// recovered while held, nothing to send
	c.alert("Chain", "stalled: have not seen a new block on chain in 10 minutes", "warning", false, &id)
	c.alert("Chain", "stalled: have not seen a new block on chain in 10 minutes", "info", true, &id)
	if outbox() != 1 {
		t.Fatal("a held alert's recovery was sent")
	}

	window(now.Add(-3*time.Hour), now.Add(-2*time.Hour))
	c.releaseHeld()
	if outbox() != 2 {
		t.Fatalf("after quiet hours %d notifications are queued, want the critical and the held warning", outbox())
	}
	heldMux.Lock()
	defer heldMux.Unlock()
	if len(held) != 0 {
		t.Fatal("held alerts were kept after release")
	}
}
