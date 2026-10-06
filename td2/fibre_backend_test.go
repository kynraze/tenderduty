package tenderduty

import (
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	metricsv1 "go.opentelemetry.io/proto/otlp/metrics/v1"
)

func TestFibreBackendNames(t *testing.T) {
	for _, name := range []string{"local", "object", "s3", ""} {
		want := name
		if want == "" {
			want = "object"
		}
		if got := fibreBackendName(kvs("backend", name)); got != want {
			t.Errorf("%q: got %q", name, got)
		}
	}
	if got := fibreBackendName(nil); got != "object" {
		t.Fatal(got)
	}
}

func TestFibreBackendAlertsAndLabels(t *testing.T) {
	for _, backend := range []string{"local", "s3", "object"} {
		t.Run(backend, func(t *testing.T) {
			r := newFibreRig(t)
			f := r.feed(time.Minute)
			r.st.record("Chain", f.instance, "", []*metricsv1.Metric{
				f.hist("backend.get.duration", []float64{1}, plain(115, "backend", backend, "outcome", "success"), plain(2, "backend", backend, "outcome", "not_found"), plain(0, "backend", backend, "outcome", "canceled"), plain(2, "backend", backend, "outcome", "timeout"), plain(1, "backend", backend, "outcome", "error")),
				f.sum("backend.get.bytes", 100, "backend", backend),
				f.sum("backend.get.in_flight", 3, "backend", backend),
			})
			snap := r.st.snapshot("Chain")
			want := backend + ": 3 of 120 requests failed in the last 10 minutes (timeout 2, error 1)\n" + fibreBackendAdvice(backend)
			if got := fibreMetricsDetail("Chain", r.msg(kindObject)); got != want {
				t.Fatalf("detail %q, want %q", got, want)
			}
			if r.cc.fibreVerdicts(snap, r.st.started, r.clock.now)[kindObject] != fibreRaise {
				t.Fatal("backend failures did not raise")
			}
			if snap.Backend.Sum.backendTotal != 120 || len(snap.BackendNames) != 1 || snap.BackendNames[0] != backend {
				t.Fatalf("snapshot %+v", snap)
			}
			if r.cc.fibreReport(r.clock.now).dashboard().StorageBackend != backend {
				t.Fatal("missing dashboard backend")
			}
			if got := strings.Join(r.cc.fibreReport(r.clock.now).telegramLines(), "\n"); !strings.Contains(got, "<b>Storage backend:</b> "+backend) {
				t.Fatal(got)
			}
			if backend == "local" && fibreBackendAdvice(backend) != "Check Fibre's data disk: space, health and permissions." {
				t.Fatal("wrong local advice")
			}
			if backend != "local" && fibreBackendAdvice(backend) != "Check the bucket's credentials, rate limits, network, and that it is in the same region as Fibre." {
				t.Fatal("wrong object advice")
			}
			registry := prometheus.NewPedanticRegistry()
			registry.MustRegister(&fibreCollector{config: r.c})
			families, err := registry.Gather()
			if err != nil {
				t.Fatal(err)
			}
			seen := make(map[string]bool)
			for _, family := range families {
				name := family.GetName()
				if strings.Contains(name, "object_storage") {
					t.Fatal("old metric exported", name)
				}
				if !strings.HasPrefix(name, "tenderduty_fibre_storage_backend_") {
					continue
				}
				seen[name] = true
				for _, metric := range family.Metric {
					have := false
					for _, label := range metric.Label {
						if label.GetName() == "backend" && label.GetValue() == backend {
							have = true
						}
					}
					if !have {
						t.Fatal("missing backend label", name)
					}
				}
			}
			for _, suffix := range []string{"bytes_total", "in_flight", "requests_total"} {
				if !seen["tenderduty_fibre_storage_backend_"+suffix] {
					t.Fatal("missing metric", suffix)
				}
			}
		})
	}
}

func TestFibreBackendNonFailuresAndMultipleNames(t *testing.T) {
	r := newFibreRig(t)
	f := r.feed(time.Minute)
	r.st.record("Chain", f.instance, "", []*metricsv1.Metric{
		f.hist("backend.get.duration", []float64{1}, plain(20, "backend", "local", "outcome", "not_found"), plain(20, "outcome", "canceled")),
		f.sum("backend.get.bytes", 10, "backend", "local"),
		f.sum("backend.get.bytes", 20),
	})
	snap := r.st.snapshot("Chain")
	if strings.Join(snap.BackendNames, ", ") != "local, object" || snap.Backend.Sum.backendTotal != 40 {
		t.Fatalf("snapshot %+v", snap)
	}
	if r.cc.fibreVerdicts(snap, r.st.started, r.clock.now)[kindObject] != fibreClear {
		t.Fatal("not_found/canceled counted as failures")
	}
	if fibreBackendDetail(snap) != "" {
		t.Fatal("nonfailures appeared in detail")
	}
}

func TestFibreLastSuccessGaugeAndFallback(t *testing.T) {
	for _, gaugeValue := range []int64{-1, 0, 123} {
		t.Run(strings.Repeat("x", int(max(gaugeValue, 0))), func(t *testing.T) {
			r := newFibreRig(t)
			r.cc.Alerts.FibreIdleMinutes = 15
			f := r.feed(time.Minute)
			metrics := []*metricsv1.Metric{f.hist("upload_shard.duration", []float64{1}, plain(1, "success", true))}
			if gaugeValue >= 0 {
				metrics = append(metrics, gauge("upload_shard.last_success_timestamp", gaugeValue))
			}
			r.st.record("Chain", f.instance, "", metrics)
			snap := r.st.snapshot("Chain")
			want := r.clock.now.Unix()
			if gaugeValue > 0 {
				want = gaugeValue
			}
			if !snap.HaveSuccess || snap.LastSuccess.Unix() != want {
				t.Fatalf("last success %+v, want %d", snap, want)
			}
			verdict := fibreClear
			if gaugeValue > 0 {
				verdict = fibreRaise
			}
			if got := r.cc.fibreVerdicts(snap, r.st.started, r.clock.now)[kindIdle]; got != verdict {
				t.Fatalf("idle verdict %d, want %d", got, verdict)
			}
		})
	}
	r := newFibreRig(t)
	r.st.record("Chain", "host", "", []*metricsv1.Metric{gauge("upload_shard.last_success_timestamp", 0)})
	if r.st.snapshot("Chain").HaveSuccess {
		t.Fatal("zero gauge became a successful upload")
	}
}

func TestFibreOldBackendAlarmClearsQuietly(t *testing.T) {
	r := newFibreRig(t)
	old := "Fibre object storage requests are failing on celestia"
	id := "valcons1fibremetricsobjectstorage"
	r.c.alert("Chain", old, "warning", false, &id)
	for len(r.c.alertChan) > 0 {
		<-r.c.alertChan
	}
	sent := r.alarms()
	if r.active(old) {
		t.Fatal("old object storage alarm still active")
	}
	if len(sent) != 1 || !sent[0].resolved || !sent[0].quiet || sent[0].message != old {
		t.Fatalf("old alarm did not clear quietly: %s", describe(sent))
	}
}
