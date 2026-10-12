package tenderduty

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

type bridgeCollector struct {
	config *Config
}

var bridgePromNames = []string{"up", "head", "lag_blocks", "sampled_head"}

func bridgeDesc(name string) *prometheus.Desc {
	return prometheus.NewDesc("tenderduty_bridge_"+name, "Bridge "+name, []string{"name", "chain_id", "moniker"}, nil)
}

var bridgeInfoDesc = prometheus.NewDesc("tenderduty_bridge_info", "Bridge version", []string{"name", "chain_id", "moniker", "version"}, nil)

func (*bridgeCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- bridgeInfoDesc
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
		values := []float64{up, float64(s.Head), float64(s.Lag), float64(s.Sampled)}
		for i, suffix := range bridgePromNames {
			ch <- prometheus.MustNewConstMetric(bridgeDesc(suffix), prometheus.GaugeValue, values[i], name, cc.ChainId, info.Moniker)
		}
	}
}
