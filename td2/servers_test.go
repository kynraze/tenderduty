package tenderduty

import (
	"bytes"
	"context"
	"net"
	"net/url"
	"os"
	"strings"
	"testing"
)

func TestServerNames(t *testing.T) {
	c := auditConfig(t)
	c.Servers = map[string]string{
		"203.0.113.7":     "Box RPC",
		"RPC.Example.org": "Example RPC",
		"2001:db8::1":     "IPv6 RPC",
	}
	if fatal, problems := validateConfig(c); fatal {
		t.Fatalf("validation failed: %v", problems)
	}
	cc := &ChainConfig{Nodes: []*NodeConfig{
		{Url: "https://rpc.example.org:443", Server: "own label"},
		{Url: "http://203.0.113.7:26657"},
		{Url: "https://rPc.EXAMPLE.ORG:26657"},
		{Url: "http://[2001:db8::1]:26657"},
		{Url: "http://unnamed:26657", AlertIfDown: true},
	}}
	c.Chains["Chain"] = cc
	old := lookupIPs
	t.Cleanup(func() { lookupIPs = old })
	lookupIPs = func(context.Context, string) ([]net.IPAddr, error) {
		t.Fatal("display and direct host matches must not use DNS")
		return nil, nil
	}
	for i, want := range []string{"own label", "Box RPC", "Example RPC", "IPv6 RPC"} {
		node := cc.Nodes[i]
		if got := c.nodeServer("Chain", node.Url); got != want {
			t.Errorf("group %s = %q, want %q", node.Url, got, want)
		}
		for _, show := range []bool{true, false} {
			c.AlertNodeAddress = yes(show)
			display := want
			if show {
				display += " · " + nodeEndpoint(node.Url)
			}
			if got := c.nodeDisplay("Chain", node.Url); got != display {
				t.Errorf("display %s = %q, want %q", node.Url, got, display)
			}
		}
	}
	c.AlertNodeAddress = yes(false)
	if got := c.nodeDisplay("Chain", cc.Nodes[4].Url); got != "own node 1" {
		t.Errorf("unmapped display = %q", got)
	}
	if got := c.nodeDisplay("Removed", "http://rpc.example.org:26657"); got != "Example RPC" {
		t.Errorf("removed node display = %q", got)
	}
}

func TestServerNamesResolvedIP(t *testing.T) {
	c := auditConfig(t)
	c.Servers = map[string]string{"203.0.113.7": "Box RPC"}
	old := lookupIPs
	t.Cleanup(func() { lookupIPs = old; clearServerCache() })
	clearServerCache()
	calls := 0
	lookupIPs = func(context.Context, string) ([]net.IPAddr, error) {
		calls++
		return []net.IPAddr{{IP: net.ParseIP("203.0.113.7")}}, nil
	}
	for _, nodeURL := range []string{"https://first.example:443", "http://second.example:26657"} {
		if got := c.nodeServer("Chain", nodeURL); got != "Box RPC" {
			t.Errorf("group %s = %q", nodeURL, got)
		}
		before := calls
		if got := c.nodeDisplay("Chain", nodeURL); got != nodeEndpoint(nodeURL) || calls != before {
			t.Errorf("display %s = %q, DNS calls %d", nodeURL, got, calls-before)
		}
	}
	if calls != 2 {
		t.Errorf("DNS calls = %d, want 2", calls)
	}
}

func TestServerNamesReload(t *testing.T) {
	c := auditConfig(t)
	chain := reloadTestChain()
	c.Chains["Chain"] = chain
	nodeURL := chain.Nodes[0].Url
	parsed, err := url.Parse(nodeURL)
	if err != nil || parsed.Hostname() == "" {
		t.Fatalf("invalid test node URL %q: %v", nodeURL, err)
	}
	for _, name := range []string{"first", "changed", ""} {
		next := &Config{AlertNodeAddress: yes(false), Chains: map[string]*ChainConfig{"Chain": reloadTestChain()}}
		if name != "" {
			next.Servers = map[string]string{parsed.Hostname(): name}
		}
		diff := c.applyReload(next)
		if len(diff.Restarted) != 0 || c.Chains["Chain"] != chain {
			t.Fatalf("server map restarted the chain: %+v", diff)
		}
		if got := c.serverName(nodeURL); got != name {
			t.Errorf("reloaded name = %q, want %q", got, name)
		}
		if name != "" {
			if got := c.nodeServer("Chain", nodeURL); got != name {
				t.Errorf("reloaded group = %q, want %q", got, name)
			}
			if got := c.nodeDisplay("Chain", nodeURL); got != name {
				t.Errorf("reloaded display = %q, want %q", got, name)
			}
		}
	}
}

func TestServerNamesValidation(t *testing.T) {
	c := auditConfig(t)
	data := []byte("servers:\n  rpc.example.com: RPC\n  empty.example.com: ''\n  blank.example.com: '  '\n  https://rpc.example.com: URL\n  rpc.example.com/path: path\n")
	if err := decodeConfig(data, c); err != nil {
		t.Fatal(err)
	}
	fatal, problems := validateConfig(c)
	if fatal || len(c.Servers) != 1 || c.Servers["rpc.example.com"] != "RPC" {
		t.Fatalf("servers = %v, problems = %v", c.Servers, problems)
	}
	text := strings.Join(problems, "\n")
	if strings.Count(text, "servers key must be an IP or hostname") != 2 || strings.Count(text, "empty name") != 2 {
		t.Fatalf("warnings = %s", text)
	}
}

func TestConfigShowServers(t *testing.T) {
	options, _, _ := chainCommandFixture(t)
	if err := os.WriteFile(options.ConfigFile, []byte("servers:\n  203.0.113.7: Box RPC\n  rpc.example.org: Example RPC\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := ConfigCommand(t.Context(), options, []string{"show", "general"}, strings.NewReader(""), &output); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"servers:", "203.0.113.7: Box RPC", "rpc.example.org: Example RPC"} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("show lacks %q: %s", want, &output)
		}
	}
}

func TestServerNamesNormalization(t *testing.T) {
	for _, tc := range []struct {
		name     string
		servers  map[string]string
		host     string
		label    string
		warnings []string
	}{
		{
			name: "IPv4 port", servers: map[string]string{"192.0.2.2:26657": "RPC-2"},
			host: "192.0.2.2", label: "RPC-2",
			warnings: []string{`warning: servers key "192.0.2.2:26657" has a port, using 192.0.2.2`},
		},
		{
			name: "IPv6 port", servers: map[string]string{"[2001:DB8::2]:26657": "IPv6"},
			host: "2001:db8::2", label: "IPv6",
			warnings: []string{`warning: servers key "[2001:DB8::2]:26657" has a port, using 2001:db8::2`},
		},
		{
			name: "IPv6 brackets", servers: map[string]string{"[2001:DB8::2]": "IPv6"},
			host: "2001:db8::2", label: "IPv6",
		},
		{
			name: "hostname port", servers: map[string]string{"RPC.EXAMPLE.COM:443": "RPC"},
			host: "rpc.example.com", label: "RPC",
			warnings: []string{`warning: servers key "RPC.EXAMPLE.COM:443" has a port, using rpc.example.com`},
		},
		{
			name: "case duplicates", servers: map[string]string{"rpc.example.com": "lower", "RPC.EXAMPLE.COM": "upper"},
			host: "rpc.example.com", label: "upper",
			warnings: []string{`warning: servers has rpc.example.com more than once, using "upper"`},
		},
		{
			name: "port duplicates", servers: map[string]string{"rpc.example.com:443": "port", "rpc.example.com": "plain"},
			host: "rpc.example.com", label: "plain",
			warnings: []string{
				`warning: servers key "rpc.example.com:443" has a port, using rpc.example.com`,
				`warning: servers has rpc.example.com more than once, using "plain"`,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := auditConfig(t)
			// Reuse the original map to exercise deterministic selection and ensure
			// validation builds a new map rather than changing the input.
			for i := 0; i < 20; i++ {
				c.Servers = tc.servers
				fatal, problems := validateConfig(c)
				if fatal || len(c.Servers) != 1 || c.Servers[tc.host] != tc.label {
					t.Fatalf("servers = %v, problems = %v", c.Servers, problems)
				}
				var warnings []string
				for _, problem := range problems {
					if strings.HasPrefix(problem, "warning: servers") {
						warnings = append(warnings, problem)
					}
				}
				if strings.Join(warnings, "\n") != strings.Join(tc.warnings, "\n") {
					t.Fatalf("warnings = %v, want %v", warnings, tc.warnings)
				}
				for _, host := range []string{tc.host, strings.ToUpper(tc.host)} {
					if got := c.namedServer(host); got != tc.label {
						t.Errorf("namedServer(%q) = %q, want %q", host, got, tc.label)
					}
				}
			}
		})
	}
}
