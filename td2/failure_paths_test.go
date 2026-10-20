package tenderduty

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestFailurePathSeveritySettings(t *testing.T) {
	shared, _ := configSettingTypes()
	for _, keys := range []map[string]reflect.Type{chainSettingTypes(), shared} {
		for key := range keys {
			if !strings.HasSuffix(key, "_severity") || strings.HasSuffix(key, ".min_severity") {
				continue
			}
			for _, value := range []string{"loud", "escalation", "", "INFO", "info", "warning", "error", "critical"} {
				_, err := parseSetting(keys, key+"="+value)
				if validAlertSeverity(value) {
					if err != nil {
						t.Fatalf("%s=%s: %v", key, value, err)
					}
				} else if want := fmt.Sprintf("%s must be info, warning, error or critical, got %q", key, value); err == nil || err.Error() != want {
					t.Fatalf("%s=%s: got %v, want %s", key, value, err, want)
				}
			}
		}
	}
}

func TestFailurePathSeverityFallbacks(t *testing.T) {
	for _, inherited := range []bool{false, true} {
		t.Run(fmt.Sprint(inherited), func(t *testing.T) {
			options, filename, original := chainCommandFixture(t)
			severities := "  gov_severity: loud\n  fibre_severity: loud\n  bridge_severity: loud\n  missed_proposal_severity: loud\n  changes_severity: loud\n"
			config := "node_down_alert_severity: loud\n"
			if inherited {
				config += "alert_defaults:\n" + severities
			} else if err := os.WriteFile(filename, bytes.Replace(original, []byte("alerts:\n"), []byte("alerts:\n"+severities), 1), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(options.ConfigFile, []byte(config), 0600); err != nil {
				t.Fatal(err)
			}
			c, err := loadConfigQuiet(options.ConfigFile, options.Directory, options.Password)
			if err != nil {
				t.Fatal(err)
			}
			defer c.cancel()
			defer c.deliveryCancel()
			_, problems := validateConfig(c)
			a := c.Chains["Celestia"].Alerts
			if a.GovSeverity != "info" || a.FibreSeverity != "warning" || a.BridgeSeverity != "warning" || a.ProposalSeverity != "warning" || a.ChangesSeverity != "info" || c.NodeDownSeverity != "critical" {
				t.Fatalf("invalid severity survived: %+v, node down %q", a, c.NodeDownSeverity)
			}
			warnings := strings.Join(problems, "\n")
			for key, fallback := range map[string]string{"gov_severity": "info", "fibre_severity": "warning", "bridge_severity": "warning", "missed_proposal_severity": "warning", "changes_severity": "info"} {
				want := fmt.Sprintf("warning: Celestia %s %q is not info, warning, error or critical, using %s", key, "loud", fallback)
				if !strings.Contains(warnings, want) {
					t.Fatalf("missing %s in %s", want, warnings)
				}
			}
			if !strings.Contains(warnings, `warning: node_down_alert_severity "loud" is not info, warning, error or critical, using critical`) {
				t.Fatal(warnings)
			}
		})
	}
}

func TestFailurePathSeverityEditsLeaveFilesAlone(t *testing.T) {
	options, filename, original := chainCommandFixture(t)
	config, err := os.ReadFile(options.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	err = ChainCommand(context.Background(), options, []string{"set", "Celestia", "alerts.gov_severity=loud"}, strings.NewReader(""), io.Discard)
	if err == nil || err.Error() != `alerts.gov_severity must be info, warning, error or critical, got "loud"` {
		t.Fatalf("chain set: %v", err)
	}
	err = ConfigCommand(context.Background(), options, []string{"set", "alert_defaults.gov_severity=loud"}, strings.NewReader(""), io.Discard)
	if err == nil || err.Error() != `alert_defaults.gov_severity must be info, warning, error or critical, got "loud"` {
		t.Fatalf("config set: %v", err)
	}
	for name, want := range map[string][]byte{filename: original, options.ConfigFile: config} {
		got, err := os.ReadFile(name)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("refused edit changed %s: %v", name, err)
		}
	}
}

func TestFailurePathNodeDownMinutes(t *testing.T) {
	for _, minutes := range []int{0, -5} {
		c := &Config{NodeDownMin: minutes}
		_, problems := validateConfig(c)
		text := "is not set"
		if minutes < 0 {
			text = "can't be negative"
		}
		want := "warning: 'node_down_alert_minutes' " + text + ", using 3 minutes"
		if c.NodeDownMin != 3 || !strings.Contains(strings.Join(problems, "\n"), want) {
			t.Fatalf("minutes %d: %v", minutes, problems)
		}
	}
}

func TestFailurePathRenameName(t *testing.T) {
	options, _, _ := chainCommandFixture(t)
	err := renameChainFile(options, "Celestia", "bad/name", io.Discard)
	if err == nil || err.Error() != "the new name must be a file name without periods, path separators or reserved punctuation" {
		t.Fatal(err)
	}
}

func TestFailurePathYAMLFilename(t *testing.T) {
	options, _, _ := chainCommandFixture(t)
	broken := filepath.Join(options.Directory, "Broken.yml")
	if err := os.WriteFile(broken, []byte("[\n"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := loadConfigQuiet(options.ConfigFile, options.Directory, options.Password)
	if err == nil || !strings.Contains(err.Error(), "Broken.yml: yaml:") {
		t.Fatalf("chain error: %v", err)
	}
	if err := os.WriteFile(options.ConfigFile, []byte("[\n"), 0600); err != nil {
		t.Fatal(err)
	}
	err = ConfigCommand(context.Background(), options, []string{"set", "telegram.enabled=yes"}, strings.NewReader(""), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "config.yml: yaml:") {
		t.Fatalf("config set error: %v", err)
	}
	_, err = loadConfigForEdit(options.ConfigFile, options.Directory, options.Password)
	if err == nil || !strings.Contains(err.Error(), "config.yml: yaml:") {
		t.Fatalf("config load error: %v", err)
	}
}

func TestFailurePathConfigShowWithoutChains(t *testing.T) {
	for _, directoryPresent := range []bool{false, true} {
		t.Run(fmt.Sprint(directoryPresent), func(t *testing.T) {
			options, filename, _ := chainCommandFixture(t)
			if err := os.Remove(filename); err != nil {
				t.Fatal(err)
			}
			if !directoryPresent {
				if err := os.Remove(options.Directory); err != nil {
					t.Fatal(err)
				}
			}
			var output bytes.Buffer
			if err := ConfigCommand(context.Background(), options, []string{"show", "telegram"}, strings.NewReader(""), &output); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(output.String(), "telegram:") {
				t.Fatal(output.String())
			}
		})
	}
}

func TestFailurePathSuggestionThreshold(t *testing.T) {
	shared, _ := configSettingTypes()
	for _, test := range []struct {
		config bool
		key    string
		want   string
	}{
		{false, "nosuchkey", "see tenderduty chain show <Name>"},
		{false, "alerts.nosuch", "see tenderduty chain show <Name>"},
		{true, "nosuchkey", "see tenderduty config show"},
		{false, "alerts.fibre_enabld", "did you mean alerts.fibre_enabled?"},
		{true, "listen_prt", "did you mean listen_port?"},
	} {
		keys := chainSettingTypes()
		if test.config {
			keys = shared
		}
		_, err := parseSetting(keys, test.key+"=yes")
		if err == nil || !strings.HasSuffix(err.Error(), test.want) {
			t.Fatalf("%s: %v, want %s", test.key, err, test.want)
		}
	}
}
