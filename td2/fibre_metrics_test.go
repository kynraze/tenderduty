package tenderduty

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	commonv1 "go.opentelemetry.io/proto/otlp/common/v1"
	metricsv1 "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcev1 "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/proto"
)

// exports captured from otlpmetrichttp and sdk/metric, the libraries Fibre uses
func fibreFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "fibre", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// the fixtures' start time is what the first export is measured against
type fibreClock struct{ now time.Time }

func (f *fibreClock) Now() time.Time { return f.now }

type fibreRig struct {
	t     *testing.T
	c     *Config
	cc    *ChainConfig
	st    *fibreStore
	clock *fibreClock
	vi    *ValInfo
}

func newFibreRig(t *testing.T) *fibreRig {
	t.Helper()
	c := auditConfig(t)
	c.NodeDownMin = 3
	c.FibreMetrics.Listen = "127.0.0.1:4318"
	// the provider check has run: the chain has Fibre, so silence means something
	cc := &ChainConfig{name: "Chain", ChainId: "celestia", valInfo: &ValInfo{Valcons: "valcons1", Moniker: "Kynraze"},
		fibreLoaded: true, fibre: fibreState{Registered: true, Host: "1.2.3.4:7980", Reachable: true}}
	cc.Alerts.FibreAlerts, cc.Alerts.FibrePriority = true, "warning"
	c.Chains["Chain"] = cc
	clock := &fibreClock{now: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}
	st := newFibreStore()
	st.now, st.started = clock.Now, clock.now
	c.fibreStore = st
	return &fibreRig{t: t, c: c, cc: cc, st: st, clock: clock, vi: cc.valInfo}
}

func (r *fibreRig) advance(d time.Duration) { r.clock.now = r.clock.now.Add(d) }

// exports every minute, starting at the rig's clock
type fibreFeed struct {
	r                       *fibreRig
	instance                string
	start                   uint64
	rejected                int64
	signFast, signSlow      int64 // successful signatures at or under 10ms, and over it
	signFailed              int64
	put, get, prune         int64
	timeout, throttled, err int64
	backendOK               int64 // requests that succeeded
	ignoredOutcomes         int64 // canceled requests, a request that is neither a success nor a failure
	occupancy, budget       *int64
	lastSuccess             *int64
	delta                   bool // report each export's change instead of the running total
	last                    []int64
}

func (f *fibreFeed) counters() []*int64 {
	return []*int64{&f.rejected, &f.signFast, &f.signSlow, &f.signFailed, &f.put, &f.get, &f.prune, &f.timeout, &f.throttled, &f.err, &f.backendOK, &f.ignoredOutcomes}
}

func (r *fibreRig) feed(startedAgo time.Duration) *fibreFeed {
	return &fibreFeed{r: r, instance: "fibre-host-1", start: uint64(r.clock.now.Add(-startedAgo).UnixNano())}
}

func i64(v int64) *int64 { return &v }

func kvs(pairs ...interface{}) []*commonv1.KeyValue {
	var out []*commonv1.KeyValue
	for i := 0; i < len(pairs); i += 2 {
		v := &commonv1.AnyValue{}
		switch x := pairs[i+1].(type) {
		case string:
			v.Value = &commonv1.AnyValue_StringValue{StringValue: x}
		case bool:
			v.Value = &commonv1.AnyValue_BoolValue{BoolValue: x}
		case int:
			v.Value = &commonv1.AnyValue_IntValue{IntValue: int64(x)}
		}
		out = append(out, &commonv1.KeyValue{Key: pairs[i].(string), Value: v})
	}
	return out
}

func (f *fibreFeed) temporality() metricsv1.AggregationTemporality {
	if f.delta {
		return metricsv1.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA
	}
	return metricsv1.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE
}

func (f *fibreFeed) sum(name string, total int64, attrs ...interface{}) *metricsv1.Metric {
	return &metricsv1.Metric{Name: "fibre.server." + name, Data: &metricsv1.Metric_Sum{Sum: &metricsv1.Sum{
		IsMonotonic: true, AggregationTemporality: f.temporality(),
		DataPoints: []*metricsv1.NumberDataPoint{{Attributes: kvs(attrs...), StartTimeUnixNano: f.start, Value: &metricsv1.NumberDataPoint_AsInt{AsInt: total}}},
	}}}
}

func (f *fibreFeed) hist(name string, bounds []float64, series ...*metricsv1.HistogramDataPoint) *metricsv1.Metric {
	for _, dp := range series {
		dp.StartTimeUnixNano, dp.ExplicitBounds = f.start, bounds
	}
	return &metricsv1.Metric{Name: "fibre.server." + name, Data: &metricsv1.Metric_Histogram{Histogram: &metricsv1.Histogram{
		AggregationTemporality: f.temporality(), DataPoints: series,
	}}}
}

func gauge(name string, v int64) *metricsv1.Metric {
	return &metricsv1.Metric{Name: "fibre.server." + name, Data: &metricsv1.Metric_Gauge{Gauge: &metricsv1.Gauge{
		DataPoints: []*metricsv1.NumberDataPoint{{TimeUnixNano: 1, Value: &metricsv1.NumberDataPoint_AsInt{AsInt: v}}},
	}}}
}

// every observation in the first bucket; only the count matters
func plain(n int64, attrs ...interface{}) *metricsv1.HistogramDataPoint {
	return &metricsv1.HistogramDataPoint{Attributes: kvs(attrs...), Count: uint64(n), BucketCounts: []uint64{uint64(n), 0}}
}

func (f *fibreFeed) data() *metricsv1.MetricsData {
	signBounds := []float64{0.001, 0.005, 0.01, 0.05, 0.1}
	var metrics []*metricsv1.Metric
	if f.rejected > 0 {
		metrics = append(metrics, f.sum("upload_shard.rejected", f.rejected, "reason", "budget_exceeded"))
	}
	var signs []*metricsv1.HistogramDataPoint
	if f.signFast+f.signSlow > 0 {
		signs = append(signs, &metricsv1.HistogramDataPoint{
			Attributes: kvs("success", true), Count: uint64(f.signFast + f.signSlow),
			BucketCounts: []uint64{0, 0, uint64(f.signFast), uint64(f.signSlow), 0, 0},
		})
	}
	if f.signFailed > 0 {
		signs = append(signs, &metricsv1.HistogramDataPoint{
			Attributes: kvs("success", false), Count: uint64(f.signFailed),
			BucketCounts: []uint64{0, 0, uint64(f.signFailed), 0, 0, 0},
		})
	}
	if len(signs) > 0 {
		metrics = append(metrics, f.hist("sign.duration", signBounds, signs...))
	}
	for name, n := range map[string]int64{"store.put.duration": f.put, "store.get.duration": f.get, "prune.duration": f.prune} {
		if n > 0 {
			metrics = append(metrics, f.hist(name, []float64{1}, plain(n, "success", false), plain(50, "success", true)))
		}
	}
	var outcomes []*metricsv1.HistogramDataPoint
	for outcome, n := range map[string]int64{"timeout": f.timeout, "throttled": f.throttled, "error": f.err, "canceled": f.ignoredOutcomes, "success": f.backendOK} {
		if n > 0 {
			outcomes = append(outcomes, plain(n, "outcome", outcome))
		}
	}
	metrics = append(metrics, f.hist("backend.get.duration", []float64{1}, outcomes...))
	for name, v := range map[string]*int64{"upload_shard.occupancy_bytes": f.occupancy, "upload_shard.budget_bytes": f.budget, "upload_shard.last_success_timestamp": f.lastSuccess} {
		if v != nil {
			metrics = append(metrics, gauge(name, *v))
		}
	}
	// runtime and client metrics ride along and must be ignored
	metrics = append(metrics, gauge("not.a.thing", 1), &metricsv1.Metric{Name: "fibre.client.upload.duration"}, &metricsv1.Metric{Name: "go.goroutine.count"})
	return resourceData(f.instance, "fibre", metrics...)
}

func resourceData(instance, service string, metrics ...*metricsv1.Metric) *metricsv1.MetricsData {
	return &metricsv1.MetricsData{ResourceMetrics: []*metricsv1.ResourceMetrics{{
		Resource:     &resourcev1.Resource{Attributes: kvs("service.name", service, "service.instance.id", instance, "service.version", "v10.4.0-mocha")},
		ScopeMetrics: []*metricsv1.ScopeMetrics{{Metrics: metrics}},
	}}}
}

// counters are running totals unless the feed reports deltas
func (f *fibreFeed) send() {
	f.r.t.Helper()
	if f.delta {
		counters := f.counters()
		if f.last == nil {
			f.last = make([]int64, len(counters))
		}
		cur := make([]int64, len(counters))
		for i, p := range counters {
			cur[i], *p = *p, *p-f.last[i]
		}
		defer func() {
			for i, p := range counters {
				*p = cur[i]
			}
			f.last = cur
		}()
	}
	f.r.c.ingestFibreMetrics(f.data())
}

func (f *fibreFeed) run(minutes int) {
	for i := 0; i < minutes; i++ {
		f.r.advance(time.Minute)
		f.send()
	}
}

func (r *fibreRig) alarms() []*alertMsg {
	r.t.Helper()
	r.cc.fibreMetricsAlarms(r.vi, false)
	var sent []*alertMsg
	for len(r.c.alertChan) > 0 {
		sent = append(sent, <-r.c.alertChan)
	}
	return sent
}

func (r *fibreRig) active(message string) bool { return alarmIsActive("Chain", message) }

func (r *fibreRig) msg(kind string) string { return r.cc.fibreMetricsMessage(kind) }

func (r *fibreRig) raised(sent []*alertMsg, kind, detail, icon, label string) {
	r.t.Helper()
	if len(sent) != 1 || sent[0].resolved || sent[0].message != r.msg(kind) {
		r.t.Fatalf("%s: expected one raised alert %q, got %s", kind, r.msg(kind), describe(sent))
	}
	got := sent[0]
	if want := got.message + "\n" + detail; got.summary() != want {
		r.t.Errorf("%s: summary is %q, want %q", kind, got.summary(), want)
	}
	if i, s := got.header(); i != icon || s != label {
		r.t.Errorf("%s: header is %s %s, want %s %s", kind, i, s, icon, label)
	}
	if got.severity != "warning" || got.uniqueId != "valcons1fibremetrics"+kind {
		r.t.Errorf("%s: severity %q, id %q", kind, got.severity, got.uniqueId)
	}
	if !r.active(got.message) {
		r.t.Errorf("%s: not an active alarm", kind)
	}
}

func (r *fibreRig) cleared(sent []*alertMsg, kind, label, text string) {
	r.t.Helper()
	if len(sent) != 1 || !sent[0].resolved || sent[0].message != r.msg(kind) {
		r.t.Fatalf("%s: expected one resolved alert %q, got %s", kind, r.msg(kind), describe(sent))
	}
	got := sent[0]
	if got.summary() != text {
		r.t.Errorf("%s: resolved summary is %q, want %q", kind, got.summary(), text)
	}
	if i, s := got.header(); i != "✅" || s != label {
		r.t.Errorf("%s: resolved header is %s %s, want ✅ %s", kind, i, s, label)
	}
	if strings.Contains(got.summary(), "failing") || strings.Contains(got.summary(), "over") && kind != kindStorage {
		r.t.Errorf("%s: a resolved notification repeats the problem: %q", kind, got.summary())
	}
	if r.active(got.message) {
		r.t.Errorf("%s: still active", kind)
	}
}

func describe(sent []*alertMsg) string {
	var parts []string
	for _, a := range sent {
		state := "raised"
		if a.resolved {
			state = "resolved"
		}
		parts = append(parts, state+" "+strings.ReplaceAll(a.message, "\n", " / "))
	}
	return "[" + strings.Join(parts, "; ") + "]"
}

func (r *fibreRig) quiet(sent []*alertMsg, what string) {
	r.t.Helper()
	if len(sent) != 0 {
		r.t.Fatalf("%s: unexpected alerts %s", what, describe(sent))
	}
}

func TestFibreMetricsFixtures(t *testing.T) {
	r := newFibreRig(t)
	r.cc.Alerts.FibreInstance = "fibre-host-1"
	var first metricsv1.MetricsData
	if err := proto.Unmarshal(fibreFixture(t, "export1.pb"), &first); err != nil {
		t.Fatal(err)
	}
	// the sender started a moment before its first export
	started := time.Unix(0, int64(first.ResourceMetrics[0].ScopeMetrics[0].Metrics[0].GetSum().DataPoints[0].StartTimeUnixNano))
	r.clock.now, r.st.started = started.Add(30*time.Second), started.Add(30*time.Second)

	r.c.ingestFibreMetrics(&first)
	snap := r.st.snapshot("Chain")
	w := snap.Window
	if !snap.Found || snap.Version != "v10.4.0-mocha" || snap.Occupancy != 900000000 || snap.Budget != 1000000000 || !snap.HaveStorage ||
		!snap.HaveSuccess || snap.LastSuccess.Unix() != 1791018097 {
		t.Fatalf("gauges: %+v", snap)
	}
	if w.rejected != 3 || w.signTotal != 2 || w.signOK != 2 || w.signFailed != 0 || w.signFast != 1 ||
		w.storePut != 1 || w.storeGet != 0 || w.prune != 0 || w.backendTimeout+w.backendThrottled+w.backendError != 0 {
		t.Fatalf("export 1 counted %+v", w)
	}

	var second metricsv1.MetricsData
	if err := proto.Unmarshal(fibreFixture(t, "export2.pb"), &second); err != nil {
		t.Fatal(err)
	}
	r.advance(time.Minute)
	r.c.ingestFibreMetrics(&second)
	// the counters are cumulative: 3 rejections became 5, with the same start time
	w2 := r.st.snapshot("Chain").Window
	if w2.rejected-w.rejected != 2 {
		t.Fatalf("export 2 added rejected %d, window %+v", w2.rejected-w.rejected, w2)
	}
}

func TestFibreMetricsDeltas(t *testing.T) {
	t.Run("a sender that started before this process is a baseline", func(t *testing.T) {
		r := newFibreRig(t)
		r.cc.Alerts.FibreInstance = "fibre-host-1"
		f := r.feed(48 * time.Hour)
		f.signFast, f.signSlow, f.signFailed, f.rejected = 5000, 400, 3, 30
		f.send()
		if w := r.st.snapshot("Chain").Window; !w.empty() {
			t.Fatalf("the history before tenderduty started counted: %+v", w)
		}
		f.signFast, f.signSlow, f.signFailed, f.rejected = 5010, 401, 4, 32
		f.run(1)
		if w := r.st.snapshot("Chain").Window; w.signFast != 10 || w.signOK != 11 || w.signFailed != 1 || w.signTotal != 12 || w.rejected != 2 {
			t.Fatalf("growth counted as %+v", w)
		}
	})
	t.Run("a sender that just started counts everything", func(t *testing.T) {
		r := newFibreRig(t)
		r.cc.Alerts.FibreInstance = "fibre-host-1"
		f := r.feed(time.Minute)
		f.signFast, f.signSlow, f.signFailed = 30, 3, 2
		f.send()
		if w := r.st.snapshot("Chain").Window; w.signFast != 30 || w.signOK != 33 || w.signFailed != 2 || w.signTotal != 35 {
			t.Fatalf("first export counted as %+v", w)
		}
	})
	t.Run("a series that appears later is new", func(t *testing.T) {
		r := newFibreRig(t)
		r.cc.Alerts.FibreInstance = "fibre-host-1"
		f := r.feed(48 * time.Hour)
		f.signFast = 100
		f.send()
		f.rejected = 4 // the first rejection ever makes the series
		f.run(1)
		if w := r.st.snapshot("Chain").Window; w.rejected != 4 {
			t.Fatalf("a new series counted as %+v", w)
		}
	})
	t.Run("a restart counts what it has", func(t *testing.T) {
		r := newFibreRig(t)
		r.cc.Alerts.FibreInstance = "fibre-host-1"
		f := r.feed(48 * time.Hour)
		f.rejected = 10
		f.send()
		f.rejected = 14
		f.run(1)
		// a new start time, and the value is lower
		f.start = uint64(r.clock.now.Add(-20 * time.Second).UnixNano())
		f.rejected = 3
		f.run(1)
		if w := r.st.snapshot("Chain").Window; w.rejected != 4+3 {
			t.Fatalf("after a restart the window has %d rejected, want 7", w.rejected)
		}
		// a lower value under the same start time can't be growth either
		f.rejected = 1
		f.run(1)
		if w := r.st.snapshot("Chain").Window; w.rejected != 4+3+1 {
			t.Fatalf("after a reset the window has %d rejected, want 8", w.rejected)
		}
		// a changed start time with a higher value is still a restart, the old total is not subtracted
		f.start = uint64(r.clock.now.Add(-5 * time.Second).UnixNano())
		f.rejected = 20
		f.run(1)
		if w := r.st.snapshot("Chain").Window; w.rejected != 4+3+1+20 {
			t.Fatalf("after a second restart the window has %d rejected, want 28", w.rejected)
		}
	})
	t.Run("delta temporality is taken as it comes", func(t *testing.T) {
		r := newFibreRig(t)
		r.cc.Alerts.FibreInstance = "fibre-host-1"
		f := r.feed(48 * time.Hour)
		f.delta = true
		f.signFast, f.signFailed, f.rejected = 5000, 400, 30
		f.send() // the first delta isn't a baseline, it is what happened in the interval
		if w := r.st.snapshot("Chain").Window; w.signFast != 5000 || w.signFailed != 400 || w.rejected != 30 {
			t.Fatalf("first delta counted as %+v", w)
		}
		f.signFast, f.signFailed, f.rejected = 5003, 401, 31
		f.run(1)
		if w := r.st.snapshot("Chain").Window; w.signFast != 5003 || w.signFailed != 401 || w.rejected != 31 {
			t.Fatalf("second delta counted as %+v", w)
		}
	})
	t.Run("attribute sets are summed and only the right outcomes count", func(t *testing.T) {
		r := newFibreRig(t)
		r.cc.Alerts.FibreInstance = "fibre-host-1"
		f := r.feed(time.Minute)
		f.timeout, f.throttled, f.err, f.ignoredOutcomes = 1, 2, 3, 40
		f.put, f.get, f.prune = 4, 5, 6
		f.backendOK, f.signFast = 100, 10
		data := f.data()
		// a second series for the same signatures, here with another attribute
		for _, m := range data.ResourceMetrics[0].ScopeMetrics[0].Metrics {
			if m.Name == "fibre.server.sign.duration" {
				more := &metricsv1.HistogramDataPoint{Attributes: kvs("success", true, "key", "other"), Count: 5, BucketCounts: []uint64{0, 0, 2, 3, 0, 0},
					StartTimeUnixNano: f.start, ExplicitBounds: m.GetHistogram().DataPoints[0].ExplicitBounds}
				m.GetHistogram().DataPoints = append(m.GetHistogram().DataPoints, more)
			}
		}
		r.c.ingestFibreMetrics(data)
		w := r.st.snapshot("Chain").Window
		if w.signOK != 15 || w.signFast != 12 || w.signTotal != 15 || w.backendTimeout != 1 || w.backendThrottled != 2 || w.backendError != 3 ||
			w.backendTotal != 1+2+3+40+100 || w.storePut != 4 || w.storeGet != 5 || w.prune != 6 {
			t.Fatalf("counted %+v", w)
		}
	})
	t.Run("signing is skipped when 0.01 is not a bucket bound", func(t *testing.T) {
		r := newFibreRig(t)
		r.cc.Alerts.FibreInstance = "fibre-host-1"
		f := r.feed(time.Minute)
		odd := f.hist("sign.duration", []float64{0.005, 0.02}, &metricsv1.HistogramDataPoint{Attributes: kvs("success", true), Count: 50, BucketCounts: []uint64{0, 50, 0}})
		r.c.ingestFibreMetrics(resourceData("fibre-host-1", "fibre", odd))
		if w := r.st.snapshot("Chain").Window; w.signTotal != 0 || w.signOK != 0 || w.signFast != 0 {
			t.Fatalf("signing counted without a 10ms bucket: %+v", w)
		}
		// a failure is a failure whatever the buckets are
		bad := f.hist("sign.duration", []float64{0.005, 0.02}, &metricsv1.HistogramDataPoint{Attributes: kvs("success", false), Count: 4, BucketCounts: []uint64{0, 4, 0}})
		r.c.ingestFibreMetrics(resourceData("fibre-host-1", "fibre", bad))
		if w := r.st.snapshot("Chain").Window; w.signTotal != 4 || w.signFailed != 4 || w.signOK != 0 {
			t.Fatalf("failed signatures counted %+v", w)
		}
		// found by value, wherever it is
		moved := f.hist("sign.duration", []float64{0.0001, 0.01, 0.1}, &metricsv1.HistogramDataPoint{Attributes: kvs("success", true), Count: 10, BucketCounts: []uint64{2, 3, 4, 1}})
		r.c.ingestFibreMetrics(resourceData("fibre-host-1", "fibre", moved))
		if w := r.st.snapshot("Chain").Window; w.signTotal != 14 || w.signOK != 10 || w.signFast != 5 {
			t.Fatalf("signing counted %+v", w)
		}
	})
}

func TestFibreMetricsChainMapping(t *testing.T) {
	cfg := func(chains map[string][2]interface{}) *Config {
		c := &Config{Chains: map[string]*ChainConfig{}}
		for name, v := range chains {
			cc := &ChainConfig{name: name}
			cc.Alerts.FibreAlerts, cc.Alerts.FibreInstance = v[0].(bool), v[1].(string)
			c.Chains[name] = cc
		}
		return c
	}
	for _, tc := range []struct {
		name     string
		chains   map[string][2]interface{}
		instance string
		want     string
	}{
		{"named", map[string][2]interface{}{"A": {true, "host-a"}, "B": {true, "host-b"}}, "host-b", "B"},
		{"named wins over the unnamed one", map[string][2]interface{}{"A": {true, ""}, "B": {true, "host-b"}}, "host-b", "B"},
		{"the one unnamed chain takes the rest", map[string][2]interface{}{"A": {true, ""}, "B": {true, "host-b"}}, "other", "A"},
		{"a single chain with no name", map[string][2]interface{}{"A": {true, ""}}, "anything", "A"},
		{"two unnamed chains can't take it", map[string][2]interface{}{"A": {true, ""}, "B": {true, ""}}, "anything", ""},
		{"a chain without Fibre alerts doesn't take it", map[string][2]interface{}{"A": {false, ""}}, "anything", ""},
		{"no match", map[string][2]interface{}{"A": {true, "host-a"}}, "host-z", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := cfg(tc.chains).fibreChainFor(tc.instance); got != tc.want {
				t.Fatalf("mapped to %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFibreMetricsIgnoresWhatIsNotFibre(t *testing.T) {
	r := newFibreRig(t)
	r.cc.Alerts.FibreInstance = "fibre-host-1"
	f := r.feed(time.Minute)
	f.rejected = 5
	good := f.data()
	// a collector forwards other apps, and a Fibre nobody claimed
	other := resourceData("fibre-host-1", "someone-else", f.sum("upload_shard.rejected", 99, "reason", "x"))
	stranger := resourceData("fibre-host-9", "fibre", f.sum("upload_shard.rejected", 77, "reason", "x"))
	good.ResourceMetrics = append(good.ResourceMetrics, other.ResourceMetrics[0], stranger.ResourceMetrics[0])
	r.c.ingestFibreMetrics(good)
	if w := r.st.snapshot("Chain").Window; w.rejected != 5 {
		t.Fatalf("window is %+v, only the Fibre at fibre-host-1 counts", w)
	}
	_, strangerNoted := r.st.ignored["fibre-host-9"]
	_, otherNoted := r.st.ignored["someone-else"]
	if len(r.st.senders) != 1 || !strangerNoted || otherNoted {
		t.Fatalf("senders %d, ignored %v", len(r.st.senders), r.st.ignored)
	}
	r.c.ingestFibreMetrics(stranger)
	if len(r.st.senders) != 1 {
		t.Fatal("a Fibre nobody claimed was kept")
	}
}

func fibreServer(t *testing.T, c *Config) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(c.fibreMetricsHandler())
	t.Cleanup(server.Close)
	return server
}

func post(t *testing.T, url string, body io.Reader, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, body)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func gz(t *testing.T, b []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	w := gzip.NewWriter(&out)
	if _, err := w.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

var protobufHeader = map[string]string{"Content-Type": "application/x-protobuf"}

func TestFibreReceiverRoutes(t *testing.T) {
	r := newFibreRig(t)
	r.cc.Alerts.FibreInstance = "fibre-host-1"
	server := fibreServer(t, r.c)
	body := fibreFixture(t, "export1.pb")
	for _, tc := range []struct {
		path   string
		status int
		kept   bool
	}{
		{"/v1/metrics", 200, true},
		{"/fibre/v1/metrics", 200, true}, // a reverse proxy's prefix is kept
		{"/a/b/c/v1/metrics/", 200, true},
		{"/v1/traces", 200, false},
		{"/proxy/v1/traces", 200, false},
		{"/v1/logs", 404, false},
		{"/", 404, false},
		{"/metrics", 404, false},
		{"/v1/metrics/extra", 404, false},
	} {
		r.st.senders = map[string]*fibreSender{}
		resp := post(t, server.URL+tc.path, bytes.NewReader(body), protobufHeader)
		if resp.StatusCode != tc.status {
			t.Errorf("%s answered %d, want %d", tc.path, resp.StatusCode, tc.status)
		}
		if got := len(r.st.senders) == 1; got != tc.kept {
			t.Errorf("%s kept the metrics: %v, want %v", tc.path, got, tc.kept)
		}
	}
	// a success is a valid empty ExportMetricsServiceResponse
	resp := post(t, server.URL+"/v1/metrics", bytes.NewReader(body), protobufHeader)
	answer, _ := io.ReadAll(resp.Body)
	if resp.Header.Get("Content-Type") != "application/x-protobuf" || len(answer) != 0 {
		t.Errorf("answer %q with type %q", answer, resp.Header.Get("Content-Type"))
	}
	// traces are accepted and dropped, whatever they hold
	resp = post(t, server.URL+"/v1/traces", strings.NewReader("not even protobuf"), nil)
	answer, _ = io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || len(answer) != 0 {
		t.Errorf("traces answered %d %q", resp.StatusCode, answer)
	}
}

func TestFibreReceiverRejects(t *testing.T) {
	r := newFibreRig(t)
	r.cc.Alerts.FibreInstance = "fibre-host-1"
	server := fibreServer(t, r.c)
	url := server.URL + "/v1/metrics"
	body := fibreFixture(t, "export1.pb")

	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		req, _ := http.NewRequest(method, url, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != 405 || resp.Header.Get("Allow") != "POST" {
			t.Errorf("%s answered %d", method, resp.StatusCode)
		}
	}
	// the path is checked before the method
	req, _ := http.NewRequest(http.MethodGet, server.URL+"/nothing", nil)
	resp, _ := http.DefaultClient.Do(req)
	_ = resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("GET on an unknown path answered %d", resp.StatusCode)
	}

	for _, tc := range []struct {
		name    string
		headers map[string]string
		status  int
	}{
		{"json", map[string]string{"Content-Type": "application/json"}, 415},
		{"no type", nil, 415},
		{"protobuf with a charset", map[string]string{"Content-Type": "application/x-protobuf; charset=utf-8"}, 200},
		{"deflate", map[string]string{"Content-Type": "application/x-protobuf", "Content-Encoding": "deflate"}, 415},
		{"br", map[string]string{"Content-Type": "application/x-protobuf", "Content-Encoding": "br"}, 415},
	} {
		if got := post(t, url, bytes.NewReader(body), tc.headers).StatusCode; got != tc.status {
			t.Errorf("%s answered %d, want %d", tc.name, got, tc.status)
		}
	}
	if got := post(t, url, strings.NewReader("this is not protobuf at all"), protobufHeader).StatusCode; got != 400 {
		t.Errorf("garbage answered %d, want 400", got)
	}
	// valid protobuf that isn't a metrics request is still garbage here
	if got := post(t, url, bytes.NewReader(append([]byte{0x0a, 0x7f}, make([]byte, 3)...)), protobufHeader).StatusCode; got != 400 {
		t.Errorf("a truncated message answered %d, want 400", got)
	}
	if got := post(t, url, strings.NewReader("plain text"), map[string]string{"Content-Type": "application/x-protobuf", "Content-Encoding": "gzip"}).StatusCode; got != 400 {
		t.Errorf("gzip that isn't gzip answered %d, want 400", got)
	}
	// an empty export is a valid, empty request
	if got := post(t, url, bytes.NewReader(nil), protobufHeader).StatusCode; got != 200 {
		t.Errorf("an empty body answered %d, want 200", got)
	}
}

func TestFibreReceiverGzip(t *testing.T) {
	r := newFibreRig(t)
	r.cc.Alerts.FibreInstance = "fibre-host-1"
	server := fibreServer(t, r.c)
	headers := map[string]string{"Content-Type": "application/x-protobuf", "Content-Encoding": "gzip"}
	resp := post(t, server.URL+"/v1/metrics", bytes.NewReader(gz(t, fibreFixture(t, "export1.pb"))), headers)
	if resp.StatusCode != 200 {
		t.Fatalf("gzip answered %d", resp.StatusCode)
	}
	if snap := r.st.snapshot("Chain"); !snap.Found || snap.Occupancy != 900000000 {
		t.Fatalf("the gzip body wasn't read: %+v", snap)
	}
}

type unreadable struct{ t *testing.T }

func (u unreadable) Read([]byte) (int, error) {
	u.t.Helper()
	u.t.Error("the body was read")
	return 0, io.EOF
}

func TestFibreReceiverTokenIsChecked(t *testing.T) {
	r := newFibreRig(t)
	r.cc.Alerts.FibreInstance = "fibre-host-1"
	r.c.FibreMetrics.Token = "s3cret-token"
	server := fibreServer(t, r.c)
	url := server.URL + "/v1/metrics"
	body := fibreFixture(t, "export1.pb")
	for _, auth := range []string{"", "s3cret-token", "Bearer", "Bearer ", "Bearer s3cret-toke", "Bearer  s3cret-token", "Bearer s3cret-tokens", "bearer s3cret-token", "Basic s3cret-token", "Bearer S3CRET-TOKEN"} {
		headers := map[string]string{"Content-Type": "application/x-protobuf"}
		if auth != "" {
			headers["Authorization"] = auth
		}
		resp := post(t, url, bytes.NewReader(body), headers)
		if resp.StatusCode != 401 {
			t.Errorf("Authorization %q answered %d, want 401", auth, resp.StatusCode)
		}
	}
	if len(r.st.senders) != 0 {
		t.Fatal("metrics without the token were kept")
	}
	// the token is checked before anything else about the body, a wrong type or a huge body is still a 401
	if got := post(t, url, strings.NewReader("{}"), map[string]string{"Content-Type": "application/json"}).StatusCode; got != 401 {
		t.Errorf("unauthorized json answered %d, want 401", got)
	}
	// and the body isn't read at all
	req := httptest.NewRequest(http.MethodPost, "/v1/metrics", unreadable{t})
	req.Header.Set("Content-Type", "application/x-protobuf")
	rec := httptest.NewRecorder()
	r.c.fibreMetricsHandler().ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Errorf("an unauthorized request answered %d, want 401", rec.Code)
	}
	// and the same for traces
	if got := post(t, server.URL+"/v1/traces", nil, nil).StatusCode; got != 401 {
		t.Errorf("unauthorized traces answered %d, want 401", got)
	}
	headers := map[string]string{"Content-Type": "application/x-protobuf", "Authorization": "Bearer s3cret-token"}
	if got := post(t, url, bytes.NewReader(body), headers).StatusCode; got != 200 || len(r.st.senders) != 1 {
		t.Fatalf("the right token answered %d, kept %d senders", got, len(r.st.senders))
	}
}

func TestFibreReceiverAllowWithoutTokenStillChecksToken(t *testing.T) {
	r := newFibreRig(t)
	r.c.FibreMetrics = FibreMetricsConfig{Listen: "0.0.0.0:4318", Token: "secret", AllowWithoutToken: true}
	server := fibreServer(t, r.c)
	body := fibreFixture(t, "export1.pb")
	if got := post(t, server.URL+"/v1/metrics", bytes.NewReader(body), protobufHeader).StatusCode; got != http.StatusUnauthorized {
		t.Fatalf("without a token answered %d, want 401", got)
	}
	headers := map[string]string{"Content-Type": "application/x-protobuf", "Authorization": "Bearer secret"}
	if got := post(t, server.URL+"/v1/metrics", bytes.NewReader(body), headers).StatusCode; got != http.StatusOK {
		t.Fatalf("with the token answered %d, want 200", got)
	}
}

func TestFibreReceiverSizeLimits(t *testing.T) {
	r := newFibreRig(t)
	r.cc.Alerts.FibreInstance = "fibre-host-1"
	server := fibreServer(t, r.c)
	url := server.URL + "/v1/metrics"
	gzipped := map[string]string{"Content-Type": "application/x-protobuf", "Content-Encoding": "gzip"}

	// one byte over as received, with a Content-Length
	if got := post(t, url, bytes.NewReader(make([]byte, fibreMaxBody+1)), protobufHeader).StatusCode; got != 413 {
		t.Errorf("a body over 1 MiB answered %d, want 413", got)
	}
	// and with no Content-Length at all, a chunked body can't get past the reader's limit
	chunked := io.MultiReader(bytes.NewReader(make([]byte, fibreMaxBody+1)))
	if got := post(t, url, chunked, protobufHeader).StatusCode; got != 413 {
		t.Errorf("a chunked body over 1 MiB answered %d, want 413", got)
	}
	// a small gzip that inflates past 4 MiB
	bomb := gz(t, make([]byte, fibreMaxUnzipped+1))
	if len(bomb) > fibreMaxBody {
		t.Fatalf("the test's gzip is %d bytes", len(bomb))
	}
	if got := post(t, url, bytes.NewReader(bomb), gzipped).StatusCode; got != 413 {
		t.Errorf("a body over 4 MiB unzipped answered %d, want 413", got)
	}
	// exactly at the limit isn't over it: zeros aren't a valid request, but the size gate lets it through to the parser
	atLimit := gz(t, make([]byte, fibreMaxUnzipped))
	if got := post(t, url, bytes.NewReader(atLimit), gzipped).StatusCode; got == 413 {
		t.Errorf("a body of exactly 4 MiB unzipped was refused as too big")
	}
	// gzip bigger than 1 MiB as received, however it would unzip
	noise := make([]byte, fibreMaxBody+1024)
	if _, err := rand.Read(noise); err != nil {
		t.Fatal(err)
	}
	if got := post(t, url, io.MultiReader(bytes.NewReader(gz(t, noise))), gzipped).StatusCode; got != 413 {
		t.Errorf("a gzip body over 1 MiB as received answered %d, want 413", got)
	}
}

func TestFibreOversizeMessageAndRateLimit(t *testing.T) {
	for name, tc := range map[string]struct {
		size     int64
		unzipped bool
		want     string
	}{
		"known size":   {1363149, false, "Fibre export of 1.3 MiB is over the 1 MiB limit"},
		"unknown size": {-1, false, "Fibre export is over the 1 MiB limit"},
		"unzipped":     {-1, true, "Fibre export is over the 4 MiB limit once decompressed"},
	} {
		if got := fibreOversizeMessage(tc.size, tc.unzipped); got != tc.want {
			t.Errorf("%s: %q, want %q", name, got, tc.want)
		}
	}
	st := newFibreStore()
	now := time.Unix(1000, 0)
	st.now = func() time.Time { return now }
	if !st.noteOversized("10.0.0.1") || st.noteOversized("10.0.0.1") {
		t.Fatal("a sender is logged once, then not again")
	}
	if !st.noteOversized("10.0.0.2") {
		t.Error("another sender is logged on its own")
	}
	now = now.Add(61 * time.Minute)
	if !st.noteOversized("10.0.0.1") {
		t.Error("a sender is logged again after an hour")
	}
	for i := 0; i < 2*fibreMaxIgnored; i++ {
		st.noteOversized(fmt.Sprintf("sender-%d", i))
	}
	if len(st.oversized) > fibreMaxIgnored {
		t.Errorf("%d senders remembered, cap is %d", len(st.oversized), fibreMaxIgnored)
	}
}

func selfSigned(t *testing.T) (certFile, keyFile string, pool *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "tenderduty-test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile, keyFile = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	pool = x509.NewCertPool()
	pool.AddCert(cert)
	return
}

func TestFibreReceiverServesHTTPSAndStops(t *testing.T) {
	r := newFibreRig(t)
	r.cc.Alerts.FibreInstance = "fibre-host-1"
	certFile, keyFile, pool := selfSigned(t)
	r.c.FibreMetrics.TLSCert, r.c.FibreMetrics.TLSKey = certFile, keyFile
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.c.serveFibreMetricsOn(ctx, listener) }()
	url := "https://" + listener.Addr().String() + "/v1/metrics"

	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(fibreFixture(t, "export1.pb")))
	req.Header.Set("Content-Type", "application/x-protobuf")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || resp.TLS == nil || !r.st.snapshot("Chain").Found {
		t.Fatalf("https answered %d, tls %v", resp.StatusCode, resp.TLS != nil)
	}
	// plain HTTP to the TLS port doesn't get metrics in
	insecure, err := http.Post(strings.Replace(url, "https", "http", 1), "application/x-protobuf", bytes.NewReader(fibreFixture(t, "export1.pb")))
	if err == nil {
		_ = insecure.Body.Close()
		if insecure.StatusCode == 200 {
			t.Error("plain HTTP was accepted on the TLS port")
		}
	}
	// nothing older than TLS 1.2
	old := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MaxVersion: tls.VersionTLS11}}}
	if resp, err := old.Post(url, "application/x-protobuf", nil); err == nil {
		_ = resp.Body.Close()
		t.Error("a TLS 1.1 client got through")
	}
	if server := r.c.fibreMetricsServer(); server.TLSConfig == nil || server.TLSConfig.MinVersion != tls.VersionTLS12 ||
		server.ReadHeaderTimeout != 10*time.Second || server.ReadTimeout != 30*time.Second || server.WriteTimeout != 30*time.Second || server.IdleTimeout != 60*time.Second {
		t.Errorf("server settings: %+v", server)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("stopped with %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the receiver did not stop with its context")
	}
}

func TestFibreReceiverServesPlainHTTP(t *testing.T) {
	r := newFibreRig(t)
	r.cc.Alerts.FibreInstance = "fibre-host-1"
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.c.serveFibreMetricsOn(ctx, listener) }()
	resp := post(t, "http://"+listener.Addr().String()+"/v1/metrics", bytes.NewReader(fibreFixture(t, "export1.pb")), protobufHeader)
	if resp.StatusCode != 200 || !r.st.snapshot("Chain").Found {
		t.Fatalf("http answered %d", resp.StatusCode)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("stopped with %v", err)
	}
	// a listen address that is taken is an error, not a silent no-op
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = taken.Close() }()
	r.c.FibreMetrics.Listen = taken.Addr().String()
	if err := r.c.serveFibreMetrics(context.Background()); err == nil {
		t.Fatal("listening on a port that is taken worked")
	}
}

// fibre_enabled can sit in alert_defaults for every chain; one without x/valaddr never reads "silent"
func TestNoMetricsAlarmsOnAChainWithoutFibre(t *testing.T) {
	r := newFibreRig(t)
	r.cc.fibreLoaded, r.cc.fibreUnsupported = false, true
	alarms.AllAlarms["Chain"] = map[string]time.Time{r.msg(kindSignFailed): time.Now()}
	r.advance(time.Hour)
	sent := r.alarms()
	if len(sent) != 1 || !sent[0].resolved || sent[0].message != r.msg(kindSignFailed) {
		t.Fatalf("a chain without Fibre sent %s", describe(sent))
	}
	if _, label := sent[0].header(); label != "No longer watched" {
		t.Fatalf("the restored alarm cleared as %q", label)
	}
	r.quiet(r.alarms(), "nothing more for a chain without Fibre")
}

// silence waits for the provider check: a small node_down_alert_minutes must not beat it
func TestSilentWaitsForStageOne(t *testing.T) {
	r := newFibreRig(t)
	r.cc.fibreLoaded = false
	r.advance(10 * time.Minute)
	r.quiet(r.alarms(), "silent before stage 1 has looked")
	r.cc.fibreLoaded = true
	r.raised(r.alarms(), kindSilent, "no metrics since tenderduty started", "🔇", "Fibre silent")
}

func TestFibreSilentAlarm(t *testing.T) {
	r := newFibreRig(t)
	r.cc.Alerts.FibreInstance = "fibre-host-1"
	r.advance(2 * time.Minute)
	r.quiet(r.alarms(), "two minutes after start")
	// nothing has arrived since tenderduty started
	r.advance(time.Minute)
	if got, want := r.msg(kindSilent), "Fibre has sent no metrics for over 3 minutes on celestia"; got != want {
		t.Fatalf("message %q, want %q", got, want)
	}
	r.raised(r.alarms(), kindSilent, "no metrics since tenderduty started", "🔇", "Fibre silent")
	r.quiet(r.alarms(), "the alarm is raised once")

	f := r.feed(time.Minute)
	f.send()
	r.cleared(r.alarms(), kindSilent, "Fibre sending", "Fibre metrics are arriving again on celestia")

	r.advance(2 * time.Minute)
	r.quiet(r.alarms(), "two minutes since the last export")
	r.advance(time.Minute)
	r.raised(r.alarms(), kindSilent, "last metrics 3m ago", "🔇", "Fibre silent")
	// silence holds everything else: no window rule raises or clears on stale numbers
	r.advance(time.Hour)
	if sent := r.alarms(); len(sent) != 0 {
		t.Fatalf("an hour of silence sent %s", describe(sent))
	}
	if got := (&alertMsg{chain: "Chain", message: r.msg(kindSilent)}).summary(); got != r.msg(kindSilent)+"\nlast metrics 1h 3m ago" {
		t.Errorf("summary is %q", got)
	}
	// one minute is a singular
	r.c.NodeDownMin = 1
	if got := r.msg(kindSilent); got != "Fibre has sent no metrics for over 1 minute on celestia" {
		t.Errorf("message %q", got)
	}
}

const storageSecondLine = "The budget is set by the chain from your stake, the whole network fills together"

func TestFibreStorageIsOffByDefault(t *testing.T) {
	r := newFibreRig(t)
	r.cc.Alerts.FibreInstance = "fibre-host-1"
	for _, kind := range r.cc.fibreMetricsKindsOn() {
		if kind == kindStorage {
			t.Fatal("the storage full alert is on without fibre_storage_percent")
		}
	}
	f := r.feed(time.Minute)
	f.occupancy, f.budget, f.rejected = i64(990), i64(1000), 40
	f.run(1)
	r.quiet(r.alarms(), "99% used and uploads turned away, with the alert off")
	// an alarm raised before it was turned off clears as no longer watched, it didn't get better
	alarms.AllAlarms["Chain"] = map[string]time.Time{r.msg(kindStorage): time.Now()}
	sent := r.alarms()
	if len(sent) != 1 || !sent[0].resolved || sent[0].message != r.msg(kindStorage) {
		t.Fatalf("turning the alert off sent %s", describe(sent))
	}
	if _, label := sent[0].header(); label != "No longer watched" {
		t.Fatalf("the alarm cleared as %q", label)
	}
}

func TestFibreStorageAlarm(t *testing.T) {
	r := newFibreRig(t)
	r.cc.Alerts.FibreInstance = "fibre-host-1"
	r.cc.Alerts.FibreStoragePercent = 90
	f := r.feed(time.Minute)
	f.occupancy, f.budget = i64(899_999_999), i64(1_000_000_000)
	f.run(1)
	r.quiet(r.alarms(), "just under 90%")

	f.occupancy = i64(900_000_000)
	f.run(1)
	if got, want := r.msg(kindStorage), "Fibre storage is nearly full on celestia, the budget the chain gives this validator is almost used up"; got != want {
		t.Fatalf("message %q, want %q", got, want)
	}
	r.raised(r.alarms(), kindStorage, "900 MB of 1 GB, 0 uploads turned away in the last 10 minutes. "+storageSecondLine, "🗄️", "Fibre storage full")

	// between 75% and 90% it stays up however long it has been quiet, so a network hovering at the limit sends nothing
	f.occupancy = i64(800_000_000)
	f.run(45)
	r.quiet(r.alarms(), "80% of the budget")
	f.occupancy = i64(750_000_000)
	f.run(1)
	r.quiet(r.alarms(), "exactly 75%")
	if !r.active(r.msg(kindStorage)) {
		t.Fatal("cleared at 75%, it clears only below 75%")
	}
	f.occupancy = i64(900_000_000)
	f.run(1)
	r.quiet(r.alarms(), "back over 90%, already raised")
	f.occupancy = i64(749_999_999)
	f.run(1)
	r.cleared(r.alarms(), kindStorage, "Storage OK", "Fibre storage is back under 75% of its budget on celestia, no uploads turned away for 30 minutes")

	// a different threshold
	r.cc.Alerts.FibreStoragePercent = 50
	f.occupancy, f.budget = i64(1_500_000_000), i64(3_000_000_000)
	f.run(1)
	r.raised(r.alarms(), kindStorage, "1.5 GB of 3 GB, 0 uploads turned away in the last 10 minutes. "+storageSecondLine, "🗄️", "Fibre storage full")
	f.occupancy = i64(1_049_999_999)
	f.run(1)
	r.cleared(r.alarms(), kindStorage, "Storage OK", "Fibre storage is back under 35% of its budget on celestia, no uploads turned away for 30 minutes")
}

func TestFibreStorageBudgetZeroNeverRaises(t *testing.T) {
	r := newFibreRig(t)
	r.cc.Alerts.FibreInstance = "fibre-host-1"
	r.cc.Alerts.FibreStoragePercent = 90
	f := r.feed(time.Minute)
	// 0 is the limiter being off (--unlimited-budget), never a full disk
	f.occupancy, f.budget = i64(5_000_000_000_000), i64(0)
	f.run(40)
	r.quiet(r.alarms(), "budget 0")

	// the limiter being switched off clears a raised alarm, as nothing was turned away for 30 watched minutes
	f.occupancy, f.budget = i64(950), i64(1000)
	f.run(1)
	r.raised(r.alarms(), kindStorage, "950 B of 1 KB, 0 uploads turned away in the last 10 minutes. "+storageSecondLine, "🗄️", "Fibre storage full")
	f.budget = i64(0)
	f.run(1)
	r.cleared(r.alarms(), kindStorage, "Storage OK", "Fibre storage is back under 75% of its budget on celestia, no uploads turned away for 30 minutes")
}

func TestFibreStorageRejections(t *testing.T) {
	r := newFibreRig(t)
	r.cc.Alerts.FibreInstance = "fibre-host-1"
	r.cc.Alerts.FibreStoragePercent = 90
	f := r.feed(time.Minute)
	f.occupancy, f.budget = i64(100_000_000), i64(1_000_000_000)
	f.run(1)
	r.quiet(r.alarms(), "10% used")

	// an upload turned away raises it however empty the budget looks
	f.rejected = 3
	f.run(1)
	r.raised(r.alarms(), kindStorage, "100 MB of 1 GB, 3 uploads turned away in the last 10 minutes. "+storageSecondLine, "🗄️", "Fibre storage full")
	// out of the last 10 minutes, still inside the 30 it takes to clear
	f.run(29)
	r.quiet(r.alarms(), "29 minutes after the rejection")
	if !r.active(r.msg(kindStorage)) {
		t.Fatal("cleared before 30 minutes without a rejection")
	}
	f.run(1)
	r.cleared(r.alarms(), kindStorage, "Storage OK", "Fibre storage is back under 75% of its budget on celestia, no uploads turned away for 30 minutes")

	// a rejection while it is up starts the 30 minutes again
	f.rejected = 4
	f.run(1)
	r.raised(r.alarms(), kindStorage, "100 MB of 1 GB, 1 upload turned away in the last 10 minutes. "+storageSecondLine, "🗄️", "Fibre storage full")
	f.run(20)
	f.rejected = 5
	f.run(1)
	r.quiet(r.alarms(), "another rejection")
	f.run(29)
	r.quiet(r.alarms(), "29 minutes after the second rejection")
	f.run(1)
	r.cleared(r.alarms(), kindStorage, "Storage OK", "Fibre storage is back under 75% of its budget on celestia, no uploads turned away for 30 minutes")
}

func TestFibreStorageWithoutGauges(t *testing.T) {
	r := newFibreRig(t)
	r.cc.Alerts.FibreInstance = "fibre-host-1"
	r.cc.Alerts.FibreStoragePercent = 90
	f := r.feed(time.Minute)
	f.rejected = 2
	f.run(1)
	r.raised(r.alarms(), kindStorage, "2 uploads turned away in the last 10 minutes. "+storageSecondLine, "🗄️", "Fibre storage full")
	// nothing says how full it is, so a quiet spell isn't enough
	f.run(60)
	r.quiet(r.alarms(), "no occupancy gauge")
	if !r.active(r.msg(kindStorage)) {
		t.Fatal("cleared without knowing how full the budget is")
	}
}

func TestFibreStorageLow(t *testing.T) {
	for percent, want := range map[int]int{100: 85, 90: 75, 16: 1, 15: 15, 10: 10, 1: 1} {
		cc := &ChainConfig{}
		cc.Alerts.FibreStoragePercent = percent
		if got := cc.fibreStorageLow(); got != want {
			t.Errorf("at %d%% it clears under %d%%, want %d%%", percent, got, want)
		}
	}
}

// a sender that exports a few signatures an hour, and one that exports a lot a minute
func quietFeed(t *testing.T) (*fibreRig, *fibreFeed) {
	t.Helper()
	r := newFibreRig(t)
	r.cc.Alerts.FibreInstance = "fibre-host-1"
	return r, r.feed(time.Minute)
}

func TestFibreAdaptiveWindow(t *testing.T) {
	t.Run("a quiet sender is judged on its last 20 samples", func(t *testing.T) {
		r, f := quietFeed(t)
		f.signFast = 3
		f.send()
		for i := 0; i < 9; i++ {
			r.advance(time.Hour)
			f.signFast += 3
			f.send()
		}
		// ten exports of 3 an hour apart: the newest seven are the first to reach 20
		span := r.st.snapshot("Chain").Sign
		if span.Sum.signTotal != 21 || !span.Far || span.Reach != 6*time.Hour || span.text() != "over the last 6h 0m" {
			t.Fatalf("span %+v %q", span, span.text())
		}
		if w := r.st.snapshot("Chain").Window; w.signTotal != 3 {
			t.Fatalf("the 10 minute window holds %d signatures, want 3", w.signTotal)
		}
	})
	t.Run("a busy sender only on its last 10 minutes", func(t *testing.T) {
		r, f := quietFeed(t)
		for i := 0; i < 60; i++ {
			r.advance(time.Minute)
			f.signFast += 1000
			f.send()
		}
		snap := r.st.snapshot("Chain")
		if snap.Sign.Sum.signTotal != 10_000 || snap.Sign.Far || snap.Sign.text() != "in the last 10 minutes" || snap.Window.signTotal != 10_000 {
			t.Fatalf("span %+v %q, window %+v", snap.Sign, snap.Sign.text(), snap.Window)
		}
	})
	t.Run("each rule counts its own samples", func(t *testing.T) {
		r, f := quietFeed(t)
		// 4 failed signatures an hour for 10 hours: the signature rule needs 5 exports, the slow rule has no samples at all
		for i := 0; i < 10; i++ {
			f.signFailed += 4
			f.send()
			r.advance(time.Hour)
		}
		snap := r.st.snapshot("Chain")
		if snap.Sign.Sum.signTotal != 20 || snap.SignOK.Sum.signOK != 0 || !snap.SignOK.Far {
			t.Fatalf("signatures %+v, successful %+v", snap.Sign, snap.SignOK)
		}
	})
	t.Run("nothing older than 24 hours counts", func(t *testing.T) {
		r, f := quietFeed(t)
		f.signSlow = 5
		f.send()
		r.advance(24*time.Hour - time.Minute)
		if span := r.st.snapshot("Chain").Sign; span.Sum.signTotal != 5 || !span.Far {
			t.Fatalf("23h59m on: %+v", span)
		}
		r.advance(time.Minute)
		if span := r.st.snapshot("Chain").Sign; span.Sum.signTotal != 0 || span.Reach != 0 {
			t.Fatalf("24h on: %+v", span)
		}
	})
	t.Run("an export drops what is older than 24 hours", func(t *testing.T) {
		r, f := quietFeed(t)
		f.signFast = 5
		f.send()
		r.advance(25 * time.Hour)
		f.signFast = 7
		f.send()
		if window := r.st.senders["fibre-host-1"].window; len(window) != 1 || window[0].signTotal != 2 {
			t.Fatalf("history is %+v", window)
		}
	})
	t.Run("the history is capped by merging the older half", func(t *testing.T) {
		r, f := quietFeed(t)
		for i := 0; i < fibreMaxDeltas+100; i++ {
			r.advance(time.Second)
			f.signFast++
			f.send()
		}
		window := r.st.senders["fibre-host-1"].window
		total := int64(0)
		for i, d := range window {
			total += d.signTotal
			if i > 0 && !d.at.After(window[i-1].at) {
				t.Fatalf("delta %d at %v is not after %v", i, d.at, window[i-1].at)
			}
		}
		if len(window) > fibreMaxDeltas || !window[len(window)-1].at.Equal(r.clock.now) || total != fibreMaxDeltas+100 {
			t.Fatalf("%d deltas holding %d signatures, the newest at %v, now %v", len(window), total, window[len(window)-1].at, r.clock.now)
		}
		if span := r.st.snapshot("Chain").Sign; span.Sum.signTotal != 600 {
			t.Fatalf("the newest 10 minutes hold %d signatures, want 600", span.Sum.signTotal)
		}
	})
}

// the sent alerts of one kind
func (r *fibreRig) only(sent []*alertMsg, kind string) []*alertMsg {
	var out []*alertMsg
	for _, a := range sent {
		if fibreMetricsKind(a.message) == kind {
			out = append(out, a)
		}
	}
	return out
}

func TestFibreSignFailedAlarm(t *testing.T) {
	t.Run("a single failure on a quiet network raises and stays until it falls out of the window", func(t *testing.T) {
		r, f := quietFeed(t)
		f.signFast, f.signFailed = 3, 1
		f.send()
		if got := r.msg(kindSignFailed); got != "Fibre could not sign payment promises on celestia" {
			t.Fatalf("message %q", got)
		}
		r.raised(r.alarms(), kindSignFailed, "1 of 4 signatures failed in the last 10 minutes", "✍️", "Signing failed")
		r.quiet(r.alarms(), "raised once")
		// 19 newer signatures an hour apart are not enough to push it out
		for i := 0; i < 19; i++ {
			r.advance(30 * time.Minute)
			f.signFast++
			f.send()
		}
		r.quiet(r.alarms(), "19 newer signatures")
		if !r.active(r.msg(kindSignFailed)) {
			t.Fatal("cleared while the failure was still in the window")
		}
		if got := (&alertMsg{chain: "Chain", message: r.msg(kindSignFailed)}).summary(); got != r.msg(kindSignFailed)+"\n1 of 23 signatures failed over the last 9h 30m" {
			t.Fatalf("summary is %q", got)
		}
		r.advance(30 * time.Minute)
		f.signFast++
		f.send()
		r.cleared(r.alarms(), kindSignFailed, "Signing OK", "Fibre is signing payment promises again on celestia")
	})
	t.Run("with nothing newer it stays for 24 hours", func(t *testing.T) {
		r, f := quietFeed(t)
		f.signFailed = 1
		f.send()
		r.raised(r.alarms(), kindSignFailed, "1 of 1 signatures failed in the last 10 minutes", "✍️", "Signing failed")
		f.run(24*60 - 1)
		r.quiet(r.alarms(), "23h59m on")
		if !r.active(r.msg(kindSignFailed)) {
			t.Fatal("cleared before the failure was a day old")
		}
		f.run(1)
		r.cleared(r.alarms(), kindSignFailed, "Signing OK", "Fibre is signing payment promises again on celestia")
	})
	t.Run("on a busy network one in 10 000 doesn't raise, and 1% isn't over 1%", func(t *testing.T) {
		r, f := quietFeed(t)
		f.signFast, f.signFailed = 9999, 1
		f.send()
		r.quiet(r.alarms(), "1 in 10 000")
		r, f = quietFeed(t)
		f.signFast, f.signFailed = 9900, 100
		f.send()
		r.quiet(r.alarms(), "exactly 1%")
	})
	t.Run("on a busy network 2% raises and it clears when the failures leave the last 10 minutes", func(t *testing.T) {
		r, f := quietFeed(t)
		f.signFast, f.signFailed = 9800, 200
		f.send()
		r.raised(r.alarms(), kindSignFailed, "200 of 10000 signatures failed in the last 10 minutes", "✍️", "Signing failed")
		for i := 0; i < 9; i++ {
			f.signFast += 1000
			f.run(1)
		}
		r.quiet(r.alarms(), "the failures are still in the window")
		f.signFast += 1000
		f.run(1)
		r.cleared(r.alarms(), kindSignFailed, "Signing OK", "Fibre is signing payment promises again on celestia")
	})
	t.Run("a failure that is under 1% of the window doesn't raise", func(t *testing.T) {
		r, f := quietFeed(t)
		f.signFast, f.signFailed = 149, 1
		f.send()
		r.quiet(r.alarms(), "0.67%")
	})
}

func TestFibreSigningAlarm(t *testing.T) {
	t.Run("holds under 20 successful signatures", func(t *testing.T) {
		r, f := quietFeed(t)
		// failed signatures aren't successful ones
		f.signSlow, f.signFailed = 19, 500
		f.send()
		if sent := r.only(r.alarms(), kindSigning); len(sent) != 0 {
			t.Fatalf("19 slow signatures sent %s", describe(sent))
		}
		if got := r.msg(kindSigning); got != "Fibre signing is slower than 10ms on celestia" {
			t.Fatalf("message %q", got)
		}
		f.signSlow = 20
		f.run(1)
		r.raised(r.only(r.alarms(), kindSigning), kindSigning, "20 of 20 signatures took over 10ms in the last 10 minutes", "🐢", "Signing slow")
		// fewer than 20 successful ones never clears it, however good they are
		f.run(15)
		f.signFast = 5
		f.run(1)
		if sent := r.only(r.alarms(), kindSigning); len(sent) != 0 || !r.active(r.msg(kindSigning)) {
			t.Fatalf("5 fast signatures changed the alarm: %s", describe(sent))
		}
	})
	t.Run("half at 10ms is fine, a share over half slow is not", func(t *testing.T) {
		r, f := quietFeed(t)
		f.signFast, f.signSlow = 15, 15
		f.send()
		r.quiet(r.alarms(), "half at 10ms")
		f.signSlow = 31
		f.run(1)
		r.raised(r.alarms(), kindSigning, "31 of 46 signatures took over 10ms in the last 10 minutes", "🐢", "Signing slow")
		f.signFast = 200
		f.run(1)
		r.cleared(r.alarms(), kindSigning, "Signing OK", "Fibre signing is back under 10ms on celestia")
	})
	t.Run("only successful signatures are counted", func(t *testing.T) {
		r, f := quietFeed(t)
		f.signFast, f.signSlow, f.signFailed = 5000, 5001, 50
		f.send()
		sent := r.alarms()
		r.raised(r.only(sent, kindSigning), kindSigning, "5001 of 10001 signatures took over 10ms in the last 10 minutes", "🐢", "Signing slow")
		if len(sent) != 1 {
			t.Fatalf("0.5%% failed signatures raised more: %s", describe(sent))
		}
	})
	t.Run("a quiet sender is judged on its last 20 successful signatures", func(t *testing.T) {
		r, f := quietFeed(t)
		// 5 slow signatures an hour, the 20 it takes come from four exports
		for i := 0; i < 3; i++ {
			f.signSlow += 5
			f.send()
			r.quiet(r.alarms(), "fewer than 20 signatures")
			r.advance(time.Hour)
		}
		f.signSlow += 5
		f.send()
		r.raised(r.alarms(), kindSigning, "20 of 20 signatures took over 10ms over the last 3h 0m", "🐢", "Signing slow")
		// 5 fast ones leave 15 slow in the last 20
		r.advance(time.Hour)
		f.signFast += 5
		f.send()
		r.quiet(r.alarms(), "5 fast of 20")
		if !r.active(r.msg(kindSigning)) {
			t.Fatal("cleared with a quarter of the last 20 fast")
		}
		// 10 fast and 10 slow is half
		r.advance(time.Hour)
		f.signFast += 5
		f.send()
		r.cleared(r.alarms(), kindSigning, "Signing OK", "Fibre signing is back under 10ms on celestia")
	})
	t.Run("a busy sender is judged on its last 10 minutes", func(t *testing.T) {
		r, f := quietFeed(t)
		f.signSlow = 5000
		f.send()
		r.raised(r.alarms(), kindSigning, "5000 of 5000 signatures took over 10ms in the last 10 minutes", "🐢", "Signing slow")
		for i := 0; i < 4; i++ {
			f.signFast += 1000
			f.run(1)
		}
		r.quiet(r.alarms(), "the slow minute is still in the window")
		f.signFast += 1000
		f.run(1)
		r.cleared(r.alarms(), kindSigning, "Signing OK", "Fibre signing is back under 10ms on celestia")
	})
}

func TestFibreObjectStorageAlarm(t *testing.T) {
	t.Run("a quiet sender is judged on the few requests it has", func(t *testing.T) {
		r, f := quietFeed(t)
		f.backendOK, f.timeout = 2, 1
		f.send()
		if got := r.msg(kindObject); got != "Fibre object storage requests are failing on celestia" {
			t.Fatalf("message %q", got)
		}
		r.raised(r.alarms(), kindObject, "timeout 1, throttled 0, error 0 of 3 requests in the last 10 minutes", "🚨", "Object storage errors")
	})
	t.Run("one failure in 10 000 requests doesn't raise", func(t *testing.T) {
		r, f := quietFeed(t)
		f.backendOK, f.err = 9999, 1
		f.send()
		r.quiet(r.alarms(), "1 in 10 000")
	})
	t.Run("5% raises and it clears when the failures leave the window", func(t *testing.T) {
		r, f := quietFeed(t)
		f.backendOK, f.timeout, f.throttled, f.err = 9500, 300, 100, 100
		f.send()
		r.raised(r.alarms(), kindObject, "timeout 300, throttled 100, error 100 of 10000 requests in the last 10 minutes", "🚨", "Object storage errors")
		for i := 0; i < 9; i++ {
			f.backendOK += 1000
			f.run(1)
		}
		r.quiet(r.alarms(), "the failures are still in the window")
		f.backendOK += 1000
		f.run(1)
		r.cleared(r.alarms(), kindObject, "Object storage OK", "Fibre object storage requests are succeeding again on celestia")
	})
	t.Run("every outcome is a request", func(t *testing.T) {
		// 11 failures: over 1% of 1011 requests, under 1% of 1111
		r, f := quietFeed(t)
		f.ignoredOutcomes, f.timeout = 1000, 11
		f.send()
		r.raised(r.alarms(), kindObject, "timeout 11, throttled 0, error 0 of 1011 requests in the last 10 minutes", "🚨", "Object storage errors")
		r, f = quietFeed(t)
		f.ignoredOutcomes, f.timeout = 1100, 11
		f.send()
		r.quiet(r.alarms(), "11 of 1111")
		r, f = quietFeed(t)
		f.backendOK, f.timeout = 1100, 11
		f.send()
		r.quiet(r.alarms(), "11 of 1111 with successes")
	})
}

func TestHumanBytes(t *testing.T) {
	for n, want := range map[int64]string{0: "0 B", 999: "999 B", 1000: "1 KB", 1500: "1.5 KB", 900_000_000: "900 MB", 1_000_000_000: "1 GB", 1_234_000_000: "1.2 GB", 2_500_000_000_000: "2.5 TB", 5_000_000_000_000_000: "5000 TB"} {
		if got := humanBytes(n); got != want {
			t.Errorf("%d is %q, want %q", n, got, want)
		}
	}
}

func TestFibreStorageErrorsAlarm(t *testing.T) {
	r := newFibreRig(t)
	r.cc.Alerts.FibreInstance = "fibre-host-1"
	f := r.feed(time.Hour)
	f.signFast = 50
	f.send()
	f.run(1)
	r.quiet(r.alarms(), "healthy")
	f.put, f.get, f.prune = 1, 2, 0
	f.run(1)
	if got := r.msg(kindStorageErrors); got != "Fibre storage operations are failing on celestia" {
		t.Fatalf("message %q", got)
	}
	r.raised(r.alarms(), kindStorageErrors, "put 1, get 2, prune 0 failed in the last 10 minutes", "🚨", "Storage errors")
	f.run(11)
	r.cleared(r.alarms(), kindStorageErrors, "Storage OK", "Fibre storage operations are succeeding again on celestia")

	// a prune failure alone raises it
	r = newFibreRig(t)
	r.cc.Alerts.FibreInstance = "fibre-host-1"
	f = r.feed(time.Minute)
	f.prune = 1
	f.send()
	r.raised(r.alarms(), kindStorageErrors, "put 0, get 0, prune 1 failed in the last 10 minutes", "🚨", "Storage errors")
}

func TestFibreIdleAlarm(t *testing.T) {
	r := newFibreRig(t)
	r.cc.Alerts.FibreInstance = "fibre-host-1"
	// off by default
	f := r.feed(time.Minute)
	stale := r.clock.now.Add(-5 * time.Hour).Unix()
	f.lastSuccess = i64(stale)
	f.run(1)
	r.quiet(r.alarms(), "idle alerts off")

	r.cc.Alerts.FibreIdleMinutes = 30
	stale = r.clock.now.Add(-30 * time.Minute).Unix()
	f.lastSuccess = i64(stale)
	f.run(1) // 31 minutes now
	if got := r.msg(kindIdle); got != "Fibre has had no successful upload for over 30 minutes on celestia" {
		t.Fatalf("message %q", got)
	}
	r.raised(r.alarms(), kindIdle, "last successful upload 31m ago", "💤", "No uploads")
	f.lastSuccess = i64(r.clock.now.Unix())
	f.run(1)
	r.cleared(r.alarms(), kindIdle, "Uploads arriving", "Fibre stored an upload again on celestia")

	// no timestamp seen yet is not a reason to alert
	r = newFibreRig(t)
	r.cc.Alerts.FibreInstance = "fibre-host-1"
	r.cc.Alerts.FibreIdleMinutes = 30
	f = r.feed(time.Minute)
	f.signFast = 5
	f.run(40)
	r.quiet(r.alarms(), "no last_success_timestamp")
}

// alarms are restored from the state file, metrics are not: nothing clears until this process has seen evidence
func TestRestartKeepsFibreMetricsAlarmsUntilThereIsEvidence(t *testing.T) {
	r := newFibreRig(t)
	r.cc.Alerts.FibreInstance = "fibre-host-1"
	r.cc.Alerts.FibreStoragePercent, r.cc.Alerts.FibreIdleMinutes = 90, 30
	all := r.cc.fibreMetricsKindsOn()
	if len(all) != 7 {
		t.Fatalf("kinds on: %v", all)
	}
	restored := make(map[string]time.Time)
	for _, kind := range all {
		restored[r.msg(kind)] = r.clock.now.Add(-time.Hour)
	}
	alarms.AllAlarms["Chain"] = restored
	stays := func(when string, kinds ...string) {
		t.Helper()
		for _, kind := range kinds {
			if !r.active(r.msg(kind)) {
				t.Fatalf("%s: the %s alarm cleared without evidence", when, kind)
			}
		}
	}
	clearedKinds := func(when string, sent []*alertMsg) []string {
		t.Helper()
		var kinds []string
		for _, a := range sent {
			if !a.resolved {
				t.Fatalf("%s: raised %q", when, a.message)
			}
			kinds = append(kinds, fibreMetricsKind(a.message))
		}
		sort.Strings(kinds)
		return kinds
	}
	// Fibre exports a fresh last-success time with every export, so the idle alarm has nothing to say
	f := r.feed(48 * time.Hour)
	tick := func(n int) {
		for i := 0; i < n; i++ {
			f.lastSuccess = i64(r.clock.now.Add(time.Minute).Unix())
			f.run(1)
		}
	}

	// nothing at all has arrived yet
	r.quiet(r.alarms(), "at start")
	stays("at start", all...)
	r.advance(2 * time.Minute)
	r.quiet(r.alarms(), "two minutes in")
	stays("two minutes in", all...)

	// the first export is healthy, and its counters are a baseline. Silent and idle have what they need, the rest
	// hasn't been seen long enough or has nothing counted
	f.signFast, f.occupancy, f.budget = 3000, i64(100), i64(1000)
	f.lastSuccess = i64(r.clock.now.Unix())
	f.send()
	if got := clearedKinds("the first export", r.alarms()); strings.Join(got, ",") != kindIdle+","+kindSilent {
		t.Fatalf("the first export cleared %v, want idle and silent", got)
	}
	stays("after one export", kindStorage, kindSignFailed, kindSigning, kindStorageErrors, kindObject)

	// a few minutes of quiet isn't the whole 10 minutes
	tick(9)
	r.quiet(r.alarms(), "nine minutes in")
	stays("nine minutes in", kindStorage, kindSignFailed, kindSigning, kindStorageErrors, kindObject)
	// 10 minutes watched with no disk errors
	tick(1)
	if got := clearedKinds("ten minutes in", r.alarms()); strings.Join(got, ",") != kindStorageErrors {
		t.Fatalf("ten minutes cleared %v, want storage errors", got)
	}
	// storage waits for 30 watched minutes with no rejection
	tick(19)
	r.quiet(r.alarms(), "29 minutes in")
	stays("29 minutes in", kindStorage, kindSignFailed, kindSigning, kindObject)
	tick(1)
	if got := clearedKinds("30 minutes in", r.alarms()); strings.Join(got, ",") != kindStorage {
		t.Fatalf("30 minutes cleared %v, want storage", got)
	}

	// the ratio rules clear on 20 samples this process saw: 10 aren't enough
	f.signFast, f.backendOK = 3010, 10
	tick(1)
	r.quiet(r.alarms(), "10 samples")
	stays("10 samples", kindSignFailed, kindSigning, kindObject)
	f.signFast, f.backendOK = 3030, 30
	tick(1)
	if got := clearedKinds("30 samples", r.alarms()); strings.Join(got, ",") != kindObject+","+kindSignFailed+","+kindSigning {
		t.Fatalf("30 samples cleared %v, want object storage, signing failed and signing slow", got)
	}
}

func TestRestoredSignFailedAlarmClearsOnEnoughGoodSignatures(t *testing.T) {
	r := newFibreRig(t)
	r.cc.Alerts.FibreInstance = "fibre-host-1"
	alarms.AllAlarms["Chain"] = map[string]time.Time{r.msg(kindSignFailed): r.clock.now.Add(-time.Hour)}
	f := r.feed(48 * time.Hour)
	f.signFast = 1000
	f.send()
	r.quiet(r.alarms(), "baseline")
	// the failure that raised it may have been before the restart, 10 good signatures don't say it is over
	f.signFast = 1010
	f.run(1)
	r.quiet(r.alarms(), "10 signatures")
	f.signFast = 1030
	f.run(1)
	r.cleared(r.alarms(), kindSignFailed, "Signing OK", "Fibre is signing payment promises again on celestia")
}

func TestRestoredSilentAlarmStaysUntilAnExportArrives(t *testing.T) {
	r := newFibreRig(t)
	r.cc.Alerts.FibreInstance = "fibre-host-1"
	alarms.AllAlarms["Chain"] = map[string]time.Time{r.msg(kindSilent): r.clock.now.Add(-time.Hour)}
	r.advance(2 * time.Minute)
	r.quiet(r.alarms(), "restored")
	if !r.active(r.msg(kindSilent)) {
		t.Fatal("a restored silent alarm cleared with no export")
	}
	r.advance(5 * time.Minute)
	r.quiet(r.alarms(), "still silent")
	if !r.active(r.msg(kindSilent)) {
		t.Fatal("a restored silent alarm cleared with no export")
	}
	// the first loop after a restart sends the restored alarms again
	r.cc.fibreMetricsAlarms(r.vi, true)
	if len(r.c.alertChan) != 1 {
		t.Fatalf("reconcile sent %d alerts", len(r.c.alertChan))
	}
	<-r.c.alertChan
	f := r.feed(time.Minute)
	f.send()
	r.cleared(r.alarms(), kindSilent, "Fibre sending", "Fibre metrics are arriving again on celestia")
}

func TestFibreMetricsMessagesAreDistinct(t *testing.T) {
	cc := &ChainConfig{ChainId: "mocha-5"}
	cc.Alerts.FibreStoragePercent, cc.Alerts.FibreIdleMinutes = 90, 30
	old := td
	td = &Config{NodeDownMin: 3}
	defer func() { td = old }()
	seen := map[string]string{}
	all := []string{kindSilent, kindStorage, kindSignFailed, kindSigning, kindStorageErrors, kindObject, kindIdle}
	for _, kind := range all {
		message := cc.fibreMetricsMessage(kind)
		if !isFibreMetricsMessage(message) || fibreMetricsKind(message) != kind {
			t.Errorf("%q is %q, want %q", message, fibreMetricsKind(message), kind)
		}
		if isFibreMessage(message) || strings.HasPrefix(message, fibreHostPrefix) || strings.HasPrefix(message, fibreNotRegisteredPrefix) {
			t.Errorf("%q looks like a stage 1 Fibre message", message)
		}
		if other, ok := seen[message]; ok {
			t.Errorf("%q is the message of %s and %s", message, other, kind)
		}
		seen[message] = kind
	}
	for _, message := range []string{fibreNotRegisteredMessage("mocha-5"), fibreDownMessage("1.2.3.4:7980", "mocha-5"), fibreIdentityMessage("1.2.3.4:7980", "mocha-5"), "Fibre at x is unreachable on y", ""} {
		if isFibreMetricsMessage(message) {
			t.Errorf("%q was taken for a metrics message", message)
		}
	}
	// a host called like a metrics message can't confuse the two
	for _, host := range []string{"storage", "has", "uploads"} {
		if isFibreMetricsMessage(fibreDownMessage(host, "mocha-5")) || isFibreMetricsMessage(fibreIdentityMessage(host, "mocha-5")) {
			t.Errorf("host %q made a stage 1 message a metrics one", host)
		}
	}
}

func TestFibreMetricsOrphans(t *testing.T) {
	setup := func(enabled bool, listen string) (*fibreRig, map[string]string) {
		r := newFibreRig(t)
		r.cc.Alerts.FibreAlerts, r.cc.Alerts.FibreIdleMinutes = enabled, 30
		r.c.FibreMetrics.Listen = listen
		restored := map[string]string{}
		alarms.AllAlarms["Chain"] = map[string]time.Time{}
		r.cc.Alerts.FibreStoragePercent = 90
		for _, kind := range []string{kindSilent, kindStorage, kindSignFailed, kindSigning, kindStorageErrors, kindObject, kindIdle} {
			alarms.AllAlarms["Chain"][r.msg(kind)] = time.Now()
			restored[r.msg(kind)] = "valcons1fibremetrics" + kind
		}
		return r, restored
	}
	drain := func(r *fibreRig) []*alertMsg {
		var sent []*alertMsg
		for len(r.c.alertChan) > 0 {
			sent = append(sent, <-r.c.alertChan)
		}
		return sent
	}
	for _, tc := range []struct {
		name    string
		enabled bool
		listen  string
	}{{"alerts off", false, "127.0.0.1:4318"}, {"no receiver", true, ""}} {
		t.Run(tc.name, func(t *testing.T) {
			r, restored := setup(tc.enabled, tc.listen)
			r.cc.clearOrphanedAlarms(r.vi, "norpc")
			sent := drain(r)
			if len(sent) != len(restored) {
				t.Fatalf("cleared %d of %d: %s", len(sent), len(restored), describe(sent))
			}
			for _, a := range sent {
				icon, label := a.header()
				if !a.resolved || a.uniqueId != restored[a.message] || icon != "ℹ️" || label != "No longer watched" ||
					!strings.HasSuffix(a.summary(), "\nThe alert was turned off or its settings changed") {
					t.Errorf("%q went out as %s %s %q id %q", a.message, icon, label, a.summary(), a.uniqueId)
				}
			}
		})
	}
	t.Run("on", func(t *testing.T) {
		r, _ := setup(true, "127.0.0.1:4318")
		r.cc.clearOrphanedAlarms(r.vi, "norpc")
		r.quiet(drain(r), "everything is still expected")
	})
	t.Run("a changed threshold keeps its alarm, it isn't in the message", func(t *testing.T) {
		r, _ := setup(true, "127.0.0.1:4318")
		r.cc.Alerts.FibreStoragePercent = 80
		r.cc.clearOrphanedAlarms(r.vi, "norpc")
		r.quiet(drain(r), "the storage alarm is still expected")
	})
	t.Run("the optional ones turned off", func(t *testing.T) {
		r, _ := setup(true, "127.0.0.1:4318")
		old := r.msg(kindStorage)
		r.cc.Alerts.FibreStoragePercent = 0
		r.cc.Alerts.FibreIdleMinutes = 0
		r.cc.clearOrphanedAlarms(r.vi, "norpc")
		sent := drain(r)
		got := map[string]bool{}
		for _, a := range sent {
			got[a.message] = a.resolved
		}
		if len(sent) != 2 || !got[old] || !got["Fibre has had no successful upload for over 30 minutes on celestia"] {
			t.Fatalf("cleared %s", describe(sent))
		}
	})
}

func TestFibreMetricsAlarmsLeaveStageOneAlone(t *testing.T) {
	r := newFibreRig(t)
	r.cc.Alerts.FibreInstance = "fibre-host-1"
	down := fibreDownMessage("1.2.3.4:7980", "celestia")
	alarms.AllAlarms["Chain"] = map[string]time.Time{down: time.Now(), fibreNotRegisteredMessage("celestia"): time.Now()}
	f := r.feed(time.Minute)
	f.signFast = 30
	f.send()
	r.quiet(r.alarms(), "healthy")
	if !r.active(down) || !r.active(fibreNotRegisteredMessage("celestia")) {
		t.Fatal("the metrics alarms resolved a stage 1 alarm")
	}
}

func fibreValidateConfig(listen, token string, chains ...[2]interface{}) *Config {
	c := &Config{NodeDownMin: 3, FibreMetrics: FibreMetricsConfig{Listen: listen, Token: token}, Chains: map[string]*ChainConfig{}}
	for i, v := range chains {
		name := string(rune('A' + i))
		cc := &ChainConfig{ChainId: name, ValAddress: "valoper", Nodes: []*NodeConfig{{Url: "http://127.0.0.1:26657"}}}
		cc.Alerts.StalledAlerts, cc.Alerts.Stalled = true, 10
		cc.Alerts.FibreAlerts, cc.Alerts.FibreInstance = v[0].(bool), v[1].(string)
		c.Chains[name] = cc
	}
	return c
}

func TestValidateFibreMetrics(t *testing.T) {
	one := [2]interface{}{true, ""}
	for _, tc := range []struct {
		name    string
		listen  string
		token   string
		chains  [][2]interface{}
		fatal   string // empty is not fatal
		warning string
	}{
		{name: "off", chains: nil},
		{name: "loopback needs no token", listen: "127.0.0.1:4318", chains: [][2]interface{}{one}},
		{name: "the whole 127 range", listen: "127.5.6.7:4318", chains: [][2]interface{}{one}},
		{name: "ipv6 loopback", listen: "[::1]:4318", chains: [][2]interface{}{one}},
		{name: "localhost", listen: "localhost:4318", chains: [][2]interface{}{one}},
		{name: "LOCALHOST", listen: "LOCALHOST:4318", chains: [][2]interface{}{one}},
		{name: "a private address with a token", listen: "10.0.0.5:4318", token: "t", chains: [][2]interface{}{one}, warning: "cleartext"},
		{name: "everywhere with a token", listen: ":4318", token: "t", chains: [][2]interface{}{one}, warning: "cleartext"},
		{name: "a private address", listen: "10.0.0.5:4318", chains: [][2]interface{}{one}, fatal: "healthy"},
		{name: "all interfaces", listen: "0.0.0.0:4318", chains: [][2]interface{}{one}, fatal: "without a token"},
		{name: "no host is every interface", listen: ":4318", chains: [][2]interface{}{one}, fatal: "without a token"},
		{name: "ipv6 any", listen: "[::]:4318", chains: [][2]interface{}{one}, fatal: "without a token"},
		{name: "a hostname", listen: "example.com:4318", chains: [][2]interface{}{one}, fatal: "without a token"},
		{name: "a host that only looks local", listen: "localhost.example.com:4318", chains: [][2]interface{}{one}, fatal: "without a token"},
		{name: "no port", listen: "127.0.0.1", chains: [][2]interface{}{one}, fatal: "not a valid host:port"},
		{name: "a port that is not a number", listen: "127.0.0.1:otlp", chains: [][2]interface{}{one}, fatal: "not a valid host:port"},
		{name: "port 0", listen: "127.0.0.1:0", chains: [][2]interface{}{one}, fatal: "not a valid host:port"},
		{name: "port too high", listen: "127.0.0.1:70000", chains: [][2]interface{}{one}, fatal: "not a valid host:port"},
		{name: "a url", listen: "http://127.0.0.1:4318", chains: [][2]interface{}{one}, fatal: "not a valid host:port"},
		{name: "no chain has Fibre alerts", listen: "127.0.0.1:4318", chains: [][2]interface{}{{false, ""}}, warning: "no chain has fibre_enabled"},
		{name: "no chains with Fibre", listen: "127.0.0.1:4318", warning: "no chain has fibre_enabled"},
		{name: "two chains, one unnamed", listen: "127.0.0.1:4318", chains: [][2]interface{}{{true, "host-a"}, {true, ""}}, warning: "no fibre_instance"},
		{name: "two chains, both unnamed", listen: "127.0.0.1:4318", chains: [][2]interface{}{{true, ""}, {true, ""}}, warning: "no fibre_instance"},
		{name: "two chains, both named", listen: "127.0.0.1:4318", chains: [][2]interface{}{{true, "host-a"}, {true, "host-b"}}},
		{name: "two chains, one with Fibre alerts off", listen: "127.0.0.1:4318", chains: [][2]interface{}{{true, ""}, {false, ""}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fatal, problems := validateConfig(fibreValidateConfig(tc.listen, tc.token, tc.chains...))
			all := strings.Join(problems, "\n")
			if tc.fatal != "" && (!fatal || !strings.Contains(all, tc.fatal)) {
				t.Fatalf("fatal %v, problems %q, want an error with %q", fatal, all, tc.fatal)
			}
			if tc.fatal == "" && fatal {
				t.Fatalf("fatal: %q", all)
			}
			if tc.warning != "" && !strings.Contains(all, tc.warning) {
				t.Fatalf("problems %q, want a warning with %q", all, tc.warning)
			}
			if tc.warning == "" && tc.fatal == "" {
				for _, problem := range problems {
					if strings.Contains(problem, "fibre") {
						t.Fatalf("unexpected problem %q", problem)
					}
				}
			}
		})
	}
	t.Run("tls needs both files", func(t *testing.T) {
		for _, tc := range []struct {
			cert, key string
			fatal     bool
		}{{"", "", false}, {"c.pem", "k.pem", false}, {"c.pem", "", true}, {"", "k.pem", true}} {
			c := fibreValidateConfig("127.0.0.1:4318", "", [2]interface{}{true, ""})
			c.FibreMetrics.TLSCert, c.FibreMetrics.TLSKey = tc.cert, tc.key
			if fatal, problems := validateConfig(c); fatal != tc.fatal {
				t.Errorf("cert %q key %q: fatal %v, %v", tc.cert, tc.key, fatal, problems)
			}
		}
		// the receiver being off doesn't excuse half a pair
		c := fibreValidateConfig("", "")
		c.FibreMetrics.TLSCert = "c.pem"
		if fatal, _ := validateConfig(c); !fatal {
			t.Error("a certificate without a key was accepted with the receiver off")
		}
	})
	t.Run("the settings per chain", func(t *testing.T) {
		for _, tc := range []struct {
			percent, idle         int
			wantPercent, wantIdle int
			warnings              []string
		}{
			{0, 0, 0, 0, nil}, // off
			{75, 45, 75, 45, nil},
			{100, 0, 100, 0, nil},
			{1, 0, 1, 0, nil},
			{101, 0, 0, 0, []string{"fibre_storage_percent"}},
			{-3, 0, 0, 0, []string{"fibre_storage_percent"}},
			{90, -5, 90, 0, []string{"fibre_idle_minutes"}},
			{150, -5, 0, 0, []string{"fibre_storage_percent", "fibre_idle_minutes"}},
		} {
			c := fibreValidateConfig("127.0.0.1:4318", "", [2]interface{}{true, ""})
			c.Chains["A"].Alerts.FibreStoragePercent, c.Chains["A"].Alerts.FibreIdleMinutes = tc.percent, tc.idle
			fatal, problems := validateConfig(c)
			if fatal || c.Chains["A"].Alerts.FibreStoragePercent != tc.wantPercent || c.Chains["A"].Alerts.FibreIdleMinutes != tc.wantIdle {
				t.Errorf("%+v: got %d and %d, fatal %v", tc, c.Chains["A"].Alerts.FibreStoragePercent, c.Chains["A"].Alerts.FibreIdleMinutes, fatal)
			}
			var found []string
			for _, p := range problems {
				for _, key := range []string{"fibre_storage_percent", "fibre_idle_minutes"} {
					if strings.Contains(p, key) {
						found = append(found, key)
					}
				}
			}
			if strings.Join(found, ",") != strings.Join(tc.warnings, ",") {
				t.Errorf("%+v: warned about %v, want %v", tc, found, tc.warnings)
			}
		}
	})
}

func TestValidateFibreMetricsAllowWithoutToken(t *testing.T) {
	for _, listen := range []string{"0.0.0.0:4318", "10.0.0.5:4318", ":4318", "[::]:4318", "example.com:4318", "127.0.0.1:4318"} {
		for _, token := range []string{"", "secret"} {
			t.Run(listen+"/"+token, func(t *testing.T) {
				c := fibreValidateConfig(listen, token, [2]interface{}{true, ""})
				c.FibreMetrics.AllowWithoutToken = true
				fatal, problems := validateFibreMetrics(c)
				if fatal {
					t.Fatalf("fatal: %v", problems)
				}
				want := "warning: fibre_metrics listens on " + listen + " without a token; make sure a firewall lets only Fibre's IP reach it"
				found := false
				for _, problem := range problems {
					if problem == want {
						found = true
					} else if strings.Contains(problem, "without a token") {
						t.Fatalf("unexpected missing-token warning: %q", problem)
					}
				}
				if expected := token == "" && listen != "127.0.0.1:4318"; found != expected {
					t.Fatalf("missing-token warning %v, want %v: %v", found, expected, problems)
				}
				if token != "" && listen != "127.0.0.1:4318" && !strings.Contains(strings.Join(problems, "\n"), "cleartext") {
					t.Fatalf("cleartext warning missing: %v", problems)
				}
			})
		}
	}
}

func TestFibreMetricsConfigLoads(t *testing.T) {
	c := &Config{}
	yaml := "fibre_metrics:\n  listen: 127.0.0.1:4318\n  token: abc\n  allow_without_token: yes\n  tls_cert: /c.pem\n  tls_key: /k.pem\nalert_defaults:\n  fibre_instance: host\n  fibre_storage_percent: 80\n  fibre_idle_minutes: 15\n"
	if err := unmarshalConfig("test", []byte(yaml), c, &Config{}); err != nil {
		t.Fatal(err)
	}
	if c.FibreMetrics != (FibreMetricsConfig{Listen: "127.0.0.1:4318", Token: "abc", AllowWithoutToken: true, TLSCert: "/c.pem", TLSKey: "/k.pem"}) {
		t.Fatalf("loaded %+v", c.FibreMetrics)
	}
	if a := c.AlertDefaults; a.FibreInstance != "host" || a.FibreStoragePercent != 80 || a.FibreIdleMinutes != 15 {
		t.Fatalf("alert defaults %+v", a)
	}
}
