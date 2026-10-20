package tenderduty

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	dash "github.com/kynraze/tenderduty/v2/td2/dashboard"
	"github.com/prometheus/client_golang/prometheus"
	commonv1 "go.opentelemetry.io/proto/otlp/common/v1"
	metricsv1 "go.opentelemetry.io/proto/otlp/metrics/v1"
)

func TestBridgeHistoryMergeAndTrim(t *testing.T) {
	c := &Config{}
	st := c.bridgeData()
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	st.now = func() time.Time { return now }
	st.record("chain", now, map[string]float64{"served_bytes": 3, "head_lag": 9, "head_lag_max": 9, "full_peers_min": 4, "serve_seconds": 1, "serve_requests": 2, "put_seconds": 2, "puts": 4, "da_lag_seconds": 3, "da_blocks": 6}, "v1")
	st.record("chain", now.Add(10*time.Second), map[string]float64{"served_bytes": 4, "head_lag": 1, "head_lag_max": 1, "full_peers_min": 2}, "v2")
	for _, span := range bridgeRanges {
		series := st.series("chain", span)
		p := series[len(series)-1]
		for k, want := range map[string]float64{"served_bytes": 7, "head_lag": 1, "head_lag_max": 9, "full_peers_min": 2, "serve_ms": 500, "put_ms": 500, "da_lag_ms": 500} {
			if p.Metrics[k] != want {
				t.Errorf("%s %s = %v, want %v", span, k, p.Metrics[k], want)
			}
		}
		if p.Version != "v2" || series[0].Metrics != nil {
			t.Fatal("version or missing intervals", p)
		}
		if span == "30d" && p.Time%(6*3600) != 0 {
			t.Fatal("30d alignment")
		}
	}
	// Expire every tier on both read and write.
	now = now.Add(31 * 24 * time.Hour)
	for _, span := range bridgeRanges {
		st.series("chain", span)
		if len(st.tiers["chain"][span]) != 0 {
			t.Fatal("read did not trim", span)
		}
	}
	st.record("chain", now, map[string]float64{"served_bytes": 1}, "")
	now = now.Add(31 * 24 * time.Hour)
	st.record("chain", now, map[string]float64{"served_bytes": 2}, "")
	for _, span := range bridgeRanges {
		if len(st.tiers["chain"][span]) != 1 {
			t.Fatal("write did not trim", span)
		}
	}
}

func TestBridgeHistoryRestore(t *testing.T) {
	cc, _ := newBridgeFixture(t)
	now := time.Now().UTC().Truncate(6 * time.Hour)
	td.bridgeData().now = func() time.Time { return now }
	values := make(map[string]float64)
	for key := range bridgeFields {
		values[key] = 2
	}
	td.bridgeData().record(cc.name, now, values, "v0.34.3")
	encoded, err := json.Marshal(savedState{BridgeHistory: td.savedBridgeHistory()})
	if err != nil {
		t.Fatal(err)
	}
	var state savedState
	if json.Unmarshal(encoded, &state) != nil {
		t.Fatal("saved state decode")
	}
	saved := state.BridgeHistory
	c := &Config{Chains: map[string]*ChainConfig{cc.name: cc}}
	c.bridgeData().now = func() time.Time { return now }
	c.restoreBridgeHistory(saved)
	for _, span := range bridgeRanges {
		if !reflect.DeepEqual(td.bridgeData().series(cc.name, span), c.bridgeData().series(cc.name, span)) {
			t.Fatal("round trip", span)
		}
	}
	invalid := []bridgeBucket{
		{Time: now.Unix(), Values: map[string]float64{"unknown": 1}},
		{Time: now.Unix(), Values: map[string]float64{"served_bytes": -1}},
		{Time: now.Unix() + 1, Values: values},
		{Time: now.Add(-2 * time.Hour).Unix(), Values: values},
		{Time: now.Unix(), Version: strings.Repeat("v", 65)},
	}
	for _, b := range invalid {
		data, _ := json.Marshal(map[string]interface{}{cc.name: map[string]interface{}{"1h": map[int64]bridgeBucket{b.Time: b}}})
		fresh := &Config{Chains: c.Chains}
		fresh.bridgeData().now = func() time.Time { return now }
		fresh.restoreBridgeHistory(data)
		if len(fresh.bridgeData().tiers[cc.name]["1h"]) != 0 {
			t.Fatal("invalid bucket accepted", b)
		}
	}
	// Malformed values invalidate only their bucket.
	data := json.RawMessage(fmt.Sprintf(`{%q:{"1h":{%q:{"time":%d,"values":{"served_bytes":NaN}},%q:{"time":%d,"values":{"served_bytes":2}}},"bad":{}}}`, cc.name, fmt.Sprint(now.Unix()), now.Unix(), fmt.Sprint(now.Add(-time.Minute).Unix()), now.Add(-time.Minute).Unix()))
	// JSON cannot encode NaN: reject non-finite numbers encoded as overflow instead.
	data = json.RawMessage(strings.ReplaceAll(string(data), "NaN", "1e999"))
	fresh := &Config{Chains: c.Chains}
	fresh.bridgeData().now = func() time.Time { return now }
	fresh.restoreBridgeHistory(data)
	if len(fresh.bridgeData().tiers[cc.name]["1h"]) != 1 {
		t.Fatal("valid sibling lost")
	}
	if validBridgeValue(math.NaN()) || validBridgeValue(math.Inf(1)) {
		t.Fatal("non-finite validation")
	}
	cc.Bridge.URL = ""
	var filtered map[string]interface{}
	_ = json.Unmarshal(td.savedBridgeHistory(), &filtered)
	if len(filtered) != 0 {
		t.Fatal("saved unwatched chain")
	}
}

func TestBridgeHistoryAPI(t *testing.T) {
	cc, _ := newBridgeFixture(t)
	handler := dash.BridgeHandler(td.bridgeHistory)
	for span, count := range map[string]int{"1h": 60, "24h": 144, "7d": 168, "30d": 120} {
		w := httptest.NewRecorder()
		handler(w, httptest.NewRequest("GET", "/bridge?range="+span+"&chain="+cc.name, nil))
		var result dash.BridgeHistory
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || len(result.Chains) != 1 || len(result.Chains[0].Series) != count || result.Range != span || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("%s: %d %s", span, w.Code, w.Body.String())
		}
	}
	for path, code := range map[string]int{"/bridge?range=bad": 400, "/bridge?chain=unknown": 404} {
		w := httptest.NewRecorder()
		handler(w, httptest.NewRequest("GET", path, nil))
		if w.Code != code {
			t.Fatal(path, w.Code)
		}
	}
	w := httptest.NewRecorder()
	handler(w, httptest.NewRequest("POST", "/bridge", nil))
	if w.Code != 405 || w.Header().Get("Allow") != "GET" {
		t.Fatal("method check")
	}
	cc.Bridge.URL = ""
	w = httptest.NewRecorder()
	handler(w, httptest.NewRequest("GET", "/bridge?chain="+cc.name, nil))
	if w.Code != 404 {
		t.Fatal("unwatched chain")
	}
}

func bridgeDurationMetric(name string, start uint64, sum float64, count uint64, attrs ...*commonv1.KeyValue) *metricsv1.Metric {
	return &metricsv1.Metric{Name: name, Data: &metricsv1.Metric_Histogram{Histogram: &metricsv1.Histogram{DataPoints: []*metricsv1.HistogramDataPoint{{StartTimeUnixNano: start, Sum: &sum, Count: count, Attributes: attrs}}}}}
}

func TestBridgeHistogramPairs(t *testing.T) {
	s := bridgeState{series: make(map[string]bridgeSeries)}
	for i, sample := range []struct {
		start              uint64
		sum                float64
		count              uint64
		wantSum, wantCount float64
		observed           bool
	}{{1, 10, 10, 0, 0, false}, {1, 12, 15, 2, 5, true}, {1, 1, 16, 1, 16, true}, {1, 2, 1, 2, 1, true}, {2, 3, 4, 3, 4, true}, {2, 3, 4, 0, 0, true}} {
		p := bridgeDurationMetric("duration", sample.start, sample.sum, sample.count).GetHistogram().DataPoints[0]
		sum, count, ok := s.histogramDelta("series", p)
		if sum != sample.wantSum || count != sample.wantCount || ok != sample.observed {
			t.Fatalf("sample %d: %v %v %v", i, sum, count, ok)
		}
	}
}

func TestBridgeHistoryIngest(t *testing.T) {
	cc, _ := newBridgeFixture(t)
	now := time.Now()
	td.fibreData().now = func() time.Time { return now }
	td.bridgeData().now = func() time.Time { return now }
	cc.bridge.Head, cc.blockTime = 700, 6*time.Second
	statuses := []string{"success", "not_found", "internal_err", "rate_limited", "resource_exhausted", "read_req_err", "bad_request", "send_status_err", "send_resp_err", "unknown"}
	feed := func(value int64, started int64) {
		metrics := []*metricsv1.Metric{
			bridgeDurationMetric("shrex_server_request_duration", 1, float64(value)/10, uint64(value), latencyAttr("status", "success")),
			bridgeDurationMetric("shrex_server_request_duration", 1, float64(value), uint64(value), latencyAttr("status", "internal_err")),
			bridgeDurationMetric("eds_store_put_time_histogram", 1, float64(value)/5, uint64(value), &commonv1.KeyValue{Key: "failed", Value: &commonv1.AnyValue{Value: &commonv1.AnyValue_BoolValue{BoolValue: false}}}),
			bridgeDurationMetric("eds_store_put_time_histogram", 1, float64(value), uint64(value), &commonv1.KeyValue{Key: "failed", Value: &commonv1.AnyValue{Value: &commonv1.AnyValue_BoolValue{BoolValue: true}}}),
			bridgeDurationMetric("core_to_da_lag_seconds", 1, float64(value)/2, uint64(value)),
			bridgeCounter("eds_store_bytes_reclaimed_total", 1, value, ""),
			runtimeGauge("node_start_ts", runtimePoint(started)),
			runtimeGauge("prnr_last_pruned", runtimePoint(100)),
			runtimeGauge("go.goroutine.count", runtimePoint(20)),
			runtimeGauge("go.memory.used", runtimePoint(1024, latencyAttr("go.memory.type", "heap")), runtimePoint(512, latencyAttr("go.memory.type", "stack"))),
		}
		for _, status := range statuses {
			metrics = append(metrics, bridgeCounter("shrex_server_total_responses", 1, value, status))
		}
		td.ingestFibreMetrics(bridgeExport("mocha-5", "Bridge", metrics...))
	}
	feed(10, 100)
	if _, ok := cc.bridge.totals["serve_seconds"]; ok {
		t.Fatal("histogram baseline counted")
	}
	feed(20, 100)
	feed(20, 101)
	p := td.bridgeData().series(cc.name, "1h")[59]
	for k, want := range map[string]float64{"responses_success": 10, "responses_not_found": 10, "responses_internal_err": 10, "responses_rate_limited": 20, "responses_peer_err": 40, "responses_other": 10, "serve_seconds": 1, "serve_requests": 10, "put_seconds": 2, "puts": 10, "put_failures": 10, "da_lag_seconds": 5, "da_blocks": 10, "reclaimed_bytes": 10, "retained_hours": 1, "restarts": 1, "memory_bytes": 1536, "goroutines": 20} {
		if p.Metrics[k] != want {
			t.Errorf("%s: %v want %v", k, p.Metrics[k], want)
		}
	}
	cc.blockTime = 0
	if _, ok := cc.bridgeRetainedHours(); ok {
		t.Fatal("retained hours without block time")
	}
	// A response delta of zero still records all six groups.
	now = now.Add(time.Minute)
	td.ingestFibreMetrics(bridgeExport("mocha-5", "Bridge", bridgeCounter("shrex_server_total_responses", 1, 20, "success")))
	p = td.bridgeData().series(cc.name, "1h")[59]
	for _, status := range bridgeResponseStatuses {
		if v, ok := p.Metrics["responses_"+status]; !ok || v != 0 {
			t.Fatal("missing observed zero", status)
		}
	}
}

func TestBridgePollHistory(t *testing.T) {
	cc, f := newBridgeFixture(t)
	now := time.Now()
	td.bridgeData().now = func() time.Time { return now }
	cc.lastBlockNum, f.sampled, f.network = 110, 98, 100
	cc.refreshBridge(context.Background(), now)
	p := td.bridgeData().series(cc.name, "1h")[59]
	if p.Metrics["head_lag"] != 10 || p.Metrics["sample_lag"] != 2 {
		t.Fatal(p)
	}
	now = now.Add(time.Minute)
	f.code = 500
	cc.refreshBridge(context.Background(), now)
	if td.bridgeData().series(cc.name, "1h")[59].Metrics != nil {
		t.Fatal("failed poll recorded")
	}
}

func TestBridgeNewPrometheusFamilies(t *testing.T) {
	cc, _ := newBridgeFixture(t)
	cc.bridge.MetricsAt = time.Now()
	cc.bridge.totals = map[string]float64{"served_bytes": 42, "responses_success": 3}
	cc.bridge.resources = map[string]float64{"memory_bytes": 1024, "goroutines": 10}
	cc.bridge.Head, cc.bridge.LastPruned, cc.bridge.PrunerKnown, cc.blockTime = 700, 100, true, 6*time.Second
	r := prometheus.NewRegistry()
	r.MustRegister(&bridgeCollector{config: td})
	families, err := r.Gather()
	if err != nil {
		t.Fatal(err)
	}
	found := make(map[string]bool)
	for _, family := range families {
		found[family.GetName()] = true
		if family.GetName() == "tenderduty_bridge_served_bytes_total" && family.Metric[0].Counter.GetValue() != 42 {
			t.Fatal("counter value")
		}
	}
	for suffix := range bridgeCounters {
		if !found["tenderduty_bridge_"+suffix] {
			t.Fatal("missing counter", suffix)
		}
	}
	for _, suffix := range []string{"responses_total", "sampling_lag_blocks", "retained_hours", "memory_bytes", "goroutines"} {
		if !found["tenderduty_bridge_"+suffix] {
			t.Fatal("missing family", suffix)
		}
	}
	cc.bridge.MetricsAt = time.Now().Add(-3 * time.Minute)
	families, err = r.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if strings.HasSuffix(family.GetName(), "_total") {
			t.Fatal("stale counter", family.GetName())
		}
	}
}

func TestBridgeRawExport(t *testing.T) {
	cc, _ := newBridgeFixture(t)
	td.Prom = true
	data := bridgeExport("mocha-5", "Bridge", bridgeCounter("shrex_payload_served_bytes", 1, 100, ""))
	td.ingestFibreMetrics(data)
	if len(td.fibreData().raw[cc.name]) != 0 {
		t.Fatal("raw while off")
	}
	td.FibreMetrics.ExportRaw = true
	td.ingestFibreMetrics(data)
	if len(td.fibreData().raw[cc.name]) != 1 {
		t.Fatal("bridge raw missing")
	}
	td.FibreMetrics.ExportRaw = false
	td.ingestFibreMetrics(data)
	if len(td.fibreData().raw[cc.name]) != 0 {
		t.Fatal("raw not removed")
	}
}
