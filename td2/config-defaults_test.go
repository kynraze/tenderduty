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
  telegram:
    enabled: yes
chains:
  Osmosis:
    alerts:
      consecutive_missed: 3
      telegram:
        enabled: no
`)
	files := map[string][]byte{
		"Juno": []byte("alerts:\n  stalled_enabled: no\n"),
	}
	if err := applyAlertDefaults(c, config, files); err != nil {
		t.Fatal(err)
	}
	if c.Chains["Osmosis"].Alerts.ConsecutiveMissed != 3 || c.Chains["Osmosis"].Alerts.Telegram.Enabled {
		t.Fatalf("Osmosis override was not applied: %+v", c.Chains["Osmosis"].Alerts)
	}
	if c.Chains["Juno"].Alerts.StalledAlerts || c.Chains["Juno"].Alerts.Stalled != 10 || !c.Chains["Juno"].Alerts.Telegram.Enabled {
		t.Fatalf("Juno did not inherit the expected defaults: %+v", c.Chains["Juno"].Alerts)
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
	if err := expandValidators(c); err != nil {
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

func TestValidatorsRejectChainNodes(t *testing.T) {
	c := &Config{Chains: map[string]*ChainConfig{
		"Realio": {
			ChainId: "realionetwork_3301-1",
			Nodes:   []*NodeConfig{{Url: "http://node-a:26657"}},
			Validators: []ValidatorConfig{
				{Name: "RIO", ValAddress: "realiovaloper1rio"},
				{Name: "dstrx", ValAddress: "realiovaloper1dstrx"},
			},
		},
	}}
	if err := expandValidators(c); err == nil || !strings.Contains(err.Error(), "under each validator") {
		t.Fatalf("nodes shared by several validators were accepted: %v", err)
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
	for _, name := range []string{"Osmosis", "Realio / RIO", "Realio / dstrx"} {
		if c.Chains[name] == nil {
			t.Fatalf("example chain %s is missing: %v", name, c.Chains)
		}
	}
}
