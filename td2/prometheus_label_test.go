package tenderduty

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

func TestEndpointLabelHidesCredentials(t *testing.T) {
	for raw, want := range map[string]string{
		"http://localhost:26657":                                 "http://localhost:26657",
		"https://rpc.example:443/path":                           "https://rpc.example:443/path",
		"https://rpc.example:443/p%20q":                          "https://rpc.example:443/p%20q",
		"https://user:pass@rpc.example:443/path?apikey=KEY#frag": "https://rpc.example:443/path",
		"https://rpc.example/?key=KEY":                           "https://rpc.example/",
		"https://rpc.example/path?":                              "https://rpc.example/path",
		"http://token@host:1":                                    "http://host:1",
		// does not parse: still no userinfo or query
		"http://user:pa@ss@host:abc/x?token=KEY": "host:abc/x",
	} {
		if got := endpointLabel(raw); got != want {
			t.Errorf("endpointLabel(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestNodeMetricsLabelHasNoSecrets(t *testing.T) {
	gauge := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "test_node_down", Help: "test"}, []string{"name", "chain_id", "moniker", "endpoint"})
	registry := prometheus.NewRegistry()
	registry.MustRegister(gauge)
	metrics{metricNodeDownSeconds: gauge}.setStat(&promUpdate{metric: metricNodeDownSeconds, name: "Chain", chainId: "chain-1", endpoint: "https://u:p@rpc.example:443/?apikey=SECRET", counter: 5})
	all, err := registry.Gather()
	if err != nil || len(all) != 1 || len(all[0].Metric) != 1 {
		t.Fatalf("gathered %v, %v", all, err)
	}
	for _, label := range all[0].Metric[0].Label {
		if label.GetName() == "endpoint" && label.GetValue() != "https://rpc.example:443/" {
			t.Fatalf("endpoint label %q", label.GetValue())
		}
		if strings.Contains(label.GetValue(), "SECRET") || strings.Contains(label.GetValue(), "u:p") {
			t.Fatalf("label %s leaks: %q", label.GetName(), label.GetValue())
		}
	}
}
