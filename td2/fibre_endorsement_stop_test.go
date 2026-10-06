package tenderduty

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	dash "github.com/kynraze/tenderduty/v2/td2/dashboard"
)

func TestFibreEndorsementStopRun(t *testing.T) {
	r := newFibreRig(t)
	a := AlertConfig{FibreEndorsementBlobs: 20, FibreEndorsementMinutes: 30}
	start := r.clock.now
	for i := 0; i < 19; i++ {
		r.st.recordEndorsement("Chain", "missed", dash.FibreMissedBlob{Height: int64(100 + i)})
	}
	r.advance(30 * time.Minute)
	if r.st.endorsingSummary("Chain", a, r.clock.now).Stopped {
		t.Fatal("minutes alone raised")
	}
	r.st.recordEndorsement("Chain", "unknown", dash.FibreMissedBlob{Height: 120})
	if s := r.st.endorsingSummary("Chain", a, r.clock.now); s.Blobs != 19 || s.Since != start.Unix() {
		t.Fatalf("unknown changed run: %+v", s)
	}
	r.st.recordEndorsement("Chain", "missed", dash.FibreMissedBlob{Height: 121})
	s := r.st.endorsingSummary("Chain", a, r.clock.now)
	if !s.Stopped || s.Blobs != 20 || s.Height != 100 || s.Text != "stopped since 12:00 UTC · 0 of 20 blobs" {
		t.Fatalf("stop: %+v", s)
	}
	restored := newFibreStore()
	restored.now = r.clock.Now
	restored.restoreEndorsing(r.st.savedEndorsing())
	if got := restored.endorsingSummary("Chain", a, r.clock.now); got != s {
		t.Fatalf("restart: %+v != %+v", got, s)
	}
	r.st.recordEndorsement("Chain", "endorsed", dash.FibreMissedBlob{Height: 122})
	if got := r.st.endorsingSummary("Chain", a, r.clock.now); got.Stopped || got.Blobs != 0 || got.LastEndorsed != r.clock.now.Unix() {
		t.Fatalf("reset: %+v", got)
	}
	for i := 0; i < 20; i++ {
		r.st.recordEndorsement("Chain", "missed", dash.FibreMissedBlob{Height: 123})
	}
	if r.st.endorsingSummary("Chain", a, r.clock.now).Stopped {
		t.Fatal("blobs alone raised")
	}
}

func (r *fibreRig) stopBlob(result string) {
	blob := dash.FibreMissedBlob{Height: 100, Time: r.clock.now.Unix()}
	r.st.recordEndorsement("Chain", result, blob)
	r.cc.observeEndorsementAlert(result, blob)
}

func TestFibreEndorsementAlertAndRecovery(t *testing.T) {
	r := newFibreRig(t)
	r.vi.Bonded = true
	r.cc.Alerts.FibreEndorsementAlert = true
	for i := 0; i < 50; i++ {
		r.stopBlob("missed")
	}
	r.stopBlob("unknown")
	if r.st.endorsing["Chain"].AlertRun.Blobs != 50 {
		t.Fatal("unknown changed alert run")
	}
	r.cc.fibreEndorsementAlarms(r.vi, false)
	if len(r.c.alertChan) != 0 {
		t.Fatal("raised before minutes threshold")
	}
	r.advance(150 * time.Minute)
	r.cc.fibreEndorsementAlarms(r.vi, false)
	if len(r.c.alertChan) != 1 {
		t.Fatal("missing stop alarm")
	}
	msg := <-r.c.alertChan
	want := "Fibre is not endorsing on celestia\n0 of the last 50 Fibre blobs since 12:00 UTC\ncheck that Fibre can reach celestia-appd (app gRPC and signer)"
	if msg.summary() != want || msg.severity != "warning" || msg.uniqueId != "valcons1fibreendorsement" {
		t.Fatalf("alert: %+v / %s", msg, msg.summary())
	}
	found := false
	for _, f := range msg.card().Fields {
		if f.Icon == "✍️" && f.Label == "Endorsed" && strings.Contains(f.Value, dotRed) {
			found = true
		}
	}
	if !found {
		t.Fatal("missing red Endorsed card row")
	}
	r.stopBlob("endorsed")
	if len(r.c.alertChan) != 1 {
		t.Fatal("next endorsement did not resolve")
	}
	back := <-r.c.alertChan
	if !back.resolved || back.summary() != "Fibre endorses again on celestia\nmissed 50 blobs over 2h 30m" {
		t.Fatalf("recovery: %s", back.summary())
	}
}

func TestFibreEndorsementAlertRunRestart(t *testing.T) {
	r := newFibreRig(t)
	r.vi.Bonded = true
	r.cc.Alerts.FibreEndorsementAlert = true
	for i := 0; i < 50; i++ {
		r.stopBlob("missed")
	}
	r.advance(2 * time.Hour)
	restored := newFibreStore()
	restored.now = r.clock.Now
	restored.restoreEndorsing(r.st.savedEndorsing())
	r.c.fibreStore, r.st = restored, restored
	r.cc.fibreEndorsementAlarms(r.vi, false)
	if len(r.c.alertChan) != 1 {
		t.Fatal("eligible run lost on restart")
	}
}

func TestFibreEndorsementDefaults(t *testing.T) {
	c := &Config{Chains: map[string]*ChainConfig{"Chain": {}}}
	if err := applyAlertDefaults(c, []byte("chains:\n  Chain: {}\n"), nil); err != nil {
		t.Fatal(err)
	}
	a := c.Chains["Chain"].Alerts
	if a.FibreEndorsementAlert || a.FibreEndorsementBlobs != 50 || a.FibreEndorsementMinutes != 120 {
		t.Fatalf("defaults: %+v", a)
	}
	blobs, duration := endorsementLimits(AlertConfig{FibreEndorsementBlobs: -1, FibreEndorsementMinutes: -1})
	if blobs != 50 || duration != 120*time.Minute {
		t.Fatal("invalid thresholds did not fall back")
	}
}

func TestFibreEndorsementSuppressionAndOptIn(t *testing.T) {
	for _, mode := range []string{"default", "node", "jailed", "unbonded"} {
		t.Run(mode, func(t *testing.T) {
			r := newFibreRig(t)
			r.vi.Bonded = mode != "unbonded"
			r.vi.Jailed = mode == "jailed"
			r.cc.Alerts.FibreEndorsementAlert = mode != "default"
			message := nodeDownMessage("http://own:26657", r.cc.ChainId)
			if mode == "node" {
				id := "own"
				r.c.record("Chain", message, "warning", false, false, &id)
			}
			for len(r.c.alertChan) > 0 {
				<-r.c.alertChan
			}
			for i := 0; i < 50; i++ {
				r.stopBlob("missed")
			}
			r.advance(2 * time.Hour)
			r.cc.fibreEndorsementAlarms(r.vi, false)
			if len(r.c.alertChan) != 0 {
				t.Fatal("suppressed/default alarm raised")
			}
			if !r.st.endorsingSummary("Chain", r.cc.Alerts, r.clock.now).Stopped {
				t.Fatal("suppression hid info")
			}
			// Suppressed blobs and time must not contribute once healthy.
			r.vi.Jailed, r.vi.Bonded = false, true
			r.cc.Alerts.FibreEndorsementAlert = true
			if mode == "node" {
				id := "own"
				r.c.record("Chain", message, "info", true, true, &id)
			}
			for len(r.c.alertChan) > 0 {
				<-r.c.alertChan
			}
			r.stopBlob("missed")
			r.cc.fibreEndorsementAlarms(r.vi, false)
			if len(r.c.alertChan) != 0 {
				t.Fatal("suppressed blobs counted toward raising")
			}
		})
	}
}

func TestFibreEndorsementReportAndDashboard(t *testing.T) {
	r := newFibreRig(t)
	start := r.clock.now
	for i := 0; i < 50; i++ {
		r.st.recordEndorsement("Chain", "missed", dash.FibreMissedBlob{Height: 100})
	}
	r.advance(150 * time.Minute)
	report := r.cc.fibreReportFrom(r.clock.now, r.st)
	if !strings.Contains(strings.Join(report.telegramLines(), "\n"), "stopped since 12:00 UTC · 0 of 50 blobs") {
		t.Fatal("Telegram missing stop")
	}
	b, err := json.Marshal(report.dashboard())
	if err != nil || !strings.Contains(string(b), `"endorsing":{"stopped":true,"since":`) {
		t.Fatalf("dashboard: %s, %v", b, err)
	}
	r.st.recordEndorsement("Chain", "endorsed", dash.FibreMissedBlob{Height: 101})
	r.advance(time.Minute)
	for _, weekly := range []bool{false, true} {
		lines := r.c.reportFibre("Chain", start, r.clock.now, weekly)
		if len(lines) == 0 || !strings.Contains(lines[0], "endorsed 1 of 51 (2.0%) · stopped 2h 30m") {
			t.Fatalf("report: %v", lines)
		}
	}
	r.st.mu.Lock()
	longest := r.st.longestEndorsementStop("Chain", r.cc.Alerts, start.Add(time.Hour), r.clock.now)
	r.st.mu.Unlock()
	if longest != 90*time.Minute {
		t.Fatalf("period clipping: %s", longest)
	}
}
