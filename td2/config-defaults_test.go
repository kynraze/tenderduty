package tenderduty

import (
	"os"
	"path/filepath"
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

func TestMultipleValidatorsKeepSeparateNodeState(t *testing.T) {
	c := &Config{Chains: map[string]*ChainConfig{
		"Osmosis": {
			ChainId: "osmosis-1",
			Nodes:   []*NodeConfig{{Url: "http://localhost:26657"}},
			Validators: []ValidatorConfig{
				{Name: "Primary", ValAddress: "osmovaloper1primary"},
				{Name: "Backup", ValAddress: "osmovaloper1backup"},
			},
		},
	}}
	if err := expandValidators(c); err != nil {
		t.Fatal(err)
	}
	primary := c.Chains["Osmosis / Primary"]
	backup := c.Chains["Osmosis / Backup"]
	if primary == nil || backup == nil || primary.ValAddress == backup.ValAddress {
		t.Fatalf("validators were not expanded: %+v", c.Chains)
	}
	primary.Nodes[0].down = true
	if backup.Nodes[0].down {
		t.Fatal("validator node state is shared")
	}
}
