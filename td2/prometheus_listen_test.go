package tenderduty

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
)

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
