package tenderduty

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigYAMLRejectsUnknownFieldsDuplicatesAndExtraDocuments(t *testing.T) {
	for _, data := range []string{
		"enable_dashbord: yes\n",
		"chains:\n  Chain:\n    chain_id: demo\n    unknown_field: true\n",
		"enable_dashboard: yes\nenable_dashboard: no\n",
		"enable_dashboard: yes\n---\nenable_dashboard: no\n",
	} {
		if err := decodeConfig([]byte(data), &Config{}); err == nil {
			t.Fatalf("invalid configuration accepted: %s", data)
		}
	}
}

func TestConfigYAMLBooleanDefaultsAndOverrides(t *testing.T) {
	for _, spelling := range []struct{ yes, no string }{{"yes", "no"}, {"on", "off"}, {"true", "false"}, {"YES", "NO"}} {
		data := []byte("alert_defaults:\n  stalled_enabled: " + spelling.yes + "\n  stalled_minutes: 10\n  telegram:\n    enabled: " + spelling.yes + "\nchains:\n  Chain:\n    chain_id: demo\n    valoper_address: demovaloper1example\n    alerts:\n      telegram:\n        enabled: " + spelling.no + "\n")
		c := &Config{}
		if err := decodeConfig(data, c); err != nil {
			t.Fatal(err)
		}
		if err := applyAlertDefaults(c, data, nil); err != nil {
			t.Fatal(err)
		}
		if !c.Chains["Chain"].Alerts.StalledAlerts || c.Chains["Chain"].Alerts.Telegram.Enabled {
			t.Fatalf("boolean inheritance changed for %+v", spelling)
		}
	}
}

func TestExampleConfigurationAndChainFileStillLoad(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "example-config.yml"))
	if err != nil {
		t.Fatal(err)
	}
	c := &Config{}
	if err := decodeConfig(data, c); err != nil {
		t.Fatal(err)
	}
	if c.Healthcheck.PingRate != 60 {
		t.Fatal("numeric healthcheck rate no longer represents seconds")
	}
	if err := applyAlertDefaults(c, data, nil); err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	configFile := filepath.Join(directory, "config.yml")
	chainDirectory := filepath.Join(directory, "chains.d")
	if err := os.Mkdir(chainDirectory, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configFile, []byte("node_down_alert_minutes: 3\nalert_defaults:\n  consecutive_enabled: yes\n  consecutive_missed: 5\n  telegram:\n    enabled: yes\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(chainDirectory, "Chain.yml"), []byte("chain_id: demo\nvaloper_address: demovaloper1example\nalerts:\n  telegram:\n    enabled: no\nnodes:\n  - url: http://localhost:26657\n"), 0600); err != nil {
		t.Fatal(err)
	}
	old := alarms
	t.Cleanup(func() { alarms = old })
	password := ""
	loaded, err := loadConfig(configFile, filepath.Join(directory, "state.json"), chainDirectory, &password)
	if err != nil {
		t.Fatal(err)
	}
	defer loaded.cancel()
	defer loaded.deliveryCancel()
	if !loaded.Chains["Chain"].Alerts.ConsecutiveAlerts || loaded.Chains["Chain"].Alerts.Telegram.Enabled {
		t.Fatal("chain file defaults or explicit false override changed")
	}
}

func TestConfigYAMLRejectsAliasExpansion(t *testing.T) {
	data := "a: &a [x,x,x,x,x,x,x,x,x]\nb: &b [*a,*a,*a,*a,*a,*a,*a,*a,*a]\nc: &c [*b,*b,*b,*b,*b,*b,*b,*b,*b]\nd: &d [*c,*c,*c,*c,*c,*c,*c,*c,*c]\ne: &e [*d,*d,*d,*d,*d,*d,*d,*d,*d]\nf: [*e,*e,*e,*e,*e,*e,*e,*e,*e]\n"
	var target map[string]interface{}
	if err := decodeConfig([]byte(data), &target); err == nil || !strings.Contains(err.Error(), "alias") {
		t.Fatalf("excessive aliases were not rejected: %v", err)
	}
}

func TestConfigYAMLStaysLenientLikeOlderVersions(t *testing.T) {
	c := &Config{}
	if err := unmarshalConfig("test", []byte("listen_port: \"1\"\nlisten_port: \"2\"\nunknown_key: yes\n"), c, &Config{}); err != nil || c.Listen != "2" {
		t.Fatalf("duplicate or unknown key stopped loading: %v %q", err, c.Listen)
	}
	var raw yamlMap
	if err := unmarshalLenient([]byte("alert_defaults:\n  stalled_minutes: 5\n  stalled_minutes: 10\n"), &raw); err != nil {
		t.Fatal(err)
	}
	if defaults, _ := asYAMLMap(raw["alert_defaults"]); defaults["stalled_minutes"] != 10 {
		t.Fatalf("nested duplicate did not keep its last value: %v", raw)
	}
	for _, empty := range []string{"", "# only a comment\n"} {
		if err := decodeConfig([]byte(empty), &ChainConfig{}); err != nil {
			t.Fatalf("empty file was reported as a problem: %v", err)
		}
	}
}
