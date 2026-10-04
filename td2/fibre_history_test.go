package tenderduty

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	dash "github.com/kynraze/tenderduty/v2/td2/dashboard"
	"github.com/prometheus/client_golang/prometheus"
	metricsv1 "go.opentelemetry.io/proto/otlp/metrics/v1"
)

func TestFibreHoursAndStorage(t *testing.T) {
	r := newFibreRig(t)
	r.cc.Alerts.FibreInstance = "fibre-host-1"
	r.clock.now = r.clock.now.Add(59*time.Minute + 59*time.Second)
	f := r.feed(time.Minute)
	f.rejected, f.occupancy, f.budget = 3, i64(100), i64(500)
	f.send()
	first := r.clock.now.Truncate(time.Hour).Unix()
	r.advance(time.Second)
	f.rejected, f.occupancy = 5, i64(80)
	f.send()
	second := r.clock.now.Truncate(time.Hour).Unix()
	if len(r.st.hours["Chain"]) != 2 || r.st.hours["Chain"][first].Counts["uploads_rejected"] != 3 || r.st.hours["Chain"][second].Counts["uploads_rejected"] != 2 {
		t.Fatalf("incorrect hour boundaries: %+v", r.st.hours["Chain"])
	}
	f.occupancy = i64(150)
	r.advance(time.Minute)
	r.c.ingestFibreMetrics(f.statusData(statusTotals{uploadsNow: 7}))
	r.advance(time.Minute)
	r.c.ingestFibreMetrics(f.statusData(statusTotals{uploadsNow: 3}))
	h := r.st.hours["Chain"][second]
	if h.MaxUploads != 7 || h.Exports != 3 || *h.Occupancy != 150 || h.MaxOccupancy != 150 || *h.Budget != 500 {
		t.Fatalf("storage or coverage missing: %+v", h)
	}
	r.advance(fibreRollupKeep + time.Hour)
	r.st.savedHours()
	if len(r.st.hours) != 0 {
		t.Fatal("expired hours retained")
	}
}

func TestFibreStateRollupsRoundTrip(t *testing.T) {
	r := newFibreRig(t)
	r.st.recordReach("Chain", fibreState{Reachable: true, KeyKnown: true, IdentityOK: true})
	path := filepath.Join(t.TempDir(), "state.json")
	if err := saveState(path); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved savedState
	if err := json.Unmarshal(body, &saved); err != nil || len(saved.FibreRollups) == 0 {
		t.Fatalf("rollups not saved: %s, %v", body, err)
	}
	store := newFibreStore()
	store.now = r.clock.Now
	store.restoreHours(saved.FibreRollups)
	if len(store.totals) != 0 || string(store.savedHours()) != string(r.st.savedHours()) {
		t.Fatal("round trip changed rollups")
	}
	var old savedState
	if err := json.Unmarshal([]byte(`{"blocks":{"Chain":[3,0]},"heights":{"Chain":123}}`), &old); err != nil {
		t.Fatal(err)
	}
	fresh := newFibreStore()
	fresh.restoreHours(old.FibreRollups)
	if len(fresh.hours) != 0 || old.Heights["Chain"] != 123 {
		t.Fatal("old state did not load")
	}
	// Invalid entries do not spoil valid chains or the rest of the saved state.
	mixed := fmt.Sprintf(`{"bad":"garbage","Chain":{"bad-hour":{},%q:{"time":%d,"checks":2,"reachable":1,"verified":1},"0":{"time":0,"counts":{"signatures":-1}}}}`, fmt.Sprint(r.clock.now.Unix()), r.clock.now.Unix())
	fresh.now = r.clock.Now
	fresh.restoreHours(json.RawMessage(mixed))
	if len(fresh.hours["Chain"]) != 1 || fresh.hours["bad"] != nil {
		t.Fatalf("garbage restored: %+v", fresh.hours)
	}
	var garbage savedState
	if err := json.Unmarshal([]byte(`{"fibre_rollups":"garbage","heights":{"Chain":123}}`), &garbage); err != nil {
		t.Fatal(err)
	}
	fresh.restoreHours(garbage.FibreRollups)
	if garbage.Heights["Chain"] != 123 {
		t.Fatal("garbage rollups affected other state")
	}
}

func TestFibreCompactionKeepsLaterGauges(t *testing.T) {
	now := time.Now()
	window := []fibreDelta{
		{at: now, uploadOK: 2, occupancy: 200, budget: 500, haveOccupancy: true, haveBudget: true, known: 1},
		{at: now.Add(time.Minute), uploadOK: 3, occupancy: 100, budget: 0, haveOccupancy: true, haveBudget: true, known: 2},
		{at: now.Add(2 * time.Minute)},
		{at: now.Add(3 * time.Minute)},
	}
	merged := compactDeltas(window)
	if len(merged) != 3 || merged[0].uploadOK != 5 || merged[0].occupancy != 100 || merged[0].budget != 0 || !merged[0].haveBudget || merged[0].known != 3 || !merged[0].at.Equal(window[1].at) {
		t.Fatalf("compacted values lost: %+v", merged)
	}
}

func TestFibreReachSamples(t *testing.T) {
	r := newFibreRig(t)
	r.st.recordReach("Chain", fibreState{Reachable: true, KeyKnown: true, IdentityOK: true})
	r.st.recordReach("Chain", fibreState{Reachable: false, KeyKnown: true, IdentityOK: true})
	h := r.st.hours["Chain"][r.clock.now.Unix()]
	if h.Checks != 2 || h.Reachable != 1 || h.Verified != 1 {
		t.Fatalf("checks wrong: %+v", h)
	}
	for _, span := range []string{"1h", "24h", "7d", "30d"} {
		series := r.st.series("Chain", span)
		last := series[len(series)-1]
		if last.Reachable == nil || *last.Reachable != .5 || last.Metrics != nil {
			t.Fatalf("%s reach-only point: %+v", span, last)
		}
	}
	r.advance(25 * time.Hour)
	r.st.recordReach("Chain", fibreState{})
	if len(r.st.reach["Chain"]) != 1 {
		t.Fatal("recent check history did not trim")
	}
}

func TestFibreReachLoneGapTakesTheEarlierCheck(t *testing.T) {
	r := newFibreRig(t)
	check := func(reachable bool, after time.Duration) {
		r.st.recordReach("Chain", fibreState{Reachable: reachable})
		r.advance(after)
	}
	check(true, 61*time.Second)   // minute 0
	check(false, 119*time.Second) // minute 1, then minute 2 is skipped
	check(true, 4*time.Minute)    // minute 3, then minutes 4 to 6 are skipped
	check(true, time.Second)      // minute 7
	series := r.st.series("Chain", "1h")
	reach := func(back int) *float64 { return series[len(series)-1-back].Reachable }
	if v := reach(5); v == nil || *v != 0 {
		t.Fatalf("the lone empty minute is %v, want the earlier check (unreachable)", v)
	}
	for back := 1; back <= 3; back++ {
		if v := reach(back); v != nil {
			t.Fatalf("a longer silence was filled %d minutes back: %v", back, *v)
		}
	}
	if v := r.st.series("Chain", "24h"); len(v) == 0 {
		t.Fatal("no 24h series")
	}
}

func TestFibreEndpointRangesGapsAndBounds(t *testing.T) {
	r := statusRig(t, true, nil)
	handler := dash.FibreHandler(r.c.fibreHistory)
	for span, count := range map[string]int{"1h": 60, "24h": 144, "7d": 168, "30d": 120} {
		response := httptest.NewRecorder()
		handler(response, httptest.NewRequest("GET", "/fibre?chain=Chain&range="+span, nil))
		if response.Code != 200 || response.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("%s: %d %s", span, response.Code, response.Body.String())
		}
		var data dash.FibreHistory
		if err := json.Unmarshal(response.Body.Bytes(), &data); err != nil {
			t.Fatal(err)
		}
		if len(data.Chains) != 1 || len(data.Chains[0].Series) != count || !data.Chains[0].Summary.MetricsReceived || response.Body.Len() > 512<<10 {
			t.Fatalf("unexpected range or response size: %+v", data)
		}
		first := data.Chains[0].Series[0]
		if first.Metrics != nil || first.Occupancy != nil || first.Budget != nil || first.Reachable != nil || !strings.Contains(response.Body.String(), `"metrics":null`) {
			t.Fatal("missing intervals became zeros")
		}
	}
	for _, tc := range []struct {
		url, method string
		code        int
	}{
		{"/fibre?range=bad", "GET", 400},
		{"/fibre?chain=unknown", "GET", 404},
		{"/fibre", "POST", 405},
		{"/fibre", "GET", 200},
	} {
		response := httptest.NewRecorder()
		handler(response, httptest.NewRequest(tc.method, tc.url, nil))
		if response.Code != tc.code {
			t.Fatalf("%s %s: got %d", tc.method, tc.url, response.Code)
		}
	}
	oversized := dash.FibreHandler(func(string, string) (dash.FibreHistory, error) {
		return dash.FibreHistory{Chains: []dash.FibreChainHistory{{Name: strings.Repeat("x", 4<<20)}}}, nil
	})
	response := httptest.NewRecorder()
	oversized(response, httptest.NewRequest("GET", "/fibre", nil))
	if response.Code != http.StatusRequestEntityTooLarge || response.Body.Len() > 1024 {
		t.Fatal("response size was not bounded")
	}
}

func TestFibreSeriesWeightsAndUnknownCounters(t *testing.T) {
	r := newFibreRig(t)
	r.cc.Alerts.FibreInstance = "fibre-host-1"
	f := r.feed(time.Minute)
	f.delta = true
	f.occupancy, f.budget = i64(100), i64(0)
	r.c.ingestFibreMetrics(f.statusData(statusTotals{signOK: 10, signFast: 10, signSec: .1}))
	r.advance(time.Minute)
	f.occupancy = i64(200)
	r.c.ingestFibreMetrics(f.statusData(statusTotals{signOK: 30, signFast: 10, signSec: .9}))
	for _, span := range []string{"24h", "7d", "30d"} {
		series := r.st.series("Chain", span)
		last := series[len(series)-1]
		if last.Metrics["sign_fast_percent"] != 50 || last.Metrics["avg_sign_ms"] != 25 || *last.Occupancy != 200 || *last.Budget != 0 {
			t.Fatalf("%s values not weighted: %+v", span, last)
		}
		if _, ok := last.Metrics["downloads_ok"]; ok {
			t.Fatal("unreported download counter became zero")
		}
	}
}

func TestFibreRejectionsAreZeroBeforeTheFirstOne(t *testing.T) {
	r := newFibreRig(t)
	r.cc.Alerts.FibreInstance = "fibre-host-1"
	f := r.feed(time.Minute)
	without := func(data *metricsv1.MetricsData) *metricsv1.MetricsData {
		sm := data.ResourceMetrics[0].ScopeMetrics[0]
		kept := sm.Metrics[:0]
		for _, m := range sm.Metrics {
			if m.GetName() != "fibre.server.upload_shard.rejected" {
				kept = append(kept, m)
			}
		}
		sm.Metrics = kept
		return data
	}
	r.c.ingestFibreMetrics(without(f.statusData(statusTotals{uploadsA: 2, uploadSecA: .02})))
	r.advance(time.Minute)
	r.c.ingestFibreMetrics(without(f.statusData(statusTotals{uploadsA: 5, uploadSecA: .05})))
	series := r.st.series("Chain", "1h")
	last := series[len(series)-1]
	if v, ok := last.Metrics["uploads_rejected"]; !ok || v != 0 {
		t.Fatalf("rejections are %v (known %v) next to %v uploads", v, ok, last.Metrics["uploads_ok"])
	}
}

func gatheredFibre(t *testing.T, c *Config) map[string]float64 {
	t.Helper()
	registry := prometheus.NewPedanticRegistry()
	if err := registry.Register(&fibreCollector{config: c}); err != nil {
		t.Fatal(err)
	}
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	values := make(map[string]float64)
	for _, family := range families {
		for _, metric := range family.Metric {
			key := family.GetName()
			for _, label := range metric.Label {
				if label.GetName() == "result" || label.GetName() == "op" || label.GetName() == "outcome" {
					key += "/" + label.GetValue()
				}
			}
			if metric.Gauge != nil {
				values[key] = metric.Gauge.GetValue()
			}
			if metric.Histogram != nil {
				values[key+"_count"] = float64(metric.Histogram.GetSampleCount())
				values[key+"_sum"] = metric.Histogram.GetSampleSum()
			}
			if metric.Counter != nil {
				values[key] = metric.Counter.GetValue()
			}
		}
	}
	return values
}

func TestFibrePrometheusDeltasAndKnownValues(t *testing.T) {
	r := statusRig(t, true, nil)
	values := gatheredFibre(t, r.c)
	for key, want := range map[string]float64{
		"uploads_total/ok": 42, "rows": 148, "rows_total": 4096,
		"registered": 1, "reachable": 1, "identity_verified": 1,
		"occupancy_bytes": 12 << 20, "signatures_total/ok": 50,
		"signatures_fast_total": 49, "sign_seconds_total": .2,
	} {
		if values["tenderduty_fibre_"+key] != want {
			t.Fatalf("%s = %v, want %v", key, values["tenderduty_fibre_"+key], want)
		}
	}
	f := r.feed(time.Minute)
	f.delta = true
	f.backendOK, f.timeout, f.ignoredOutcomes = 10, 2, 3
	r.c.ingestFibreMetrics(f.statusData(statusTotals{uploadsA: 8, uploadSecA: .1}))
	values = gatheredFibre(t, r.c)
	if values["tenderduty_fibre_uploads_total/ok"] != 50 || values["tenderduty_fibre_object_storage_requests_total/success"] != 10 || values["tenderduty_fibre_object_storage_requests_total/timeout"] != 2 || values["tenderduty_fibre_object_storage_requests_total/canceled"] != 3 {
		t.Fatalf("counter deltas did not add: %+v", values)
	}
	r.cc.Alerts.FibreAlerts = false
	if len(gatheredFibre(t, r.c)) != 0 {
		t.Fatal("unwatched chain exported metrics")
	}
	if r.c.fibreChainFor("fibre-host-1") != "" {
		t.Fatal("unwatched chain accepted exports")
	}
}

func TestFibrePrometheusWithoutExports(t *testing.T) {
	r := statusRig(t, false, nil)
	values := gatheredFibre(t, r.c)
	if _, ok := values["tenderduty_fibre_registered"]; !ok {
		t.Fatal("chain-side metrics missing")
	}
	for _, suffix := range []string{"occupancy_bytes", "budget_bytes", "metrics_age_seconds", "uploads_total/ok", "uploads_in_flight", "go_memory_used_bytes", "go_memory_limit_bytes", "go_gc_goal_bytes", "go_goroutines", "go_processor_limit", "go_gogc", "go_memory_allocated_bytes_total", "go_memory_allocations_total", "prune_entries_total", "object_storage_bytes_total", "object_storage_in_flight", "upload_seconds_count", "download_seconds_count", "sign_seconds_count", "store_put_seconds_count", "store_get_seconds_count", "prune_seconds_count", "go_schedule_seconds_count"} {
		if _, ok := values["tenderduty_fibre_"+suffix]; ok {
			t.Fatalf("unobserved %s exported", suffix)
		}
	}
}

func TestFibreRollupIncludesExportCounters(t *testing.T) {
	r := newFibreRig(t)
	r.cc.Alerts.FibreInstance = "fibre-host-1"
	f := r.feed(time.Minute)
	f.delta = true
	f.rejected, f.put, f.get, f.prune = 3, 5, 6, 7
	f.backendOK, f.timeout, f.throttled, f.err = 8, 2, 4, 9
	r.c.ingestFibreMetrics(f.statusData(statusTotals{
		uploadsA: 11, uploadsFailed: 12, uploadBytes: 100,
		downloadsOK: 13, downloadsFailed: 14, downloadBytes: 200,
		signOK: 10, signFailed: 9, signFast: 8, signSec: .4, pruneEntries: 15,
	}))
	h := r.st.hours["Chain"][r.clock.now.Unix()]
	for key, want := range map[string]float64{
		"uploads_ok": 11, "uploads_failed": 12, "uploads_rejected": 3, "bytes_received": 100,
		"downloads_ok": 13, "downloads_failed": 14, "bytes_served": 200,
		"signatures": 19, "signatures_ok": 10, "signatures_failed": 9, "signatures_fast": 8, "sign_seconds": .4,
		"store_put_errors": 5, "store_get_errors": 6, "prune_errors": 7, "prune_entries": 15,
		"object_storage_requests": 23, "object_storage_timeout": 2, "object_storage_throttled": 4, "object_storage_error": 9,
	} {
		if got, ok := h.Counts[key]; !ok || got != want {
			t.Fatalf("rollup %s = %v (known %v), want %v", key, got, ok, want)
		}
	}
}

func TestFibreRefreshRecordsCompletedChecks(t *testing.T) {
	r := newFibreRig(t)
	queries := 0
	r.cc.refreshFibre(r.c.context(), fibreNode(t, []byte{0x0a, 0x00}, &queries))
	h := r.st.hours["Chain"][r.clock.now.Unix()]
	if h == nil || h.Checks != 1 || h.Reachable != 0 || h.Verified != 0 {
		t.Fatalf("completed registration check not sampled: %+v", h)
	}
	// A failed chain query proves nothing about the provider's reachability.
	r.cc.refreshFibre(r.c.context(), signalNode(t, "sdk", 1, &queries))
	if h.Checks != 1 {
		t.Fatal("failed chain query counted as a provider check")
	}
}
