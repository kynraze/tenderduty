package tenderduty

import (
	"reflect"
	"strings"
	"testing"
)

func TestReloadSharedSettingsSummary(t *testing.T) {
	old, next := &Config{}, &Config{}
	next.FibreMetrics.ExportRaw = true
	next.Telegram.MinSeverity = "warning"
	next.Telegram.ApiKey = "secret-new-token"
	next.Listen = ":9000"
	next.Chains = map[string]*ChainConfig{"Chain": {ChainId: "new"}}
	restarts := keepListeners(old, next)
	applied := changedSharedSettings(old, next)
	want := []string{"fibre_metrics.export_raw", "telegram.api_key", "telegram.min_severity"}
	if !reflect.DeepEqual(applied, want) {
		t.Fatalf("applied = %v, want %v", applied, want)
	}
	summary := reloadSummary(chainDiff{}, applied, restarts)
	if !strings.HasPrefix(summary, "reloaded config: settings applied: "+strings.Join(want, ", ")+"; ") || !strings.Contains(summary, "1 settings need a restart") || strings.Contains(summary, next.Telegram.ApiKey) {
		t.Fatalf("incorrect or secret-bearing summary: %s", summary)
	}
	if got := reloadSummary(chainDiff{}, changedSharedSettings(old, old), nil); got != "reloaded config: no changes" {
		t.Fatal(got)
	}
	if got := reloadSummary(chainDiff{Added: []string{"Chain"}}, nil, nil); got != "reloaded config: 1 added, 0 removed, 0 restarted, 0 updated, 0 settings need a restart" {
		t.Fatal(got)
	}
}

func TestUnknownConfigKeySuggestions(t *testing.T) {
	for _, test := range []struct {
		data   string
		target interface{}
		want   []string
	}{
		{"enable_dashbord: yes\n", &Config{}, []string{"warning: enable_dashbord is not a setting; did you mean enable_dashboard?"}},
		{"alert_defaults:\n  gov_priority: warning\n", &Config{}, []string{"warning: alert_defaults.gov_priority is not a setting; did you mean alert_defaults.gov_severity?"}},
		{"alerts:\n  gov_priority: warning\n  telegram:\n    min_severty: info\n", &ChainConfig{}, []string{
			"warning: alerts.gov_priority is not a setting; did you mean alerts.gov_severity?",
			"warning: alerts.telegram.min_severty is not a setting; did you mean alerts.telegram.min_severity?",
		}},
		{"chains:\n  Chain:\n    alerts:\n      gov_priority: warning\n", &Config{}, []string{"warning: chains.Chain.alerts.gov_priority is not a setting; did you mean chains.Chain.alerts.gov_severity?"}},
		{"fibre_metrics:\n  export_raw: yes\n", &Config{}, nil},
		{"alerts:\n  gov_priority: warning\n  gov_priority: info\n", &ChainConfig{}, []string{"warning: alerts.gov_priority is not a setting; did you mean alerts.gov_severity?"}},
	} {
		if got := unknownConfigWarnings([]byte(test.data), test.target); !reflect.DeepEqual(got, test.want) {
			t.Fatalf("%s: got %v, want %v", test.data, got, test.want)
		}
	}
}

func TestConfigSuggestionsKeepOtherDecodeProblems(t *testing.T) {
	data := []byte("node_down_alert_minutes: invalid\nenable_dashbord: yes\n")
	err := decodeConfig(data, &Config{})
	if err == nil {
		t.Fatal("invalid config accepted")
	}
	problems := configDecodeProblems(err)
	if len(problems) != 1 || !strings.Contains(problems[0], "cannot unmarshal") {
		t.Fatalf("lost value error or repeated unknown key: %v", problems)
	}
	if warnings := unknownConfigWarnings(data, &Config{}); len(warnings) != 1 || !strings.Contains(warnings[0], "did you mean enable_dashboard?") {
		t.Fatalf("missing key suggestion: %v", warnings)
	}
}
