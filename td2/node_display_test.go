package tenderduty

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestNodeDisplay(t *testing.T) {
	c := auditConfig(t)
	cc := &ChainConfig{name: "Chain", Nodes: []*NodeConfig{
		{Url: "http://public:26657"},
		{Url: "http://10.0.0.1:26657", AlertIfDown: true, Server: "validator"},
		{Url: "http://10.0.0.2:26657", AlertIfDown: true},
		{Url: "http://other:26657"},
	}}
	c.Chains[cc.name] = cc
	for _, tc := range []struct {
		url, shown, hidden string
	}{
		{cc.Nodes[0].Url, "public:26657", "node 1"},
		{cc.Nodes[1].Url, "validator · 10.0.0.1:26657", "validator"},
		{cc.Nodes[2].Url, "10.0.0.2:26657", "own node 2"},
		{cc.Nodes[3].Url, "other:26657", "node 4"},
		{"http://unknown:26657", "unknown:26657", "node"},
	} {
		for _, show := range []bool{true, false} {
			c.AlertNodeAddress = yes(show)
			want := tc.hidden
			if show {
				want = tc.shown
			}
			if got := c.nodeDisplay(cc.name, tc.url); got != want {
				t.Errorf("display(%s, %v) = %q, want %q", tc.url, show, got, want)
			}
		}
	}
	c.AlertNodeAddress = nil
	if got := c.nodeDisplay("Removed", cc.Nodes[1].Url); got != "10.0.0.1:26657" {
		t.Fatalf("default display %q", got)
	}
	c.liveSettings = &Config{AlertNodeAddress: yes(false)}
	if got := c.nodeDisplay(cc.name, cc.Nodes[1].Url); got != "validator" {
		t.Fatalf("live display %q", got)
	}
}

func TestNodeAlertDisplays(t *testing.T) {
	c := auditConfig(t)
	c.NodeDownMin, c.NodeDownSeverity = 3, "critical"
	node := &NodeConfig{Url: "http://10.0.0.1:26657", AlertIfDown: true, Server: "validator"}
	cc := &ChainConfig{name: "Chain", ChainId: "chain-1", Nodes: []*NodeConfig{node}}
	c.Chains[cc.name] = cc
	message := nodeDownMessage(node.Url, cc.ChainId)
	for _, show := range []bool{false, true} {
		c.AlertNodeAddress = yes(show)
		for _, label := range []string{"validator", ""} {
			node.Server = label
			want := c.nodeDisplay(cc.name, node.Url)
			for _, recovered := range []bool{false, true} {
				msg := &alertMsg{chain: cc.name, message: message, severity: "critical", resolved: recovered}
				for _, style := range []string{"plain", "embed"} {
					msg.discStyle = style
					card := msg.card()
					if !card.known() {
						t.Fatal("node card fell back to text")
					}
					outputs := []interface{}{card, buildTelegramMessage(msg), buildDiscordMessage(msg), buildSlackMessage(msg), msg.summary()}
					for _, output := range outputs {
						data, err := json.Marshal(output)
						if err != nil {
							t.Fatal(err)
						}
						text := string(data)
						if !strings.Contains(text, want) || !show && strings.Contains(text, "10.0.0.1") {
							t.Errorf("show=%v recovered=%v: %s", show, recovered, text)
						}
					}
				}
				if msg.message != message || !strings.Contains(msg.message, node.Url) || c.alarmID(cc.name, message) != node.Url {
					t.Fatal("display changed the alarm identity")
				}
			}
		}
	}
}

func TestPrivateGroupedAndUpgradeNodeDisplays(t *testing.T) {
	c := auditConfig(t)
	c.AlertNodeAddress = yes(false)
	node := &NodeConfig{Url: "http://10.0.0.1:26657", AlertIfDown: true, Server: "validator"}
	node.recordBinary("10.4.0", true)
	cc := &ChainConfig{name: "Chain", Nodes: []*NodeConfig{node}, lastBlockNum: 1000, signalWatch: signalUpgrade{Version: 11, Height: 1000}}
	node.markDown("down", false)
	c.Chains[cc.name] = cc
	group := &groupCard{Server: "10.0.0.1", Members: []groupLine{{Chain: cc.name, URL: node.Url}}, Down: "3 minutes"}
	for _, recovered := range []bool{false, true} {
		msg := &alertMsg{chain: cc.name, group: group, resolved: recovered}
		data, _ := json.Marshal([]interface{}{msg.heading(), msg.card(), msg.summary()})
		if strings.Contains(string(data), "10.0.0.1") || !strings.Contains(string(data), "validator") {
			t.Fatalf("group display %s", data)
		}
	}
	for _, text := range []string{cc.ownNodeDetails(11, false), cc.notUpgradedNodes(11)} {
		if strings.Contains(text, "10.0.0.1") || !strings.Contains(text, "validator") {
			t.Fatalf("upgrade display %q", text)
		}
	}
	node.Server = ""
	if got := group.heading(); strings.Contains(got, "10.0.0.1") || !strings.Contains(got, "own node 1") {
		t.Fatalf("automatic server heading %q", got)
	}
}

func TestNodeAddressConfigCLI(t *testing.T) {
	options, _, _ := chainCommandFixture(t)
	calls := 0
	options.reload = func(string) (int, error) { calls++; return 123, nil }
	var output bytes.Buffer
	if err := ConfigCommand(context.Background(), options, []string{"show", "general"}, strings.NewReader(""), &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "alert_node_address: yes") {
		t.Fatalf("default show %s", &output)
	}
	output.Reset()
	if err := ConfigCommand(context.Background(), options, []string{"set", "alert_node_address=no"}, strings.NewReader(""), &output); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || restartSetting("alert_node_address") {
		t.Fatalf("reloads %d, output %s", calls, &output)
	}
	output.Reset()
	if err := ConfigCommand(context.Background(), options, []string{"show", "general"}, strings.NewReader(""), &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "alert_node_address: no") {
		t.Fatalf("show %s", &output)
	}
}

func TestPrivateNodeRaiseResolvePair(t *testing.T) {
	h := newGroupHarness(t, nil)
	h.c.AlertNodeAddress = yes(false)
	h.c.GroupNodeAlerts = false
	cc := h.chain("Chain", "http://10.0.0.1:26657", "@one")
	cc.Nodes[0].Server = "validator"
	h.withPagerduty()
	h.run()
	h.down(cc.name)
	h.waitTelegram(1)
	h.waitFor("PagerDuty trigger", func() bool { triggers, _ := h.pagerduty(); return len(triggers) == 1 })
	h.up(cc.name)
	sent := h.waitTelegram(2)
	h.waitFor("PagerDuty resolve", func() bool { _, recovered := h.pagerduty(); return len(recovered) == 1 })
	for _, item := range sent {
		if strings.Contains(item.text, "10.0.0.1") || !strings.Contains(item.text, "validator") {
			t.Errorf("telegram %s", item.text)
		}
	}
	h.mux.Lock()
	defer h.mux.Unlock()
	for _, event := range h.pd {
		if event.Key != cc.Nodes[0].Url || strings.Contains(event.Summary+event.Source, "10.0.0.1") || !strings.Contains(event.Summary, "validator") {
			t.Errorf("PagerDuty %+v", event)
		}
	}
}

func TestNodeAddressReload(t *testing.T) {
	c := auditConfig(t)
	cc := reloadTestChain()
	cc.Nodes[0].Server = "validator"
	c.Chains[cc.name] = cc
	message := nodeDownMessage(cc.Nodes[0].Url, cc.ChainId)
	msg := &alertMsg{chain: cc.name, message: message}
	if !strings.Contains(msg.summary(), "localhost:26657") {
		t.Fatal("default address hidden")
	}
	next := &Config{AlertNodeAddress: yes(false), Chains: map[string]*ChainConfig{cc.name: reloadTestChain()}}
	next.Chains[cc.name].Nodes[0].Server = "validator"
	diff := c.applyReload(next)
	if len(diff.Restarted) != 0 || c.Chains[cc.name] != cc || strings.Contains(msg.summary(), "localhost") || !strings.Contains(msg.summary(), "validator") {
		t.Fatalf("reload %+v, summary %s", diff, msg.summary())
	}
	if msg.message != message || c.alarmID(cc.name, message) != cc.Nodes[0].Url {
		t.Fatal("reload changed the alarm identity")
	}
}
