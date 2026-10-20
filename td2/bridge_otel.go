package tenderduty

import (
	"fmt"
	"math"
	"strings"
	"time"

	metricsv1 "go.opentelemetry.io/proto/otlp/metrics/v1"
)

type bridgeSeries struct {
	value float64
	start uint64
}

var bridgeResponseStatuses = []string{"success", "not_found", "rate_limited", "internal_err", "peer_err", "other"}

func bridgeResponseGroup(status string) string {
	switch status {
	case "success", "not_found", "internal_err":
		return status
	case "rate_limited", "resource_exhausted":
		return "rate_limited"
	case "read_req_err", "bad_request", "send_status_err", "send_resp_err":
		return "peer_err"
	default:
		return "other"
	}
}

// Sum and count reset together, even when only one decreased.
func (s *bridgeState) histogramDelta(key string, dp *metricsv1.HistogramDataPoint) (float64, float64, bool) {
	sum, count := dp.GetSum(), float64(dp.GetCount())
	if dp.Sum == nil || !validBridgeValue(sum) {
		return 0, 0, false
	}
	if s.series == nil {
		s.series = make(map[string]bridgeSeries)
	}
	priorSum, sumKnown := s.series[key+"\x00sum"]
	priorCount, countKnown := s.series[key+"\x00count"]
	start := dp.GetStartTimeUnixNano()
	s.series[key+"\x00sum"] = bridgeSeries{value: sum, start: start}
	s.series[key+"\x00count"] = bridgeSeries{value: count, start: start}
	if !sumKnown || !countKnown {
		return 0, 0, false
	}
	if start == priorSum.start && start == priorCount.start && sum >= priorSum.value && count >= priorCount.value {
		sum -= priorSum.value
		count -= priorCount.value
	}
	return sum, count, true
}

type bridgeDelta struct {
	at                time.Time
	responses, errors int64
}

func (s bridgeState) metricsFresh(now time.Time) bool {
	return !s.MetricsAt.IsZero() && now.Sub(s.MetricsAt) <= 2*time.Minute
}

func (s bridgeState) servingWindow(now time.Time, window time.Duration) (responses, errors int64) {
	for _, d := range s.responses {
		if now.Sub(d.at) <= window {
			responses += d.responses
			errors += d.errors
		}
	}
	return
}

func (c *Config) ingestBridgeMetrics(rm *metricsv1.ResourceMetrics, now time.Time) string {
	attrs := rm.GetResource().GetAttributes()
	namespace, _ := fibreAttr(attrs, "service.namespace")
	instance, _ := fibreAttr(attrs, "service.instance.id")
	c.chainsMux.RLock()
	var cc *ChainConfig
	count := 0
	for _, chain := range c.Chains {
		if chain != nil && chain.ChainId == namespace && chain.Bridge.URL != "" {
			cc = chain
			count++
		}
	}
	c.chainsMux.RUnlock()
	// metrics don't say which bridge they come from; two candidates would mix them
	if count != 1 {
		reason := "no chain with that chain_id has bridge.url"
		if count > 1 {
			reason = "multiple chains with that chain_id have bridge.url"
		}
		if c.fibreData().noteIgnored("bridge:" + namespace + "\x00" + instance) {
			l(fmt.Sprintf("ignoring bridge metrics from %s on %s: %s", instance, namespace, reason))
		}
		return ""
	}
	window := time.Duration(cc.alertSettings().BridgeLagMinutes) * time.Minute
	cc.stateMux.Lock()
	s := &cc.bridge
	if !s.metricsFresh(now) || !s.Up {
		s.NoPeersSince = time.Time{}
	}
	if !s.metricsFresh(now) {
		// after a gap, pruning windows start over
		s.PrunedAt, s.PrunedHead = now, s.Head
		if !s.FailingSince.IsZero() {
			s.FailingSince = now
		}
	}
	s.MetricsAt = now
	if s.series == nil {
		s.series = make(map[string]bridgeSeries)
	}
	values := make(map[string]float64)
	version := ""
	responsesObserved := false
	prunerObserved := false
	d := bridgeDelta{at: now}
	var served float64
	for _, scope := range rm.GetScopeMetrics() {
		for _, m := range scope.GetMetrics() {
			if m.GetName() == "go.memory.used" || m.GetName() == "go.goroutine.count" {
				if v, ok := fibreRuntimeValue(m.GetName(), m); ok && validBridgeValue(v) {
					if m.GetName() == "go.memory.used" {
						values["memory_bytes"], values["memory_max_bytes"] = v, v
					} else {
						values["goroutines"], values["goroutines_max"] = v, v
					}
				}
			}
			for _, dp := range m.GetGauge().GetDataPoints() {
				value := fibreNumber(dp)
				if !validBridgeValue(value) {
					continue
				}
				switch m.GetName() {
				case "prnr_last_pruned":
					prunerObserved = true
					if !s.PrunerKnown || int64(value) > s.LastPruned {
						s.PrunedAt, s.PrunedHead = now, s.Head
					}
					s.LastPruned, s.PrunerKnown = int64(value), true
				case "prnr_failed_prunes_amount":
					if value > 0 && s.FailedPrunes == 0 {
						s.FailingSince = now
					} else if value == 0 {
						s.FailingSince = time.Time{}
					}
					s.FailedPrunes = int64(value)
					values["failed_prunes"] = value
				case "build_info":
					version, _ = fibreAttr(dp.GetAttributes(), "semantic_version")
					s.Version = clipBytes(version, 64)
					version = s.Version
				case "node_start_ts":
					if value > 0 {
						started := time.Unix(int64(value), 0)
						values["restarts"] += 0
						if !s.StartedAt.IsZero() && !s.StartedAt.Equal(started) {
							values["restarts"]++
						}
						s.StartedAt = started
					}
				case "full_discovery_amount_of_peers":
					s.FullPeers, s.PeersKnown = int64(value), true
					values["full_peers"], values["full_peers_min"] = value, value
				case "archival_discovery_amount_of_peers":
					s.ArchivalPeers = int64(value)
					values["archival_peers"] = value
				}
			}
			for _, dp := range m.GetHistogram().GetDataPoints() {
				key := instance + "\x00" + fibreSeriesKey(m.GetName(), dp.GetAttributes())
				switch m.GetName() {
				case "shrex_server_request_duration", "eds_store_put_time_histogram", "core_to_da_lag_seconds":
					seconds, count, ok := s.histogramDelta(key, dp)
					if ok {
						switch m.GetName() {
						case "shrex_server_request_duration":
							status, _ := fibreAttr(dp.GetAttributes(), "status")
							if status == "success" {
								values["serve_seconds"] += seconds
								values["serve_requests"] += count
							}
						case "eds_store_put_time_histogram":
							failed, _ := fibreAttr(dp.GetAttributes(), "failed")
							if failed == "false" {
								values["put_seconds"] += seconds
								values["puts"] += count
							} else if failed == "true" {
								values["put_failures"] += count
							}
						case "core_to_da_lag_seconds":
							values["da_lag_seconds"] += seconds
							values["da_blocks"] += count
						}
					}
				}
				if dp.GetCount() == 0 {
					continue
				}
				switch m.GetName() {
				case "eds_store_remove_odsq4_time_histogram":
					s.Archival, s.ModeKnown = false, true
				case "eds_store_remove_q4_time_histogram":
					if !s.ModeKnown {
						s.Archival, s.ModeKnown = true, true
					}
				}
			}
			if m.GetName() != "shrex_payload_served_bytes" && m.GetName() != "shrex_server_total_responses" && m.GetName() != "eds_store_bytes_reclaimed_total" {
				continue
			}
			for _, dp := range m.GetSum().GetDataPoints() {
				value := fibreNumber(dp)
				if value < 0 || math.IsNaN(value) || math.IsInf(value, 0) {
					continue
				}
				key := instance + "\x00" + fibreSeriesKey(m.GetName(), dp.GetAttributes())
				prior, known := s.series[key]
				s.series[key] = bridgeSeries{value: value, start: dp.GetStartTimeUnixNano()}
				if !known {
					continue
				}
				delta := value
				if value >= prior.value && prior.start == dp.GetStartTimeUnixNano() {
					delta -= prior.value
				}
				if m.GetName() == "shrex_payload_served_bytes" {
					served += delta
					values["served_bytes"] += delta
					continue
				}
				if m.GetName() == "eds_store_bytes_reclaimed_total" {
					values["reclaimed_bytes"] += delta
					continue
				}
				n := int64(delta)
				d.responses += n
				status, _ := fibreAttr(dp.GetAttributes(), "status")
				responsesObserved = true
				values["responses_"+bridgeResponseGroup(status)] += delta
				// Only internal_err means the bridge failed to read its own store.
				if status == "internal_err" {
					d.errors += n
				}
			}
		}
	}
	if s.PeersKnown && s.FullPeers < 2 && s.Up {
		if s.NoPeersSince.IsZero() {
			s.NoPeersSince = now
		}
	} else {
		s.NoPeersSince = time.Time{}
	}
	kept := make([]bridgeDelta, 0, len(s.responses)+1)
	for _, old := range s.responses {
		if now.Sub(old.at) <= window {
			kept = append(kept, old)
		}
	}
	if d.responses > 0 {
		kept = append(kept, d)
	}
	s.responses = kept
	if responsesObserved {
		for _, status := range bridgeResponseStatuses {
			values["responses_"+status] += 0
		}
	}
	if _, observed := values["failed_prunes"]; observed || prunerObserved {
		if hours, ok := cc.bridgeRetainedHours(); ok {
			values["retained_hours"] = hours
		}
	}
	if s.totals == nil {
		s.totals = make(map[string]float64)
	}
	if s.resources == nil {
		s.resources = make(map[string]float64)
	}
	for k, v := range values {
		if !validBridgeValue(v) {
			delete(values, k)
			continue
		}
		if bridgeFields[k] == "sum" {
			if total := s.totals[k] + v; validBridgeValue(total) {
				s.totals[k] = total
			}
		} else {
			s.resources[k] = v
		}
	}
	cc.stateMux.Unlock()
	c.bridgeData().record(cc.name, now, values, version)
	if served > 0 || d.responses > 0 {
		c.reportsMux.Lock()
		h := c.reportHourLocked(cc.name, now)
		h.BridgeServed += served
		h.BridgeResponses += d.responses
		h.BridgeErrors += d.errors
		c.reportsMux.Unlock()
	}
	return cc.name
}

func bridgeVersion(version string) string { return "v" + strings.TrimPrefix(version, "v") }

func (cc *ChainConfig) reportBridge(total reportHour, now time.Time, weekly bool) string {
	cc.stateMux.RLock()
	s := cc.bridge
	configured := cc.Bridge.URL != ""
	cc.stateMux.RUnlock()
	fresh := configured && s.metricsFresh(now)
	if total.BridgeServed <= 0 && total.BridgeResponses <= 0 && !fresh {
		return ""
	}
	if weekly {
		return "🌉 served " + humanBytes(int64(total.BridgeServed))
	}
	parts := []string{"🌉 Bridge"}
	if fresh && s.Version != "" {
		parts[0] += " " + bridgeVersion(s.Version)
	}
	parts = append(parts, "served "+humanBytes(int64(total.BridgeServed)))
	if fresh && s.PeersKnown {
		parts = append(parts, fmt.Sprintf("%d full / %d archival peers", s.FullPeers, s.ArchivalPeers))
	}
	return strings.Join(parts, " · ")
}
