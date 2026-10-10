package tenderduty

import "github.com/prometheus/client_golang/prometheus"

type bridgeCollector struct {
	config *Config
}

var bridgePromNames = []string{"up", "head", "lag_blocks", "sampled_head"}

func bridgeDesc(name string) *prometheus.Desc {
	return prometheus.NewDesc("tenderduty_bridge_"+name, "Bridge "+name, []string{"name", "chain_id", "moniker"}, nil)
}

func (*bridgeCollector) Describe(ch chan<- *prometheus.Desc) {
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
		values := []float64{up, float64(s.Head), float64(s.Lag), float64(s.Sampled)}
		for i, suffix := range bridgePromNames {
			ch <- prometheus.MustNewConstMetric(bridgeDesc(suffix), prometheus.GaugeValue, values[i], name, cc.ChainId, info.Moniker)
		}
	}
}
