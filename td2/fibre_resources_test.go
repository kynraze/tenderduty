package tenderduty

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	commonv1 "go.opentelemetry.io/proto/otlp/common/v1"
	metricsv1 "go.opentelemetry.io/proto/otlp/metrics/v1"
)

func latencyAttr(key, value string) *commonv1.KeyValue {
	return &commonv1.KeyValue{Key: key, Value: &commonv1.AnyValue{Value: &commonv1.AnyValue_StringValue{StringValue: value}}}
}

func latencyPoint(start uint64, bounds []float64, buckets []uint64, sum float64, attrs ...*commonv1.KeyValue) *metricsv1.HistogramDataPoint {
	var count uint64
	for _, n := range buckets {
		count += n
	}
	return &metricsv1.HistogramDataPoint{StartTimeUnixNano: start, Count: count, Sum: &sum, ExplicitBounds: bounds, BucketCounts: buckets, Attributes: attrs}
}

func latencyMetric(name string, points ...*metricsv1.HistogramDataPoint) *metricsv1.Metric {
	return &metricsv1.Metric{Name: name, Data: &metricsv1.Metric_Histogram{Histogram: &metricsv1.Histogram{AggregationTemporality: metricsv1.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE, DataPoints: points}}}
}

func runtimePoint(v int64, attrs ...*commonv1.KeyValue) *metricsv1.NumberDataPoint {
	return &metricsv1.NumberDataPoint{Value: &metricsv1.NumberDataPoint_AsInt{AsInt: v}, Attributes: attrs}
}

func runtimeGauge(name string, points ...*metricsv1.NumberDataPoint) *metricsv1.Metric {
	return &metricsv1.Metric{Name: name, Data: &metricsv1.Metric_Gauge{Gauge: &metricsv1.Gauge{DataPoints: points}}}
}

func runtimeCounter(name string, start uint64, v int64) *metricsv1.Metric {
	p := runtimePoint(v)
	p.StartTimeUnixNano = start
	return &metricsv1.Metric{Name: name, Data: &metricsv1.Metric_Sum{Sum: &metricsv1.Sum{IsMonotonic: true, AggregationTemporality: metricsv1.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE, DataPoints: []*metricsv1.NumberDataPoint{p}}}}
}

func TestFibreHistogramDeltasRestartsAndBounds(t *testing.T) {
	r := statusRig(t, false, nil)
	start := uint64(r.clock.now.Add(-time.Minute).UnixNano())
	bounds := []float64{.01, .1}
	attrs := []*commonv1.KeyValue{latencyAttr("success", "true"), latencyAttr("upload_size", "100")}
	feed := func(at uint64, b []float64, counts []uint64, sum float64) {
		r.st.record("Chain", "fibre-host-1", "v10", []*metricsv1.Metric{latencyMetric("fibre.server.upload_shard.duration", latencyPoint(at, b, counts, sum, attrs...))})
		r.advance(time.Minute)
	}
	feed(start, bounds, []uint64{2, 3, 0}, .2)
	feed(start, bounds, []uint64{4, 5, 1}, .5)
	h := r.st.histograms["Chain"]["upload"]
	if h.Count != 10 || math.Abs(h.Sum-.5) > 1e-12 || h.Buckets[0] != 4 || h.Buckets[1] != 5 {
		t.Fatalf("growth did not accumulate: %+v", h)
	}
	// Counts dropping without a changed start, then an explicit start change, both count fresh observations.
	feed(start, bounds, []uint64{1, 1, 0}, .05)
	feed(uint64(r.clock.now.UnixNano()), bounds, []uint64{1, 0, 0}, .005)
	h = r.st.histograms["Chain"]["upload"]
	if h.Count != 13 || math.Abs(h.Sum-.555) > 1e-12 {
		t.Fatalf("restart lowered totals: %+v", h)
	}
	feed(start+1, []float64{.02, .2}, []uint64{10, 10, 0}, 2)
	h = r.st.histograms["Chain"]["upload"]
	if h.Count != 0 || h.Sum != 0 || h.Bounds[0] != .02 {
		t.Fatalf("new schema did not establish baseline: %+v", h)
	}
	feed(start+1, []float64{.02, .2}, []uint64{12, 11, 0}, 2.1)
	if r.st.histograms["Chain"]["upload"].Count != 3 {
		t.Fatal("new schema growth was lost")
	}
}

func TestFibreHistogramSizeSeriesAndFailures(t *testing.T) {
	r := statusRig(t, false, nil)
	start := uint64(r.clock.now.Add(-time.Minute).UnixNano())
	b := []float64{.01, .1}
	metric := func(a, c uint64) *metricsv1.Metric {
		return latencyMetric("fibre.server.upload_shard.duration",
			latencyPoint(start, b, []uint64{a, 0, 0}, float64(a)*.005, latencyAttr("success", "true"), latencyAttr("upload_size", "100")),
			latencyPoint(start, b, []uint64{0, c, 0}, float64(c)*.05, latencyAttr("success", "true"), latencyAttr("upload_size", "200")),
			latencyPoint(start, b, []uint64{100, 0, 0}, .5, latencyAttr("success", "false")))
	}
	r.st.record("Chain", "fibre-host-1", "", []*metricsv1.Metric{metric(2, 3)})
	r.advance(time.Minute)
	r.st.record("Chain", "fibre-host-1", "", []*metricsv1.Metric{metric(4, 5)})
	h := r.st.histograms["Chain"]["upload"]
	if h.Count != 9 || h.Buckets[0] != 4 || h.Buckets[1] != 5 {
		t.Fatalf("size series not summed or failures counted: %+v", h)
	}
	p := r.st.series("Chain", "24h")
	if p[len(p)-1].Metrics["upload_p50_ms"] <= 0 {
		t.Fatal("per-export quantile absent")
	}
}

// a quiet minute takes in earlier minutes, so one slow signature doesn't make p95 jump
func TestFibreRangeLatencyAddsTheBucketsUp(t *testing.T) {
	r := statusRig(t, false, nil)
	start := uint64(r.clock.now.Add(-time.Minute).UnixNano())
	bounds := []float64{.0005, .001, .005}
	cumulative := []uint64{0, 0, 0, 0}
	feed := func(counts []uint64) {
		for i, n := range counts {
			cumulative[i] += n
		}
		var sum float64
		r.st.record("Chain", "fibre-host-1", "", []*metricsv1.Metric{latencyMetric("fibre.server.sign.duration",
			latencyPoint(start, bounds, append([]uint64(nil), cumulative...), sum, latencyAttr("success", "true")))})
		r.advance(time.Minute)
	}
	feed([]uint64{0, 0, 0, 0}) // the baseline
	for i := 0; i < 9; i++ {
		feed([]uint64{10, 0, 0, 0}) // fast minutes
	}
	feed([]uint64{0, 0, 1, 0}) // one slow signature alone in the last minute
	points, latency := r.st.seriesAndLatency("Chain", "1h")
	last := points[len(points)-2].Metrics
	if last["sign_p95_ms"] > .5 || last["sign_samples"] != 51 {
		t.Fatalf("the last minute's p95 is %v from %v signatures, want under half a millisecond from 51", last["sign_p95_ms"], last["sign_samples"])
	}
	if latency["sign_p95_ms"] > .5 {
		t.Fatalf("the range's p95 is %v ms, 90 of its 91 signatures took under half a millisecond", latency["sign_p95_ms"])
	}
}

func TestFibreBucketQuantiles(t *testing.T) {
	h := fibreHistogram{Bounds: []float64{1, 2, 4}, Buckets: []uint64{2, 4, 4, 0}, Count: 10, Sum: 20}
	for _, tc := range []struct {
		q, want float64
	}{{.5, 1.75}, {.95, 3.75}} {
		v := h.quantile(tc.q)
		if v == nil || math.Abs(*v-tc.want) > 1e-12 {
			t.Fatalf("q%v = %v, want %v", tc.q, v, tc.want)
		}
	}
	if (fibreHistogram{}).quantile(.95) != nil {
		t.Fatal("empty quantile must be null")
	}
	h.Buckets, h.Count = []uint64{0, 0, 0, 10}, 10
	if v := h.quantile(.95); v == nil || *v != 4 {
		t.Fatal("infinite bucket must return last finite bound")
	}
}

func TestFibreRuntimeMetricsAndOmissions(t *testing.T) {
	r := statusRig(t, false, nil)
	for _, suffix := range []string{"go_memory_used_bytes", "go_memory_limit_bytes", "go_memory_allocated_bytes_total", "go_schedule_seconds_count"} {
		if _, ok := gatheredFibre(t, r.c)["tenderduty_fibre_"+suffix]; ok {
			t.Fatalf("unreported %s exported", suffix)
		}
	}
	start := uint64(r.clock.now.Add(-time.Minute).UnixNano())
	metrics := []*metricsv1.Metric{
		runtimeGauge("go.memory.used", runtimePoint(100, latencyAttr("go.memory.type", "stack")), runtimePoint(400, latencyAttr("go.memory.type", "other"))),
		runtimeGauge("go.memory.limit", runtimePoint(math.MaxInt64)),
		runtimeGauge("go.memory.gc.goal", runtimePoint(800)),
		runtimeGauge("go.goroutine.count", runtimePoint(12)),
		runtimeGauge("go.processor.limit", runtimePoint(4)),
		runtimeGauge("go.config.gogc", runtimePoint(100)),
		runtimeCounter("go.memory.allocated", start, 1000),
		runtimeCounter("go.memory.allocations", start, 10),
		latencyMetric("go.schedule.duration", latencyPoint(start, []float64{.001, .01}, []uint64{1, 9, 0}, .02)),
	}
	r.st.record("Chain", "fibre-host-1", "", metrics)
	r.advance(time.Minute)
	r.st.record("Chain", "fibre-host-1", "", []*metricsv1.Metric{runtimeCounter("go.memory.allocated", start, 1500)})
	r.advance(time.Minute)
	r.st.record("Chain", "fibre-host-1", "", []*metricsv1.Metric{runtimeCounter("go.memory.allocated", start+1, 200)})
	values := gatheredFibre(t, r.c)
	if values["tenderduty_fibre_go_memory_used_bytes"] != 500 || values["tenderduty_fibre_go_memory_allocated_bytes_total"] != 1700 || values["tenderduty_fibre_go_memory_allocations_total"] != 10 {
		t.Fatalf("runtime metrics incorrect: %+v", values)
	}
	if _, ok := values["tenderduty_fibre_go_memory_limit_bytes"]; ok {
		t.Fatal("unlimited memory was emitted")
	}
	if r.st.snapshot("Chain").Resources["go_memory_no_limit"] != 1 {
		t.Fatal("unlimited marker absent")
	}
	r.st.record("Chain", "fibre-host-1", "", []*metricsv1.Metric{runtimeGauge("go.memory.limit", runtimePoint(2000))})
	if gatheredFibre(t, r.c)["tenderduty_fibre_go_memory_limit_bytes"] != 2000 {
		t.Fatal("finite limit was not restored")
	}
}

func TestFibreNewPrometheusFamilies(t *testing.T) {
	r := statusRig(t, false, nil)
	start := uint64(r.clock.now.Add(-time.Minute).UnixNano())
	metrics := []*metricsv1.Metric{
		runtimeCounter("fibre.server.prune.entries", start, 5),
		runtimeCounter("fibre.server.backend.get.bytes", start, 100),
		runtimeCounter("fibre.server.backend.get.in_flight", start, 3),
		runtimeCounter("go.memory.allocated", start, 200),
		runtimeCounter("go.memory.allocations", start, 4),
	}
	for name := range fibreRuntimeGauges {
		p := runtimePoint(100)
		if name == "go.memory.used" {
			p.Attributes = []*commonv1.KeyValue{latencyAttr("go.memory.type", "other")}
		}
		metrics = append(metrics, runtimeGauge(name, p))
	}
	for name, op := range fibreHistogramNames {
		attrs := []*commonv1.KeyValue{latencyAttr("success", "true")}
		if op != "go_schedule" {
			name = fibreMetricPfx + name
		}
		metrics = append(metrics, latencyMetric(name, latencyPoint(start, []float64{.001, .01, .1}, []uint64{1, 2, 3, 0}, .15, attrs...)))
	}
	r.st.record("Chain", "fibre-host-1", "", metrics)
	values := gatheredFibre(t, r.c)
	if values["tenderduty_fibre_prune_entries_total"] != 5 || values["tenderduty_fibre_object_storage_bytes_total"] != 100 || values["tenderduty_fibre_object_storage_in_flight"] != 3 {
		t.Fatalf("dropped metrics still missing: %+v", values)
	}
	registry := prometheus.NewPedanticRegistry()
	registry.MustRegister(&fibreCollector{config: r.c})
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]string)
	for _, family := range families {
		seen[family.GetName()] = family.GetType().String()
		for _, metric := range family.Metric {
			if (metric.Histogram != nil || family.GetName() == "tenderduty_fibre_go_goroutines") && len(metric.Label) != 3 {
				t.Fatalf("unexpected labels on %s", family.GetName())
			}
			if metric.Histogram != nil {
				for _, label := range metric.Label {
					if label.GetName() != "name" && label.GetName() != "chain_id" && label.GetName() != "moniker" {
						t.Fatalf("unexpected histogram label: %s", label.GetName())
					}
				}
			}
			if metric.Histogram != nil && (metric.Histogram.GetSampleCount() != 6 || len(metric.Histogram.Bucket) != 3 || metric.Histogram.Bucket[1].GetCumulativeCount() != 3) {
				t.Fatalf("incorrect histogram: %+v", metric.Histogram)
			}
		}
	}
	for suffix, kind := range map[string]string{
		"go_memory_used_bytes": "GAUGE", "go_memory_limit_bytes": "GAUGE", "go_gc_goal_bytes": "GAUGE", "go_goroutines": "GAUGE", "go_processor_limit": "GAUGE", "go_gogc": "GAUGE",
		"go_memory_allocated_bytes_total": "COUNTER", "go_memory_allocations_total": "COUNTER", "prune_entries_total": "COUNTER", "object_storage_bytes_total": "COUNTER", "object_storage_in_flight": "GAUGE",
	} {
		if seen["tenderduty_fibre_"+suffix] != kind {
			t.Fatalf("%s type = %s, want %s", suffix, seen["tenderduty_fibre_"+suffix], kind)
		}
	}
	for _, op := range fibreHistogramNames {
		if seen["tenderduty_fibre_"+op+"_seconds"] != "HISTOGRAM" {
			t.Fatalf("missing histogram %s", op)
		}
	}
}

func TestFibreResourceRollupsRoundTrip(t *testing.T) {
	r := statusRig(t, false, nil)
	start := uint64(r.clock.now.Add(-time.Minute).UnixNano())
	r.st.record("Chain", "fibre-host-1", "", []*metricsv1.Metric{
		runtimeGauge("go.memory.used", runtimePoint(300, latencyAttr("go.memory.type", "other"))),
		runtimeGauge("go.goroutine.count", runtimePoint(10)),
		runtimeCounter("go.memory.allocated", start, 100),
		latencyMetric("go.schedule.duration", latencyPoint(start, []float64{.01, .1}, []uint64{2, 8, 0}, .2)),
	})
	r.advance(time.Minute)
	r.st.record("Chain", "fibre-host-1", "", []*metricsv1.Metric{
		runtimeGauge("go.memory.used", runtimePoint(200, latencyAttr("go.memory.type", "other"))),
		runtimeGauge("go.goroutine.count", runtimePoint(5)),
	})
	raw := r.st.savedHours()
	store := newFibreStore()
	store.now = r.clock.Now
	store.restoreHours(raw)
	if string(store.savedHours()) != string(raw) {
		t.Fatal("resource rollup round trip changed fields")
	}
	for _, span := range []string{"7d", "30d"} {
		points := store.series("Chain", span)
		last := points[len(points)-1].Metrics
		if last["go_memory_used_bytes"] != 200 || last["go_memory_max_bytes"] != 300 || last["go_goroutines_max"] != 10 || last["go_memory_allocated_bytes"] != 100 || last["go_schedule_p95_ms"] <= 0 {
			t.Fatalf("%s rollup values lost: %+v", span, last)
		}
	}
	// Extra fields are optional in old saved hours.
	for _, hours := range r.st.hours {
		for _, hour := range hours {
			hour.Extra = nil
		}
	}
	old, _ := json.Marshal(r.st.hours)
	store = newFibreStore()
	store.now = r.clock.Now
	store.restoreHours(old)
	if len(store.hours["Chain"]) == 0 {
		t.Fatal("old hourly state did not load")
	}
}

func TestFibreHistogramDeltaTemporalityAndCopying(t *testing.T) {
	r := statusRig(t, false, nil)
	metric := latencyMetric("fibre.server.sign.duration", latencyPoint(0, []float64{.01, .1}, []uint64{2, 3, 0}, .1, latencyAttr("success", "true")))
	metric.GetHistogram().AggregationTemporality = metricsv1.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA
	r.st.record("Chain", "fibre-host-1", "", []*metricsv1.Metric{metric})
	r.advance(time.Minute)
	r.st.record("Chain", "fibre-host-1", "", []*metricsv1.Metric{metric})
	if r.st.histograms["Chain"]["sign"].Count != 10 {
		t.Fatal("delta histogram exports were not added")
	}
	first := r.st.senders["fibre-host-1"].window[0].extra
	second := r.st.senders["fibre-host-1"].window[1].extra
	merged := mergeFibreExtra(first, second)
	if merged.Histograms["sign"].Count != 10 || first.Histograms["sign"].Count != 5 || first.Histograms["sign"].Buckets[0] != 2 {
		t.Fatal("merging mutated an earlier interval")
	}
	values := gatheredFibre(t, r.c)
	if values["tenderduty_fibre_sign_seconds_count"] != 10 || math.Abs(values["tenderduty_fibre_sign_seconds_sum"]-.2) > 1e-12 {
		t.Fatalf("Prometheus histogram accumulation incorrect: %+v", values)
	}
}

func TestFibreRuntimeOnlyExactNamesAndReportedPoints(t *testing.T) {
	r := statusRig(t, false, nil)
	r.st.record("Chain", "fibre-host-1", "", []*metricsv1.Metric{
		runtimeGauge("go.memory.used.unrelated", runtimePoint(999)),
		runtimeGauge("fibre.server.go.memory.used", runtimePoint(999)),
		runtimeGauge("go.memory.limit"),
		{Name: "go.memory.allocated"},
		latencyMetric("fibre.server.go.schedule.duration", latencyPoint(0, []float64{1}, []uint64{1, 0}, .5)),
		latencyMetric("upload_shard.duration", latencyPoint(0, []float64{1}, []uint64{1, 0}, .5, latencyAttr("success", "true"))),
	})
	if len(r.st.snapshot("Chain").Resources) != 0 || len(r.st.histograms["Chain"]) != 0 {
		t.Fatal("unrelated or empty runtime instruments became observations")
	}
	for _, suffix := range []string{"go_memory_used_bytes", "go_memory_limit_bytes", "go_memory_allocated_bytes_total", "go_schedule_seconds_count"} {
		if _, ok := gatheredFibre(t, r.c)["tenderduty_fibre_"+suffix]; ok {
			t.Fatalf("unreported %s emitted", suffix)
		}
	}
}

func TestFibreRuntimeGaugeLastValueAndGCDisabled(t *testing.T) {
	old := runtimePoint(100, latencyAttr("go.memory.type", "stack"))
	old.TimeUnixNano = 1
	latest := runtimePoint(200, latencyAttr("go.memory.type", "stack"))
	latest.TimeUnixNano = 2
	other := runtimePoint(300, latencyAttr("go.memory.type", "other"))
	v, ok := fibreRuntimeValue("go.memory.used", runtimeGauge("go.memory.used", latest, old, other))
	if !ok || v != 500 {
		t.Fatalf("memory types did not use latest values: %v, %v", v, ok)
	}
	r := statusRig(t, false, nil)
	r.st.record("Chain", "fibre-host-1", "", []*metricsv1.Metric{runtimeGauge("go.config.gogc", runtimePoint(-1))})
	if v := gatheredFibre(t, r.c)["tenderduty_fibre_go_gogc"]; v != -1 {
		t.Fatalf("disabled GC lost: %v", v)
	}
}

func TestFibreHistogramOldBaselineAndHourlyQuantiles(t *testing.T) {
	r := statusRig(t, false, nil)
	start := uint64(r.clock.now.Add(-48 * time.Hour).UnixNano())
	feed := func(counts []uint64, sum float64) {
		r.st.record("Chain", "fibre-host-1", "", []*metricsv1.Metric{latencyMetric("fibre.server.store.put.duration", latencyPoint(start, []float64{1, 2, 4}, counts, sum, latencyAttr("success", "true")))})
	}
	feed([]uint64{10, 20, 20, 0}, 100)
	if h := r.st.histograms["Chain"]["store_put"]; h.Count != 0 || h.Sum != 0 {
		t.Fatalf("pre-receiver observations imported: %+v", h)
	}
	r.advance(time.Minute)
	feed([]uint64{12, 24, 24, 0}, 120)
	for _, span := range []string{"1h", "24h", "7d", "30d"} {
		points := r.st.series("Chain", span)
		last := points[len(points)-1].Metrics
		if math.Abs(last["store_put_p50_ms"]-1750) > 1e-12 || math.Abs(last["store_put_p95_ms"]-3750) > 1e-12 {
			t.Fatalf("%s percentiles incorrect: %+v", span, last)
		}
	}
}

func TestFibreLatencyWindowWidensOnlyWhenQuiet(t *testing.T) {
	hist := func(n uint64) *fibreExtra {
		return &fibreExtra{Histograms: map[string]fibreHistogram{"upload": {Bounds: []float64{0.01}, Buckets: []uint64{n, 0}, Count: n}}}
	}
	extras := []*fibreExtra{hist(5), hist(5), hist(5), nil, hist(5), hist(5), hist(5), hist(100)}
	if got := fibreLatencyWindow(extras, 7, "upload").Count; got != 100 {
		t.Fatalf("busy interval took in others: %d", got)
	}
	// 5 back at most, the empty one is skipped
	if got := fibreLatencyWindow(extras, 6, "upload").Count; got != 25 {
		t.Fatalf("quiet interval got %d, want 25", got)
	}
	extras[5].Histograms["upload"] = fibreHistogram{Bounds: []float64{0.02}, Buckets: []uint64{5, 0}, Count: 5}
	if got := fibreLatencyWindow(extras, 6, "upload").Count; got != 5 {
		t.Fatalf("other bounds were added: %d", got)
	}
}
