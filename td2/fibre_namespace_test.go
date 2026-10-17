package tenderduty

import (
	"bytes"
	"strings"
	"testing"
	"time"

	dash "github.com/kynraze/tenderduty/v2/td2/dashboard"
	metricsv1 "go.opentelemetry.io/proto/otlp/metrics/v1"
)

func fibreNamespaceData(namespace, instance string, metrics ...*metricsv1.Metric) *metricsv1.MetricsData {
	data := resourceData(instance, "fibre", metrics...)
	data.ResourceMetrics[0].Resource.Attributes = append(data.ResourceMetrics[0].Resource.Attributes, kvs("service.namespace", namespace)...)
	return data
}

func TestFibreNamespaceRouting(t *testing.T) {
	for _, mode := range []string{"sole", "instance-not-needed", "unknown", "disabled", "unsupported", "match", "no-match", "duplicate-match", "empty-instance", "key-name"} {
		t.Run(mode, func(t *testing.T) {
			r := newFibreRig(t)
			r.cc.ChainId = "mocha-5"
			r.cc.Alerts.FibreInstance = "different-host"
			if mode == "sole" {
				r.cc.Alerts.FibreInstance = ""
			}
			want, namespace, instance := "Chain", "mocha-5", "host"
			switch mode {
			case "unknown":
				namespace, want = "unknown", ""
			case "disabled":
				r.cc.Alerts.FibreAlerts, want = false, ""
			case "unsupported":
				r.cc.fibreUnsupported, want = true, ""
			case "match", "no-match", "duplicate-match", "empty-instance":
				other := &ChainConfig{name: "Other", ChainId: namespace}
				other.Alerts.FibreAlerts, other.Alerts.FibreInstance = true, instance
				r.c.Chains["Other"] = other
				want = "Other"
				if mode == "no-match" {
					instance, want = "unclaimed", ""
				}
				if mode == "duplicate-match" {
					r.cc.Alerts.FibreInstance, want = instance, ""
				}
				if mode == "empty-instance" {
					instance, other.Alerts.FibreInstance, want = "", "", ""
				}
			case "key-name":
				r.cc.name = ""
			}
			// Repeat to catch routing that depends on map iteration order.
			for i := 0; i < 30; i++ {
				r.c.ingestFibreMetrics(fibreNamespaceData(namespace, instance, gauge("upload_shard.occupancy_bytes", 7)))
			}
			if want == "" {
				if len(r.st.senders) != 0 || len(r.st.ignored) != 1 {
					t.Fatalf("senders %d, ignored %d", len(r.st.senders), len(r.st.ignored))
				}
			} else if snap := r.st.snapshot(want); !snap.Found || snap.Occupancy != 7 || len(r.st.senders) != 1 {
				t.Fatalf("routed to %q: %+v, senders %d", want, snap, len(r.st.senders))
			}
		})
	}
}

func TestFibreNamespacesKeepHostCountersSeparate(t *testing.T) {
	r := newFibreRig(t)
	r.cc.ChainId = "A"
	other := &ChainConfig{name: "Other", ChainId: "B"}
	other.Alerts.FibreAlerts = true
	r.c.Chains["Other"] = other
	f := r.feed(time.Minute)
	send := func(namespace string, count int64) {
		r.c.ingestFibreMetrics(fibreNamespaceData(namespace, f.instance,
			f.hist("upload_shard.duration", []float64{1}, plain(count, "success", true))))
	}
	send("A", 10)
	send("B", 20)
	send("A", 10)
	send("B", 20)
	r.advance(time.Minute)
	send("B", 23)
	send("A", 12)
	if len(r.st.senders) != 2 {
		t.Fatalf("%d senders", len(r.st.senders))
	}
	for chain, want := range map[string]int64{"Chain": 12, "Other": 23} {
		if got := r.st.snapshot(chain).Window.uploadOK; got != want {
			t.Errorf("%s uploads %d, want %d", chain, got, want)
		}
		if got := r.st.totals[chain]["uploads_ok"]; got != float64(want) {
			t.Errorf("%s total %v, want %d", chain, got, want)
		}
	}
}

func TestFibreNamespaceIgnoredLogs(t *testing.T) {
	r := newFibreRig(t)
	r.c.EnableDash = true
	r.c.logChan = make(chan dash.LogMessage, 8)
	for _, namespace := range []string{"unknown-A", "unknown-A", "unknown-B", "unknown-B"} {
		r.c.ingestFibreMetrics(fibreNamespaceData(namespace, "host"))
	}
	for _, namespace := range []string{"unknown-A", "unknown-B"} {
		select {
		case msg := <-r.c.logChan:
			want := "ignoring Fibre metrics from \"host\" on " + namespace + ": no chain with that chain_id has fibre_enabled"
			if msg.Msg != want {
				t.Fatalf("log %q, want %q", msg.Msg, want)
			}
		default:
			t.Fatal("missing ignored log")
		}
	}
	if len(r.c.logChan) != 0 || len(r.st.ignored) != 2 {
		t.Fatal("ignored notes shared between namespaces or logged twice")
	}
	other := &ChainConfig{name: "Other", ChainId: r.cc.ChainId}
	other.Alerts.FibreAlerts = true
	r.c.Chains["Other"] = other
	for i := 0; i < 2; i++ {
		r.c.ingestFibreMetrics(fibreNamespaceData(r.cc.ChainId, "host"))
	}
	select {
	case msg := <-r.c.logChan:
		if !strings.Contains(msg.Msg, "multiple chains with that chain_id have fibre_enabled; need one matching fibre_instance") {
			t.Fatal(msg.Msg)
		}
	default:
		t.Fatal("missing ambiguous log")
	}
	if len(r.c.logChan) != 0 {
		t.Fatal("ambiguous sender logged twice")
	}
}

func TestFibreSenderNamespaceKey(t *testing.T) {
	if got := fibreSenderKey("", "host"); got != "host" {
		t.Fatalf("legacy key changed: %q", got)
	}
	if fibreSenderKey("a:b", "c") == fibreSenderKey("a", "b:c") {
		t.Fatal("namespace and instance boundaries collide")
	}
}

func TestFibreNamespaceIdentityClipped(t *testing.T) {
	r := newFibreRig(t)
	r.cc.ChainId = strings.Repeat("a", fibreMaxIdentity)
	r.c.ingestFibreMetrics(fibreNamespaceData(r.cc.ChainId+"extra", "host", gauge("upload_shard.occupancy_bytes", 7)))
	if !r.st.snapshot("Chain").Found {
		t.Fatal("namespace was not clipped")
	}
}

func TestPrintMetricsSetupNamespace(t *testing.T) {
	for _, token := range []string{"", "secret"} {
		var output bytes.Buffer
		printMetricsSetup(FibreMetricsConfig{Listen: "127.0.0.1:4318", Token: token}, "http://127.0.0.1:4318", "mocha-5", false, &output)
		want := "OTEL_RESOURCE_ATTRIBUTES=\"service.namespace=mocha-5\" \\\n  fibre start --otel-endpoint http://127.0.0.1:4318\n"
		if token != "" {
			want = "token: fibre_metrics.token in config.yml (config show hides it)\nOTEL_EXPORTER_OTLP_HEADERS=\"authorization=Bearer <token>\" \\\n" + want
		}
		if output.String() != want {
			t.Fatalf("setup %q, want %q", output.String(), want)
		}
	}
}

// Labeling only the new Fibre (mainnet) leaves the unlabeled one (testnet) on the other chain, even from the same host.
func TestFibreUnlabeledGoesToTheChainWithoutALabeledFibre(t *testing.T) {
	r := newFibreRig(t)
	r.cc.ChainId, r.cc.Alerts.FibreInstance = "mocha-5", ""
	mainnet := &ChainConfig{name: "Mainnet", ChainId: "celestia"}
	mainnet.Alerts.FibreAlerts = true
	r.c.Chains["Mainnet"] = mainnet
	r.c.ingestFibreMetrics(resourceData("host", "fibre", gauge("upload_shard.occupancy_bytes", 5)))
	if len(r.st.senders) != 0 {
		t.Fatal("two unlabeled chains took an unlabeled Fibre")
	}
	r.c.ingestFibreMetrics(fibreNamespaceData("celestia", "host", gauge("upload_shard.occupancy_bytes", 9)))
	// Repeat to catch routing that depends on map iteration order.
	for i := 0; i < 30; i++ {
		r.c.ingestFibreMetrics(resourceData("host", "fibre", gauge("upload_shard.occupancy_bytes", 5)))
	}
	if a, b := r.st.snapshot("Chain"), r.st.snapshot("Mainnet"); a.Occupancy != 5 || b.Occupancy != 9 {
		t.Fatalf("testnet %+v, mainnet %+v", a, b)
	}
}
