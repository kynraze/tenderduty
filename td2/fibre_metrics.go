package tenderduty

import (
	"compress/gzip"
	"context"
	"crypto/subtle"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	commonv1 "go.opentelemetry.io/proto/otlp/common/v1"
	metricsv1 "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/protobuf/proto"
)

const (
	fibreMaxBody     = 1 << 20 // as received, real exports are tens of KB
	fibreMaxUnzipped = 4 << 20 // after gunzip
	fibreMaxDecoding = 4       // requests read and decoded at once, the rest get a 503
	fibreMaxSenders  = 64      // per chain, the least recently seen goes
	fibreMaxIgnored  = 256     // instances remembered as unclaimed, the least recently seen goes
	fibreMaxIdentity = 128     // bytes of service.instance.id and service.version
	fibreMinSamples  = 20
	fibreMetricPfx   = "fibre.server."
	fibreFastBound   = 0.01 // seconds, the signing threshold
)

const (
	fibreWindow = 10 * time.Minute // what the count rules look at, and where the ratio rules start
	fibreKeep   = 24 * time.Hour   // how far back a ratio rule may reach for samples on a quiet network
	// per sender; past it the older half is merged
	fibreMaxDeltas = 5000
	// storage alarm clears after this long without a rejection and this many percent under its threshold
	fibreStorageQuiet = 30 * time.Minute
	fibreStorageGap   = 15
)

type fibreDelta struct {
	extra                                          *fibreExtra
	at                                             time.Time
	rejected                                       int64
	signTotal, signFailed                          int64 // every signature, and the ones that failed
	signOK, signFast                               int64 // the successful ones, and those at or under 10ms
	storePut, storeGet, prune                      int64 // the ones that failed
	backendTotal                                   int64 // every outcome
	backendTimeout, backendThrottled, backendError int64

	// shown in status; the seconds are time taken by successful operations, for averages
	uploadOK, uploadFailed     int64
	uploadSec                  float64
	uploadBytes                int64
	downloadOK, downloadFailed int64
	downloadSec                float64
	downloadBytes              int64
	putOK, getOK, pruneOK      int64
	putSec, getSec, pruneSec   float64
	pruneEntries               int64
	signSec                    float64 // of the signatures in signOK
	occupancy, budget          int64
	haveOccupancy, haveBudget  bool
	known                      uint32
}

func (d *fibreDelta) add(o fibreDelta) {
	d.extra = mergeFibreExtra(d.extra, o.extra)
	d.rejected += o.rejected
	d.signTotal += o.signTotal
	d.signFailed += o.signFailed
	d.signOK += o.signOK
	d.signFast += o.signFast
	d.storePut += o.storePut
	d.storeGet += o.storeGet
	d.prune += o.prune
	d.backendTotal += o.backendTotal
	d.backendTimeout += o.backendTimeout
	d.backendThrottled += o.backendThrottled
	d.backendError += o.backendError
	d.uploadOK += o.uploadOK
	d.uploadFailed += o.uploadFailed
	d.uploadSec += o.uploadSec
	d.uploadBytes += o.uploadBytes
	d.downloadOK += o.downloadOK
	d.downloadFailed += o.downloadFailed
	d.downloadSec += o.downloadSec
	d.downloadBytes += o.downloadBytes
	d.putOK += o.putOK
	d.getOK += o.getOK
	d.pruneOK += o.pruneOK
	d.putSec += o.putSec
	d.getSec += o.getSec
	d.pruneSec += o.pruneSec
	d.pruneEntries += o.pruneEntries
	d.signSec += o.signSec
	d.known |= o.known
	if o.haveOccupancy {
		d.occupancy, d.haveOccupancy = o.occupancy, true
	}
	if o.haveBudget {
		d.budget, d.haveBudget = o.budget, true
	}
}

// What a ratio rule is judged on: the last 10 minutes, reaching back (newest first) up to 24h until it holds 20 samples.
type fibreSpan struct {
	Sum   fibreDelta
	Reach time.Duration // age of the oldest delta taken
	Far   bool          // it had to reach past 10 minutes
}

// deltas are oldest first; samples is what the rule counts in one delta.
func fibreSpanOf(deltas []fibreDelta, now time.Time, samples func(fibreDelta) int64) fibreSpan {
	var span fibreSpan
	var have int64
	for i := len(deltas) - 1; i >= 0; i-- {
		if deltas[i].empty() {
			continue
		}
		age := now.Sub(deltas[i].at)
		if age >= fibreKeep {
			break
		}
		if age >= fibreWindow {
			if have >= fibreMinSamples {
				break
			}
			span.Far = true
		}
		span.Sum.add(deltas[i])
		span.Reach = age
		have += samples(deltas[i])
	}
	return span
}

func (s fibreSpan) text() string {
	if s.Far {
		return "over the last " + shortDuration(s.Reach)
	}
	return "in the last 10 minutes"
}

func (d fibreDelta) empty() bool {
	d.at = time.Time{}
	d.occupancy, d.budget, d.haveOccupancy, d.haveBudget, d.known = 0, 0, false, false, 0
	d.extra = nil
	return d == fibreDelta{}
}

// Last cumulative value of a series, to tell a restart from growth.
type fibreSeries struct {
	start uint64
	value uint64
	sum   float64 // of a histogram: seconds that went with the count
	seen  time.Time
}

type fibreSender struct {
	histSeries    map[string]fibreHistogramBaseline
	resources     map[string]float64
	chain         string
	version       string
	since         time.Time // the first export this process got from it
	lastReceived  time.Time
	series        map[string]fibreSeries
	window        []fibreDelta
	occupancy     int64
	budget        int64
	lastSuccess   int64
	uploadsNow    int64 // in flight now, the last value exported
	downloadsNow  int64
	haveSuccess   bool
	haveOccupancy bool
	haveBudget    bool
	haveUploads   bool
	haveDownloads bool
}

type fibreStore struct {
	histograms map[string]map[string]fibreHistogram
	mu         sync.Mutex
	raw        map[string]map[string]*fibreRawSeries // per chain, what export_raw keeps
	rawNotes   map[string]bool
	rawSkipped uint64 // export_raw series dropped for a limit
	started    time.Time
	now        func() time.Time
	senders    map[string]*fibreSender
	ignored    map[string]time.Time // instance, last seen
	oversized  map[string]time.Time // sender address, last logged
	hours      map[string]map[int64]*fibreHour
	totals     map[string]map[string]float64
	outcomes   map[string]map[string]float64
	reach      map[string][]fibreReach
}

func newFibreStore() *fibreStore {
	return &fibreStore{
		started: time.Now(), now: time.Now, senders: make(map[string]*fibreSender), ignored: make(map[string]time.Time),
		hours: make(map[string]map[int64]*fibreHour), totals: make(map[string]map[string]float64),
		histograms: make(map[string]map[string]fibreHistogram),
		outcomes:   make(map[string]map[string]float64), reach: make(map[string][]fibreReach),
	}
}

// Created on first use so silence counts from about when tenderduty started.
func (c *Config) fibreData() *fibreStore {
	c.fibreOnce.Do(func() {
		if c.fibreStore == nil {
			c.fibreStore = newFibreStore()
		}
	})
	return c.fibreStore
}

type fibreSnapshot struct {
	Resources    map[string]float64
	Found        bool
	LastReceived time.Time
	Since        time.Time
	Version      string
	Occupancy    int64
	Budget       int64
	HaveStorage  bool
	LastSuccess  time.Time
	HaveSuccess  bool
	Window       fibreDelta // the last 10 minutes, for the rules that count
	Rejected30   int64      // rejections in the last 30 minutes
	Sign         fibreSpan  // every signature
	SignOK       fibreSpan  // the successful ones
	Backend      fibreSpan  // every object storage request

	HaveOccupancy, HaveBudget bool // a budget of 0 is the limiter being off, so HaveBudget says it was reported
	Ten, Day                  fibreSummary
	UploadsNow, DownloadsNow  int64 // requests being handled at the last export
	HaveInFlight              bool
}

// Covers is how much of the window this process has seen; the history is in memory.
type fibreSummary struct {
	Sum    fibreDelta
	Covers time.Duration
}

// The sender that exported last, if several hostnames map to the chain.
func (st *fibreStore) snapshot(chain string) fibreSnapshot {
	st.mu.Lock()
	defer st.mu.Unlock()
	var best *fibreSender
	for _, s := range st.senders {
		if s.chain == chain && (best == nil || s.lastReceived.After(best.lastReceived)) {
			best = s
		}
	}
	if best == nil {
		return fibreSnapshot{}
	}
	now := st.now()
	snap := fibreSnapshot{Found: true, LastReceived: best.lastReceived, Since: best.since, Version: best.version,
		Occupancy: best.occupancy, Budget: best.budget, HaveStorage: best.haveOccupancy && best.haveBudget, HaveSuccess: best.haveSuccess,
		HaveOccupancy: best.haveOccupancy, HaveBudget: best.haveBudget, UploadsNow: best.uploadsNow, DownloadsNow: best.downloadsNow,
		HaveInFlight: best.haveUploads || best.haveDownloads}
	snap.Resources = make(map[string]float64)
	addFibreValues(snap.Resources, best.resources)
	if best.haveSuccess {
		snap.LastSuccess = time.Unix(best.lastSuccess, 0)
	}
	for _, d := range best.window {
		age := now.Sub(d.at)
		if age < fibreWindow {
			snap.Window.add(d)
		}
		if age < fibreStorageQuiet {
			snap.Rejected30 += d.rejected
		}
		if age < fibreKeep {
			snap.Day.Sum.add(d)
		}
	}
	snap.Ten.Sum = snap.Window
	// merged at the cap, not cut, so it covers everything since the first export
	seen := max(now.Sub(best.since), 0)
	snap.Ten.Covers, snap.Day.Covers = min(fibreWindow, seen), min(fibreKeep, seen)
	snap.Sign = fibreSpanOf(best.window, now, func(d fibreDelta) int64 { return d.signTotal })
	snap.SignOK = fibreSpanOf(best.window, now, func(d fibreDelta) int64 { return d.signOK })
	snap.Backend = fibreSpanOf(best.window, now, func(d fibreDelta) int64 { return d.backendTotal })
	return snap
}

// The chain naming the hostname, else the only chain with Fibre alerts that names none.
func (c *Config) fibreChainFor(instance string) string {
	c.chainsMux.RLock()
	defer c.chainsMux.RUnlock()
	unnamed, count := "", 0
	for key, cc := range c.Chains {
		if cc == nil || !cc.alertSettings().FibreAlerts {
			continue
		}
		name := cc.name
		if name == "" {
			name = key
		}
		if instance != "" && cc.alertSettings().FibreInstance == instance {
			return name
		}
		if cc.alertSettings().FibreAlerts && cc.alertSettings().FibreInstance == "" {
			unnamed, count = name, count+1
		}
	}
	if count == 1 {
		return unnamed
	}
	return ""
}

func fibreAttr(attrs []*commonv1.KeyValue, key string) (string, bool) {
	for _, kv := range attrs {
		if kv.GetKey() == key {
			return fibreAttrText(kv.GetValue()), true
		}
	}
	return "", false
}

func fibreAttrText(v *commonv1.AnyValue) string {
	switch v.GetValue().(type) {
	case *commonv1.AnyValue_StringValue:
		return v.GetStringValue()
	case *commonv1.AnyValue_BoolValue:
		return strconv.FormatBool(v.GetBoolValue())
	case *commonv1.AnyValue_IntValue:
		return strconv.FormatInt(v.GetIntValue(), 10)
	case *commonv1.AnyValue_DoubleValue:
		return strconv.FormatFloat(v.GetDoubleValue(), 'g', -1, 64)
	}
	return ""
}

// Attribute order doesn't matter.
func fibreSeriesKey(metric string, attrs []*commonv1.KeyValue) string {
	parts := make([]string, 0, len(attrs))
	for _, kv := range attrs {
		parts = append(parts, kv.GetKey()+"="+fibreAttrText(kv.GetValue()))
	}
	sort.Strings(parts)
	return metric + "|" + strings.Join(parts, ",")
}

func fibreNumber(dp *metricsv1.NumberDataPoint) float64 {
	if _, ok := dp.GetValue().(*metricsv1.NumberDataPoint_AsDouble); ok {
		return dp.GetAsDouble()
	}
	return float64(dp.GetAsInt())
}

// A monotonic sum is never negative.
func fibreCount(dp *metricsv1.NumberDataPoint) uint64 {
	v := fibreNumber(dp)
	if v < 0 || math.IsNaN(v) {
		return 0
	}
	return uint64(math.Round(v))
}

// Also reads a sum, for senders that report a gauge that way.
func fibreGauge(m *metricsv1.Metric) (int64, bool) {
	var points []*metricsv1.NumberDataPoint
	switch {
	case m.GetGauge() != nil:
		points = m.GetGauge().GetDataPoints()
	case m.GetSum() != nil:
		points = m.GetSum().GetDataPoints()
	}
	var latest *metricsv1.NumberDataPoint
	for _, dp := range points {
		if latest == nil || dp.GetTimeUnixNano() >= latest.GetTimeUnixNano() {
			latest = dp
		}
	}
	if latest == nil {
		return 0, false
	}
	return int64(math.Round(fibreNumber(latest))), true
}

// Observations at or under 10ms. The bound is found by value because the sender chooses its buckets.
func fibreFast(dp *metricsv1.HistogramDataPoint) (uint64, bool) {
	counts := dp.GetBucketCounts()
	for i, bound := range dp.GetExplicitBounds() {
		if math.Abs(bound-fibreFastBound) < 1e-9 {
			if i >= len(counts) {
				return 0, false
			}
			var fast uint64
			for _, n := range counts[:i+1] {
				fast += n
			}
			return fast, true
		}
	}
	return 0, false
}

func clampInt64(v uint64) int64 {
	if v > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(v)
}

// A changed start time or a lower value is a restart: all of it is new. So is a series first seen on a known sender.
// On this process's first export, history counts only if Fibre started within the window.
func (s *fibreSender) step(key string, start, value uint64, cumulative, known bool, now time.Time) int64 {
	n, _ := s.advance(key, start, value, 0, cumulative, known, now)
	return n
}

// step for a histogram: the seconds follow the count, so a restart resets both.
func (s *fibreSender) advance(key string, start, value uint64, sum float64, cumulative, known bool, now time.Time) (int64, float64) {
	if math.IsNaN(sum) || sum < 0 {
		sum = 0
	}
	if !cumulative {
		return clampInt64(value), sum
	}
	prev, seen := s.series[key]
	s.series[key] = fibreSeries{start, value, sum, now}
	switch {
	case seen && prev.start == start && value >= prev.value:
		return clampInt64(value - prev.value), math.Max(sum-prev.sum, 0)
	case seen, known:
		return clampInt64(value), sum
	case start != 0 && now.Sub(time.Unix(0, clampInt64(start))) <= fibreWindow:
		return clampInt64(value), sum
	}
	return 0, 0
}

func (s *fibreSender) counter(name string, m *metricsv1.Metric, known bool, now time.Time) int64 {
	sum := m.GetSum()
	var n int64
	for _, dp := range sum.GetDataPoints() {
		n += s.step(fibreSeriesKey(name, dp.GetAttributes()), dp.GetStartTimeUnixNano(), fibreCount(dp), temporalityCumulative(sum.GetAggregationTemporality()), known, now)
	}
	return n
}

// Split on the success attribute, summed over the others (upload size). A series without it, the SDK's overflow one, is skipped.
type fibreDurations struct {
	ok, failed int64
	okSec      float64
}

func (s *fibreSender) durations(name string, m *metricsv1.Metric, known bool, now time.Time) fibreDurations {
	hist := m.GetHistogram()
	cumulative := temporalityCumulative(hist.GetAggregationTemporality())
	var out fibreDurations
	for _, dp := range hist.GetDataPoints() {
		success, ok := fibreAttr(dp.GetAttributes(), "success")
		if !ok {
			continue
		}
		n, sec := s.advance(fibreSeriesKey(name, dp.GetAttributes()), dp.GetStartTimeUnixNano(), dp.GetCount(), dp.GetSum(), cumulative, known, now)
		if success == "false" {
			out.failed += n
		} else {
			out.ok += n
			out.okSec += sec
		}
	}
	return out
}

// A cumulative up-down sum is the current number, not growth; a sender reporting changes is added up.
func (s *fibreSender) inFlight(m *metricsv1.Metric, current int64) int64 {
	sum := m.GetSum()
	var total int64
	for _, dp := range sum.GetDataPoints() {
		total += int64(math.Round(fibreNumber(dp)))
	}
	if !temporalityCumulative(sum.GetAggregationTemporality()) {
		total += current
	}
	return max(total, 0)
}

func temporalityCumulative(t metricsv1.AggregationTemporality) bool {
	return t != metricsv1.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA
}

func (st *fibreStore) record(chain, instance, version string, metrics []*metricsv1.Metric) {
	st.mu.Lock()
	defer st.mu.Unlock()
	now := st.now()
	s := st.senders[instance]
	if s == nil {
		st.makeRoom(chain)
		s = &fibreSender{series: make(map[string]fibreSeries)}
		st.senders[instance] = s
	}
	known := !s.lastReceived.IsZero()
	if !known {
		s.since = now
	}
	s.chain, s.version, s.lastReceived = chain, version, now
	d := fibreDelta{at: now, extra: &fibreExtra{Histograms: make(map[string]fibreHistogram), Gauges: make(map[string]float64), Counters: make(map[string]float64)}}
	st.recordExtra(s, &d, metrics, known, now)
	for _, m := range metrics {
		name, ok := strings.CutPrefix(m.GetName(), fibreMetricPfx)
		if !ok {
			continue
		}
		switch name {
		case "upload_shard.occupancy_bytes":
			if v, ok := fibreGauge(m); ok {
				s.occupancy, s.haveOccupancy = v, true
				d.occupancy, d.haveOccupancy = v, true
			}
		case "upload_shard.budget_bytes":
			if v, ok := fibreGauge(m); ok {
				s.budget, s.haveBudget = v, true
				d.budget, d.haveBudget = v, true
			}
		case "upload_shard.last_success_timestamp":
			if v, ok := fibreGauge(m); ok {
				s.lastSuccess, s.haveSuccess = v, true
			}
		case "upload_shard.rejected":
			d.rejected += s.counter(name, m, known, now)
		case "upload_shard.bytes":
			d.uploadBytes += s.counter(name, m, known, now)
		case "download_shard.bytes":
			d.downloadBytes += s.counter(name, m, known, now)
		case "prune.entries":
			d.pruneEntries += s.counter(name, m, known, now)
		case "upload_shard.in_flight":
			if len(m.GetSum().GetDataPoints()) > 0 {
				s.uploadsNow, s.haveUploads = s.inFlight(m, s.uploadsNow), true
			}
		case "download_shard.in_flight":
			if len(m.GetSum().GetDataPoints()) > 0 {
				s.downloadsNow, s.haveDownloads = s.inFlight(m, s.downloadsNow), true
			}
		case "upload_shard.duration":
			u := s.durations(name, m, known, now)
			d.uploadOK += u.ok
			d.uploadFailed += u.failed
			d.uploadSec += u.okSec
		case "download_shard.duration":
			u := s.durations(name, m, known, now)
			d.downloadOK += u.ok
			d.downloadFailed += u.failed
			d.downloadSec += u.okSec
		case "sign.duration":
			hist := m.GetHistogram()
			for _, dp := range hist.GetDataPoints() {
				key, cumulative := fibreSeriesKey(name, dp.GetAttributes()), temporalityCumulative(hist.GetAggregationTemporality())
				success, _ := fibreAttr(dp.GetAttributes(), "success")
				if success == "false" {
					n := s.step(key+"#total", dp.GetStartTimeUnixNano(), dp.GetCount(), cumulative, known, now)
					d.signTotal += n
					d.signFailed += n
					continue
				}
				// no 10ms bound in the buckets: can't say how fast it signs
				fast, ok := fibreFast(dp)
				if !ok {
					continue
				}
				n, sec := s.advance(key+"#total", dp.GetStartTimeUnixNano(), dp.GetCount(), dp.GetSum(), cumulative, known, now)
				d.signTotal += n
				d.signOK += n
				d.signSec += sec
				d.signFast += s.step(key+"#fast", dp.GetStartTimeUnixNano(), fast, cumulative, known, now)
			}
		case "store.put.duration":
			u := s.durations(name, m, known, now)
			d.storePut += u.failed
			d.putOK += u.ok
			d.putSec += u.okSec
		case "store.get.duration":
			u := s.durations(name, m, known, now)
			d.storeGet += u.failed
			d.getOK += u.ok
			d.getSec += u.okSec
		case "prune.duration":
			u := s.durations(name, m, known, now)
			d.prune += u.failed
			d.pruneOK += u.ok
			d.pruneSec += u.okSec
		case "backend.get.duration":
			hist := m.GetHistogram()
			for _, dp := range hist.GetDataPoints() {
				outcome, _ := fibreAttr(dp.GetAttributes(), "outcome")
				n := s.step(fibreSeriesKey(name, dp.GetAttributes()), dp.GetStartTimeUnixNano(), dp.GetCount(), temporalityCumulative(hist.GetAggregationTemporality()), known, now)
				d.backendTotal += n // success, not_found and canceled are requests too
				if st.outcomes[chain] == nil {
					st.outcomes[chain] = make(map[string]float64)
				}
				switch outcome {
				case "success", "not_found", "canceled", "timeout", "throttled", "error":
				default:
					outcome = "unknown"
				}
				st.outcomes[chain][outcome] += float64(n)
				switch outcome {
				case "timeout":
					d.backendTimeout += n
				case "throttled":
					d.backendThrottled += n
				case "error":
					d.backendError += n
				}
			}
		}
	}
	st.recordHour(chain, &d, s, metrics)
	kept := s.window[:0]
	for _, old := range s.window {
		if now.Sub(old.at) < fibreKeep {
			kept = append(kept, old)
		}
	}
	s.window = kept
	s.window = append(s.window, d)
	if len(s.window) > fibreMaxDeltas {
		s.window = compactDeltas(s.window)
	}
	// series carry the upload size as a label, so ones that stopped coming are dropped
	for key, series := range s.series {
		if now.Sub(series.seen) >= fibreKeep {
			delete(s.series, key)
		}
	}
}

// Drops the chain's least recently seen sender when it has the most allowed. Caller holds the lock.
func (st *fibreStore) makeRoom(chain string) {
	count, found := 0, false
	var oldest string
	var oldestAt time.Time
	for instance, s := range st.senders {
		if s.chain != chain {
			continue
		}
		count++
		if !found || s.lastReceived.Before(oldestAt) {
			oldest, oldestAt, found = instance, s.lastReceived, true
		}
	}
	if count >= fibreMaxSenders {
		delete(st.senders, oldest)
	}
}

// Remembers an unclaimed instance, the least recently seen goes at the cap. True the first time.
func (st *fibreStore) noteIgnored(instance string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.ignored == nil {
		st.ignored = make(map[string]time.Time)
	}
	_, known := st.ignored[instance]
	now := st.now()
	st.ignored[instance] = now
	for len(st.ignored) > fibreMaxIgnored {
		oldest, oldestAt, found := "", now, false
		for id, seen := range st.ignored {
			if id != instance && (!found || seen.Before(oldestAt)) {
				oldest, oldestAt, found = id, seen, true
			}
		}
		delete(st.ignored, oldest)
	}
	return !known
}

// clipBytes cuts at n bytes without splitting a character.
func clipBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// Merges the older half pairwise: coarser but still covers the day. The older half is well past the count rules' 10 minutes.
func compactDeltas(window []fibreDelta) []fibreDelta {
	half := len(window) / 2
	out := make([]fibreDelta, 0, (half+1)/2+len(window)-half)
	for i := 0; i < half; i += 2 {
		d := window[i]
		if i+1 < half {
			d.add(window[i+1])
			d.at = window[i+1].at
		}
		out = append(out, d)
	}
	return append(out, window[half:]...)
}

// Ignores other apps' metrics a collector forwards, and a Fibre no chain claims.
func (c *Config) ingestFibreMetrics(data *metricsv1.MetricsData) {
	st := c.fibreData()
	settings := c.settings()
	// read per export: a reload can turn it on or off
	raw := settings.FibreMetrics.ExportRaw && settings.Prom
	if !raw {
		st.dropRaw()
	}
	for _, rm := range data.GetResourceMetrics() {
		attrs := rm.GetResource().GetAttributes()
		if name, _ := fibreAttr(attrs, "service.name"); name != "fibre" {
			continue
		}
		instance, _ := fibreAttr(attrs, "service.instance.id")
		version, _ := fibreAttr(attrs, "service.version")
		instance, version = clipBytes(instance, fibreMaxIdentity), clipBytes(version, fibreMaxIdentity)
		chain := c.fibreChainFor(instance)
		if chain == "" {
			if st.noteIgnored(instance) {
				l("ignoring Fibre metrics from", strconv.Quote(instance)+": no chain has fibre_instance set to it, and not exactly one chain with fibre_enabled leaves it empty")
			}
			continue
		}
		var metrics []*metricsv1.Metric
		for _, sm := range rm.GetScopeMetrics() {
			metrics = append(metrics, sm.GetMetrics()...)
		}
		st.record(chain, instance, version, metrics)
		if raw {
			st.recordRaw(chain, rm)
		}
	}
}

// OTLP/HTTP receiver. Paths match by suffix so a reverse proxy's prefix works.
func (c *Config) fibreMetricsHandler() http.Handler {
	decoding := make(chan struct{}, fibreMaxDecoding)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimSuffix(r.URL.Path, "/")
		traces := strings.HasSuffix(path, "/v1/traces")
		if !traces && !strings.HasSuffix(path, "/v1/metrics") {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "use POST", http.StatusMethodNotAllowed)
			return
		}
		// before reading the body
		if token := c.settings().FibreMetrics.Token; token != "" &&
			subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if traces {
			respondFibreOK(w)
			return
		}
		contentType, _, _ := strings.Cut(r.Header.Get("Content-Type"), ";")
		if !strings.EqualFold(strings.TrimSpace(contentType), "application/x-protobuf") {
			http.Error(w, "send application/x-protobuf", http.StatusUnsupportedMediaType)
			return
		}
		encoding := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Encoding")))
		if encoding != "" && encoding != "gzip" {
			http.Error(w, "Content-Encoding can be gzip or empty", http.StatusUnsupportedMediaType)
			return
		}
		select {
		case decoding <- struct{}{}:
			defer func() { <-decoding }()
		default:
			w.Header().Set("Retry-After", "10")
			http.Error(w, "busy, try again", http.StatusServiceUnavailable)
			return
		}
		body, status, err := readFibreBody(w, r, encoding == "gzip")
		if err != nil {
			var tooLarge *fibreTooLargeError
			if errors.As(err, &tooLarge) {
				host, _, splitErr := net.SplitHostPort(r.RemoteAddr)
				if splitErr != nil {
					host = r.RemoteAddr
				}
				if c.fibreData().noteOversized(host) {
					l(fibreOversizeMessage(r.ContentLength, tooLarge.unzipped))
				}
			}
			http.Error(w, err.Error(), status)
			return
		}
		var data metricsv1.MetricsData
		if err := proto.Unmarshal(body, &data); err != nil {
			http.Error(w, "not an OTLP metrics request", http.StatusBadRequest)
			return
		}
		c.ingestFibreMetrics(&data)
		respondFibreOK(w)
	})
}

// An empty ExportMetricsServiceResponse.
func respondFibreOK(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/x-protobuf")
	w.WriteHeader(http.StatusOK)
}

type fibreTooLargeError struct{ unzipped bool }

func (e *fibreTooLargeError) Error() string {
	if e.unzipped {
		return "request body is too large once decompressed"
	}
	return "request body is too large"
}

// fibreOversizeMessage uses the declared size when there is one, else only the limit.
func fibreOversizeMessage(size int64, unzipped bool) string {
	if unzipped {
		return "Fibre export is over the 4 MiB limit once decompressed"
	}
	if size > fibreMaxBody {
		return fmt.Sprintf("Fibre export of %.1f MiB is over the 1 MiB limit", float64(size)/(1<<20))
	}
	return "Fibre export is over the 1 MiB limit"
}

// True when the sender has not been logged for an hour; the least recently logged goes at the cap.
func (st *fibreStore) noteOversized(sender string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.oversized == nil {
		st.oversized = make(map[string]time.Time)
	}
	now := st.now()
	if last, ok := st.oversized[sender]; ok && now.Sub(last) < time.Hour {
		return false
	}
	st.oversized[sender] = now
	for len(st.oversized) > fibreMaxIgnored {
		oldest, oldestAt, found := "", now, false
		for id, seen := range st.oversized {
			if id != sender && (!found || seen.Before(oldestAt)) {
				oldest, oldestAt, found = id, seen, true
			}
		}
		delete(st.oversized, oldest)
	}
	return true
}

func readFibreBody(w http.ResponseWriter, r *http.Request, gzipped bool) ([]byte, int, error) {
	var tooBig *http.MaxBytesError
	var reader io.Reader = http.MaxBytesReader(w, r.Body, fibreMaxBody)
	if gzipped {
		zr, err := gzip.NewReader(reader)
		if err != nil {
			if errors.As(err, &tooBig) {
				return nil, http.StatusRequestEntityTooLarge, &fibreTooLargeError{}
			}
			return nil, http.StatusBadRequest, errors.New("body is not valid gzip")
		}
		defer func() { _ = zr.Close() }()
		reader = zr
	}
	body, err := io.ReadAll(io.LimitReader(reader, fibreMaxUnzipped+1))
	switch {
	case errors.As(err, &tooBig):
		return nil, http.StatusRequestEntityTooLarge, &fibreTooLargeError{}
	case err != nil:
		return nil, http.StatusBadRequest, errors.New("could not read the body")
	case len(body) > fibreMaxUnzipped:
		return nil, http.StatusRequestEntityTooLarge, &fibreTooLargeError{unzipped: true}
	}
	return body, http.StatusOK, nil
}

func (c *Config) fibreMetricsServer() *http.Server {
	srv := &http.Server{
		Handler:           c.fibreMetricsHandler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	if c.FibreMetrics.TLSCert != "" {
		srv.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	return srv
}

func (c *Config) serveFibreMetrics(ctx context.Context) error {
	listener, err := net.Listen("tcp", c.FibreMetrics.Listen)
	if err != nil {
		return err
	}
	return c.serveFibreMetricsOn(ctx, listener)
}

func (c *Config) serveFibreMetricsOn(ctx context.Context, listener net.Listener) error {
	srv := c.fibreMetricsServer()
	scheme := "http"
	if c.FibreMetrics.TLSCert != "" {
		scheme = "https"
	}
	l(fmt.Sprintf("receiving Fibre metrics at %s://%s", scheme, listener.Addr()))
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = srv.Close()
		case <-done:
		}
	}()
	var err error
	if c.FibreMetrics.TLSCert != "" {
		err = srv.ServeTLS(listener, c.FibreMetrics.TLSCert, c.FibreMetrics.TLSKey)
	} else {
		err = srv.Serve(listener)
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func fibreMetricsLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func validateFibreMetrics(c *Config) (fatal bool, problems []string) {
	m := c.FibreMetrics
	if (m.TLSCert == "") != (m.TLSKey == "") {
		fatal = true
		problems = append(problems, "error: fibre_metrics needs both tls_cert and tls_key, or neither")
	}
	if m.ExportRaw && !c.Prom {
		problems = append(problems, "warning: fibre_metrics export_raw needs prometheus_enabled, there is nowhere to export the metrics to")
	}
	if m.Listen == "" {
		return
	}
	host, port, err := net.SplitHostPort(m.Listen)
	if n, convErr := strconv.Atoi(port); err != nil || convErr != nil || n < 1 || n > 65535 {
		fatal = true
		problems = append(problems, fmt.Sprintf("error: fibre_metrics listen %q is not a valid host:port", m.Listen))
		return
	}
	if !fibreMetricsLoopback(host) && m.Token == "" {
		if m.AllowWithoutToken {
			problems = append(problems, fmt.Sprintf("warning: fibre_metrics listens on %s without a token; make sure a firewall lets only Fibre's IP reach it", m.Listen))
		} else {
			fatal = true
			problems = append(problems, fmt.Sprintf("error: fibre_metrics listens on %s without a token. Anyone who can reach it could send metrics that look healthy and hide a Fibre that is down, set a token or listen on 127.0.0.1", m.Listen))
		}
	}
	if !fibreMetricsLoopback(host) && m.Token != "" && m.TLSCert == "" {
		problems = append(problems, fmt.Sprintf("warning: fibre_metrics listens on %s with a token but no tls_cert, the token travels in cleartext. Set tls_cert and tls_key, or put it behind a TLS proxy", m.Listen))
	}
	enabled, unnamed := 0, 0
	for _, v := range c.Chains {
		if v != nil && v.Alerts.FibreAlerts {
			enabled++
			if v.Alerts.FibreInstance == "" {
				unnamed++
			}
		}
	}
	if enabled == 0 {
		problems = append(problems, "warning: fibre_metrics is listening but no chain has fibre_enabled, nothing will use the metrics")
	}
	if enabled > 1 && unnamed > 0 {
		problems = append(problems, "warning: more than one chain has fibre_enabled and some have no fibre_instance, only one chain can take metrics from a Fibre that isn't named")
	}
	return
}

// messages are alarm keys; none may start like a provider message ("Fibre at ", "Validator is not")
const (
	fibreSilentPrefix        = "Fibre has sent no metrics for over "
	fibreStoragePrefix       = "Fibre storage is nearly full on "
	fibreSignFailedPrefix    = "Fibre could not sign payment promises on "
	fibreSigningPrefix       = "Fibre signing is slower than 10ms on "
	fibreStorageErrorsPrefix = "Fibre storage operations are failing on "
	fibreObjectPrefix        = "Fibre object storage requests are failing on "
	fibreIdlePrefix          = "Fibre has had no successful upload for over "
)

const (
	kindSilent        = "silent"
	kindStorage       = "storage"
	kindSignFailed    = "signfailed"
	kindSigning       = "signing"
	kindStorageErrors = "storageerrors"
	kindObject        = "objectstorage"
	kindIdle          = "idle"
)

var fibreMetricsKinds = []struct{ kind, prefix string }{
	{kindSilent, fibreSilentPrefix},
	{kindStorage, fibreStoragePrefix},
	{kindSignFailed, fibreSignFailedPrefix},
	{kindSigning, fibreSigningPrefix},
	{kindStorageErrors, fibreStorageErrorsPrefix},
	{kindObject, fibreObjectPrefix},
	{kindIdle, fibreIdlePrefix},
}

func fibreMetricsKind(message string) string {
	for _, k := range fibreMetricsKinds {
		if strings.HasPrefix(message, k.prefix) {
			return k.kind
		}
	}
	return ""
}

func isFibreMetricsMessage(message string) bool {
	return fibreMetricsKind(message) != ""
}

func (cc *ChainConfig) fibreMetricsMessage(kind string) string {
	switch kind {
	case kindSilent:
		return fmt.Sprintf("%s%s on %s", fibreSilentPrefix, plural(td.settings().NodeDownMin, "minute"), cc.ChainId)
	case kindStorage:
		return fibreStoragePrefix + cc.ChainId + ", the budget the chain gives this validator is almost used up"
	case kindSignFailed:
		return fibreSignFailedPrefix + cc.ChainId
	case kindSigning:
		return fibreSigningPrefix + cc.ChainId
	case kindStorageErrors:
		return fibreStorageErrorsPrefix + cc.ChainId
	case kindObject:
		return fibreObjectPrefix + cc.ChainId
	}
	return fmt.Sprintf("%s%s on %s", fibreIdlePrefix, plural(cc.alertSettings().FibreIdleMinutes, "minute"), cc.ChainId)
}

// storage and idle are opt-in
func (cc *ChainConfig) fibreMetricsKindsOn() []string {
	kinds := []string{kindSilent, kindSignFailed, kindSigning, kindStorageErrors, kindObject}
	if cc.alertSettings().FibreStoragePercent > 0 {
		kinds = append(kinds, kindStorage)
	}
	if cc.alertSettings().FibreIdleMinutes > 0 {
		kinds = append(kinds, kindIdle)
	}
	return kinds
}

func fibreMetricsAlarmId(valInfo *ValInfo, message string) string {
	return valInfo.Valcons + "fibremetrics" + fibreMetricsKind(message)
}

const (
	fibreHold = iota // no evidence either way, an alarm keeps its state
	fibreRaise
	fibreClear
)

// Alarms are restored from the state file but metrics are not, so a kind is held until an export says otherwise.
func (cc *ChainConfig) fibreVerdicts(snap fibreSnapshot, started, now time.Time) map[string]int {
	silentAfter := time.Duration(td.settings().NodeDownMin) * time.Minute
	v := make(map[string]int)
	switch since := snap.LastReceived; {
	// silence counts from tenderduty's start while nothing has arrived
	case since.IsZero() && now.Sub(started) >= silentAfter, !since.IsZero() && now.Sub(since) >= silentAfter:
		v[kindSilent] = fibreRaise
	case since.IsZero():
		v[kindSilent] = fibreHold
	default:
		v[kindSilent] = fibreClear
	}
	fresh := !snap.LastReceived.IsZero() && now.Sub(snap.LastReceived) < silentAfter
	// a quiet spell counts only once this process has watched all of it
	watched := func(d time.Duration) bool { return fresh && now.Sub(snap.Since) >= d }
	for _, kind := range cc.fibreMetricsKindsOn() {
		if kind == kindSilent {
			continue
		}
		v[kind] = fibreHold
	}
	if !fresh {
		return v
	}
	w := snap.Window
	// raises on any count, clears once the window is quiet and fully seen
	switch n := w.storePut + w.storeGet + w.prune; {
	case n > 0:
		v[kindStorageErrors] = fibreRaise
	case watched(fibreWindow):
		v[kindStorageErrors] = fibreClear
	}
	if cc.alertSettings().FibreStoragePercent > 0 {
		v[kindStorage] = cc.fibreStorageVerdict(snap, watched(fibreStorageQuiet))
	}
	// raise over 1%, clear at or under it once the failures are behind the window
	v[kindSignFailed] = fibreShare(snap.Sign.Sum.signFailed, snap.Sign.Sum.signTotal, watched(fibreKeep))
	v[kindObject] = fibreShare(snap.Backend.Sum.backendTimeout+snap.Backend.Sum.backendThrottled+snap.Backend.Sum.backendError,
		snap.Backend.Sum.backendTotal, watched(fibreKeep))
	// too few samples say nothing about speed: keep state
	if ok := snap.SignOK.Sum; ok.signOK >= fibreMinSamples {
		v[kindSigning] = fibreClear
		if 2*ok.signFast < ok.signOK {
			v[kindSigning] = fibreRaise
		}
	}
	if cc.alertSettings().FibreIdleMinutes > 0 && snap.HaveSuccess {
		v[kindIdle] = fibreClear
		if now.Sub(snap.LastSuccess) > time.Duration(cc.alertSettings().FibreIdleMinutes)*time.Minute {
			v[kindIdle] = fibreRaise
		}
	}
	return v
}

// Over 1% raises. Clears only with enough samples, or after a watched day (a quiet network's last failure is out of the
// window by then); otherwise hold.
func fibreShare(failed, total int64, watchedDay bool) int {
	switch {
	case failed > 0 && failed*100 > total:
		return fibreRaise
	case total >= fibreMinSamples || watchedDay:
		return fibreClear
	}
	return fibreHold
}

// Percent of budget storage must fall under to clear; a threshold of 15 or less has no room for the gap.
func (cc *ChainConfig) fibreStorageLow() int {
	if low := cc.alertSettings().FibreStoragePercent - fibreStorageGap; low >= 1 {
		return low
	}
	return cc.alertSettings().FibreStoragePercent
}

// Raises at the threshold or on a rejection in the last 10 minutes. Clears under the lower threshold with none for 30
// watched minutes, so a network hovering at the limit doesn't flap.
func (cc *ChainConfig) fibreStorageVerdict(snap fibreSnapshot, watched30 bool) int {
	full := snap.HaveStorage && snap.Budget > 0 && snap.Occupancy*100 >= int64(cc.alertSettings().FibreStoragePercent)*snap.Budget
	if full || snap.Window.rejected > 0 {
		return fibreRaise
	}
	// a budget of 0 is the limiter being off, not an empty disk
	low := snap.HaveStorage && (snap.Budget <= 0 || snap.Occupancy*100 < int64(cc.fibreStorageLow())*snap.Budget)
	if low && snap.Rejected30 == 0 && watched30 {
		return fibreClear
	}
	return fibreHold
}

func (cc *ChainConfig) fibreMetricsResolution(message string, snap fibreSnapshot) resolution {
	switch fibreMetricsKind(message) {
	case kindSilent:
		return resolution{"✅", "Fibre sending", "Fibre metrics are arriving again on " + cc.ChainId}
	case kindStorage:
		return resolution{"✅", "Storage OK", fmt.Sprintf("Fibre storage is back under %d%% of its budget on %s, no uploads turned away for 30 minutes", cc.fibreStorageLow(), cc.ChainId)}
	case kindSignFailed:
		return resolution{"✅", "Signing OK", "Fibre is signing payment promises again on " + cc.ChainId}
	case kindSigning:
		return resolution{"✅", "Signing OK", "Fibre signing is back under 10ms on " + cc.ChainId}
	case kindStorageErrors:
		return resolution{"✅", "Storage OK", "Fibre storage operations are succeeding again on " + cc.ChainId}
	case kindObject:
		return resolution{"✅", "Object storage OK", "Fibre object storage requests are succeeding again on " + cc.ChainId}
	}
	return resolution{"✅", "Uploads arriving", "Fibre stored an upload again on " + cc.ChainId}
}

// Like fibreAlarms.
func (cc *ChainConfig) fibreMetricsAlarms(valInfo *ValInfo, reconcile bool) {
	active := make(map[string]bool)
	for message := range activeMessages(cc.name, "Fibre ") {
		if isFibreMetricsMessage(message) {
			active[message] = true
		}
	}
	cc.stateMux.RLock()
	unsupported, providerChecked := cc.fibreUnsupported, cc.fibreLoaded
	cc.stateMux.RUnlock()
	// no x/valaddr: no Fibre to hear from. fibre_enabled can sit in alert_defaults for every chain; these must not read
	// "silent", and anything restored clears.
	if unsupported {
		for message := range active {
			resolve(cc.name, message, orphanResolution(message))
			id := fibreMetricsAlarmId(valInfo, message)
			td.alert(cc.name, message, "info", true, &id)
		}
		return
	}
	st := td.fibreData()
	snap := st.snapshot(cc.name)
	verdicts := cc.fibreVerdicts(snap, st.started, st.now())
	// silence means nothing until the provider check has seen the chain has Fibre
	if !providerChecked && verdicts[kindSilent] == fibreRaise {
		verdicts[kindSilent] = fibreHold
	}
	wanted := make(map[string]string)
	for kind, verdict := range verdicts {
		message := cc.fibreMetricsMessage(kind)
		if verdict == fibreRaise || verdict == fibreHold && active[message] {
			wanted[message] = fibreMetricsAlarmId(valInfo, message)
		}
	}
	on := make(map[string]bool)
	for _, kind := range cc.fibreMetricsKindsOn() {
		on[kind] = true
	}
	for message := range active {
		if _, ok := wanted[message]; !ok {
			// turned off since it was raised: not better, just no longer watched
			if on[fibreMetricsKind(message)] {
				resolve(cc.name, message, cc.fibreMetricsResolution(message, snap))
			} else {
				resolve(cc.name, message, orphanResolution(message))
			}
			id := fibreMetricsAlarmId(valInfo, message)
			td.alert(cc.name, message, "info", true, &id)
		}
	}
	for message, id := range wanted {
		if !active[message] || reconcile {
			id := id
			td.alert(cc.name, message, cc.alertSettings().FibrePriority, false, &id)
		}
	}
}

func fibreMetricsLabel(msg *alertMsg) (icon, state string) {
	if msg.resolved {
		return "✅", "Resolved"
	}
	switch fibreMetricsKind(msg.message) {
	case kindSilent:
		return "🔇", "Fibre silent"
	case kindStorage:
		return "🗄️", "Fibre storage full"
	case kindSignFailed:
		return "✍️", "Signing failed"
	case kindSigning:
		return "🐢", "Signing slow"
	case kindStorageErrors:
		return "🚨", "Storage errors"
	case kindObject:
		return "🚨", "Object storage errors"
	}
	return "💤", "No uploads"
}

// decimal units: 900 MB, 1.5 GB
func humanBytes(n int64) string {
	value, unit := float64(n), 0
	for value >= 1000 && unit < 4 {
		value /= 1000
		unit++
	}
	text := strconv.FormatFloat(value, 'f', 1, 64)
	text = strings.TrimSuffix(text, ".0")
	return text + " " + []string{"B", "KB", "MB", "GB", "TB"}[unit]
}

// Second line of an alert, from the last export.
func fibreMetricsDetail(chain, message string) string {
	td.chainsMux.RLock()
	cc := td.Chains[chain]
	td.chainsMux.RUnlock()
	if cc == nil {
		return ""
	}
	st := td.fibreData()
	snap, now := st.snapshot(cc.name), st.now()
	w := snap.Window
	switch fibreMetricsKind(message) {
	case kindSilent:
		if snap.LastReceived.IsZero() {
			return "no metrics since tenderduty started"
		}
		return "last metrics " + shortDuration(now.Sub(snap.LastReceived)) + " ago"
	case kindStorage:
		used := ""
		switch {
		case snap.HaveStorage && snap.Budget > 0:
			used = humanBytes(snap.Occupancy) + " of " + humanBytes(snap.Budget) + ", "
		case snap.HaveStorage:
			used = humanBytes(snap.Occupancy) + " used, "
		}
		noun := "uploads"
		if w.rejected == 1 {
			noun = "upload"
		}
		return fmt.Sprintf("%s%d %s turned away in the last 10 minutes. The budget is set by the chain from your stake, the whole network fills together", used, w.rejected, noun)
	case kindSignFailed:
		if s := snap.Sign; s.Sum.signTotal > 0 {
			return fmt.Sprintf("%d of %d signatures failed %s", s.Sum.signFailed, s.Sum.signTotal, s.text())
		}
	case kindSigning:
		if s := snap.SignOK; s.Sum.signOK > 0 {
			return fmt.Sprintf("%d of %d signatures took over 10ms %s", s.Sum.signOK-s.Sum.signFast, s.Sum.signOK, s.text())
		}
	case kindStorageErrors:
		return fmt.Sprintf("put %d, get %d, prune %d failed in the last 10 minutes", w.storePut, w.storeGet, w.prune)
	case kindObject:
		if s := snap.Backend; s.Sum.backendTotal > 0 {
			return fmt.Sprintf("timeout %d, throttled %d, error %d of %d requests %s", s.Sum.backendTimeout, s.Sum.backendThrottled, s.Sum.backendError, s.Sum.backendTotal, s.text())
		}
	case kindIdle:
		if snap.HaveSuccess {
			return "last successful upload " + shortDuration(now.Sub(snap.LastSuccess)) + " ago"
		}
	}
	return ""
}
