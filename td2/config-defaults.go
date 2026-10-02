package tenderduty

import (
	"fmt"
	"net/url"

	"go.yaml.in/yaml/v3"
)

type yamlMap map[interface{}]interface{}

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
		merged := mergeMaps(defaults, overrides)
		if len(merged) == 0 {
			continue
		}
		data, err := yaml.Marshal(merged)
		if err != nil {
			return err
		}
		if err := yaml.Unmarshal(data, &chain.Alerts); err != nil {
			return fmt.Errorf("%s alerts: %w", name, err)
		}
	}
	return nil
}
