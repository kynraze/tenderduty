package tenderduty

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	dash "github.com/kynraze/tenderduty/v2/td2/dashboard"
)

const fibreEndorsementPrefix = "Fibre is not endorsing on "

type fibreEndorsementRun struct {
	Since  time.Time `json:"since"`
	Height int64     `json:"height"`
	Blobs  int64     `json:"blobs"`
	End    time.Time `json:"end,omitempty"`
}

type fibreEndorsing struct {
	Run          fibreEndorsementRun   `json:"run"`
	AlertRun     fibreEndorsementRun   `json:"alert_run"`
	LastEndorsed time.Time             `json:"last_endorsed"`
	Previous     fibreEndorsementRun   `json:"previous"`
	Stops        []fibreEndorsementRun `json:"stops,omitempty"`
}

func endorsementLimits(a AlertConfig) (int64, time.Duration) {
	blobs, minutes := a.FibreEndorsementBlobs, a.FibreEndorsementMinutes
	if blobs < 1 {
		blobs = 50
	}
	if minutes < 1 {
		minutes = 120
	}
	return int64(blobs), time.Duration(minutes) * time.Minute
}

func (r fibreEndorsementRun) stopped(a AlertConfig, now time.Time) bool {
	blobs, duration := endorsementLimits(a)
	return r.Blobs >= blobs && !r.Since.IsZero() && now.Sub(r.Since) >= duration
}

func (r *fibreEndorsementRun) miss(blob dash.FibreMissedBlob, now time.Time) {
	if r.Blobs == 0 {
		r.Since, r.Height = now, blob.Height
	}
	r.Blobs++
}

// Called with the store locked. Unknown never changes a run.
func (st *fibreStore) recordEndorsing(chain, result string, blob dash.FibreMissedBlob, now time.Time) {
	s := st.endorsing[chain]
	if s == nil {
		s = &fibreEndorsing{}
		st.endorsing[chain] = s
	}
	switch result {
	case "missed":
		s.Run.miss(blob, now)
	case "endorsed":
		if s.Run.Blobs > 0 {
			s.Previous = s.Run
			s.Previous.End = now
			s.Stops = append(s.Stops, s.Previous)
		}
		s.Run = fibreEndorsementRun{}
		s.LastEndorsed = now
	}
	kept := s.Stops[:0]
	for _, stop := range s.Stops {
		if stop.End.After(now.Add(-fibreRollupKeep)) {
			kept = append(kept, stop)
		}
	}
	s.Stops = kept
}

func (st *fibreStore) endorsingSummary(chain string, a AlertConfig, now time.Time) dash.FibreEndorsing {
	st.mu.Lock()
	defer st.mu.Unlock()
	f := dash.FibreEndorsing{Text: "none seen yet"}
	if s := st.endorsing[chain]; s != nil {
		f.Stopped, f.Height, f.Blobs = s.Run.stopped(a, now), s.Run.Height, s.Run.Blobs
		if !s.Run.Since.IsZero() {
			f.Since = s.Run.Since.Unix()
		}
		if !s.LastEndorsed.IsZero() {
			f.LastEndorsed = s.LastEndorsed.Unix()
			f.Text = "last endorsed " + ago(now, s.LastEndorsed)
		}
		if f.Stopped {
			f.Text = fmt.Sprintf("stopped since %s · 0 of %d blobs", s.Run.Since.In(alertLocation()).Format("15:04 MST"), f.Blobs)
		}
	}
	return f
}

// Caller holds st.mu; clip each qualifying stop to the report period.
func (st *fibreStore) longestEndorsementStop(chain string, a AlertConfig, start, end time.Time) time.Duration {
	s := st.endorsing[chain]
	if s == nil {
		return 0
	}
	longest := time.Duration(0)
	runs := append(append([]fibreEndorsementRun(nil), s.Stops...), s.Run)
	for _, r := range runs {
		until := r.End
		if until.IsZero() || until.After(end) {
			until = end
		}
		if !r.stopped(a, until) {
			continue
		}
		since := r.Since
		if since.Before(start) {
			since = start
		}
		longest = max(longest, until.Sub(since))
	}
	return longest
}

func (st *fibreStore) savedEndorsing() json.RawMessage {
	st.mu.Lock()
	defer st.mu.Unlock()
	b, _ := json.Marshal(st.endorsing)
	return b
}

func (st *fibreStore) restoreEndorsing(raw json.RawMessage) {
	var chains map[string]*fibreEndorsing
	if json.Unmarshal(raw, &chains) != nil {
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	now := st.now()
	valid := func(r fibreEndorsementRun) bool {
		return r.Blobs >= 0 && !r.Since.After(now) && !r.End.After(now) &&
			(r.Blobs == 0 || !r.Since.IsZero() && r.Height > 0 && (r.End.IsZero() || !r.End.Before(r.Since)))
	}
	for chain, s := range chains {
		if len(chain) > 256 || s == nil || s.LastEndorsed.After(now) || !valid(s.Run) || !valid(s.AlertRun) || !valid(s.Previous) {
			continue
		}
		kept := s.Stops[:0]
		for _, stop := range s.Stops {
			if valid(stop) && !stop.End.IsZero() && stop.End.After(now.Add(-fibreRollupKeep)) {
				kept = append(kept, stop)
			}
		}
		s.Stops = kept
		st.endorsing[chain] = s
	}
}

func (cc *ChainConfig) endorsementSuppressed(info *ValInfo) bool {
	if info.Jailed || !info.Bonded {
		return true
	}
	for message := range activeMessages(cc.name, "") {
		if strings.Contains(message, "RPC node ") && strings.Contains(message, " has been down for ") || message == "no RPC endpoints are working for "+cc.ChainId {
			return true
		}
	}
	return false
}

func (cc *ChainConfig) observeEndorsementAlert(result string, blob dash.FibreMissedBlob) {
	info, _ := cc.validatorState()
	suppressed := cc.endorsementSuppressed(info)
	settings := cc.alertSettings()
	st := td.fibreData()
	st.mu.Lock()
	s := st.endorsing[cc.name]
	if s == nil {
		s = &fibreEndorsing{}
		st.endorsing[cc.name] = s
	}
	previous := s.Previous
	if result == "endorsed" || suppressed || !settings.FibreEndorsementAlert {
		s.AlertRun = fibreEndorsementRun{}
	} else if result == "missed" {
		s.AlertRun.miss(blob, st.now())
	}
	st.mu.Unlock()
	message := fibreEndorsementPrefix + cc.ChainId
	if result == "endorsed" && alarmIsActive(cc.name, message) {
		resolve(cc.name, message, resolution{"✅", "Fibre endorsing", fmt.Sprintf("Fibre endorses again on %s\nmissed %d blobs over %s", cc.ChainId, previous.Blobs, shortDuration(previous.End.Sub(previous.Since)))})
		id := info.Valcons + "fibreendorsement"
		td.alert(cc.name, message, "info", true, &id)
	}
}

func (cc *ChainConfig) fibreEndorsementAlarms(info *ValInfo, reconcile bool) {
	st := td.fibreData()
	suppressed := cc.endorsementSuppressed(info)
	settings := cc.alertSettings()
	st.mu.Lock()
	s := st.endorsing[cc.name]
	raise := false
	if s != nil {
		if suppressed {
			s.AlertRun = fibreEndorsementRun{}
		}
		raise = !suppressed && s.AlertRun.stopped(settings, st.now())
	}
	st.mu.Unlock()
	message := fibreEndorsementPrefix + cc.ChainId
	if settings.FibreEndorsementAlert && raise && (!alarmIsActive(cc.name, message) || reconcile) {
		id := info.Valcons + "fibreendorsement"
		td.alert(cc.name, message, settings.FibreSeverity, false, &id)
	}
}

func (cc *ChainConfig) endorsementAlertDetail() string {
	st := td.fibreData()
	st.mu.Lock()
	defer st.mu.Unlock()
	if s := st.endorsing[cc.name]; s != nil {
		return fmt.Sprintf("0 of the last %d Fibre blobs since %s\ncheck that Fibre can reach celestia-appd (app gRPC and signer)", s.Run.Blobs, s.Run.Since.In(alertLocation()).Format("15:04 MST"))
	}
	return ""
}
