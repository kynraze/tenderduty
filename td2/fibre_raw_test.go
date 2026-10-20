package tenderduty

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	metricsv1 "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcev1 "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/proto"
)

func TestFibreRawName(t *testing.T) {
	for _, tc := range []struct {
		name, unit string
		counter    bool
		gauge      bool
		want       string
	}{
		{"fibre.server.upload_shard.duration", "s", false, false, "fibre_server_upload_shard_duration_seconds"},
		{"fibre.server.upload_shard.bytes", "By", true, false, "fibre_server_upload_shard_bytes_total"},
		{"fibre.server.prune.entries", "{entry}", true, false, "fibre_server_prune_entries_total"},
		{"fibre.server.upload_shard.in_flight", "{request}", false, true, "fibre_server_upload_shard_in_flight"},
		{"go.memory.used", "By", false, true, "go_memory_used_bytes"},
		{"go.config.gogc", "%", false, true, "go_config_gogc_percent"},
		{"some.ratio", "1", false, true, "some_ratio"},
		{"cache.hit", "1", false, true, "cache_hit_ratio"},
		{"requests", "1", true, false, "requests_total"},
		{"requests.total", "1", true, false, "requests_total"},
		{"latency_seconds", "s", false, false, "latency_seconds"},
		{"io.rate", "By/s", false, true, "io_rate_bytes_per_second"},
		{"temp", "Cel", false, true, "temp_celsius"},
		{"odd", "widgets", false, true, "odd_widgets"},
		{"9lives", "", false, true, "_9lives"},
	} {
		if got := fibreRawName(tc.name, tc.unit, tc.counter, tc.gauge); got != tc.want {
			t.Errorf("%s (%q): got %s, want %s", tc.name, tc.unit, got, tc.want)
		}
	}
	for key, want := range map[string]string{"service.name": "service_name", "9a": "key_9a", "_x": "key_x", "__x": "__x", "a-b c": "a_b_c"} {
		if got := fibreRawLabel(key); got != want {
			t.Errorf("label %q: got %s, want %s", key, got, want)
		}
	}
}

func rawRig(t *testing.T) *fibreRig {
	t.Helper()
	r := newFibreRig(t)
	r.cc.ChainId = "celestia"
	r.cc.Alerts.FibreInstance = "fibre-host-1"
	r.c.Prom = true
	r.c.FibreMetrics.ExportRaw = true
	return r
}

func scrape(t *testing.T, c *Config) string {
	t.Helper()
	reg := prometheus.NewRegistry()
	if err := reg.Register(&fibreRawCollector{config: c}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	promhttp.HandlerFor(reg, promhttp.HandlerOpts{}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("scrape: %d %s", rec.Code, rec.Body)
	}
	return rec.Body.String()
}

func fixtureExport(t *testing.T, name string) *metricsv1.MetricsData {
	t.Helper()
	var data metricsv1.MetricsData
	if err := proto.Unmarshal(fibreFixture(t, name), &data); err != nil {
		t.Fatal(err)
	}
	return &data
}

// histogram names include their suffixes
func producedNames(t *testing.T, c *Config) map[string]bool {
	t.Helper()
	reg := prometheus.NewRegistry()
	if err := reg.Register(&fibreRawCollector{config: c}); err != nil {
		t.Fatal(err)
	}
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	names := make(map[string]bool)
	for _, mf := range families {
		names[mf.GetName()] = true
		for _, m := range mf.GetMetric() {
			if m.GetHistogram() != nil {
				for _, suffix := range []string{"_bucket", "_sum", "_count"} {
					names[mf.GetName()+suffix] = true
				}
			}
		}
	}
	return names
}

var (
	promQLString = regexp.MustCompile(`"[^"]*"`)
	promQLGroup  = regexp.MustCompile(`\b(?:by|without|on|ignoring)\s*\([^)]*\)`)
	promQLBraces = regexp.MustCompile(`\{[^}]*\}`)
	promQLName   = regexp.MustCompile(`[a-zA-Z_:][a-zA-Z0-9_:]*`)
)

// names the official dashboards' queries use
func dashboardMetrics(t *testing.T) map[string]bool {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("testdata", "fibre", "dashboards", "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no dashboards: %v", err)
	}
	names := make(map[string]bool)
	var walk func(v interface{})
	walk = func(v interface{}) {
		switch x := v.(type) {
		case map[string]interface{}:
			for k, child := range x {
				if expr, ok := child.(string); ok && k == "expr" {
					expr = promQLString.ReplaceAllString(expr, "")
					expr = promQLGroup.ReplaceAllString(expr, "")
					expr = promQLBraces.ReplaceAllString(expr, "")
					for _, name := range promQLName.FindAllString(expr, -1) {
						if strings.HasPrefix(name, "fibre_") || strings.HasPrefix(name, "go_") {
							names[name] = true
						}
					}
					continue
				}
				walk(child)
			}
		case []interface{}:
			for _, child := range x {
				walk(child)
			}
		}
	}
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		var doc interface{}
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		walk(doc)
	}
	return names
}

// Fixtures omit these Fibre server and Go runtime metrics.
func declaredOnlyExport() *metricsv1.MetricsData {
	start := uint64(time.Now().Add(-time.Hour).UnixNano())
	point := func() []*metricsv1.NumberDataPoint {
		return []*metricsv1.NumberDataPoint{{StartTimeUnixNano: start, Value: &metricsv1.NumberDataPoint_AsInt{AsInt: 1}}}
	}
	counter := func(name, unit string) *metricsv1.Metric {
		return &metricsv1.Metric{Name: name, Unit: unit, Data: &metricsv1.Metric_Sum{Sum: &metricsv1.Sum{IsMonotonic: true, DataPoints: point()}}}
	}
	updown := func(name, unit string) *metricsv1.Metric {
		return &metricsv1.Metric{Name: name, Unit: unit, Data: &metricsv1.Metric_Sum{Sum: &metricsv1.Sum{DataPoints: point()}}}
	}
	gauge := func(name, unit string) *metricsv1.Metric {
		return &metricsv1.Metric{Name: name, Unit: unit, Data: &metricsv1.Metric_Gauge{Gauge: &metricsv1.Gauge{DataPoints: point()}}}
	}
	hist := func(name string) *metricsv1.Metric {
		return &metricsv1.Metric{Name: name, Unit: "s", Data: &metricsv1.Metric_Histogram{Histogram: &metricsv1.Histogram{
			DataPoints: []*metricsv1.HistogramDataPoint{{StartTimeUnixNano: start, Count: 1, ExplicitBounds: []float64{0.1}, BucketCounts: []uint64{1, 0}}}}}}
	}
	return &metricsv1.MetricsData{ResourceMetrics: []*metricsv1.ResourceMetrics{{
		Resource: &resourcev1.Resource{Attributes: kvs("service.name", "fibre", "service.instance.id", "fibre-host-1")},
		ScopeMetrics: []*metricsv1.ScopeMetrics{{Metrics: []*metricsv1.Metric{
			counter("fibre.server.upload_shard.bytes", "By"), counter("fibre.server.download_shard.bytes", "By"),
			counter("fibre.server.prune.entries", ""), updown("fibre.server.download_shard.in_flight", ""),
			hist("fibre.server.download_shard.duration"), hist("fibre.server.store.get.duration"),
			updown("go.goroutine.count", "{goroutine}"), gauge("go.config.gogc", "%"), updown("go.memory.used", "By"),
			updown("go.memory.limit", "By"), updown("go.memory.gc.goal", "By"), gauge("go.processor.limit", "{thread}"),
			counter("go.memory.allocated", "By"), counter("go.memory.allocations", "{allocation}"),
		}}},
	}}}
}

// TestFibreRawDashboardNames: the client metrics are the Fibre client's; a validator's server doesn't export them.
func TestFibreRawDashboardNames(t *testing.T) {
	r := rawRig(t)
	r.c.ingestFibreMetrics(fixtureExport(t, "export1.pb"))
	r.advance(time.Minute)
	r.c.ingestFibreMetrics(fixtureExport(t, "export2.pb"))
	r.c.ingestFibreMetrics(declaredOnlyExport())
	produced := producedNames(t, r.c)
	referenced := dashboardMetrics(t)
	var missing []string
	for name := range referenced {
		if !produced[name] && !strings.HasPrefix(name, "fibre_client_") {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	t.Logf("referenced %d names, %d produced", len(referenced), len(produced))
	for _, name := range missing {
		t.Errorf("the dashboards query %s, export_raw did not produce it", name)
	}
	for _, name := range []string{"fibre_server_upload_shard_duration_seconds_bucket", "fibre_server_upload_shard_bytes_total", "fibre_server_sign_duration_seconds_count"} {
		if !referenced[name] || !produced[name] {
			t.Errorf("%s: referenced %v produced %v", name, referenced[name], produced[name])
		}
	}
}

func TestFibreRawScrape(t *testing.T) {
	r := rawRig(t)
	r.c.ingestFibreMetrics(fixtureExport(t, "export1.pb"))
	body := scrape(t, r.c)
	for _, want := range []string{
		`# TYPE fibre_server_upload_shard_rejected_total counter`,
		`# TYPE fibre_server_upload_shard_duration_seconds histogram`,
		`# TYPE fibre_server_upload_shard_occupancy_bytes gauge`,
		`chain_id="celestia"`, `name="Chain"`, `service_name="fibre"`, `service_instance_id="fibre-host-1"`, `service_version="v10.4.0-mocha"`,
		`fibre_server_upload_shard_duration_seconds_bucket{`, `le="+Inf"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("scrape misses %q", want)
		}
	}

	// disable serving immediately and discard stored series on the next export
	r.c.FibreMetrics.ExportRaw = false
	if body := scrape(t, r.c); strings.Contains(body, "fibre_server") {
		t.Error("export_raw off still serves metrics")
	}
	r.c.ingestFibreMetrics(fixtureExport(t, "export2.pb"))
	if len(r.st.raw) != 0 {
		t.Error("export_raw off kept series")
	}
	// without the Prometheus endpoint nothing is kept either
	r.c.FibreMetrics.ExportRaw, r.c.Prom = true, false
	r.c.ingestFibreMetrics(fixtureExport(t, "export2.pb"))
	if len(r.st.raw) != 0 {
		t.Error("export_raw without prometheus_enabled kept series")
	}
}

// TestFibreRawKeepsEverythingOfAClaimedSender: includes an unused metric and an unclaimed sender.
func TestFibreRawKeepsEverythingOfAClaimedSender(t *testing.T) {
	r := rawRig(t)
	start := uint64(r.clock.now.Add(-time.Hour).UnixNano())
	export := func(instance string, value int64) *metricsv1.MetricsData {
		point := &metricsv1.NumberDataPoint{StartTimeUnixNano: start, Value: &metricsv1.NumberDataPoint_AsInt{AsInt: value},
			Attributes: kvs("some.attr", "x", "name", "clash")}
		return &metricsv1.MetricsData{ResourceMetrics: []*metricsv1.ResourceMetrics{{
			Resource: &resourcev1.Resource{Attributes: kvs("service.name", "fibre", "service.instance.id", instance)},
			ScopeMetrics: []*metricsv1.ScopeMetrics{{Metrics: []*metricsv1.Metric{
				{Name: "other.thing", Unit: "1", Data: &metricsv1.Metric_Gauge{Gauge: &metricsv1.Gauge{DataPoints: []*metricsv1.NumberDataPoint{point}}}},
				{Name: "made.up.count", Unit: "{item}", Data: &metricsv1.Metric_Sum{Sum: &metricsv1.Sum{IsMonotonic: true,
					AggregationTemporality: metricsv1.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA, DataPoints: []*metricsv1.NumberDataPoint{point}}}},
				{Name: "summary.thing", Data: &metricsv1.Metric_Summary{Summary: &metricsv1.Summary{DataPoints: []*metricsv1.SummaryDataPoint{{}}}}},
			}}},
		}}}
	}
	r.c.ingestFibreMetrics(export("fibre-host-1", 5))
	r.c.ingestFibreMetrics(export("fibre-host-1", 7))
	r.c.ingestFibreMetrics(export("stranger", 9))
	body := scrape(t, r.c)
	for _, want := range []string{`other_thing_ratio{chain_id="celestia",name="Chain",`, `some_attr="x"`, `made_up_count_total{chain_id="celestia",name="Chain",`, "} 12\n"} {
		if !strings.Contains(body, want) {
			t.Errorf("scrape misses %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "stranger") || strings.Contains(body, "summary") || strings.Contains(body, `name="clash"`) {
		t.Errorf("scrape has what it shouldn't:\n%s", body)
	}
	// delta histograms accumulate; absent series expire
	r.advance(fibreRawStale)
	if body := scrape(t, r.c); strings.Contains(body, "other_thing") {
		t.Errorf("stale series are still served:\n%s", body)
	}
}

func TestFibreRawCap(t *testing.T) {
	r := rawRig(t)
	var points []*metricsv1.NumberDataPoint
	for i := 0; i < fibreRawMaxSeries+10; i++ {
		points = append(points, &metricsv1.NumberDataPoint{Attributes: kvs("id", i)})
	}
	r.c.ingestFibreMetrics(&metricsv1.MetricsData{ResourceMetrics: []*metricsv1.ResourceMetrics{{
		Resource: &resourcev1.Resource{Attributes: kvs("service.name", "fibre", "service.instance.id", "fibre-host-1")},
		ScopeMetrics: []*metricsv1.ScopeMetrics{{Metrics: []*metricsv1.Metric{
			{Name: "many", Data: &metricsv1.Metric_Gauge{Gauge: &metricsv1.Gauge{DataPoints: points}}},
		}}},
	}}})
	if n := len(r.st.raw["Chain"]); n != fibreRawMaxSeries {
		t.Errorf("%d series kept, want %d", n, fibreRawMaxSeries)
	}
}

func TestFibreRawDeltaHistogram(t *testing.T) {
	r := rawRig(t)
	export := func(counts ...uint64) *metricsv1.MetricsData {
		return &metricsv1.MetricsData{ResourceMetrics: []*metricsv1.ResourceMetrics{{
			Resource: &resourcev1.Resource{Attributes: kvs("service.name", "fibre", "service.instance.id", "fibre-host-1")},
			ScopeMetrics: []*metricsv1.ScopeMetrics{{Metrics: []*metricsv1.Metric{{Name: "op.duration", Unit: "s", Data: &metricsv1.Metric_Histogram{Histogram: &metricsv1.Histogram{
				AggregationTemporality: metricsv1.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA,
				DataPoints:             []*metricsv1.HistogramDataPoint{{Count: counts[0] + counts[1], Sum: proto.Float64(1.5), ExplicitBounds: []float64{0.1}, BucketCounts: counts}},
			}}}}}},
		}}}
	}
	r.c.ingestFibreMetrics(export(1, 2))
	r.c.ingestFibreMetrics(export(3, 0))
	body := scrape(t, r.c)
	for _, want := range []string{`op_duration_seconds_bucket{`, `le="0.1"} 4`, `le="+Inf"} 6`, `op_duration_seconds_sum{`, `} 3`, `op_duration_seconds_count{`} {
		if !strings.Contains(body, want) {
			t.Errorf("scrape misses %q:\n%s", want, body)
		}
	}
}
