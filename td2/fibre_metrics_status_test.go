package tenderduty

import (
	"math"
	"strings"
	"testing"
	"time"

	metricsv1 "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/protobuf/proto"
)

// running totals as a sender exports them
type statusTotals struct {
	uploadsA, uploadsB, uploadsFailed int64 // successful uploads of two sizes, and the failed ones
	uploadSecA, uploadSecB, failedSec float64
	uploadBytes                       int64
	uploadsNow, downloadsNow          int64 // up-down counters, the number now
	downloadsOK, downloadsFailed      int64
	downloadSec                       float64
	downloadBytes                     int64
	putOK, getOK, pruneOK             int64
	putSec, getSec, pruneSec          float64
	pruneEntries                      int64
	signOK, signFast, signFailed      int64
	signSec                           float64
}

func durationPoint(n int64, seconds float64, attrs ...interface{}) *metricsv1.HistogramDataPoint {
	return &metricsv1.HistogramDataPoint{Attributes: kvs(attrs...), Count: uint64(n), Sum: proto.Float64(seconds), BucketCounts: []uint64{uint64(n), 0}}
}

// a non-monotonic cumulative sum; the value is the number now
func (f *fibreFeed) upDown(name string, now int64) *metricsv1.Metric {
	return &metricsv1.Metric{Name: "fibre.server." + name, Data: &metricsv1.Metric_Sum{Sum: &metricsv1.Sum{
		AggregationTemporality: f.temporality(),
		DataPoints:             []*metricsv1.NumberDataPoint{{StartTimeUnixNano: f.start, Value: &metricsv1.NumberDataPoint_AsInt{AsInt: now}}},
	}}}
}

func (f *fibreFeed) statusData(v statusTotals) *metricsv1.MetricsData {
	data := f.data()
	bounds := []float64{1}
	var more []*metricsv1.Metric
	if v.uploadsA+v.uploadsB+v.uploadsFailed > 0 {
		more = append(more, f.hist("upload_shard.duration", bounds,
			durationPoint(v.uploadsA, v.uploadSecA, "success", true, "upload_size", 1048576),
			durationPoint(v.uploadsB, v.uploadSecB, "success", true, "upload_size", 2097152),
			durationPoint(v.uploadsFailed, v.failedSec, "success", false)))
	}
	if v.downloadsOK+v.downloadsFailed > 0 {
		more = append(more, f.hist("download_shard.duration", bounds,
			durationPoint(v.downloadsOK, v.downloadSec, "success", true, "shard_size", 4096),
			durationPoint(v.downloadsFailed, 1, "success", false)))
	}
	if v.putOK > 0 {
		more = append(more, f.hist("store.put.duration", bounds, durationPoint(v.putOK, v.putSec, "success", true)))
	}
	if v.getOK > 0 {
		more = append(more, f.hist("store.get.duration", bounds, durationPoint(v.getOK, v.getSec, "success", true)))
	}
	if v.pruneOK > 0 {
		more = append(more, f.hist("prune.duration", bounds, durationPoint(v.pruneOK, v.pruneSec, "success", true)))
	}
	if v.signOK+v.signFailed > 0 {
		// 0.01 is the fifth bound, the buckets up to it count the ones at or under 10ms
		fast, slow := uint64(v.signFast), uint64(v.signOK-v.signFast)
		var points []*metricsv1.HistogramDataPoint
		if v.signOK > 0 {
			points = append(points, &metricsv1.HistogramDataPoint{Attributes: kvs("success", true), Count: uint64(v.signOK), Sum: proto.Float64(v.signSec),
				BucketCounts: []uint64{0, 0, 0, 0, fast, slow, 0}})
		}
		if v.signFailed > 0 {
			points = append(points, &metricsv1.HistogramDataPoint{Attributes: kvs("success", false), Count: uint64(v.signFailed), Sum: proto.Float64(1),
				BucketCounts: []uint64{0, 0, 0, 0, 0, uint64(v.signFailed), 0}})
		}
		more = append(more, f.hist("sign.duration", []float64{0.0001, 0.0005, 0.001, 0.005, 0.01, 0.05}, points...))
	}
	more = append(more,
		f.sum("upload_shard.bytes", v.uploadBytes), f.sum("download_shard.bytes", v.downloadBytes), f.sum("prune.entries", v.pruneEntries),
		f.upDown("upload_shard.in_flight", v.uploadsNow), f.upDown("download_shard.in_flight", v.downloadsNow))
	sm := data.ResourceMetrics[0].ScopeMetrics[0]
	sm.Metrics = append(sm.Metrics, more...)
	return data
}

func near(got, want float64) bool { return math.Abs(got-want) < 1e-9 }

func TestFibreMetricsStatusCounters(t *testing.T) {
	r := newFibreRig(t)
	r.cc.Alerts.FibreInstance = "fibre-host-1"
	f := r.feed(time.Minute)
	first := statusTotals{uploadsA: 8, uploadsB: 2, uploadsFailed: 1, uploadSecA: 0.16, uploadSecB: 0.08, failedSec: 0.5, uploadBytes: 10 << 20,
		uploadsNow: 3, downloadsNow: 1, downloadsOK: 4, downloadsFailed: 1, downloadSec: 0.2, downloadBytes: 4096,
		putOK: 10, putSec: 0.1, getOK: 5, getSec: 0.05, pruneOK: 2, pruneSec: 0.4, pruneEntries: 30, signOK: 20, signFast: 15, signSec: 0.1}
	f.r.c.ingestFibreMetrics(f.statusData(first))
	snap := r.st.snapshot("Chain")
	d := snap.Ten.Sum
	// the two upload sizes are separate series and both count
	if d.uploadOK != 10 || d.uploadFailed != 1 || !near(d.uploadSec, 0.24) || d.uploadBytes != 10<<20 {
		t.Fatalf("uploads counted %+v", d)
	}
	if d.downloadOK != 4 || d.downloadFailed != 1 || !near(d.downloadSec, 0.2) || d.downloadBytes != 4096 {
		t.Fatalf("downloads counted %+v", d)
	}
	if d.putOK != 10 || d.getOK != 5 || d.pruneOK != 2 || !near(d.putSec, 0.1) || !near(d.getSec, 0.05) || !near(d.pruneSec, 0.4) || d.pruneEntries != 30 {
		t.Fatalf("store and prune counted %+v", d)
	}
	if d.signOK != 20 || d.signFast != 15 || !near(d.signSec, 0.1) {
		t.Fatalf("signing counted %+v", d)
	}
	if !snap.HaveInFlight || snap.UploadsNow != 3 || snap.DownloadsNow != 1 {
		t.Fatalf("in flight %d and %d", snap.UploadsNow, snap.DownloadsNow)
	}

	// growth is the difference, in flight is the value now; a drop isn't a restart
	r.advance(time.Minute)
	second := first
	second.uploadsA, second.uploadSecA, second.uploadBytes = 18, 0.36, 30<<20
	second.uploadsNow, second.downloadsNow = 1, 0
	second.signOK, second.signFast, second.signSec = 30, 25, 0.2
	f.r.c.ingestFibreMetrics(f.statusData(second))
	snap = r.st.snapshot("Chain")
	d = snap.Ten.Sum
	if d.uploadOK != 20 || d.uploadFailed != 1 || !near(d.uploadSec, 0.44) || d.uploadBytes != 30<<20 {
		t.Fatalf("after growth uploads counted %+v", d)
	}
	if d.signOK != 30 || d.signFast != 25 || !near(d.signSec, 0.2) {
		t.Fatalf("after growth signing counted %+v", d)
	}
	if snap.UploadsNow != 1 || snap.DownloadsNow != 0 {
		t.Fatalf("in flight is %d and %d, it is the last value", snap.UploadsNow, snap.DownloadsNow)
	}

	// restart: count the new totals and durations as fresh observations
	r.advance(time.Minute)
	f.start = uint64(r.clock.now.Add(-10 * time.Second).UnixNano())
	third := statusTotals{uploadsA: 2, uploadSecA: 0.06, uploadBytes: 2 << 20, putOK: 1, putSec: 0.02, signOK: 4, signFast: 4, signSec: 0.008}
	f.r.c.ingestFibreMetrics(f.statusData(third))
	d = r.st.snapshot("Chain").Ten.Sum
	if d.uploadOK != 22 || !near(d.uploadSec, 0.5) || d.uploadBytes != 32<<20 || d.putOK != 11 || !near(d.putSec, 0.12) || d.signOK != 34 || !near(d.signSec, 0.208) {
		t.Fatalf("after a restart counted %+v", d)
	}
	for _, v := range []float64{d.uploadSec, d.downloadSec, d.putSec, d.getSec, d.pruneSec, d.signSec} {
		if v < 0 {
			t.Fatalf("a negative duration in %+v", d)
		}
	}
	if snap := r.st.snapshot("Chain"); snap.UploadsNow != 0 {
		t.Fatalf("in flight after the restart is %d", snap.UploadsNow)
	}

	// same start time, sum went down: the count grew, the seconds can't be negative
	r.advance(time.Minute)
	third.uploadsA, third.uploadSecA = 3, 0.01
	f.r.c.ingestFibreMetrics(f.statusData(third))
	if d = r.st.snapshot("Chain").Ten.Sum; d.uploadOK != 23 || !near(d.uploadSec, 0.5) {
		t.Fatalf("a sum that went down counted %+v", d)
	}
}

func TestFibreMetricsStatusFirstExportIsABaseline(t *testing.T) {
	r := newFibreRig(t)
	r.cc.Alerts.FibreInstance = "fibre-host-1"
	f := r.feed(48 * time.Hour)
	totals := statusTotals{uploadsA: 5000, uploadSecA: 100, uploadBytes: 5 << 30, uploadsNow: 2, signOK: 400, signFast: 399, signSec: 2}
	f.r.c.ingestFibreMetrics(f.statusData(totals))
	snap := r.st.snapshot("Chain")
	if !snap.Ten.Sum.empty() || !snap.Day.Sum.empty() {
		t.Fatalf("what Fibre did before tenderduty started counted: %+v", snap.Day.Sum)
	}
	if snap.UploadsNow != 2 {
		t.Fatalf("in flight is %d", snap.UploadsNow)
	}
}

func TestFibreMetricsStatusWindows(t *testing.T) {
	r := newFibreRig(t)
	r.cc.Alerts.FibreInstance = "fibre-host-1"
	f := r.feed(48 * time.Hour)
	totals := statusTotals{uploadsA: 100, uploadSecA: 2}
	f.r.c.ingestFibreMetrics(f.statusData(totals)) // the baseline
	for i := 1; i <= 3; i++ {
		r.advance(time.Hour)
		totals.uploadsA, totals.uploadSecA = totals.uploadsA+10, totals.uploadSecA+0.2
		f.r.c.ingestFibreMetrics(f.statusData(totals))
	}
	r.advance(12 * time.Minute)
	totals.uploadsA, totals.uploadSecA = totals.uploadsA+7, totals.uploadSecA+0.14
	f.r.c.ingestFibreMetrics(f.statusData(totals))
	r.advance(time.Minute)

	snap := r.st.snapshot("Chain")
	if snap.Ten.Sum.uploadOK != 7 || snap.Day.Sum.uploadOK != 37 {
		t.Fatalf("10 minutes counted %d and 24 hours %d uploads", snap.Ten.Sum.uploadOK, snap.Day.Sum.uploadOK)
	}
	// day coverage starts 3h13m ago; the 10-minute window is complete
	if snap.Ten.Covers != fibreWindow || snap.Day.Covers != 3*time.Hour+13*time.Minute {
		t.Fatalf("10 minutes cover %v and the day %v", snap.Ten.Covers, snap.Day.Covers)
	}
	if snap.Window != snap.Ten.Sum {
		t.Fatalf("window %+v is not the 10 minutes %+v", snap.Window, snap.Ten.Sum)
	}

	// after a day, trim the oldest delta and report 24-hour coverage
	r.advance(23*time.Hour + 50*time.Minute)
	snap = r.st.snapshot("Chain")
	if snap.Day.Covers != fibreKeep || snap.Day.Sum.uploadOK != 7 {
		t.Fatalf("the day covers %v and counted %d uploads", snap.Day.Covers, snap.Day.Sum.uploadOK)
	}
}

// TestFibreMetricsStatusCoversWhatIsKept: merging at fibreMaxDeltas retains all counts.
func TestFibreMetricsStatusCoversWhatIsKept(t *testing.T) {
	r := newFibreRig(t)
	r.cc.Alerts.FibreInstance = "fibre-host-1"
	f := r.feed(time.Second)
	f.send()
	before := r.st.snapshot("Chain").Day.Sum.rejected
	for i := 0; i < fibreMaxDeltas+10; i++ {
		r.advance(time.Second)
		f.rejected++
		f.send()
	}
	snap := r.st.snapshot("Chain")
	sender := r.st.senders["fibre-host-1"]
	if len(sender.window) > fibreMaxDeltas {
		t.Fatalf("%d deltas kept", len(sender.window))
	}
	if want := r.clock.now.Sub(sender.since); snap.Day.Covers != want {
		t.Fatalf("the day covers %v, want %v since the first export", snap.Day.Covers, want)
	}
	if got := snap.Day.Sum.rejected - before; got != fibreMaxDeltas+10 {
		t.Fatalf("the day counts %d rejections, want %d", got, fibreMaxDeltas+10)
	}
}

func TestFibreMetricsSeriesThatStopAreForgotten(t *testing.T) {
	r := newFibreRig(t)
	r.cc.Alerts.FibreInstance = "fibre-host-1"
	f := r.feed(time.Minute)
	f.r.c.ingestFibreMetrics(f.statusData(statusTotals{uploadsA: 1, uploadsB: 1, uploadSecA: 0.01, uploadSecB: 0.01}))
	uploads := func() int {
		n := 0
		for key := range r.st.senders["fibre-host-1"].series {
			if strings.Contains(key, "upload_shard.duration") {
				n++
			}
		}
		return n
	}
	if uploads() == 0 {
		t.Fatal("no upload series were kept")
	}
	r.advance(23 * time.Hour)
	f.r.c.ingestFibreMetrics(f.statusData(statusTotals{}))
	if uploads() == 0 {
		t.Fatal("upload series were dropped before a day")
	}
	r.advance(2 * time.Hour)
	f.r.c.ingestFibreMetrics(f.statusData(statusTotals{}))
	if n := uploads(); n != 0 {
		t.Fatalf("%d upload series are still kept a day after they stopped", n)
	}
	if len(r.st.senders["fibre-host-1"].series) == 0 {
		t.Fatal("series that still come were dropped too")
	}
}

func TestFibreMetricsStatusUpDownCounters(t *testing.T) {
	r := newFibreRig(t)
	r.cc.Alerts.FibreInstance = "fibre-host-1"
	f := r.feed(time.Minute)
	// sum in-flight values across series attributes
	m := f.upDown("upload_shard.in_flight", 2)
	m.GetSum().DataPoints = append(m.GetSum().DataPoints, &metricsv1.NumberDataPoint{Attributes: kvs("kind", "x"), Value: &metricsv1.NumberDataPoint_AsInt{AsInt: 3}})
	r.c.ingestFibreMetrics(resourceData("fibre-host-1", "fibre", m))
	if snap := r.st.snapshot("Chain"); snap.UploadsNow != 5 || snap.DownloadsNow != 0 {
		t.Fatalf("in flight is %d and %d", snap.UploadsNow, snap.DownloadsNow)
	}
	// a counter that reports changes only is added up, and never goes below zero
	f.delta = true
	r.c.ingestFibreMetrics(resourceData("fibre-host-1", "fibre", f.upDown("upload_shard.in_flight", -1)))
	if snap := r.st.snapshot("Chain"); snap.UploadsNow != 4 {
		t.Fatalf("after a change of -1 in flight is %d", snap.UploadsNow)
	}
	r.c.ingestFibreMetrics(resourceData("fibre-host-1", "fibre", f.upDown("upload_shard.in_flight", -9)))
	if snap := r.st.snapshot("Chain"); snap.UploadsNow != 0 {
		t.Fatalf("in flight is %d", snap.UploadsNow)
	}
	r.c.ingestFibreMetrics(resourceData("fibre-host-1", "fibre", &metricsv1.Metric{Name: "fibre.server.upload_shard.in_flight"}))
}

// TestFibreMetricsStatusFixtures: the alarm fixtures carry these too: 9 uploads of 1 MiB at 20ms, a failed one, 2 in flight
func TestFibreMetricsStatusFixtures(t *testing.T) {
	r := newFibreRig(t)
	r.cc.Alerts.FibreInstance = "fibre-host-1"
	var first, second metricsv1.MetricsData
	if err := proto.Unmarshal(fibreFixture(t, "export1.pb"), &first); err != nil {
		t.Fatal(err)
	}
	if err := proto.Unmarshal(fibreFixture(t, "export2.pb"), &second); err != nil {
		t.Fatal(err)
	}
	started := time.Unix(0, int64(first.ResourceMetrics[0].ScopeMetrics[0].Metrics[0].GetSum().DataPoints[0].StartTimeUnixNano))
	r.clock.now, r.st.started = started.Add(30*time.Second), started.Add(30*time.Second)
	r.c.ingestFibreMetrics(&first)
	d := r.st.snapshot("Chain").Ten.Sum
	if d.uploadOK != 9 || d.uploadFailed != 1 || !near(d.uploadSec, 0.18) || d.putOK != 1 || d.storePut != 1 || d.pruneOK != 1 || !near(d.pruneSec, 0.2) || d.signOK != 2 || !near(d.signSec, 0.024) {
		t.Fatalf("export 1 counted %+v", d)
	}
	if snap := r.st.snapshot("Chain"); snap.UploadsNow != 2 || !snap.HaveInFlight {
		t.Fatalf("in flight %d", snap.UploadsNow)
	}
	r.advance(time.Minute)
	r.c.ingestFibreMetrics(&second)
	d = r.st.snapshot("Chain").Ten.Sum
	if d.uploadOK != 9 || d.uploadFailed != 2 {
		t.Fatalf("export 2 counted %+v", d)
	}
}
