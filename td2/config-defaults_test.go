package tenderduty

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAlertDefaultsAndChainOverrides(t *testing.T) {
	c := &Config{Chains: map[string]*ChainConfig{
		"Osmosis": {},
		"Juno":    {},
	}}
	config := []byte(`
alert_defaults:
  stalled_enabled: yes
  stalled_minutes: 10
  consecutive_enabled: yes
  consecutive_missed: 5
  signal_remind_percent: 72
  telegram:
    enabled: yes
chains:
  Osmosis:
    alerts:
      consecutive_missed: 3
      signal_remind_percent: 77
      telegram:
        enabled: no
`)
	files := map[string][]byte{
		"Juno": []byte("alerts:\n  stalled_enabled: no\n"),
	}
	if err := applyAlertDefaults(c, config, files); err != nil {
		t.Fatal(err)
	}
	if c.Chains["Osmosis"].Alerts.ConsecutiveMissed != 3 || on(c.Chains["Osmosis"].Alerts.Telegram.Enabled) {
		t.Fatalf("Osmosis override was not applied: %+v", c.Chains["Osmosis"].Alerts)
	}
	if c.Chains["Osmosis"].Alerts.SignalRemindPercent != 77 || c.Chains["Juno"].Alerts.SignalRemindPercent != 72 {
		t.Fatal("signal reminder defaults or overrides were not applied")
	}
	if c.Chains["Juno"].Alerts.StalledAlerts || c.Chains["Juno"].Alerts.Stalled != 10 || !on(c.Chains["Juno"].Alerts.Telegram.Enabled) {
		t.Fatalf("Juno did not inherit the expected defaults: %+v", c.Chains["Juno"].Alerts)
	}
}

// TestChainsFollowTheSharedDestinationSwitch: a chain without a destination override follows the shared switch
func TestChainsFollowTheSharedDestinationSwitch(t *testing.T) {
	for _, tc := range []struct {
		name          string
		shared, chain *bool
		want          bool
	}{
		{"shared on, chain quiet", yes(true), nil, true},
		{"shared on, chain opts out", yes(true), yes(false), false},
		{"shared off, chain quiet", yes(false), nil, false},
		{"shared off, chain asks anyway", yes(false), yes(true), false},
		{"shared unset", nil, yes(true), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := auditConfig(t)
			c.Telegram.Enabled, c.Telegram.ApiKey, c.Telegram.Channel = tc.shared, "key", "-100"
			chain := &ChainConfig{ChainId: "atomone-1"}
			chain.Alerts.Telegram.Enabled = tc.chain
			c.Chains["AtomOne"] = chain
			if got := c.makeAlert("AtomOne", "something happened", "critical", false, nil).tg; got != tc.want {
				t.Fatalf("telegram delivery is %v, want %v", got, tc.want)
			}
		})
	}
}

// TestDestinationsNeedNoAlertDefaultsEntry: the shared switch works without destinations in alert_defaults
func TestDestinationsNeedNoAlertDefaultsEntry(t *testing.T) {
	c := &Config{Chains: map[string]*ChainConfig{"Juno": {}}}
	config := []byte("alert_defaults:\n  consecutive_enabled: yes\n  consecutive_missed: 5\n")
	if err := applyAlertDefaults(c, config, nil); err != nil {
		t.Fatal(err)
	}
	if c.Chains["Juno"].Alerts.Telegram.Enabled != nil {
		t.Fatal("a destination nobody configured should stay unset so it can follow the shared switch")
	}
	c.Telegram.Enabled = yes(true)
	if !enabledFor(c.Telegram.Enabled, c.Chains["Juno"].Alerts.Telegram.Enabled) {
		t.Fatal("the chain did not follow the shared telegram switch")
	}
}

func TestConfigLoadsWithoutAnExistingStateFile(t *testing.T) {
	directory := t.TempDir()
	configFile := filepath.Join(directory, "config.yml")
	data := []byte(`
node_down_alert_minutes: 3
alert_defaults:
  consecutive_enabled: yes
  consecutive_missed: 5
chains:
  Juno:
    chain_id: juno-1
    valoper_address: junovaloper1example
    nodes:
      - url: http://localhost:26657
`)
	if err := os.WriteFile(configFile, data, 0600); err != nil {
		t.Fatal(err)
	}
	password := ""
	c, err := loadConfig(configFile, filepath.Join(directory, "state.json"), filepath.Join(directory, "chains.d"), &password)
	if err != nil {
		t.Fatal(err)
	}
	defer c.cancel()
	if fatal, problems := validateConfig(c); fatal {
		t.Fatalf("valid configuration was rejected: %v", problems)
	}
	if len(c.Chains["Juno"].blocksResults) != showBLocks || c.Chains["Juno"].Alerts.ConsecutiveMissed != 5 {
		t.Fatal("configuration defaults or initial history are missing")
	}
}

func TestValidatorsHaveTheirOwnNodes(t *testing.T) {
	c := &Config{Chains: map[string]*ChainConfig{
		"Realio": {
			ChainId: "realionetwork_3301-1",
			Validators: []ValidatorConfig{
				{Name: "RIO", ValAddress: "realiovaloper1rio", Nodes: []*NodeConfig{{Url: "http://node-a:26657", AlertIfDown: true}}},
				{Name: "dstrx", ValAddress: "realiovaloper1dstrx", Nodes: []*NodeConfig{{Url: "http://node-b:26657", AlertIfDown: true}}},
			},
		},
	}}
	if _, err := expandValidators(c); err != nil {
		t.Fatal(err)
	}
	rio, dstrx := c.Chains["Realio / RIO"], c.Chains["Realio / dstrx"]
	if rio == nil || dstrx == nil || rio.ValAddress == dstrx.ValAddress {
		t.Fatalf("validators were not expanded: %+v", c.Chains)
	}
	if len(rio.Nodes) != 1 || rio.Nodes[0].Url != "http://node-a:26657" || len(dstrx.Nodes) != 1 || dstrx.Nodes[0].Url != "http://node-b:26657" {
		t.Fatalf("validators did not get their own nodes: %v %v", rio.Nodes, dstrx.Nodes)
	}
}

// TestValidatorsShareTheChainNodes: chain nodes are fallbacks shared by every validator
func TestValidatorsShareTheChainNodes(t *testing.T) {
	c := &Config{Chains: map[string]*ChainConfig{
		"Realio": {
			ChainId: "realionetwork_3301-1",
			Nodes:   []*NodeConfig{{Url: "http://spare:26657", AlertIfDown: true}},
			Validators: []ValidatorConfig{
				{Name: "RIO", ValAddress: "realiovaloper1rio", Nodes: []*NodeConfig{{Url: "http://node-a:26657", AlertIfDown: true}}},
				{Name: "dstrx", ValAddress: "realiovaloper1dstrx", Nodes: []*NodeConfig{{Url: "http://node-b:26657", AlertIfDown: true}}},
			},
		},
	}}
	warnings, err := expandValidators(c)
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "alert_if_down is ignored") {
		t.Fatalf("a shared node alerting once per validator was not called out: %v", warnings)
	}
	for name, own := range map[string]string{"Realio / RIO": "http://node-a:26657", "Realio / dstrx": "http://node-b:26657"} {
		nodes := c.Chains[name].Nodes
		if len(nodes) != 2 || nodes[0].Url != own || !nodes[0].AlertIfDown {
			t.Fatalf("%s should try its own node first: %+v", name, nodes)
		}
		if nodes[1].Url != "http://spare:26657" || nodes[1].AlertIfDown {
			t.Fatalf("%s did not get the shared node as a quiet fallback: %+v", name, nodes[1])
		}
	}
}

// TestValidatorsNeedANodeOrPublicFallback: a validator with no node of its own still needs somewhere to look
func TestValidatorsNeedANodeOrPublicFallback(t *testing.T) {
	chain := func() *ChainConfig {
		return &ChainConfig{ChainId: "realionetwork_3301-1", Validators: []ValidatorConfig{{Name: "RIO", ValAddress: "realiovaloper1rio"}}}
	}
	c := &Config{Chains: map[string]*ChainConfig{"Realio": chain()}}
	if _, err := expandValidators(c); err == nil || !strings.Contains(err.Error(), "no RPC nodes") {
		t.Fatalf("a validator with nowhere to look was accepted: %v", err)
	}
	withFallback := chain()
	withFallback.PublicFallback = true
	c = &Config{Chains: map[string]*ChainConfig{"Realio": withFallback}}
	if _, err := expandValidators(c); err != nil {
		t.Fatalf("public_fallback should be enough: %v", err)
	}
}

func TestExampleConfigsLoad(t *testing.T) {
	oldAlarms := alarms
	defer func() { alarms = oldAlarms }()
	password := ""
	c, err := loadConfig("../examples/config.yml", filepath.Join(t.TempDir(), "state.json"), "../examples/chains.d", &password)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { c.cancel(); c.deliveryCancel() }()
	if fatal, problems := validateConfig(c); fatal || len(problems) != 0 {
		t.Fatalf("examples do not load cleanly: %v", problems)
	}
	for _, name := range []string{"Celestia", "Osmosis", "Realio / RIO", "Realio / dstrx"} {
		if c.Chains[name] == nil {
			t.Fatalf("example chain %s is missing: %v", name, c.Chains)
		}
	}
}

func TestRPCURLsGetADefaultPort(t *testing.T) {
	for in, want := range map[string]string{
		"https://rpc.example.com":           "https://rpc.example.com:443",
		"https://rpc.example.com/celestia/": "https://rpc.example.com:443/celestia/",
		"http://10.0.0.1":                   "http://10.0.0.1:80",
		"http://[::1]/":                     "http://[::1]:80/",
		"https://rpc.example.com:26657":     "https://rpc.example.com:26657",
		"tcp://127.0.0.1:26657":             "tcp://127.0.0.1:26657",
		"not a url":                         "not a url",
	} {
		if got := withDefaultPort(in); got != want {
			t.Errorf("%s: got %s, want %s", in, got, want)
		}
	}
}

func TestAlertsEnabledByDefault(t *testing.T) {
	for _, tc := range []struct {
		name, defaults, overrides string
		want                      bool
	}{
		{"unset", "", "", true},
		{"defaults no", "no", "", false},
		{"chain no", "", "no", false},
		{"chain no over defaults yes", "yes", "no", false},
		{"chain yes over defaults no", "no", "yes", true},
	} {
		for _, file := range []bool{false, true} {
			t.Run(tc.name+map[bool]string{false: "/inline", true: "/file"}[file], func(t *testing.T) {
				config := "node_down_alert_minutes: 3\n"
				if tc.defaults != "" {
					config += "alert_defaults:\n"
					for _, key := range []string{"gov_enabled", "upgrade_enabled", "missed_blocks_enabled", "stalled_enabled", "alert_if_no_servers"} {
						config += "  " + key + ": " + tc.defaults + "\n"
					}
				}
				chainData := ""
				if tc.overrides != "" {
					chainData = "alerts:\n"
					for _, key := range []string{"gov_enabled", "upgrade_enabled", "missed_blocks_enabled", "stalled_enabled", "alert_if_no_servers"} {
						chainData += "  " + key + ": " + tc.overrides + "\n"
					}
				}
				files := map[string][]byte{}
				if file {
					files["Chain"] = []byte(chainData)
				} else if chainData != "" {
					config += "chains:\n  Chain:\n    " + strings.ReplaceAll(strings.TrimSuffix(chainData, "\n"), "\n", "\n    ") + "\n"
				}
				chain := &ChainConfig{ChainId: "chain-1", ValAddress: "valoper1test", Nodes: []*NodeConfig{{Url: "http://localhost:26657"}}}
				c := &Config{Chains: map[string]*ChainConfig{"Chain": chain}}
				if err := applyAlertDefaults(c, []byte(config), files); err != nil {
					t.Fatal(err)
				}
				if fatal, problems := validateConfig(c); fatal {
					t.Fatalf("validation failed: %v", problems)
				}
				a := chain.alertSettings()
				if a.GovAlerts != tc.want || a.UpgradeAlerts != tc.want || a.MissedBlocksAlerts != tc.want || a.StalledAlerts != tc.want || a.AlertIfNoServers != tc.want || a.consecutiveEnabled() != tc.want || a.jailRiskEnabled() != tc.want || a.jailInactiveEnabled() != tc.want {
					t.Fatalf("%+v, want %v", a, tc.want)
				}
				if tc.want && a.SignalPercent != 5 {
					t.Fatalf("signal_percent = %d, want 5", a.SignalPercent)
				}
				if tc.want && a.SignalRemindPercent != 66 {
					t.Fatalf("signal_remind_percent = %d, want 66", a.SignalRemindPercent)
				}
			})
		}
	}
}
