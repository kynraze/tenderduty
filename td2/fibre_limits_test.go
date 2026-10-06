package tenderduty

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	metricsv1 "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcev1 "go.opentelemetry.io/proto/otlp/resource/v1"
)

func TestFibreBodyLimitsFitRealExports(t *testing.T) {
	if fibreMaxBody != 1<<20 || fibreMaxUnzipped != 4<<20 {
		t.Fatalf("limits are %d and %d", fibreMaxBody, fibreMaxUnzipped)
	}
	for _, name := range []string{"export1.pb", "export2.pb"} {
		if n := len(fibreFixture(t, name)); n > fibreMaxBody/4 {
			t.Fatalf("%s is %d bytes, a real export should be far under the limit", name, n)
		}
	}
}

// blocks in Read until released, so a request stays in the decoder
type stalledBody struct {
	started chan<- struct{}
	release <-chan struct{}
}

func (b stalledBody) Read([]byte) (int, error) {
	b.started <- struct{}{}
	<-b.release
	return 0, io.EOF
}

func TestFibreReceiverDecodesFourAtATime(t *testing.T) {
	r := newFibreRig(t)
	r.cc.Alerts.FibreInstance = "fibre-host-1"
	handler := r.c.fibreMetricsHandler()
	started, release := make(chan struct{}, fibreMaxDecoding), make(chan struct{})
	request := func(body io.Reader) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/metrics", body)
		req.Header.Set("Content-Type", "application/x-protobuf")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	var running sync.WaitGroup
	for i := 0; i < fibreMaxDecoding; i++ {
		running.Add(1)
		go func() {
			defer running.Done()
			request(stalledBody{started, release})
		}()
		<-started
	}
	busy := request(strings.NewReader(""))
	if busy.Code != http.StatusServiceUnavailable || busy.Header().Get("Retry-After") == "" {
		t.Fatalf("a fifth request got %d, want 503 with Retry-After", busy.Code)
	}
	close(release)
	running.Wait()
	if got := request(strings.NewReader("")).Code; got != http.StatusOK {
		t.Fatalf("a slot was not freed: %d", got)
	}
}

func TestFibreSendersPerChainAreCapped(t *testing.T) {
	r := newFibreRig(t)
	other := &ChainConfig{name: "Other", ChainId: "other", valInfo: &ValInfo{}}
	other.Alerts.FibreAlerts, other.Alerts.FibreInstance = true, "other-host"
	r.c.Chains["Other"] = other
	export := func(instance string) {
		r.c.ingestFibreMetrics(resourceData(instance, "fibre", gauge("upload_shard.occupancy_bytes", 1)))
	}
	export("other-host")
	for i := 0; i < fibreMaxSenders; i++ {
		r.advance(time.Second)
		export(fmt.Sprintf("host-%d", i))
	}
	if len(r.st.senders) != fibreMaxSenders+1 {
		t.Fatalf("%d senders", len(r.st.senders))
	}
	r.advance(time.Second)
	export("host-0")
	r.advance(time.Second)
	export("host-new")
	if _, ok := r.st.senders["host-0"]; !ok {
		t.Error("a sender that just exported was dropped")
	}
	if _, ok := r.st.senders["host-1"]; ok {
		t.Error("the least recently seen sender was kept")
	}
	if _, ok := r.st.senders["host-new"]; !ok {
		t.Error("the new sender was not kept")
	}
	if _, ok := r.st.senders["other-host"]; !ok {
		t.Error("another chain's sender was dropped")
	}
	chain := 0
	for _, s := range r.st.senders {
		if s.chain == "Chain" {
			chain++
		}
	}
	if chain != fibreMaxSenders {
		t.Fatalf("%d senders for the chain, want %d", chain, fibreMaxSenders)
	}
}

func TestFibreIgnoredInstancesAreCapped(t *testing.T) {
	r := newFibreRig(t)
	if !r.st.noteIgnored("first") || r.st.noteIgnored("first") {
		t.Fatal("only the first sighting is new")
	}
	for i := 0; i < fibreMaxIgnored+50; i++ {
		r.advance(time.Second)
		r.st.noteIgnored(fmt.Sprintf("stranger-%d", i))
		if i == fibreMaxIgnored/2 {
			r.st.noteIgnored("stranger-0")
		}
	}
	if len(r.st.ignored) != fibreMaxIgnored {
		t.Fatalf("%d instances remembered, want %d", len(r.st.ignored), fibreMaxIgnored)
	}
	for instance, want := range map[string]bool{"first": false, "stranger-1": false, "stranger-0": true, fmt.Sprintf("stranger-%d", fibreMaxIgnored+49): true} {
		if _, ok := r.st.ignored[instance]; ok != want {
			t.Errorf("%s remembered: %v, want %v", instance, ok, want)
		}
	}
}

func TestFibreInstanceAndVersionAreCut(t *testing.T) {
	r := newFibreRig(t)
	long := strings.Repeat("é", 150) // 300 bytes, a cut at 128 lands on a character
	export := func(instance, version string) *metricsv1.MetricsData {
		return &metricsv1.MetricsData{ResourceMetrics: []*metricsv1.ResourceMetrics{{
			Resource:     &resourcev1.Resource{Attributes: kvs("service.name", "fibre", "service.instance.id", instance, "service.version", version)},
			ScopeMetrics: []*metricsv1.ScopeMetrics{{Metrics: []*metricsv1.Metric{gauge("upload_shard.occupancy_bytes", 1)}}},
		}}}
	}
	r.c.ingestFibreMetrics(export(long, long))
	if len(r.st.senders) != 1 {
		t.Fatalf("%d senders", len(r.st.senders))
	}
	for instance, s := range r.st.senders {
		if len(instance) > fibreMaxIdentity || !utf8.ValidString(instance) || len(s.version) > fibreMaxIdentity || !utf8.ValidString(s.version) {
			t.Fatalf("stored %d and %d bytes", len(instance), len(s.version))
		}
	}
	if v := r.st.snapshot("Chain").Version; len(v) == 0 || len(v) > fibreMaxIdentity {
		t.Fatalf("status shows %d bytes of version", len(v))
	}
	// unclaimed senders are bounded too
	r.cc.Alerts.FibreInstance = "fibre-host-1"
	r.c.ingestFibreMetrics(export(long+"x", "v1"))
	for instance := range r.st.ignored {
		if len(instance) > fibreMaxIdentity || !utf8.ValidString(instance) {
			t.Fatalf("remembered %d bytes", len(instance))
		}
	}
	for _, tc := range []struct {
		in   string
		n    int
		want string
	}{{"", 2, ""}, {"abc", 5, "abc"}, {"abc", 0, ""}, {"héllo", 2, "h"}, {"héllo", 3, "hé"}, {"é", 1, ""}} {
		if got := clipBytes(tc.in, tc.n); got != tc.want {
			t.Errorf("clipBytes(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
		}
	}
}

func TestFibreRawSeriesLimits(t *testing.T) {
	r := rawRig(t)
	export := func(help string, attrs ...interface{}) {
		point := &metricsv1.NumberDataPoint{Attributes: kvs(attrs...), Value: &metricsv1.NumberDataPoint_AsInt{AsInt: 1}}
		r.c.ingestFibreMetrics(&metricsv1.MetricsData{ResourceMetrics: []*metricsv1.ResourceMetrics{{
			Resource: &resourcev1.Resource{Attributes: kvs("service.name", "fibre", "service.instance.id", "fibre-host-1")},
			ScopeMetrics: []*metricsv1.ScopeMetrics{{Metrics: []*metricsv1.Metric{
				{Name: "thing", Description: help, Data: &metricsv1.Metric_Gauge{Gauge: &metricsv1.Gauge{DataPoints: []*metricsv1.NumberDataPoint{point}}}},
			}}},
		}}})
	}
	labels := func(n int) []interface{} {
		var attrs []interface{}
		for i := 0; i < n; i++ {
			attrs = append(attrs, fmt.Sprintf("attr%02d", i), "v")
		}
		return attrs
	}
	export("", labels(fibreRawMaxLabels-2)...)
	if n := len(r.st.raw["Chain"]); n != 1 || r.st.rawSkipped != 0 {
		t.Fatalf("a series with exactly %d labels: %d kept, %d skipped", fibreRawMaxLabels, n, r.st.rawSkipped)
	}
	export("", labels(fibreRawMaxLabels-1)...)
	if n := len(r.st.raw["Chain"]); n != 1 || r.st.rawSkipped != 1 {
		t.Fatalf("a series with %d labels: %d kept, %d skipped", fibreRawMaxLabels+1, n, r.st.rawSkipped)
	}
	export("", strings.Repeat("n", fibreRawMaxName+1), "v")
	if n := len(r.st.raw["Chain"]); n != 1 || r.st.rawSkipped != 2 {
		t.Fatalf("a label name over the limit: %d kept, %d skipped", n, r.st.rawSkipped)
	}

	export(strings.Repeat("h", 2000), "big", strings.Repeat("v", 1000), "multi", strings.Repeat("é", 400))
	var found *fibreRawSeries
	for _, s := range r.st.raw["Chain"] {
		if s.labels["big"] != "" {
			found = s
		}
	}
	if found == nil {
		t.Fatal("the series with long values was dropped")
	}
	if len(found.help) != fibreRawMaxHelp || len(found.labels["big"]) != fibreRawMaxValue || len(found.labels["multi"]) > fibreRawMaxValue || !utf8.ValidString(found.labels["multi"]) {
		t.Fatalf("help %d bytes, big %d, multi %d", len(found.help), len(found.labels["big"]), len(found.labels["multi"]))
	}
	r.c.ingestFibreMetrics(&metricsv1.MetricsData{ResourceMetrics: []*metricsv1.ResourceMetrics{{
		Resource: &resourcev1.Resource{Attributes: kvs("service.name", "fibre", "service.instance.id", "fibre-host-1")},
		ScopeMetrics: []*metricsv1.ScopeMetrics{{Metrics: []*metricsv1.Metric{
			{Name: strings.Repeat("m", fibreRawMaxName+1), Data: &metricsv1.Metric_Gauge{Gauge: &metricsv1.Gauge{DataPoints: []*metricsv1.NumberDataPoint{{}}}}},
		}}},
	}}})
	if r.st.rawSkipped != 3 {
		t.Fatalf("%d skipped, want 3", r.st.rawSkipped)
	}
	if body := scrape(t, r.c); !strings.Contains(body, "thing") {
		t.Fatalf("scrape lost the kept series:\n%s", body)
	}
}

func TestFibreCleartextTokenWarning(t *testing.T) {
	for _, tc := range []struct {
		name, listen, token, cert string
		warns                     bool
	}{
		{"loopback with a token", "127.0.0.1:4318", "t", "", false},
		{"open with a token", "0.0.0.0:4318", "t", "", true},
		{"open with a token and TLS", "0.0.0.0:4318", "t", "cert.pem", false},
		{"off", "", "t", "", false},
	} {
		c := fibreValidateConfig(tc.listen, tc.token, [2]interface{}{true, ""})
		if tc.cert != "" {
			c.FibreMetrics.TLSCert, c.FibreMetrics.TLSKey = tc.cert, "key.pem"
		}
		fatal, problems := validateConfig(c)
		warned := strings.Contains(strings.Join(problems, "\n"), "cleartext")
		if fatal || warned != tc.warns {
			t.Errorf("%s: fatal %v, warned %v, want %v: %v", tc.name, fatal, warned, tc.warns, problems)
		}
	}
}
