package tenderduty

import (
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Scrapes snapshot the receiver; deltas accumulate between scrapes.
type fibreCollector struct {
	config *Config
}

var fibrePromLabels = map[string]string{
	"endorsements_total":              "result",
	"prune_entries_total":             "",
	"storage_backend_bytes_total":     "backend",
	"storage_backend_in_flight":       "backend",
	"go_memory_used_bytes":            "",
	"go_memory_heap_bytes":            "",
	"go_memory_stack_bytes":           "",
	"go_memory_limit_bytes":           "",
	"go_gc_goal_bytes":                "",
	"go_goroutines":                   "",
	"go_processor_limit":              "",
	"go_gogc":                         "",
	"go_memory_allocated_bytes_total": "",
	"go_memory_allocations_total":     "",
	"upload_seconds":                  "",
	"download_seconds":                "",
	"sign_seconds":                    "",
	"store_put_seconds":               "",
	"store_get_seconds":               "",
	"prune_seconds":                   "",
	"go_schedule_seconds":             "",

	"rows":                           "",
	"rows_total":                     "",
	"row_share":                      "",
	"chain_budget_bytes":             "",
	"budget_bytes":                   "",
	"occupancy_bytes":                "",
	"registered":                     "",
	"reachable":                      "",
	"identity_verified":              "",
	"metrics_age_seconds":            "",
	"uploads_in_flight":              "",
	"downloads_in_flight":            "",
	"last_upload_timestamp_seconds":  "",
	"uploads_total":                  "result",
	"upload_bytes_total":             "",
	"downloads_total":                "result",
	"download_bytes_total":           "",
	"signatures_total":               "result",
	"signatures_fast_total":          "",
	"sign_seconds_total":             "",
	"store_errors_total":             "op",
	"storage_backend_requests_total": "backend,outcome",
}

func (*fibreCollector) Describe(ch chan<- *prometheus.Desc) {
	for suffix, extra := range fibrePromLabels {
		labels := []string{"name", "chain_id", "moniker"}
		if extra != "" {
			labels = append(labels, strings.Split(extra, ",")...)
		}
		ch <- prometheus.NewDesc("tenderduty_fibre_"+suffix, "Fibre "+suffix, labels, nil)
	}
}

func (fc *fibreCollector) Collect(ch chan<- prometheus.Metric) {
	c := fc.config
	c.chainsMux.RLock()
	defer c.chainsMux.RUnlock()
	now := time.Now()
	for key, cc := range c.Chains {
		if cc == nil {
			continue
		}
		r := cc.fibreReportFrom(now, c.fibreData())
		if r == nil {
			continue
		}
		name := cc.name
		if name == "" {
			name = key
		}
		info, _ := cc.validatorState()
		labels := []string{name, cc.ChainId, info.Moniker}
		emit := func(suffix string, value float64, kind prometheus.ValueType, extra ...string) {
			names := []string{"name", "chain_id", "moniker"}
			values := append([]string(nil), labels...)
			for i := 0; i+1 < len(extra); i += 2 {
				names = append(names, extra[i])
				values = append(values, extra[i+1])
			}
			desc := prometheus.NewDesc("tenderduty_fibre_"+suffix, "Fibre "+suffix, names, nil)
			ch <- prometheus.MustNewConstMetric(desc, kind, value, values...)
		}
		gauge := func(suffix string, value float64) { emit(suffix, value, prometheus.GaugeValue) }
		boolean := func(suffix string, value bool) {
			v := 0.0
			if value {
				v = 1
			}
			gauge(suffix, v)
		}
		if !r.Pending {
			boolean("registered", r.State.Registered)
			boolean("reachable", r.State.Reachable)
			if r.State.KeyKnown {
				boolean("identity_verified", r.State.IdentityOK)
			}
		}
		gauge("rows_total", fibreOriginalRows)
		if r.ShareKnown {
			gauge("rows", float64(r.Rows))
			gauge("row_share", float64(r.Rows)/fibreOriginalRows)
		}
		if r.ShareKnown && (r.Budget > 0 || r.Rows == 0) {
			gauge("chain_budget_bytes", float64(r.Budget))
		}
		st := c.fibreData()
		st.mu.Lock()
		endorsements := make(map[string]float64)
		addFibreValues(endorsements, st.totals[cc.name])
		st.mu.Unlock()
		for _, result := range []string{"endorsed", "missed", "unknown"} {
			emit("endorsements_total", endorsements["endorsements_"+result], prometheus.CounterValue, "result", result)
		}
		m := r.Metrics
		if !m.Found {
			continue
		}
		gauge("metrics_age_seconds", max(now.Sub(m.LastReceived).Seconds(), 0))
		if m.HaveBudget {
			gauge("budget_bytes", float64(m.Budget))
		}
		if m.HaveOccupancy {
			gauge("occupancy_bytes", float64(m.Occupancy))
		}
		if m.HaveSuccess {
			gauge("last_upload_timestamp_seconds", float64(m.LastSuccess.Unix()))
		}
		st.mu.Lock()
		var sender *fibreSender
		for _, s := range st.senders {
			if s.chain == cc.name && (sender == nil || s.lastReceived.After(sender.lastReceived)) {
				sender = s
			}
		}
		resources := make(map[string]float64)
		var uploadsNow, downloadsNow int64
		var haveUploads, haveDownloads bool
		if sender != nil {
			addFibreValues(resources, sender.resources)
			uploadsNow, downloadsNow = sender.uploadsNow, sender.downloadsNow
			haveUploads, haveDownloads = sender.haveUploads, sender.haveDownloads
		}
		totals := make(map[string]float64)
		addFibreValues(totals, st.totals[cc.name])
		for backend, b := range st.backendTotals[cc.name] {
			if b.haveBytes {
				emit("storage_backend_bytes_total", b.bytes, prometheus.CounterValue, "backend", backend)
			}
			for outcome, v := range b.outcomes {
				emit("storage_backend_requests_total", v, prometheus.CounterValue, "backend", backend, "outcome", outcome)
			}
			if sender != nil && sender.backends[backend] != nil && sender.backends[backend].haveInFlight {
				emit("storage_backend_in_flight", float64(sender.backends[backend].inFlight), prometheus.GaugeValue, "backend", backend)
			}
		}
		histograms := make(map[string]fibreHistogram)
		for op, h := range st.histograms[cc.name] {
			histograms[op] = h
		}
		st.mu.Unlock()
		for field, v := range resources {
			if field != "go_memory_no_limit" && field != "object_storage_in_flight" {
				gauge(field, v)
			}
		}
		for op, h := range histograms {
			buckets := make(map[float64]uint64)
			var count uint64
			for i, bound := range h.Bounds {
				count += h.Buckets[i]
				buckets[bound] = count
			}
			suffix := op + "_seconds"
			desc := prometheus.NewDesc("tenderduty_fibre_"+suffix, "Fibre "+suffix, []string{"name", "chain_id", "moniker"}, nil)
			ch <- prometheus.MustNewConstHistogram(desc, h.Count, h.Sum, buckets, labels...)
		}
		if haveUploads {
			gauge("uploads_in_flight", float64(uploadsNow))
		}
		if haveDownloads {
			gauge("downloads_in_flight", float64(downloadsNow))
		}
		counter := func(field, suffix string, extra ...string) {
			if v, ok := totals[field]; ok {
				emit(suffix, v, prometheus.CounterValue, extra...)
			}
		}
		counter("uploads_ok", "uploads_total", "result", "ok")
		counter("uploads_failed", "uploads_total", "result", "failed")
		counter("uploads_rejected", "uploads_total", "result", "rejected")
		counter("bytes_received", "upload_bytes_total")
		counter("downloads_ok", "downloads_total", "result", "ok")
		counter("downloads_failed", "downloads_total", "result", "failed")
		counter("bytes_served", "download_bytes_total")
		counter("prune_entries", "prune_entries_total")
		counter("go_memory_allocated_bytes", "go_memory_allocated_bytes_total")
		counter("go_memory_allocations", "go_memory_allocations_total")
		counter("signatures_ok", "signatures_total", "result", "ok")
		counter("signatures_failed", "signatures_total", "result", "failed")
		counter("signatures_fast", "signatures_fast_total")
		counter("sign_seconds", "sign_seconds_total")
		counter("store_put_errors", "store_errors_total", "op", "put")
		counter("store_get_errors", "store_errors_total", "op", "get")
		counter("prune_errors", "store_errors_total", "op", "prune")
	}
}
