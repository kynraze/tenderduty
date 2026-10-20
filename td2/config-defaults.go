package tenderduty

import (
	"fmt"
	"net/url"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

type yamlMap map[interface{}]interface{}

// Keep compatibility names in one place for loader warnings and CLI notes.
var legacyAlertSettings = map[string]string{
	"discord_alerts":       "now alerts.discord.enabled",
	"telegram_alerts":      "now alerts.telegram.enabled",
	"pagerduty_alerts":     "now alerts.pagerduty.enabled",
	"percentage_missed":    "now alerts.jail_risk_percent",
	"percentage_enabled":   "now alerts.missed_blocks_enabled",
	"percentage_priority":  "ignored; stage severities are fixed",
	"consecutive_priority": "ignored; stage severities are fixed",
}

func validHTTPURL(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && parsed.Host != "" && (parsed.Scheme == "https" || parsed.Scheme == "http")
}

func asYAMLMap(value interface{}) (yamlMap, bool) {
	switch typed := value.(type) {
	case yamlMap:
		return typed, true
	case map[interface{}]interface{}:
		return typed, true
	case map[string]interface{}:
		result := make(yamlMap, len(typed))
		for key, value := range typed {
			result[key] = value
		}
		return result, true
	default:
		return nil, false
	}
}

func optionalMap(parent yamlMap, key string) (yamlMap, error) {
	value, ok := parent[key]
	if !ok || value == nil {
		return nil, nil
	}
	result, ok := asYAMLMap(value)
	if !ok {
		return nil, fmt.Errorf("%s must be a mapping", key)
	}
	return result, nil
}

func mergeMaps(base, overrides yamlMap) yamlMap {
	result := make(yamlMap, len(base)+len(overrides))
	for key, value := range base {
		if nested, ok := asYAMLMap(value); ok {
			result[key] = mergeMaps(nested, nil)
		} else {
			result[key] = value
		}
	}
	for key, value := range overrides {
		if nested, ok := asYAMLMap(value); ok {
			if prior, ok := asYAMLMap(result[key]); ok {
				result[key] = mergeMaps(prior, nested)
			} else {
				result[key] = mergeMaps(nil, nested)
			}
		} else {
			result[key] = value
		}
	}
	return result
}

// applyAlertDefaults allows each chain to override only the settings that differ.
func applyAlertDefaults(c *Config, configData []byte, chainFiles map[string][]byte) error {
	var raw yamlMap
	if err := unmarshalLenient(configData, &raw); err != nil {
		return err
	}
	defaults, err := optionalMap(raw, "alert_defaults")
	if err != nil {
		return err
	}
	ignored := make(map[string]bool)
	percentage := false
	inspect := func(settings yamlMap) {
		for key := range settings {
			name, ok := key.(string)
			if !ok {
				continue
			}
			if strings.HasPrefix(name, "percentage_") {
				percentage = true
			}
			if name == "consecutive_priority" || name == "percentage_priority" {
				ignored[name] = true
			}
		}
	}
	inspect(defaults)
	defaults = mergeMaps(yamlMap{
		"bridge_enabled":            false,
		"bridge_severity":           "warning",
		"bridge_lag_blocks":         20,
		"bridge_lag_minutes":        5,
		"fibre_endorsement_alert":   false,
		"fibre_endorsement_blobs":   50,
		"fibre_endorsement_minutes": 120,
		"gov_early_remind_percent":  25,
		"gov_enabled":               true,
		"changes_enabled":           true,
		"changes_drop_percent":      10,
		"changes_rise_percent":      10,
		"changes_severity":          "info",
		"upgrade_enabled":           true,
		"missed_blocks_enabled":     true,
		"consecutive_enabled":       true,
		"alert_if_inactive":         true,
		"stalled_enabled":           true,
		"alert_if_no_servers":       true,
	}, defaults)
	chains, err := optionalMap(raw, "chains")
	if err != nil {
		return err
	}
	for name, chain := range c.Chains {
		if chain == nil {
			return fmt.Errorf("%s has no chain configuration", name)
		}
		var rawChain yamlMap
		if data, ok := chainFiles[name]; ok {
			if err := unmarshalLenient(data, &rawChain); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
		} else if value, ok := chains[name]; ok {
			var valid bool
			rawChain, valid = asYAMLMap(value)
			if !valid {
				return fmt.Errorf("%s must be a mapping", name)
			}
		}
		overrides, err := optionalMap(rawChain, "alerts")
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		inspect(overrides)
		var old []string
		for key, replacement := range legacyAlertSettings {
			if _, ok := overrides[key]; ok && strings.HasSuffix(key, "_alerts") {
				old = append(old, "alerts."+key+" ("+replacement+")")
			}
		}
		if len(old) > 0 {
			sort.Strings(old)
			c.configWarnings = append(c.configWarnings, "warning: old setting names on "+name+": "+strings.Join(old, "; "))
		}
		merged := mergeMaps(defaults, overrides)
		data, err := yaml.Marshal(merged)
		if err != nil {
			return err
		}
		if err := yaml.Unmarshal(data, &chain.Alerts); err != nil {
			return fmt.Errorf("%s alerts: %w", name, err)
		}
		chain.Alerts.PercentageAlerts = false
	}
	if percentage {
		c.configWarnings = append(c.configWarnings, "warning: percentage_* is replaced by jail_risk_percent, see docs/config.md")
	}
	if len(ignored) > 0 {
		names := make([]string, 0, len(ignored))
		for name := range ignored {
			names = append(names, name)
		}
		sort.Strings(names)
		c.configWarnings = append(c.configWarnings, "warning: "+strings.Join(names, ", ")+" are ignored, the alert stages have a fixed severity, see docs/config.md")
	}
	return nil
}
