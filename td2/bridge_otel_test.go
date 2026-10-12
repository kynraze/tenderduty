package tenderduty

import (
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	metricsv1 "go.opentelemetry.io/proto/otlp/metrics/v1"
)

func bridgeExport(namespace, service string, metrics ...*metricsv1.Metric) *metricsv1.MetricsData {
	data := resourceData("bridge-peer", service, metrics...)
	rm := data.ResourceMetrics[0]
	rm.Resource.Attributes = append(rm.Resource.Attributes, latencyAttr("service.namespace", namespace))
	return data
}

func bridgeCounter(name string, start uint64, value int64, status string) *metricsv1.Metric {
	p := runtimePoint(value, latencyAttr("protocol", "shrex"))
	if status != "" {
		p.Attributes = append(p.Attributes, latencyAttr("status", status))
	}
	p.StartTimeUnixNano = start
	return &metricsv1.Metric{Name: name, Data: &metricsv1.Metric_Sum{Sum: &metricsv1.Sum{IsMonotonic: true, AggregationTemporality: metricsv1.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE, DataPoints: []*metricsv1.NumberDataPoint{p}}}}
}

func TestBridgeOTelIngestion(t *testing.T) {
	cc, _ := newBridgeFixture(t)
	now := time.Now()
	td.fibreData().now = func() time.Time { return now }
	cc.bridge = bridgeState{Loaded: true, Up: true}
	feed := func(start uint64, served, success, errors int64) {
		build := runtimePoint(1, latencyAttr("semantic_version", "v0.34.3-mocha"))
		build.Value = &metricsv1.NumberDataPoint_AsDouble{AsDouble: 1}
		td.ingestFibreMetrics(bridgeExport("mocha-5", "bRiDgE",
			runtimeGauge("build_info", build), runtimeGauge("node_start_ts", runtimePoint(now.Add(-time.Hour).Unix())),
			runtimeGauge("full_discovery_amount_of_peers", runtimePoint(4)), runtimeGauge("archival_discovery_amount_of_peers", runtimePoint(2)),
			bridgeCounter("shrex_payload_served_bytes", start, served, ""),
			bridgeCounter("shrex_server_total_responses", start, success, "success"), bridgeCounter("shrex_server_total_responses", start, errors, "internal_err")))
	}
	feed(1, 1000, 100, 10)
	if s := cc.bridge; s.Version != "v0.34.3-mocha" || s.FullPeers != 4 || s.ArchivalPeers != 2 || !s.PeersKnown || s.StartedAt.Unix() != now.Add(-time.Hour).Unix() || !s.MetricsAt.Equal(now) {
		t.Fatalf("state: %+v", s)
	}
	if len(td.reportData.Hours[cc.name]) != 0 {
		t.Fatal("first sample added counters")
	}
	feed(1, 1500, 110, 15)
	feed(1, 100, 2, 1)
	feed(2, 200, 3, 2)
	total := reportTotals(td.reportData.Hours[cc.name], now.Add(-time.Hour), now.Add(time.Second))
	if total.BridgeServed != 800 || total.BridgeResponses != 23 || total.BridgeErrors != 8 {
		t.Fatalf("deltas: %+v", total)
	}
	restored := &Config{}
	restored.restoreReports(td.savedReports(now), now)
	if got := restored.reportData.Hours[cc.name][0]; got.BridgeServed != 800 || got.BridgeErrors != 8 {
		t.Fatalf("restored: %+v", got)
	}
}

func TestBridgeOTelRouting(t *testing.T) {
	cc, _ := newBridgeFixture(t)
	st := td.fibreData()
	for i := 0; i < 2; i++ {
		td.ingestFibreMetrics(bridgeExport("unknown", "Bridge"))
	}
	// noted once, so it is logged once
	if len(st.ignored) != 1 || st.noteIgnored("bridge:unknown\x00bridge-peer") {
		t.Fatalf("ignored keys=%d", len(st.ignored))
	}
	for _, service := range []string{"Light", "Full"} {
		td.ingestFibreMetrics(bridgeExport("mocha-5", service, runtimeGauge("full_discovery_amount_of_peers", runtimePoint(0))))
	}
	if !cc.bridge.MetricsAt.IsZero() {
		t.Fatal("Light/Full ingested")
	}
	cc.Bridge.URL = ""
	td.ingestFibreMetrics(bridgeExport("mocha-5", "Bridge"))
	if !cc.bridge.MetricsAt.IsZero() {
		t.Fatal("unconfigured bridge ingested")
	}
}

func TestBridgeOTelAlarms(t *testing.T) {
	cc, _ := newBridgeFixture(t)
	now := time.Now()
	cc.bridge = bridgeState{Loaded: true, Up: true, MetricsAt: now, PeersKnown: true, FullPeers: 1, NoPeersSince: now.Add(-4 * time.Minute)}
	cc.bridgeAlarms(false)
	if len(activeMessages(cc.name, "Bridge ")) != 0 {
		t.Fatal("peers alerted before window")
	}
	cc.bridge.NoPeersSince = now.Add(-6 * time.Minute)
	cc.bridgeAlarms(false)
	noPeers := "Bridge on mocha-5 has too few full peers\n1 full peer"
	if !alarmIsActive(cc.name, noPeers) {
		t.Fatal("missing peers alarm")
	}
	if _, label := bridgeLabel(&alertMsg{message: noPeers}); label != "Bridge too few peers" {
		t.Fatal(label)
	}
	card := (&alertMsg{chain: cc.name, message: noPeers, severity: "warning"}).card()
	if !card.known() || len(card.Fields) < 3 || card.Fields[1] != row("\U0001f309", "Bridge", dotRed+" too few full peers") || card.Fields[2] != row("", "Peers", "1 full peer") {
		t.Fatalf("peers card: %+v", card)
	}
	cc.bridge.FullPeers = 0
	cc.bridgeAlarms(false)
	if !alarmIsActive(cc.name, noPeers) || len(activeMessages(cc.name, "Bridge ")) != 1 {
		t.Fatal("active peers message changed")
	}
	cc.bridge.FullPeers = 2
	cc.bridgeAlarms(false)
	if alarmIsActive(cc.name, noPeers) {
		t.Fatal("peers failed to recover")
	}
	if r, ok := storedResolution(cc.name, noPeers); !ok || r.text != "Bridge on mocha-5 has enough full peers again" {
		t.Fatal("peers resolution")
	}
	card = (&alertMsg{chain: cc.name, message: noPeers, resolved: true}).card()
	if !card.known() || len(card.Fields) != 2 || card.Fields[1] != row("\U0001f309", "Bridge", dotGreen+" enough full peers again") {
		t.Fatalf("resolved peers card: %+v", card)
	}
	cc.bridge.responses = []bridgeDelta{{at: now, responses: 20, errors: 8}}
	cc.bridgeAlarms(false)
	if len(activeMessages(cc.name, "Bridge ")) != 0 {
		t.Fatal("40% errors raised serving alarm")
	}
	cc.bridge.responses = []bridgeDelta{{at: now, responses: 19, errors: 10}}
	cc.bridgeAlarms(false)
	if len(activeMessages(cc.name, "Bridge ")) != 0 {
		t.Fatal("19 responses alerted")
	}
	cc.bridge.responses[0].responses = 20
	cc.bridgeAlarms(false)
	var serving string
	for message := range activeMessages(cc.name, "Bridge ") {
		if bridgeKind(message) == "serving" {
			serving = message
		}
	}
	if serving == "" || !strings.Contains(serving, "50% of 20 requests failed") {
		t.Fatal("missing serving alarm/share")
	}
	card = (&alertMsg{chain: cc.name, message: serving, severity: "warning"}).card()
	if !card.known() || len(card.Fields) < 4 || card.Fields[2] != row("", "Requests", "50% of 20 requests failed") || card.Fields[3] != row("", "Check", "disk and bridge logs") {
		t.Fatalf("serving card: %+v", card)
	}
	for _, errors := range []int64{8, 5} {
		cc.bridge.responses[0].errors = errors
		cc.bridgeAlarms(false)
		if !alarmIsActive(cc.name, serving) || len(activeMessages(cc.name, "Bridge ")) != 1 {
			t.Fatal("serving alarm resolved at or above 25%")
		}
	}
	cc.bridge.responses[0].errors = 4
	cc.bridgeAlarms(false)
	if alarmIsActive(cc.name, serving) {
		t.Fatal("serving failed to recover")
	}
	if r, ok := storedResolution(cc.name, serving); !ok || r.text != "Bridge on mocha-5 serves data again" {
		t.Fatal("serving resolution")
	}
	for _, down := range []bool{false, true} {
		cc.bridge.MetricsAt, cc.bridge.Up = time.Now(), true
		cc.bridge.responses[0].errors = 10
		cc.bridgeAlarms(false)
		for len(td.alertChan) > 0 {
			<-td.alertChan
		}
		if down {
			cc.bridge.Up = false
		} else {
			cc.bridge.MetricsAt = now.Add(-3 * time.Minute)
		}
		cc.bridgeAlarms(false)
		if len(activeMessages(cc.name, "Bridge ")) != 0 {
			t.Fatal("stale/down serving stayed active")
		}
		for len(td.alertChan) > 0 {
			if a := <-td.alertChan; !a.resolved || !a.quiet {
				t.Fatalf("stale/down serving must resolve quietly: %+v", a)
			}
		}
	}
}

func TestBridgePeersOldText(t *testing.T) {
	cc, _ := newBridgeFixture(t)
	message := "Bridge on mocha-5 has no full peers"
	if bridgeKind(message) != "no_peers" {
		t.Fatal("old peers alarm kind")
	}
	card := (&alertMsg{chain: cc.name, message: message, severity: "warning"}).card()
	if !card.known() || len(card.Fields) < 2 || card.Fields[1] != row("\U0001f309", "Bridge", dotRed+" too few full peers") {
		t.Fatalf("old peers card: %+v", card)
	}
	card = (&alertMsg{chain: cc.name, message: message, resolved: true}).cardFrom("Bridge on mocha-5 has full peers again")
	if !card.known() || len(card.Fields) != 2 || card.Fields[1] != row("\U0001f309", "Bridge", dotGreen+" enough full peers again") {
		t.Fatalf("old resolved peers card: %+v", card)
	}
	now := time.Now()
	cc.bridge = bridgeState{Loaded: true, Up: true, MetricsAt: now, PeersKnown: true, FullPeers: 1, NoPeersSince: now.Add(-6 * time.Minute)}
	id := cc.bridgeAlarmID(message)
	td.alert(cc.name, message, cc.Alerts.BridgeSeverity, false, &id)
	cc.bridgeAlarms(false)
	if !alarmIsActive(cc.name, message) || len(activeMessages(cc.name, "Bridge ")) != 1 {
		t.Fatal("old active peers message changed")
	}
	cc.bridge.FullPeers = 2
	cc.bridgeAlarms(false)
	if alarmIsActive(cc.name, message) {
		t.Fatal("old peers alarm failed to recover")
	}
	if r, ok := storedResolution(cc.name, message); !ok || r.label != "Bridge peers again" || r.text != "Bridge on mocha-5 has enough full peers again" {
		t.Fatal("old peers resolution")
	}
}

func TestBridgeServingErrorKinds(t *testing.T) {
	for _, status := range []string{"success", "not_found", "rate_limited", "resource_exhausted", "read_req_err", "bad_request", "send_resp_err", "send_status_err", "internal_err", "unknown", ""} {
		t.Run(status, func(t *testing.T) {
			cc, _ := newBridgeFixture(t)
			now := time.Now()
			td.fibreData().now = func() time.Time { return now }
			cc.bridge = bridgeState{Loaded: true, Up: true}
			for _, value := range []int64{100, 102} {
				td.ingestFibreMetrics(bridgeExport("mocha-5", "Bridge", bridgeCounter("shrex_server_total_responses", 1, value, status)))
			}
			want := int64(0)
			if status == "internal_err" {
				want = 2
			}
			if responses, errors := cc.bridge.servingWindow(now, 5*time.Minute); responses != 2 || errors != want {
				t.Fatalf("state deltas: responses=%d errors=%d", responses, errors)
			}
			total := reportTotals(td.reportData.Hours[cc.name], now.Add(-time.Hour), now.Add(time.Second))
			if total.BridgeResponses != 2 || total.BridgeErrors != want {
				t.Fatalf("report deltas: %+v", total)
			}
		})
	}
}

func TestBridgeOTelStatusAndPrometheus(t *testing.T) {
	cc, _ := newBridgeFixture(t)
	now := time.Now()
	cc.bridge = bridgeState{Loaded: true, Up: true, Head: 100, Sampled: 100, MetricsAt: now, Version: "v0.34.3", StartedAt: now.Add(-time.Hour), PeersKnown: true, FullPeers: 4, ArchivalPeers: 2}
	text, tone := cc.bridgeStatus()
	if !strings.Contains(text, "sampled 100 \u00b7 v0.34.3 \u00b7 4 full / 2 archival peers \u00b7 up ") || tone != "ok" {
		t.Fatalf("status %q %q", text, tone)
	}
	registry := prometheus.NewRegistry()
	registry.MustRegister(&bridgeCollector{config: td})
	families, err := registry.Gather()
	if err != nil || len(families) != 8 {
		t.Fatalf("fresh metrics %d %v", len(families), err)
	}
	cc.bridge.MetricsAt = now.Add(-3 * time.Minute)
	text, _ = cc.bridgeStatus()
	if text != "head 100 (0 behind) \u00b7 sampled 100" {
		t.Fatal(text)
	}
	families, err = registry.Gather()
	if err != nil || len(families) != 4 {
		t.Fatalf("stale metrics %d %v", len(families), err)
	}
}

func TestBridgeOTelReportRows(t *testing.T) {
	cc, _ := newBridgeFixture(t)
	now := time.Now()
	cc.valInfo = &ValInfo{Moniker: "Bridge", Bonded: true}
	cc.bridge = bridgeState{MetricsAt: now, Version: "v0.34.3", PeersKnown: true, FullPeers: 4, ArchivalPeers: 2}
	for _, full := range []bool{false, true} {
		cc.valInfo.Jailed = full
		report := td.buildReport(now, false, false, nil)
		if len(report.Rows) != 1 || report.Rows[0].Compact == full {
			t.Fatal("incorrect row layout")
		}
		found := false
		for _, line := range report.Rows[0].Lines {
			if strings.HasPrefix(line, "\U0001f309 Bridge v0.34.3") && strings.Contains(line, "4 full / 2 archival peers") {
				found = true
			}
		}
		if !found {
			t.Fatalf("bridge line missing: %+v", report.Rows[0])
		}
	}
	report := td.buildReport(now, true, false, nil)
	if !strings.Contains(strings.Join(report.Rows[0].Lines, "\n"), "\U0001f309 served ") {
		t.Fatal("weekly bridge missing")
	}
	cc.bridge.MetricsAt = now.Add(-3 * time.Minute)
	td.reportsMux.Lock()
	td.reportHourLocked(cc.name, now).BridgeServed = 1024
	td.reportsMux.Unlock()
	line := cc.reportBridge(reportHour{BridgeServed: 1024}, now, false)
	if strings.Contains(line, "v0.34.3") || !strings.Contains(line, "served ") {
		t.Fatal(line)
	}
}

func TestBridgeOTelPeerContinuityAndErrorKinds(t *testing.T) {
	cc, _ := newBridgeFixture(t)
	now := time.Now()
	td.fibreData().now = func() time.Time { return now }
	cc.bridge = bridgeState{Loaded: true, Up: true}
	lowPeers := bridgeExport("mocha-5", "Bridge", runtimeGauge("full_discovery_amount_of_peers", runtimePoint(1)))
	td.ingestFibreMetrics(lowPeers)
	first := cc.bridge.NoPeersSince
	now = now.Add(time.Minute)
	td.ingestFibreMetrics(lowPeers)
	if !cc.bridge.NoPeersSince.Equal(first) {
		t.Fatal("continuous low peers reset timer")
	}
	now = now.Add(3 * time.Minute)
	td.ingestFibreMetrics(lowPeers)
	if !cc.bridge.NoPeersSince.Equal(now) {
		t.Fatal("stale gap did not reset timer")
	}
	td.ingestFibreMetrics(bridgeExport("mocha-5", "Bridge", runtimeGauge("full_discovery_amount_of_peers", runtimePoint(2))))
	if !cc.bridge.NoPeersSince.IsZero() {
		t.Fatal("two peers did not clear timer")
	}
	statuses := []string{"success", "not_found", "rate_limited", "resource_exhausted", "internal_err", "read_req_err", "bad_request", "send_status_err", "send_resp_err"}
	for _, value := range []int64{100, 102} {
		var metrics []*metricsv1.Metric
		for _, status := range statuses {
			metrics = append(metrics, bridgeCounter("shrex_server_total_responses", 1, value, status))
		}
		td.ingestFibreMetrics(bridgeExport("mocha-5", "Bridge", metrics...))
	}
	total := reportTotals(td.reportData.Hours[cc.name], now.Add(-time.Hour), now.Add(time.Second))
	if total.BridgeResponses != 18 || total.BridgeErrors != 2 {
		t.Fatalf("status deltas: %+v", total)
	}
	if responses, errors := cc.bridge.servingWindow(now, 5*time.Minute); responses != 18 || errors != 2 {
		t.Fatalf("state deltas: responses=%d errors=%d", responses, errors)
	}
	td.ingestFibreMetrics(bridgeExport("mocha-5", "Bridge", runtimeGauge("build_info", runtimePoint(1, latencyAttr("semantic_version", strings.Repeat("a", 100))))))
	if len(cc.bridge.Version) != 64 {
		t.Fatal("version not clipped")
	}
}

func bridgeHistogram(name string, count uint64) *metricsv1.Metric {
	return &metricsv1.Metric{Name: name, Data: &metricsv1.Metric_Histogram{Histogram: &metricsv1.Histogram{DataPoints: []*metricsv1.HistogramDataPoint{{Count: count}}}}}
}

func TestBridgePrunerIngestion(t *testing.T) {
	cc, _ := newBridgeFixture(t)
	now := time.Now()
	td.fibreData().now = func() time.Time {
		return now
	}
	cc.bridge = bridgeState{Loaded: true, Up: true, Head: 2000}
	feed := func(last, failed int64, metrics ...*metricsv1.Metric) {
		metrics = append(metrics, runtimeGauge("prnr_last_pruned", runtimePoint(last)), runtimeGauge("prnr_failed_prunes_amount", runtimePoint(failed)))
		td.ingestFibreMetrics(bridgeExport("mocha-5", "Bridge", metrics...))
	}
	feed(1000, 3, bridgeHistogram("eds_store_remove_q4_time_histogram", 0))
	if s := cc.bridge; !s.PrunerKnown || s.ModeKnown || s.LastPruned != 1000 || s.FailedPrunes != 3 || !s.PrunedAt.Equal(now) || s.PrunedHead != 2000 || !s.FailingSince.Equal(now) {
		t.Fatalf("first sample: %+v", s)
	}
	first := now
	now = now.Add(time.Minute)
	feed(1000, 2, bridgeHistogram("eds_store_remove_q4_time_histogram", 1))
	if s := cc.bridge; !s.ModeKnown || !s.Archival || !s.PrunedAt.Equal(first) || !s.FailingSince.Equal(first) {
		t.Fatalf("archival: %+v", s)
	}
	cc.bridge.Head = 2100
	feed(1100, 0, bridgeHistogram("eds_store_remove_odsq4_time_histogram", 1), bridgeHistogram("eds_store_remove_q4_time_histogram", 1))
	if s := cc.bridge; s.Archival || !s.ModeKnown || !s.PrunedAt.Equal(now) || s.PrunedHead != 2100 || !s.FailingSince.IsZero() {
		t.Fatalf("pruned: %+v", s)
	}
	feed(1100, 0, bridgeHistogram("eds_store_remove_q4_time_histogram", 1))
	if cc.bridge.Archival {
		t.Fatal("archival overwrote pruned mode")
	}
	registry := prometheus.NewRegistry()
	registry.MustRegister(&bridgeCollector{config: td})
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, f := range families {
		switch f.GetName() {
		case "tenderduty_bridge_last_pruned":
			found++
			if f.Metric[0].Gauge.GetValue() != 1100 {
				t.Fatal("last pruned")
			}
		case "tenderduty_bridge_failed_prunes", "tenderduty_bridge_archival":
			found++
			if f.Metric[0].Gauge.GetValue() != 0 {
				t.Fatal(f.GetName())
			}
		}
	}
	if found != 3 {
		t.Fatal("missing pruning metrics")
	}
	cc.bridge.MetricsAt = now.Add(-4 * time.Minute)
	families, err = registry.Gather()
	if err != nil || len(families) != 4 {
		t.Fatalf("stale: %d %v", len(families), err)
	}
}

func TestBridgeFacts(t *testing.T) {
	cc, _ := newBridgeFixture(t)
	now := time.Now()
	cc.blockTime = 6 * time.Second
	cc.bridge = bridgeState{Loaded: true, Up: true, Running: true, Head: 2000, Sampled: 1900, MetricsAt: now, Version: "0.34.3", PrunerKnown: true, LastPruned: 1000, FailedPrunes: 2}
	td.reportsMux.Lock()
	h := td.reportHourLocked(cc.name, now)
	h.BridgeServed, h.BridgeResponses, h.BridgeErrors = 1024, 100, 25
	td.reportsMux.Unlock()
	title, facts := cc.bridgeFacts(now)
	if title != "Bridge v0.34.3" {
		t.Fatal(title)
	}
	find := func(label string) bridgeFact {
		_, facts := cc.bridgeFacts(now)
		for _, f := range facts {
			if f.Label == label {
				return f
			}
		}
		t.Fatalf("missing %s: %+v", label, facts)
		return bridgeFact{}
	}
	if f := find("Pruned"); !strings.Contains(f.Value, "up to 1,000") || !strings.Contains(f.Value, "keeps ~2h") || !strings.Contains(f.Value, "2 failed") || f.Tone != "warning" {
		t.Fatalf("pruned: %+v", f)
	}
	for _, peers := range []int64{0, 1, 2} {
		cc.bridge.FullPeers = peers
		want := "ok"
		if peers < 2 {
			want = "warning"
		}
		if f := find("Peers"); f.Tone != want {
			t.Fatalf("peers=%d: %+v", peers, f)
		}
	}
	if f := find("Served (24h)"); !strings.Contains(f.Value, "25% errors") {
		t.Fatal(f)
	}
	cc.bridge.Archival, cc.bridge.ModeKnown = true, true
	if f := find("Archival"); !strings.Contains(f.Value, "Q4 trimmed up to 1,000") {
		t.Fatal(f)
	}
	cc.bridge.LastPruned = 0
	if f := find("Archival"); !strings.Contains(f.Value, "nothing trimmed yet") {
		t.Fatal(f)
	}
	cc.bridge.Archival = false
	if f := find("Pruned"); !strings.Contains(f.Value, "nothing yet, keeps the last 7d 1h") {
		t.Fatal(f)
	}
	detail := cc.statusDetail(cc.name, now)
	if !strings.Contains(detail, "\U0001f309 <b>Bridge</b> <code>v0.34.3</code>") || strings.Contains(detail, "<b>Bridge:</b>") {
		t.Fatal(detail)
	}
	dashboard := cc.dashboardStatus().Bridge
	if dashboard == nil || dashboard.Title != title || len(dashboard.Facts) != len(facts) {
		t.Fatalf("dashboard: %+v", dashboard)
	}
	cc.bridge.Up, cc.bridge.Reason = false, "connection failed"
	_, facts = cc.bridgeFacts(now)
	if len(facts) != 1 || facts[0].Label != "Reachable" || facts[0].Tone != "critical" {
		t.Fatal(facts)
	}
	cc.bridge.TokenRejected = true
	_, facts = cc.bridgeFacts(now)
	if len(facts) != 1 || facts[0].Label != "Token" {
		t.Fatal(facts)
	}
	cc.bridge.Loaded = false
	_, facts = cc.bridgeFacts(now)
	if len(facts) != 1 || facts[0].Label != "Status" {
		t.Fatal(facts)
	}
	cc.Bridge.URL = ""
	title, facts = cc.bridgeFacts(now)
	if title != "" || len(facts) != 0 {
		t.Fatal(title, facts)
	}
}

func TestBridgePruningAlarms(t *testing.T) {
	cc, _ := newBridgeFixture(t)
	now := time.Now()
	td.fibreData().now = func() time.Time {
		return now
	}
	old := now.Add(-bridgePruneWindow - time.Minute)
	cc.bridge = bridgeState{Loaded: true, Up: true, MetricsAt: now, PrunerKnown: true, LastPruned: 1000, Head: 2000, PrunedHead: 2000, PrunedAt: old}
	stalled := "Bridge on mocha-5 stopped pruning\nstuck at 1,000"
	failing := "Bridge on mocha-5 is failing to prune\n3 blocks failed"
	cc.bridgeAlarms(false)
	if len(activeMessages(cc.name, "Bridge ")) != 0 {
		t.Fatal("halted chain alerted")
	}
	cc.bridge.Head++
	cc.bridgeAlarms(false)
	if !alarmIsActive(cc.name, stalled) {
		t.Fatal("stalled alarm missing")
	}
	if _, tone := cc.bridgeStatus(); tone != "warning" {
		t.Fatal(tone)
	}
	if icon, label := bridgeLabel(&alertMsg{message: stalled}); icon == "" || label != "Bridge pruning stalled" {
		t.Fatal(label)
	}
	for _, message := range []string{stalled, failing} {
		card := (&alertMsg{chain: cc.name, message: message, severity: "warning"}).card()
		if !card.known() || !strings.Contains(strings.Join(card.telegramLines(), "\n"), "Why") {
			t.Fatal("pruning card missing")
		}
	}
	td.ingestFibreMetrics(bridgeExport("mocha-5", "Bridge", runtimeGauge("prnr_last_pruned", runtimePoint(1001))))
	cc.bridgeAlarms(false)
	if alarmIsActive(cc.name, stalled) {
		t.Fatal("stalled not resolved")
	}
	if r, ok := storedResolution(cc.name, stalled); !ok || r.text != "Bridge on mocha-5 is pruning again" {
		t.Fatal("stalled resolution")
	}
	cc.bridge.FailedPrunes, cc.bridge.FailingSince = 3, now
	cc.bridgeAlarms(false)
	if alarmIsActive(cc.name, failing) {
		t.Fatal("failing alerted too early")
	}
	cc.bridge.FailingSince = old
	cc.bridgeAlarms(false)
	if !alarmIsActive(cc.name, failing) {
		t.Fatal("failing alarm missing")
	}
	cc.bridge.FailedPrunes = 4
	cc.bridgeAlarms(false)
	if !alarmIsActive(cc.name, failing) || len(activeMessages(cc.name, "Bridge ")) != 1 {
		t.Fatal("active message changed")
	}
	td.ingestFibreMetrics(bridgeExport("mocha-5", "Bridge", runtimeGauge("prnr_failed_prunes_amount", runtimePoint(0))))
	cc.bridgeAlarms(false)
	if alarmIsActive(cc.name, failing) {
		t.Fatal("failing not resolved")
	}
	if r, ok := storedResolution(cc.name, failing); !ok || r.text != "Bridge on mocha-5 prunes again" {
		t.Fatal("failing resolution")
	}
	for _, down := range []bool{false, true} {
		cc.bridge.Up, cc.bridge.MetricsAt = true, now
		cc.bridge.LastPruned, cc.bridge.PrunedAt, cc.bridge.PrunedHead = 1000, old, 2000
		cc.bridge.FailedPrunes, cc.bridge.FailingSince = 3, old
		cc.bridgeAlarms(false)
		if len(activeMessages(cc.name, "Bridge ")) != 2 {
			t.Fatal("missing pruning alarms")
		}
		for len(td.alertChan) > 0 {
			<-td.alertChan
		}
		if down {
			cc.bridge.Up = false
		} else {
			cc.bridge.MetricsAt = now.Add(-3 * time.Minute)
		}
		cc.bridgeAlarms(false)
		if len(activeMessages(cc.name, "Bridge ")) != 0 {
			t.Fatal("stale/down pruning stayed active")
		}
		for len(td.alertChan) > 0 {
			if a := <-td.alertChan; !a.resolved || !a.quiet {
				t.Fatalf("must resolve quietly: %+v", a)
			}
		}
	}
}

func TestBridgeMetricsStopped(t *testing.T) {
	for _, tc := range []struct {
		name   string
		age    time.Duration
		up, on bool
		want   bool
	}{
		{"stopped", 6 * time.Minute, true, true, true},
		{"before window", 4 * time.Minute, true, true, false},
		{"never received", 0, true, true, false},
		{"down", 6 * time.Minute, false, true, false},
		{"disabled", 6 * time.Minute, true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cc, _ := newBridgeFixture(t)
			cc.Alerts.BridgeAlerts = tc.on
			cc.bridge = bridgeState{Loaded: true, Up: tc.up}
			if tc.age > 0 {
				cc.bridge.MetricsAt = time.Now().Add(-tc.age)
			}
			cc.bridgeAlarms(false)
			active := activeMessages(cc.name, "Bridge ")
			if !tc.want {
				if len(active) != 0 {
					t.Fatalf("unexpected alarms: %v", active)
				}
				return
			}
			message := "Bridge on mocha-5 stopped sending metrics\nlast received " + cc.bridge.MetricsAt.In(alertLocation()).Format("15:04")
			if len(active) != 1 || !active[message] || bridgeKind(message) != "metrics_stopped" || !bridgeMetricKind(bridgeKind(message)) {
				t.Fatalf("missing metrics alarm: %v", active)
			}
			if a := <-td.alertChan; a.resolved || a.severity != cc.Alerts.BridgeSeverity {
				t.Fatalf("raised alarm: %+v", a)
			}
			if _, label := bridgeLabel(&alertMsg{message: message}); label != "Bridge metrics stopped" {
				t.Fatal(label)
			}
			card := (&alertMsg{chain: cc.name, message: message, severity: "warning"}).card()
			if !card.known() || len(card.Fields) < 3 || card.Fields[1] != row("\U0001f309", "Metrics", dotRed+" not received since "+cc.bridge.MetricsAt.In(alertLocation()).Format("15:04")) || card.Fields[2] != row("", "Why", "OTel alerts (peers, serving, pruning) are paused") {
				t.Fatalf("raised card: %+v", card)
			}
		})
	}
}

func TestBridgeMetricsStoppedAfterRestart(t *testing.T) {
	for _, down := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh", true: "down"}[down], func(t *testing.T) {
			cc, _ := newBridgeFixture(t)
			message := "Bridge on mocha-5 stopped sending metrics\nlast received 12:00"
			alarms.AllAlarms[cc.name] = map[string]time.Time{message: time.Now().Add(-time.Hour)}
			cc.bridge = bridgeState{Loaded: true, Up: true}
			cc.bridgeAlarms(false)
			if !alarmIsActive(cc.name, message) || len(td.alertChan) != 0 {
				t.Fatal("restored metrics alarm resolved before metrics arrived")
			}
			_, facts := cc.bridgeFacts(time.Now())
			found := false
			for _, f := range facts {
				if f.Label == "Metrics" {
					found = true
					if f.Tone != "warning" || !strings.HasPrefix(f.Value, "not received") {
						t.Fatalf("metrics fact: %+v", f)
					}
				}
			}
			if !found {
				t.Fatal("missing metrics fact")
			}
			if down {
				cc.bridge.Up = false
			} else {
				td.ingestFibreMetrics(bridgeExport("mocha-5", "Bridge"))
			}
			cc.bridgeAlarms(false)
			if alarmIsActive(cc.name, message) || len(td.alertChan) != 1 {
				t.Fatal("restored metrics alarm failed to resolve")
			}
			if a := <-td.alertChan; !a.resolved || a.quiet != down {
				t.Fatalf("recovery: %+v", a)
			}
			if !down {
				if r, ok := storedResolution(cc.name, message); !ok || r.text != "Bridge on mocha-5 sends metrics again" {
					t.Fatalf("resolution: %+v", r)
				}
			}
		})
	}
}

func TestBridgeMetricsStoppedRecovery(t *testing.T) {
	for _, down := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh", true: "down"}[down], func(t *testing.T) {
			cc, _ := newBridgeFixture(t)
			cc.bridge = bridgeState{Loaded: true, Up: true, MetricsAt: time.Now().Add(-6 * time.Minute)}
			cc.bridgeAlarms(false)
			var message string
			for active := range activeMessages(cc.name, "Bridge ") {
				message = active
			}
			if message == "" {
				t.Fatal("missing metrics alarm")
			}
			for len(td.alertChan) > 0 {
				<-td.alertChan
			}
			if down {
				cc.bridge.Up = false
			} else {
				td.ingestFibreMetrics(bridgeExport("mocha-5", "Bridge"))
			}
			cc.bridgeAlarms(false)
			if alarmIsActive(cc.name, message) || len(td.alertChan) != 1 {
				t.Fatal("metrics alarm failed to resolve")
			}
			if a := <-td.alertChan; !a.resolved || a.quiet != down {
				t.Fatalf("recovery: %+v", a)
			}
			if down {
				return
			}
			r, ok := storedResolution(cc.name, message)
			if !ok || r != (resolution{"\u2705", "Bridge metrics", "Bridge on mocha-5 sends metrics again"}) {
				t.Fatalf("resolution: %+v", r)
			}
			if icon, label := bridgeLabel(&alertMsg{message: message, resolved: true}); icon != r.icon || label != r.label {
				t.Fatalf("label: %q %q", icon, label)
			}
			card := (&alertMsg{chain: cc.name, message: message, resolved: true}).card()
			if !card.known() || len(card.Fields) != 2 || card.Fields[1] != row("\U0001f309", "Metrics", dotGreen+" received again") {
				t.Fatalf("resolved card: %+v", card)
			}
		})
	}
}
