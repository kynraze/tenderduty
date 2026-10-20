package tenderduty

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	dash "github.com/kynraze/tenderduty/v2/td2/dashboard"
	metricsv1 "go.opentelemetry.io/proto/otlp/metrics/v1"
)

const fibreRollupKeep = 30 * 24 * time.Hour

// Chart aggregates persist; sender baselines do not.
type fibreHour struct {
	Extra         *fibreExtra        `json:"extra,omitempty"`
	MaxHeap       float64            `json:"max_heap,omitempty"`
	MaxStack      float64            `json:"max_stack,omitempty"`
	MaxMemory     float64            `json:"max_memory,omitempty"`
	MaxGoroutines float64            `json:"max_goroutines,omitempty"`
	Time          int64              `json:"time"`
	Counts        map[string]float64 `json:"counts,omitempty"`
	Exports       int64              `json:"exports"`
	Occupancy     *int64             `json:"occupancy,omitempty"`
	MaxOccupancy  int64              `json:"max_occupancy"`
	Budget        *int64             `json:"budget,omitempty"`
	MaxUploads    int64              `json:"max_uploads"`
	Checks        int64              `json:"checks"`
	Reachable     int64              `json:"reachable"`
	Verified      int64              `json:"verified"`
}

func fibreValues(d fibreDelta) map[string]float64 {
	return map[string]float64{
		"uploads_ok":               float64(d.uploadOK),
		"uploads_failed":           float64(d.uploadFailed),
		"uploads_rejected":         float64(d.rejected),
		"bytes_received":           float64(d.uploadBytes),
		"downloads_ok":             float64(d.downloadOK),
		"downloads_failed":         float64(d.downloadFailed),
		"bytes_served":             float64(d.downloadBytes),
		"signatures":               float64(d.signTotal),
		"signatures_ok":            float64(d.signOK),
		"signatures_failed":        float64(d.signFailed),
		"signatures_fast":          float64(d.signFast),
		"sign_seconds":             d.signSec,
		"store_put_errors":         float64(d.storePut),
		"store_get_errors":         float64(d.storeGet),
		"prune_errors":             float64(d.prune),
		"prune_entries":            float64(d.pruneEntries),
		"object_storage_requests":  float64(d.backendTotal),
		"object_storage_timeout":   float64(d.backendTimeout),
		"object_storage_throttled": float64(d.backendThrottled),
		"object_storage_error":     float64(d.backendError),
	}
}

var fibreFields = map[string][]string{
	"upload_shard.duration":   {"uploads_ok", "uploads_failed"},
	"upload_shard.rejected":   {"uploads_rejected"},
	"upload_shard.bytes":      {"bytes_received"},
	"download_shard.duration": {"downloads_ok", "downloads_failed"},
	"download_shard.bytes":    {"bytes_served"},
	"sign.duration":           {"signatures", "signatures_ok", "signatures_failed", "signatures_fast", "sign_seconds"},
	"store.put.duration":      {"store_put_errors"},
	"store.get.duration":      {"store_get_errors"},
	"prune.duration":          {"prune_errors"},
	"prune.entries":           {"prune_entries"},
	"backend.get.duration":    {"object_storage_requests", "object_storage_timeout", "object_storage_throttled", "object_storage_error"},
}

var fibreValueKeys = []string{
	"uploads_ok", "uploads_failed", "uploads_rejected", "bytes_received",
	"downloads_ok", "downloads_failed", "bytes_served",
	"signatures", "signatures_ok", "signatures_failed", "signatures_fast", "sign_seconds",
	"store_put_errors", "store_get_errors", "prune_errors", "prune_entries",
	"object_storage_requests", "object_storage_timeout", "object_storage_throttled", "object_storage_error",
}

type fibreReach struct {
	at        time.Time
	reachable bool
}

func knownFibreValues(d fibreDelta) map[string]float64 {
	values := fibreValues(d)
	for i, key := range fibreValueKeys {
		if d.known&(1<<i) == 0 {
			delete(values, key)
		}
	}
	return values
}

func addFibreValues(dst, src map[string]float64) {
	for k, v := range src {
		dst[k] += v
	}
}

func (st *fibreStore) trimHours(now time.Time) {
	cutoff := now.Add(-fibreRollupKeep).Truncate(time.Hour).Unix()
	for chain, hours := range st.hours {
		for at := range hours {
			if at < cutoff || at > now.Truncate(time.Hour).Unix() {
				delete(hours, at)
			}
		}
		if len(hours) == 0 {
			delete(st.hours, chain)
		}
	}
}

func (st *fibreStore) hour(chain string, now time.Time) *fibreHour {
	st.trimHours(now)
	if st.hours[chain] == nil {
		st.hours[chain] = make(map[int64]*fibreHour)
	}
	at := now.UTC().Truncate(time.Hour).Unix()
	if st.hours[chain][at] == nil {
		st.hours[chain][at] = &fibreHour{Time: at, Counts: make(map[string]float64)}
	}
	return st.hours[chain][at]
}

// Receiver lock held so buckets and history use the same export.
func (st *fibreStore) recordHour(chain string, d *fibreDelta, s *fibreSender, metrics []*metricsv1.Metric) {
	h := st.hour(chain, d.at)
	h.Exports++
	if d.extra != nil {
		h.Extra = mergeFibreExtra(h.Extra, d.extra)
		if memory, ok := d.extra.Gauges["go_memory_used_bytes"]; ok && memory >= h.MaxMemory {
			h.MaxMemory = memory
			h.MaxHeap = d.extra.Gauges["go_memory_heap_bytes"]
			h.MaxStack = d.extra.Gauges["go_memory_stack_bytes"]
		}
		h.MaxGoroutines = max(h.MaxGoroutines, d.extra.Gauges["go_goroutines"])
		if st.histograms[chain] == nil {
			st.histograms[chain] = make(map[string]fibreHistogram)
		}
		for op, histogram := range d.extra.Histograms {
			st.histograms[chain][op] = addFibreHistogram(st.histograms[chain][op], histogram)
		}
		if st.totals[chain] == nil {
			st.totals[chain] = make(map[string]float64)
		}
		addFibreValues(st.totals[chain], d.extra.Counters)
	}
	values := fibreValues(*d)
	known := make(map[string]float64)
	for _, m := range metrics {
		name, ok := strings.CutPrefix(m.GetName(), fibreMetricPfx)
		if !ok || (len(m.GetSum().GetDataPoints()) == 0 && len(m.GetHistogram().GetDataPoints()) == 0) {
			continue
		}
		for _, key := range fibreFields[name] {
			if name == "sign.duration" {
				have := false
				for _, dp := range m.GetHistogram().GetDataPoints() {
					success, _ := fibreAttr(dp.GetAttributes(), "success")
					_, fast := fibreFast(dp)
					if fast || (success == "false" && (key == "signatures" || key == "signatures_failed")) {
						have = true
					}
				}
				if !have {
					continue
				}
			}
			known[key] = values[key]
			for i, k := range fibreValueKeys {
				if key == k {
					d.known |= 1 << i
				}
			}
		}
	}
	// Fibre omits uploads_rejected until the first rejection.
	if _, ok := known["uploads_ok"]; ok {
		if _, ok := known["uploads_rejected"]; !ok {
			known["uploads_rejected"] = values["uploads_rejected"]
			for i, k := range fibreValueKeys {
				if k == "uploads_rejected" {
					d.known |= 1 << i
				}
			}
		}
	}
	addFibreValues(h.Counts, known)
	if st.totals[chain] == nil {
		st.totals[chain] = make(map[string]float64)
	}
	addFibreValues(st.totals[chain], known)
	if d.haveOccupancy {
		v := d.occupancy
		h.Occupancy = &v
		h.MaxOccupancy = max(h.MaxOccupancy, v)
	}
	if d.haveBudget {
		v := d.budget
		h.Budget = &v
	}
	if s.haveUploads {
		h.MaxUploads = max(h.MaxUploads, s.uploadsNow)
	}
}

func (st *fibreStore) recordReach(chain string, state fibreState) {
	st.mu.Lock()
	defer st.mu.Unlock()
	now := st.now()
	h := st.hour(chain, now)
	h.Checks++
	samples := st.reach[chain]
	kept := samples[:0]
	for _, s := range samples {
		if now.Sub(s.at) < fibreKeep {
			kept = append(kept, s)
		}
	}
	st.reach[chain] = append(kept, fibreReach{at: now, reachable: state.Reachable})
	if state.Reachable {
		h.Reachable++
	}
	// A carried identity during downtime is not a successful identity check.
	if state.Reachable && state.KeyKnown && state.IdentityOK {
		h.Verified++
	}
}

func (st *fibreStore) savedHours() json.RawMessage {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.trimHours(st.now())
	if len(st.hours) == 0 {
		return nil
	}
	b, _ := json.Marshal(st.hours)
	return b
}

func (st *fibreStore) restoreHours(raw json.RawMessage) {
	st.restoreBuckets(raw, st.hours, fibreRollupKeep, time.Hour)
}

func (st *fibreStore) restoreShort(raw json.RawMessage, span string) {
	duration, step := fibreRange(span)
	st.restoreBuckets(raw, st.short[span], duration-step, step)
}

func (st *fibreStore) restoreBuckets(raw json.RawMessage, dst map[string]map[int64]*fibreHour, duration, step time.Duration) {
	var chains map[string]json.RawMessage
	if json.Unmarshal(raw, &chains) != nil {
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	now := st.now()
	fields := fibreValues(fibreDelta{})
	for _, result := range []string{"endorsed", "missed", "unknown"} {
		fields["endorsements_"+result] = 0
	}
	for chain, rawHours := range chains {
		if len(chain) > 256 {
			continue
		}
		var hours map[string]json.RawMessage
		if json.Unmarshal(rawHours, &hours) != nil {
			continue
		}
		for key, b := range hours {
			at, err := strconv.ParseInt(key, 10, 64)
			if err != nil || at < now.Truncate(step).Add(-duration).Unix() || at > now.Truncate(step).Unix() {
				continue
			}
			var h fibreHour
			if json.Unmarshal(b, &h) != nil || h.Time != at || at%int64(step/time.Second) != 0 || h.Exports < 0 || h.Checks < 0 || h.Reachable < 0 || h.Reachable > h.Checks || h.Verified < 0 || h.Verified > h.Checks {
				continue
			}
			if (h.Occupancy != nil && *h.Occupancy < 0) || (h.Budget != nil && *h.Budget < 0) || h.MaxOccupancy < 0 || h.MaxUploads < 0 {
				continue
			}
			valid := true
			for k, v := range h.Counts {
				if _, ok := fields[k]; !ok || v < 0 || math.IsNaN(v) || math.IsInf(v, 0) || (strings.HasPrefix(k, "endorsements_") && (math.Trunc(v) != v || v >= float64(math.MaxInt64))) {
					valid = false
					break
				}
			}
			if !valid || !validFibreExtra(h.Extra) || h.MaxHeap < 0 || h.MaxStack < 0 || math.IsNaN(h.MaxHeap) || math.IsInf(h.MaxHeap, 0) || math.IsNaN(h.MaxStack) || math.IsInf(h.MaxStack, 0) || h.MaxMemory < 0 || h.MaxGoroutines < 0 || math.IsNaN(h.MaxMemory) || math.IsInf(h.MaxMemory, 0) || math.IsNaN(h.MaxGoroutines) || math.IsInf(h.MaxGoroutines, 0) {
				continue
			}
			if h.Counts == nil {
				h.Counts = make(map[string]float64)
			}
			if dst[chain] == nil {
				dst[chain] = make(map[int64]*fibreHour)
			}
			dst[chain][at] = &h
		}
	}
	for chain, buckets := range dst {
		for at := range buckets {
			if at < now.Truncate(step).Add(-duration).Unix() || at > now.Truncate(step).Unix() {
				delete(buckets, at)
			}
		}
		if len(buckets) == 0 {
			delete(dst, chain)
		}
	}
}

// Caller holds the lock; aggregation leaves sender selection and samples unchanged.
func (st *fibreStore) shortBuckets(chain, span string) map[int64]*fibreHour {
	duration, step := fibreRange(span)
	end := st.now().Truncate(step).Unix()
	start := end - int64((duration-step)/time.Second)
	buckets := make(map[int64]*fibreHour)
	for at, h := range st.short[span][chain] {
		if at < start || at > end {
			delete(st.short[span][chain], at)
			continue
		}
		copy := *h
		copy.Counts = make(map[string]float64)
		addFibreValues(copy.Counts, h.Counts)
		copy.Extra = mergeFibreExtra(nil, h.Extra)
		buckets[at] = &copy
	}
	bucket := func(at time.Time) *fibreHour {
		key := at.Truncate(step).Unix()
		if key < start || key > end {
			return nil
		}
		if buckets[key] == nil {
			buckets[key] = &fibreHour{Time: key, Counts: make(map[string]float64)}
		}
		return buckets[key]
	}
	var best *fibreSender
	for _, sender := range st.senders {
		if sender.chain == chain && (best == nil || sender.lastReceived.After(best.lastReceived)) {
			best = sender
		}
	}
	if best != nil {
		for _, d := range best.window {
			h := bucket(d.at)
			if h == nil {
				continue
			}
			h.Exports++
			addFibreValues(h.Counts, knownFibreValues(d))
			h.Extra = mergeFibreExtra(h.Extra, d.extra)
			if d.haveOccupancy {
				v := d.occupancy
				h.Occupancy = &v
			}
			if d.haveBudget {
				v := d.budget
				h.Budget = &v
			}
		}
	}
	for _, sample := range st.reach[chain] {
		if h := bucket(sample.at); h != nil {
			h.Checks++
			if sample.reachable {
				h.Reachable++
			}
		}
	}
	return buckets
}

func (st *fibreStore) savedShort(span string) json.RawMessage {
	st.mu.Lock()
	defer st.mu.Unlock()
	chains := make(map[string]bool)
	for chain := range st.short[span] {
		chains[chain] = true
	}
	for _, sender := range st.senders {
		chains[sender.chain] = true
	}
	for chain := range st.reach {
		chains[chain] = true
	}
	saved := make(map[string]map[int64]*fibreHour)
	for chain := range chains {
		if buckets := st.shortBuckets(chain, span); len(buckets) > 0 {
			saved[chain] = buckets
		}
	}
	if len(saved) == 0 {
		return nil
	}
	b, _ := json.Marshal(saved)
	return b
}

func fibreRange(span string) (time.Duration, time.Duration) {
	switch span {
	case "1h":
		return time.Hour, time.Minute
	case "7d":
		return 7 * 24 * time.Hour, time.Hour
	case "30d":
		return fibreRollupKeep, 6 * time.Hour
	default:
		return 24 * time.Hour, 10 * time.Minute
	}
}

// Include up to fibreLatencyBack earlier intervals to reach fibreLatencyMin operations.
const (
	fibreLatencyMin  = 50
	fibreLatencyBack = 5
)

func fibreLatencyWindow(extras []*fibreExtra, i int, op string) fibreHistogram {
	h := extras[i].Histograms[op]
	for j := i - 1; j >= 0 && j >= i-fibreLatencyBack && h.Count < fibreLatencyMin; j-- {
		if extras[j] == nil {
			continue
		}
		prev, ok := extras[j].Histograms[op]
		if !ok || prev.Count == 0 {
			continue
		}
		if !sameFibreBounds(h.Bounds, prev.Bounds) {
			break
		}
		h = addFibreHistogram(h, prev)
	}
	return h
}

func (st *fibreStore) series(chain, span string) []dash.FibrePoint {
	points, _ := st.seriesAndLatency(chain, span)
	return points
}

// Whole-range percentiles avoid jumps from intervals with few operations.
func (st *fibreStore) seriesAndLatency(chain, span string) ([]dash.FibrePoint, map[string]float64) {
	st.mu.Lock()
	defer st.mu.Unlock()
	now := st.now()
	duration, step := fibreRange(span)
	end := now.Truncate(step)
	start := end.Add(-duration).Add(step)
	points := make([]dash.FibrePoint, int(duration/step))
	for i := range points {
		points[i].Time = start.Add(time.Duration(i) * step).Unix()
	}
	index := func(at time.Time) int {
		if at.Before(start) || !at.Before(end.Add(step)) {
			return -1
		}
		return int(at.Sub(start) / step)
	}
	sums := make([]map[string]float64, len(points))
	extras := make([]*fibreExtra, len(points))
	checks := make([]int64, len(points))
	reachable := make([]int64, len(points))
	add := func(i int, values map[string]float64, occ, budget *int64) {
		if i < 0 {
			return
		}
		if len(values) > 0 {
			if sums[i] == nil {
				sums[i] = make(map[string]float64)
			}
			addFibreValues(sums[i], values)
		}
		if occ != nil {
			v := *occ
			points[i].Occupancy = &v
		}
		if budget != nil {
			v := *budget
			points[i].Budget = &v
		}
	}
	if span == "1h" || span == "24h" {
		for at, h := range st.shortBuckets(chain, span) {
			i := index(time.Unix(at, 0))
			add(i, h.Counts, h.Occupancy, h.Budget)
			if i >= 0 {
				extras[i] = h.Extra
				checks[i], reachable[i] = h.Checks, h.Reachable
			}
		}
	} else {
		hours := make([]int64, 0, len(st.hours[chain]))
		for at := range st.hours[chain] {
			hours = append(hours, at)
		}
		sort.Slice(hours, func(i, j int) bool { return hours[i] < hours[j] })
		for _, at := range hours {
			h := st.hours[chain][at]
			i := index(time.Unix(at, 0))
			if i < 0 {
				continue
			}
			add(i, h.Counts, h.Occupancy, h.Budget)
			extras[i] = mergeFibreExtra(extras[i], h.Extra)
			if h.Extra != nil {
				if _, ok := h.Extra.Gauges["go_memory_used_bytes"]; ok {
					if h.MaxMemory >= extras[i].Gauges["go_memory_max_bytes"] {
						extras[i].Gauges["go_memory_max_bytes"] = h.MaxMemory
						if _, split := h.Extra.Gauges["go_memory_heap_bytes"]; split {
							extras[i].Gauges["go_memory_heap_max_bytes"] = h.MaxHeap
							extras[i].Gauges["go_memory_stack_max_bytes"] = h.MaxStack
						}
					}
				}
				if _, ok := h.Extra.Gauges["go_goroutines"]; ok {
					extras[i].Gauges["go_goroutines_max"] = max(extras[i].Gauges["go_goroutines_max"], h.MaxGoroutines)
				}
			}
			checks[i] += h.Checks
			reachable[i] += h.Reachable
		}
	}
	// Short ranges use the minute reachability history, independent of metrics exports.
	if span == "1h" || span == "24h" {
		// Fill a single missing minute from the earlier reachability check.
		for i := 1; i+1 < len(points); i++ {
			previous := st.short[span][chain][points[i-1].Time]
			next := st.short[span][chain][points[i+1].Time]
			if previous != nil && previous.Checks > 0 && (next == nil || next.Checks == 0) {
				continue
			}
			if checks[i] == 0 && checks[i-1] > 0 && checks[i+1] > 0 && step <= time.Minute {
				checks[i], reachable[i] = checks[i-1], reachable[i-1]
			}
		}
	}
	for i := range points {
		values := sums[i]
		if e := extras[i]; e != nil {
			if values == nil {
				values = make(map[string]float64)
			}
			addFibreValues(values, e.Counters)
			for k, v := range e.Gauges {
				values[k] = v
			}
			for op := range e.Histograms {
				histogram := fibreLatencyWindow(extras, i, op)
				values[op+"_samples"] = float64(histogram.Count)
				for _, q := range []struct {
					name  string
					value float64
				}{{"p50", .5}, {"p95", .95}} {
					if v := histogram.quantile(q.value); v != nil {
						values[op+"_"+q.name+"_ms"] = 1000 * *v
					}
				}
			}
			if v, ok := e.Counters["go_memory_allocated_bytes"]; ok {
				values["go_allocation_bytes_per_second"] = v / step.Seconds()
			}
		}
		if values != nil {
			if n := values["signatures_ok"]; n > 0 {
				if fast, ok := values["signatures_fast"]; ok {
					values["sign_fast_percent"] = 100 * fast / n
				}
				values["avg_sign_ms"] = 1000 * values["sign_seconds"] / n
			}
			if _, ok := values["object_storage_requests"]; ok {
				values["object_storage_errors"] = values["object_storage_timeout"] + values["object_storage_throttled"] + values["object_storage_error"]
			}
		}
		points[i].Metrics = values
		if checks[i] > 0 {
			v := float64(reachable[i]) / float64(checks[i])
			points[i].Reachable = &v
		}
	}
	var whole *fibreExtra
	for _, e := range extras {
		if e != nil && len(e.Histograms) > 0 {
			whole = mergeFibreExtra(whole, &fibreExtra{Histograms: e.Histograms})
		}
	}
	latency := make(map[string]float64)
	if whole != nil {
		for op, histogram := range whole.Histograms {
			if v := histogram.quantile(.5); v != nil {
				latency[op+"_p50_ms"] = 1000 * *v
			}
			if v := histogram.quantile(.95); v != nil {
				latency[op+"_p95_ms"] = 1000 * *v
			}
		}
	}
	return points, latency
}

func (c *Config) fibreHistory(span, chain string) (dash.FibreHistory, error) {
	result := dash.FibreHistory{Range: span, Chains: make([]dash.FibreChainHistory, 0)}
	c.chainsMux.RLock()
	defer c.chainsMux.RUnlock()
	names := make([]string, 0, len(c.Chains))
	for name := range c.Chains {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		cc := c.Chains[name]
		if cc == nil || (chain != "" && chain != name) {
			continue
		}
		r := cc.fibreReportFrom(time.Now(), c.fibreData())
		if r == nil {
			continue
		}
		// Each chain has at most 288 points; the handler also bounds the encoded response.
		series, latency := c.fibreData().seriesAndLatency(cc.name, span)
		item := dash.FibreChainHistory{Name: name, Summary: r.dashboard(), Series: series, Latency: latency}
		result.Chains = append(result.Chains, item)
	}
	if chain != "" && len(result.Chains) == 0 {
		return result, fmt.Errorf("unknown or unwatched chain")
	}
	return result, nil
}
