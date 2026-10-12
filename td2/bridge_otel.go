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

func (c *Config) ingestBridgeMetrics(rm *metricsv1.ResourceMetrics, now time.Time) {
	attrs := rm.GetResource().GetAttributes()
	namespace, _ := fibreAttr(attrs, "service.namespace")
	instance, _ := fibreAttr(attrs, "service.instance.id")
	c.chainsMux.RLock()
	var cc *ChainConfig
	for _, chain := range c.Chains {
		if chain != nil && chain.ChainId == namespace && chain.Bridge.URL != "" {
			cc = chain
			break
		}
	}
	c.chainsMux.RUnlock()
	if cc == nil {
		if c.fibreData().noteIgnored("bridge:" + namespace + "\x00" + instance) {
			l(fmt.Sprintf("ignoring bridge metrics from %s on %s: no chain with that chain_id has bridge.url", instance, namespace))
		}
		return
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
	d := bridgeDelta{at: now}
	var served float64
	for _, scope := range rm.GetScopeMetrics() {
		for _, m := range scope.GetMetrics() {
			for _, dp := range m.GetGauge().GetDataPoints() {
				value := fibreNumber(dp)
				if math.IsNaN(value) || math.IsInf(value, 0) {
					continue
				}
				switch m.GetName() {
				case "prnr_last_pruned":
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
				case "build_info":
					version, _ := fibreAttr(dp.GetAttributes(), "semantic_version")
					s.Version = clipBytes(version, 64)
				case "node_start_ts":
					if value > 0 {
						s.StartedAt = time.Unix(int64(value), 0)
					}
				case "full_discovery_amount_of_peers":
					s.FullPeers, s.PeersKnown = int64(value), true
				case "archival_discovery_amount_of_peers":
					s.ArchivalPeers = int64(value)
				}
			}
			for _, dp := range m.GetHistogram().GetDataPoints() {
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
			if m.GetName() != "shrex_payload_served_bytes" && m.GetName() != "shrex_server_total_responses" {
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
					continue
				}
				n := int64(delta)
				d.responses += n
				status, _ := fibreAttr(dp.GetAttributes(), "status")
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
	cc.stateMux.Unlock()
	if served > 0 || d.responses > 0 {
		c.reportsMux.Lock()
		h := c.reportHourLocked(cc.name, now)
		h.BridgeServed += served
		h.BridgeResponses += d.responses
		h.BridgeErrors += d.errors
		c.reportsMux.Unlock()
	}
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
