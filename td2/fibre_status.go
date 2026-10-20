package tenderduty

import (
	"fmt"
	"html"
	"math"
	"strconv"
	"strings"
	"time"

	dash "github.com/kynraze/tenderduty/v2/td2/dashboard"
)

type fibreReport struct {
	Now     time.Time
	Pending bool // not checked yet
	State   fibreState

	ShareKnown  bool // the validator set was read
	InActiveSet bool
	Rows        int
	Minimum     bool  // the 148 rows minimum raised the share
	Budget      int64 // 0 when no rows or the parameter isn't read

	Endorsing    dash.FibreEndorsing
	Endorsements dash.FibreWindow
	LatestMissed []dash.FibreMissedBlob
	Metrics      fibreSnapshot // Found is false when nothing was received for the chain
}

// nil when Fibre isn't watched: fibre_enabled off or x/valaddr missing.
func (cc *ChainConfig) fibreReport(now time.Time) *fibreReport {
	return cc.fibreReportFrom(now, td.fibreData())
}

func (cc *ChainConfig) fibreReportFrom(now time.Time, store *fibreStore) *fibreReport {
	if !cc.alertSettings().FibreAlerts {
		return nil
	}
	cc.stateMux.RLock()
	state, loaded, unsupported, fullStake := cc.fibre, cc.fibreLoaded, cc.fibreUnsupported, cc.fibreFullStake
	cc.stateMux.RUnlock()
	if unsupported {
		return nil
	}
	day, missed := store.endorsementSummary(cc.name)
	endorsing := store.endorsingSummary(cc.name, cc.alertSettings(), now)
	// provider check pending after startup
	if !loaded {
		return &fibreReport{Now: now, Pending: true, Endorsing: endorsing, Endorsements: day, LatestMissed: missed}
	}
	info, _ := cc.validatorState()
	r := &fibreReport{Now: now, State: state, Endorsing: endorsing, Endorsements: day, LatestMissed: missed}
	// failed set reads also leave total power zero
	if info.TotalPower > 0 {
		r.ShareKnown = true
		r.Rows, r.Minimum = fibreRows(info.VotingPower, info.TotalPower)
		r.InActiveSet = r.Rows > 0
		r.Budget = fibreBudget(fullStake, r.Rows)
	}
	r.Metrics = store.snapshot(cc.name)
	return r
}

func humanBinary(n int64) string {
	value, unit := float64(n), 0
	for value >= 1024 && unit < 4 {
		value /= 1024
		unit++
	}
	text := strings.TrimSuffix(strconv.FormatFloat(value, 'f', 1, 64), ".0")
	return text + " " + []string{"B", "KiB", "MiB", "GiB", "TiB"}[unit]
}

// no trailing zeros: 3.61%, 75%, 100%.
func share(value float64) string {
	return strings.TrimSuffix(strings.TrimRight(strconv.FormatFloat(value, 'f', 2, 64), "0"), ".") + "%"
}

// a tiny nonzero part shows as <0.01%, not 0%.
func percent(part, whole int64) string {
	value := 100 * float64(part) / float64(whole)
	if part > 0 && value < 0.01 {
		return "<0.01%"
	}
	return share(value)
}

func ago(now, then time.Time) string {
	return shortDuration(max(now.Sub(then), 0)) + " ago"
}

// unchecked: the host never answered or the consensus key isn't known.
func (s fibreState) identity() string {
	switch {
	case !s.Registered || !s.KeyKnown:
		return "unchecked"
	case !s.IdentityOK:
		return "failed"
	}
	return "verified"
}

func (r *fibreReport) telegramLines() []string {
	endorsed := "<b>Endorsed (24h):</b> " + endorsementText(r.Endorsements.Endorsed, r.Endorsements.Missed)
	if r.Endorsing.Stopped {
		endorsed += "\n" + statusField("✍️", "Endorsing", "🔴 "+r.Endorsing.Text)
	}
	if r.Pending {
		return []string{"🧵 <b>Fibre</b>", "⏳ Not checked yet, the first check runs within a minute", endorsed}
	}
	s, m := r.State, r.Metrics
	head := "🧵 <b>Fibre</b>"
	if m.Found && m.Version != "" {
		head += " <code>" + html.EscapeString(m.Version) + "</code>"
	}
	lines := []string{head, endorsed}
	if !s.Registered {
		lines = append(lines, statusField("🌐", "Host", "🔴 not registered on the chain"))
	} else {
		lines = append(lines, statusField("🌐", "Host", "<code>"+html.EscapeString(s.Host)+"</code>"))
		reach := "🟢 yes"
		if !s.Reachable {
			reach = "🔴 no"
			if !s.DownSince.IsZero() {
				reach += ", for " + shortDuration(max(r.Now.Sub(s.DownSince), 0))
			}
			if s.DownReason != "" {
				reach += " (" + html.EscapeString(s.DownReason) + ")"
			}
		}
		lines = append(lines, statusField("📡", "Reachable", reach))
		identity := map[string]string{"verified": "🟢 verified", "failed": "🔴 NOT verified", "unchecked": "⚪ not checked"}[s.identity()]
		lines = append(lines, statusField("🔐", "Identity", identity))
	}

	switch {
	case !r.ShareKnown:
	case !r.InActiveSet:
		lines = append(lines, statusField("📊", "Share", "⚪ not in the active set, no rows"))
	default:
		note := ""
		switch {
		case r.Minimum:
			note = " min"
		case r.Rows == fibreOriginalRows:
			note = " max"
		}
		lines = append(lines, statusField("📊", "Share", fmt.Sprintf("%d/%d rows · %s%s", r.Rows, fibreOriginalRows, share(100*float64(r.Rows)/fibreOriginalRows), note)))
	}
	if m.HaveOccupancy || r.Budget > 0 || (m.HaveBudget && m.Budget == 0) || len(m.BackendNames) > 0 {
		used := "—"
		if m.HaveOccupancy {
			used = humanBinary(m.Occupancy)
		}
		if m.HaveBudget && m.Budget == 0 {
			used += " · unlimited budget"
		} else {
			if r.Budget > 0 {
				used += " of " + humanBinary(r.Budget) + " budget"
			}
			if m.HaveOccupancy && m.HaveBudget && m.Budget > 0 {
				used += " (" + html.EscapeString(percent(m.Occupancy, m.Budget)) + ")"
			}
			if r.Budget > 0 && m.HaveBudget && m.Budget != r.Budget {
				used += " (Fibre reports " + humanBinary(m.Budget) + ")"
			}
		}
		if len(m.BackendNames) > 0 {
			used += " · " + html.EscapeString(strings.Join(m.BackendNames, ", "))
		}
		lines = append(lines, statusField("🗃", "Shards", used))
	}

	if !m.Found {
		return append(lines, statusField("📨", "Metrics", "⚪ not received, see docs/fibre.md"))
	}
	w := m.Ten
	span := "10m"
	if w.Covers < fibreWindow {
		span = shortDuration(w.Covers)
	}
	uploads := fmt.Sprintf("✅ %s · ❌ %s · 🚫 %s", thousands(w.Sum.uploadOK), thousands(w.Sum.uploadFailed), thousands(w.Sum.rejected))
	lines = append(lines, statusField("⬆️", "Uploads "+span, uploads))
	last := "none yet"
	if m.HaveSuccess {
		last = ago(r.Now, m.LastSuccess)
	}
	lines = append(lines, statusField("🕒", "Last successful upload", last))

	// red for failed signatures, yellow when most successful signatures exceed 10 ms
	signing := "⚪ no signatures yet"
	fast := func(d fibreDelta) string {
		if d.signOK == 0 {
			return "🔴 all " + thousands(d.signFailed) + " failed"
		}
		// rounded down, 100% is every one of them
		share := math.Floor(100 * float64(d.signFast) / float64(d.signOK))
		dot := "🟢"
		if 2*d.signFast < d.signOK {
			dot = "🟡"
		}
		if d.signFailed > 0 {
			return fmt.Sprintf("🔴 %s failed · %d%% under 10 ms", thousands(d.signFailed), int(share))
		}
		return fmt.Sprintf("%s %d%% under 10 ms", dot, int(share))
	}
	switch {
	case m.Ten.Sum.signTotal > 0:
		signing = fast(m.Ten.Sum)
	case m.Day.Sum.signTotal > 0:
		signing = fast(m.Day.Sum) + " over " + shortDuration(m.Day.Covers)
	}
	lines = append(lines, statusField("✍️", "Signing", signing))
	// Fibre counts missing shards as failed gets, excluded from storage errors
	storage := "🟢 ok"
	if n := w.Sum.storePut + w.Sum.prune + w.Sum.backendTimeout + w.Sum.backendThrottled + w.Sum.backendError; n > 0 {
		storage = "🔴 " + thousands(n) + " errors"
		if n == 1 {
			storage = "🔴 1 error"
		}
	}
	if w.Sum.storeGet > 0 {
		storage += " · " + thousands(w.Sum.storeGet) + " gets not found"
	}
	lines = append(lines, statusField("🗄", "Storage", storage))
	return append(lines, statusField("📨", "Metrics", ago(r.Now, m.LastReceived)))
}

// avgMs is in milliseconds, nil when nothing was measured.
func avgMs(seconds float64, n int64) *float64 {
	if n <= 0 {
		return nil
	}
	v := 1000 * seconds / float64(n)
	return &v
}

func fibreWindowStatus(s fibreSummary) dash.FibreWindow {
	d := s.Sum
	w := dash.FibreWindow{
		Seconds: int64(s.Covers.Seconds()), UploadsOK: d.uploadOK, UploadsFailed: d.uploadFailed, UploadsRejected: d.rejected,
		BytesReceived: d.uploadBytes, AvgUploadMs: avgMs(d.uploadSec, d.uploadOK),
		DownloadsOK: d.downloadOK, DownloadsFailed: d.downloadFailed, BytesServed: d.downloadBytes, AvgDownloadMs: avgMs(d.downloadSec, d.downloadOK),
		Signatures: d.signTotal, SignaturesFailed: d.signFailed, AvgSignMs: avgMs(d.signSec, d.signOK),
		StorePutErrors: d.storePut, StoreGetErrors: d.storeGet, AvgPutMs: avgMs(d.putSec, d.putOK), AvgGetMs: avgMs(d.getSec, d.getOK),
		PruneErrors: d.prune, EntriesPruned: d.pruneEntries, AvgPruneMs: avgMs(d.pruneSec, d.pruneOK),
		ObjectErrors: d.backendTimeout + d.backendThrottled + d.backendError, ObjectRequests: d.backendTotal,
	}
	if d.signOK > 0 {
		share := 100 * float64(d.signFast) / float64(d.signOK)
		w.SignFastPercent = &share
	}
	return w
}

func (r *fibreReport) dashboard() *dash.FibreStatus {
	if r.Pending && r.Endorsements.Endorsed+r.Endorsements.Missed+r.Endorsements.Unknown == 0 {
		return nil
	}
	s, m := r.State, r.Metrics
	f := &dash.FibreStatus{
		StorageBackend: strings.Join(m.BackendNames, ", "),
		LatestMissed:   r.LatestMissed,
		Endorsing:      r.Endorsing,
		Pending:        r.Pending,
		Registered:     s.Registered, Host: s.Host, Reachable: s.Reachable, DownReason: s.DownReason,
		Resources: m.Resources,
		Identity:  s.identity(), IdentityError: s.IdentityError, Version: m.Version,
		InActiveSet: r.InActiveSet, Rows: r.Rows, TotalRows: fibreOriginalRows, MinimumApplies: r.Minimum, ChainBudget: r.Budget,
		MetricsReceived: m.Found, BudgetReported: m.HaveBudget, FibreBudget: m.Budget, Unlimited: m.HaveBudget && m.Budget == 0,
		OccupancyKnown: m.HaveOccupancy, Occupancy: m.Occupancy, UploadsInFlight: m.UploadsNow, DownloadsInFlight: m.DownloadsNow,
	}
	if !s.DownSince.IsZero() {
		f.DownSince = s.DownSince.Unix()
	}
	f.RowPercent = 100 * float64(r.Rows) / fibreOriginalRows
	if m.Found {
		f.MetricsAt = m.LastReceived.Unix()
		f.Last10m, f.Last24h = fibreWindowStatus(m.Ten), fibreWindowStatus(m.Day)
	}
	f.Last24h.Endorsed, f.Last24h.Missed, f.Last24h.Unknown = r.Endorsements.Endorsed, r.Endorsements.Missed, r.Endorsements.Unknown
	if m.HaveSuccess {
		f.LastUploadAt = m.LastSuccess.Unix()
	}
	return f
}
