package tenderduty

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

type bridgeCollector struct {
	config *Config
}

var bridgePromNames = []string{"up", "head", "lag_blocks", "sampled_head", "sampling_lag_blocks"}

var bridgeCounters = map[string]string{
	"served_bytes_total": "served_bytes", "serve_seconds_total": "serve_seconds", "serve_requests_total": "serve_requests",
	"store_put_seconds_total": "put_seconds", "store_puts_total": "puts", "store_put_failures_total": "put_failures",
	"core_to_da_seconds_total": "da_lag_seconds", "core_to_da_blocks_total": "da_blocks", "reclaimed_bytes_total": "reclaimed_bytes", "restarts_total": "restarts",
}
var bridgeResponsesDesc = prometheus.NewDesc("tenderduty_bridge_responses_total", "Bridge responses", []string{"name", "chain_id", "moniker", "status"}, nil)

func bridgeDesc(name string) *prometheus.Desc {
	return prometheus.NewDesc("tenderduty_bridge_"+name, "Bridge "+name, []string{"name", "chain_id", "moniker"}, nil)
}

var bridgeInfoDesc = prometheus.NewDesc("tenderduty_bridge_info", "Bridge version", []string{"name", "chain_id", "moniker", "version"}, nil)

func (*bridgeCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- bridgeInfoDesc
	ch <- bridgeResponsesDesc
	for name := range bridgeCounters {
		ch <- bridgeDesc(name)
	}
	for _, name := range []string{"retained_hours", "memory_bytes", "goroutines"} {
		ch <- bridgeDesc(name)
	}
	for _, name := range []string{"full_peers", "archival_peers", "start_time_seconds", "last_pruned", "failed_prunes", "archival"} {
		ch <- bridgeDesc(name)
	}
	for _, name := range bridgePromNames {
		ch <- bridgeDesc(name)
	}
}

func (bc *bridgeCollector) Collect(ch chan<- prometheus.Metric) {
	bc.config.chainsMux.RLock()
	defer bc.config.chainsMux.RUnlock()
	for name, cc := range bc.config.Chains {
		if cc == nil {
			continue
		}
		cc.stateMux.RLock()
		s, configured := cc.bridge, cc.Bridge.URL != ""
		totals := make(map[string]float64, len(s.totals))
		for k, v := range s.totals {
			totals[k] = v
		}
		resources := make(map[string]float64, len(s.resources))
		for k, v := range s.resources {
			resources[k] = v
		}
		retained, retainedKnown := cc.bridgeRetainedHours()
		cc.stateMux.RUnlock()
		if !configured {
			continue
		}
		info, _ := cc.validatorState()
		up := float64(0)
		if s.Loaded && s.Up {
			up = 1
		}
		if s.metricsFresh(time.Now()) {
			for suffix, key := range bridgeCounters {
				ch <- prometheus.MustNewConstMetric(bridgeDesc(suffix), prometheus.CounterValue, totals[key], name, cc.ChainId, info.Moniker)
			}
			for _, status := range bridgeResponseStatuses {
				ch <- prometheus.MustNewConstMetric(bridgeResponsesDesc, prometheus.CounterValue, totals["responses_"+status], name, cc.ChainId, info.Moniker, status)
			}
			for _, key := range []string{"memory_bytes", "goroutines"} {
				if v, ok := resources[key]; ok {
					ch <- prometheus.MustNewConstMetric(bridgeDesc(key), prometheus.GaugeValue, v, name, cc.ChainId, info.Moniker)
				}
			}
			if retainedKnown {
				ch <- prometheus.MustNewConstMetric(bridgeDesc("retained_hours"), prometheus.GaugeValue, retained, name, cc.ChainId, info.Moniker)
			}
			if s.PrunerKnown {
				ch <- prometheus.MustNewConstMetric(bridgeDesc("last_pruned"), prometheus.GaugeValue, float64(s.LastPruned), name, cc.ChainId, info.Moniker)
				ch <- prometheus.MustNewConstMetric(bridgeDesc("failed_prunes"), prometheus.GaugeValue, float64(s.FailedPrunes), name, cc.ChainId, info.Moniker)
			}
			if s.ModeKnown {
				archival := float64(0)
				if s.Archival {
					archival = 1
				}
				ch <- prometheus.MustNewConstMetric(bridgeDesc("archival"), prometheus.GaugeValue, archival, name, cc.ChainId, info.Moniker)
			}
			if s.PeersKnown {
				ch <- prometheus.MustNewConstMetric(bridgeDesc("full_peers"), prometheus.GaugeValue, float64(s.FullPeers), name, cc.ChainId, info.Moniker)
				ch <- prometheus.MustNewConstMetric(bridgeDesc("archival_peers"), prometheus.GaugeValue, float64(s.ArchivalPeers), name, cc.ChainId, info.Moniker)
			}
			if !s.StartedAt.IsZero() {
				ch <- prometheus.MustNewConstMetric(bridgeDesc("start_time_seconds"), prometheus.GaugeValue, float64(s.StartedAt.Unix()), name, cc.ChainId, info.Moniker)
			}
			if s.Version != "" {
				ch <- prometheus.MustNewConstMetric(bridgeInfoDesc, prometheus.GaugeValue, 1, name, cc.ChainId, info.Moniker, s.Version)
			}
		}
		values := []float64{up, float64(s.Head), float64(s.Lag), float64(s.Sampled), float64(max(s.Network-s.Sampled, 0))}
		for i, suffix := range bridgePromNames {
			ch <- prometheus.MustNewConstMetric(bridgeDesc(suffix), prometheus.GaugeValue, values[i], name, cc.ChainId, info.Moniker)
		}
	}
}
