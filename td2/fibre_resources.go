package tenderduty

import (
	"math"
	"time"

	metricsv1 "go.opentelemetry.io/proto/otlp/metrics/v1"
)

// OTLP buckets are disjoint; made cumulative only for Prometheus.
type fibreHistogram struct {
	Bounds  []float64 `json:"bounds"`
	Buckets []uint64  `json:"buckets"`
	Count   uint64    `json:"count"`
	Sum     float64   `json:"sum"`
}

type fibreHistogramBaseline struct {
	Histogram fibreHistogram
	Start     uint64
	Seen      time.Time
}

type fibreExtra struct {
	Histograms map[string]fibreHistogram `json:"histograms,omitempty"`
	Gauges     map[string]float64        `json:"gauges,omitempty"`
	Counters   map[string]float64        `json:"counters,omitempty"`
}

var fibreHistogramNames = map[string]string{
	"upload_shard.duration":   "upload",
	"download_shard.duration": "download",
	"sign.duration":           "sign",
	"store.put.duration":      "store_put",
	"store.get.duration":      "store_get",
	"prune.duration":          "prune",
	"go.schedule.duration":    "go_schedule",
}

var fibreRuntimeGauges = map[string]string{
	"go.memory.used":     "go_memory_used_bytes",
	"go.memory.limit":    "go_memory_limit_bytes",
	"go.memory.gc.goal":  "go_gc_goal_bytes",
	"go.goroutine.count": "go_goroutines",
	"go.processor.limit": "go_processor_limit",
	"go.config.gogc":     "go_gogc",
}

func sameFibreBounds(a, b []float64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func validFibreHistogram(h fibreHistogram) bool {
	if len(h.Bounds) > 256 || len(h.Buckets) != len(h.Bounds)+1 || math.IsNaN(h.Sum) || math.IsInf(h.Sum, 0) || h.Sum < 0 {
		return false
	}
	var count uint64
	for i, bound := range h.Bounds {
		if math.IsNaN(bound) || math.IsInf(bound, 0) || bound < 0 || (i > 0 && bound <= h.Bounds[i-1]) {
			return false
		}
	}
	for _, n := range h.Buckets {
		if n > math.MaxUint64-count {
			return false
		}
		count += n
	}
	return count == h.Count
}

func addFibreHistogram(a, b fibreHistogram) fibreHistogram {
	if !sameFibreBounds(a.Bounds, b.Bounds) || len(a.Buckets) != len(b.Buckets) {
		a = fibreHistogram{Bounds: append([]float64(nil), b.Bounds...), Buckets: make([]uint64, len(b.Buckets))}
	} else {
		a.Buckets = append([]uint64(nil), a.Buckets...)
	}
	for i, n := range b.Buckets {
		a.Buckets[i] += n
	}
	a.Count += b.Count
	a.Sum += b.Sum
	return a
}

// Copy on merge: compacting or aggregating history must not mutate earlier intervals.
func mergeFibreExtra(a, b *fibreExtra) *fibreExtra {
	if a == nil && b == nil {
		return nil
	}
	out := &fibreExtra{Histograms: make(map[string]fibreHistogram), Gauges: make(map[string]float64), Counters: make(map[string]float64)}
	for _, src := range []*fibreExtra{a, b} {
		if src == nil {
			continue
		}
		for k, h := range src.Histograms {
			out.Histograms[k] = addFibreHistogram(out.Histograms[k], h)
		}
		addFibreValues(out.Counters, src.Counters)
		for k, v := range src.Gauges {
			out.Gauges[k] = v
		}
		if src.Gauges["go_memory_no_limit"] == 1 {
			delete(out.Gauges, "go_memory_limit_bytes")
		}
	}
	return out
}

// Linear interpolation within a bucket matches histogram_quantile for nonnegative durations.
func (h fibreHistogram) quantile(q float64) *float64 {
	if h.Count == 0 || !validFibreHistogram(h) {
		return nil
	}
	rank, before, lower := q*float64(h.Count), uint64(0), 0.0
	for i, n := range h.Buckets {
		if float64(before+n) >= rank && n > 0 {
			v := lower
			if i < len(h.Bounds) {
				v += (h.Bounds[i] - lower) * (rank - float64(before)) / float64(n)
			}
			return &v
		}
		before += n
		if i < len(h.Bounds) {
			lower = h.Bounds[i]
		}
	}
	return nil
}

func (s *fibreSender) histogramDelta(key string, dp *metricsv1.HistogramDataPoint, cumulative, known bool, now time.Time) (fibreHistogram, bool) {
	h := fibreHistogram{Bounds: append([]float64(nil), dp.GetExplicitBounds()...), Buckets: append([]uint64(nil), dp.GetBucketCounts()...), Count: dp.GetCount(), Sum: dp.GetSum()}
	if !validFibreHistogram(h) {
		return fibreHistogram{}, false
	}
	if s.histSeries == nil {
		s.histSeries = make(map[string]fibreHistogramBaseline)
	}
	prev, seen := s.histSeries[key]
	s.histSeries[key] = fibreHistogramBaseline{Histogram: h, Start: dp.GetStartTimeUnixNano(), Seen: now}
	if !cumulative {
		return h, true
	}
	compatible := seen && sameFibreBounds(prev.Histogram.Bounds, h.Bounds)
	growth := compatible && prev.Start == dp.GetStartTimeUnixNano() && h.Count >= prev.Histogram.Count && h.Sum >= prev.Histogram.Sum
	if growth {
		for i, n := range h.Buckets {
			if n < prev.Histogram.Buckets[i] {
				growth = false
				break
			}
		}
	}
	if growth {
		delta := h
		delta.Buckets = append([]uint64(nil), h.Buckets...)
		for i := range delta.Buckets {
			delta.Buckets[i] -= prev.Histogram.Buckets[i]
		}
		delta.Count -= prev.Histogram.Count
		delta.Sum -= prev.Histogram.Sum
		return delta, true
	}
	if seen && !compatible {
		// New schema: baseline only, don't import old cumulative values.
		h.Count, h.Sum = 0, 0
		h.Buckets = make([]uint64, len(h.Buckets))
		return h, true
	}
	if seen || known || (dp.GetStartTimeUnixNano() != 0 && now.Sub(time.Unix(0, clampInt64(dp.GetStartTimeUnixNano()))) <= fibreWindow) {
		return h, true
	}
	h.Count, h.Sum = 0, 0
	h.Buckets = make([]uint64, len(h.Buckets))
	return h, true
}

func (st *fibreStore) recordExtra(s *fibreSender, d *fibreDelta, metrics []*metricsv1.Metric, known bool, now time.Time) {
	if s.resources == nil {
		s.resources = make(map[string]float64)
	}
	for _, m := range metrics {
		name := m.GetName()
		if len(name) >= len(fibreMetricPfx) && name[:len(fibreMetricPfx)] == fibreMetricPfx {
			name = name[len(fibreMetricPfx):]
		}
		if op, ok := fibreHistogramNames[name]; ok && ((op == "go_schedule" && m.GetName() == "go.schedule.duration") || (op != "go_schedule" && m.GetName() == fibreMetricPfx+name)) {
			hist := m.GetHistogram()
			for _, dp := range hist.GetDataPoints() {
				if op != "go_schedule" {
					success, _ := fibreAttr(dp.GetAttributes(), "success")
					if success != "true" {
						continue
					}
				}
				h, valid := s.histogramDelta(fibreSeriesKey(m.GetName(), dp.GetAttributes()), dp, temporalityCumulative(hist.GetAggregationTemporality()), known, now)
				if valid {
					d.extra.Histograms[op] = addFibreHistogram(d.extra.Histograms[op], h)
				}
			}
		}
		if field, ok := fibreRuntimeGauges[m.GetName()]; ok {
			v, have := fibreRuntimeValue(name, m)
			if have {
				if name == "go.memory.limit" && v >= float64(math.MaxInt64) {
					delete(s.resources, field)
					d.extra.Gauges["go_memory_no_limit"] = 1
					s.resources["go_memory_no_limit"] = 1
				} else if v >= 0 || field == "go_gogc" {
					d.extra.Gauges[field] = v
					s.resources[field] = v
					if name == "go.memory.limit" {
						delete(s.resources, "go_memory_no_limit")
						d.extra.Gauges["go_memory_no_limit"] = 0
					}
				}
			}
		}
		switch m.GetName() {
		case "go.memory.allocated":
			if len(m.GetSum().GetDataPoints()) > 0 {
				d.extra.Counters["go_memory_allocated_bytes"] = float64(s.counter(m.GetName(), m, known, now))
			}
		case "go.memory.allocations":
			if len(m.GetSum().GetDataPoints()) > 0 {
				d.extra.Counters["go_memory_allocations"] = float64(s.counter(m.GetName(), m, known, now))
			}
		case "fibre.server.backend.get.bytes":
			if len(m.GetSum().GetDataPoints()) > 0 {
				d.extra.Counters["object_storage_bytes"] = float64(s.counter(name, m, known, now))
			}
		case "fibre.server.backend.get.in_flight":
			if len(m.GetSum().GetDataPoints()) > 0 {
				v := s.inFlight(m, int64(s.resources["object_storage_in_flight"]))
				d.extra.Gauges["object_storage_in_flight"] = float64(v)
				s.resources["object_storage_in_flight"] = float64(v)
			}
		}
	}
	if len(d.extra.Histograms) == 0 && len(d.extra.Gauges) == 0 && len(d.extra.Counters) == 0 {
		d.extra = nil
	}
	for key, prev := range s.histSeries {
		if now.Sub(prev.Seen) >= fibreKeep {
			delete(s.histSeries, key)
		}
	}
}

// Memory types are separate series; take the latest value of each before summing.
func fibreRuntimeValue(name string, m *metricsv1.Metric) (float64, bool) {
	points := m.GetGauge().GetDataPoints()
	if m.GetSum() != nil {
		points = m.GetSum().GetDataPoints()
	}
	latest := make(map[string]*metricsv1.NumberDataPoint)
	for _, dp := range points {
		key := ""
		if name == "go.memory.used" {
			key, _ = fibreAttr(dp.GetAttributes(), "go.memory.type")
			if key != "stack" && key != "other" {
				continue
			}
		}
		if prev := latest[key]; prev == nil || dp.GetTimeUnixNano() >= prev.GetTimeUnixNano() {
			latest[key] = dp
		}
	}
	var total float64
	for _, dp := range latest {
		v := fibreNumber(dp)
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return 0, false
		}
		total += v
	}
	return total, len(latest) > 0
}

// Rejects unbounded or unknown persisted fields; old hours without extras pass.
func validFibreExtra(e *fibreExtra) bool {
	if e == nil {
		return true
	}
	ops := map[string]bool{}
	for _, op := range fibreHistogramNames {
		ops[op] = true
	}
	for op, h := range e.Histograms {
		if !ops[op] || !validFibreHistogram(h) {
			return false
		}
	}
	gauges := map[string]bool{"go_memory_no_limit": true, "object_storage_in_flight": true}
	for _, field := range fibreRuntimeGauges {
		gauges[field] = true
	}
	for k, v := range e.Gauges {
		if !gauges[k] || math.IsNaN(v) || math.IsInf(v, 0) || (v < 0 && k != "go_gogc") {
			return false
		}
	}
	for k, v := range e.Counters {
		if (k != "go_memory_allocated_bytes" && k != "go_memory_allocations" && k != "object_storage_bytes") || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			return false
		}
	}
	return true
}
