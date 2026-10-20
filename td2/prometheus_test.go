package tenderduty

import (
	"bytes"
	"context"
	"github.com/prometheus/client_golang/prometheus"
	"os"
	"strings"
	"testing"
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

func TestPrometheusListenConfig(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  string
		valid bool
	}{
		{"28686", "28686", true},
		{`"28686"`, "28686", true},
		{`"127.0.0.1:28686"`, "127.0.0.1:28686", true},
		{`"[::1]:28686"`, "[::1]:28686", true},
		{`"abc"`, "abc", false},
		{`"127.0.0.1:0"`, "127.0.0.1:0", false},
		{`"127.0.0.1:99999"`, "127.0.0.1:99999", false},
		{`":28686"`, ":28686", false},
		{`""`, "", false},
	} {
		t.Run(tc.value, func(t *testing.T) {
			c := auditConfig(t)
			data := []byte("prometheus_enabled: yes\nprometheus_listen_port: " + tc.value + "\n")
			if err := unmarshalConfig("test", data, c, &Config{}); err != nil {
				t.Fatal(err)
			}
			if c.PrometheusListenPort != tc.want {
				t.Fatalf("got %q, want %q", c.PrometheusListenPort, tc.want)
			}
			fatal, problems := validateConfig(c)
			found := false
			for _, problem := range problems {
				if problem == "error: The prometheus listen port is invalid" {
					found = true
				}
			}
			if fatal == tc.valid || found == tc.valid {
				t.Fatalf("fatal=%v, problems=%v", fatal, problems)
			}
		})
	}
}

func TestPrometheusListenAddress(t *testing.T) {
	for _, tc := range []struct{ listen, want string }{
		{"28686", ":28686"},
		{"127.0.0.1:28686", "127.0.0.1:28686"},
		{"[::1]:28686", "[::1]:28686"},
	} {
		if got := prometheusListenAddress(tc.listen); got != tc.want {
			t.Errorf("address for %q: got %q, want %q", tc.listen, got, tc.want)
		}
	}
}

func TestConfigCLISetPrometheusListen(t *testing.T) {
	options, _, _ := chainCommandFixture(t)
	if err := os.WriteFile(options.ConfigFile, []byte("prometheus_enabled: yes\nprometheus_listen_port: 28686\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := ConfigCommand(context.Background(), options, []string{"set", "prometheus_listen_port=127.0.0.1:28686"}, strings.NewReader(""), &output); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(options.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	c := &Config{}
	if err := decodeConfig(before, c); err != nil || c.PrometheusListenPort != "127.0.0.1:28686" {
		t.Fatalf("saved listen %q: %v", c.PrometheusListenPort, err)
	}
	if !strings.Contains(output.String(), "restart tenderduty to apply prometheus_listen_port") {
		t.Fatalf("missing restart notice: %s", &output)
	}
	for _, listen := range []string{"abc", "127.0.0.1:99999"} {
		err := ConfigCommand(context.Background(), options, []string{"set", "prometheus_listen_port=" + listen}, strings.NewReader(""), &output)
		if err == nil || !strings.Contains(err.Error(), "The prometheus listen port is invalid") {
			t.Fatalf("invalid listen %q: %v", listen, err)
		}
		after, err := os.ReadFile(options.ConfigFile)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before, after) {
			t.Fatalf("invalid edit changed config: %s", after)
		}
	}
}
