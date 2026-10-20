package tenderduty

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"go.yaml.in/yaml/v3"
)

func chainFileName(name string) string {
	return strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if isControl(r) || strings.ContainsRune(`.<>:"/\|?*`, r) {
			return ' '
		}
		return r
	}, name)), "-")
}

func requireChainConfig(filename, home string) (string, error) {
	if strings.HasPrefix(filename, "http://") || strings.HasPrefix(filename, "https://") {
		return filename, nil
	}
	if _, err := os.Stat(filename); !os.IsNotExist(err) {
		return filename, err
	}
	if !strings.HasSuffix(filename, ".asc") {
		if _, err := os.Stat(filename + ".asc"); !os.IsNotExist(err) {
			return filename + ".asc", err
		}
	}
	dir := filepath.Dir(filename)
	hint := "tenderduty"
	if home != "" {
		hint += fmt.Sprintf(" -home %s", home)
	}
	return "", fmt.Errorf("no %s in %s; create one first:\n  %s -example-config > %s\nthen set its notifications and run this again", filepath.Base(filename), dir, hint, filename)
}

func chainSources(options ChainCommandOptions) (map[string]string, error) {
	sources := make(map[string]string)
	entries, err := os.ReadDir(options.Directory)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".yml") {
			sources[strings.Split(entry.Name(), ".")[0]] = filepath.Join(options.Directory, entry.Name())
		}
	}
	return sources, nil
}

func duplicateChainValidator(options AddChainOptions, chainID string, target *chainValidatorEdit) error {
	if options.ConfigFile == "" || options.Name == "" && target == nil {
		return nil
	}
	password := options.Password
	if password == nil {
		empty := ""
		password = &empty
	}
	c, err := loadConfigQuiet(options.ConfigFile, options.Directory, password)
	if errors.Is(err, errNoChains) {
		return nil
	}
	if err != nil {
		return err
	}
	defer c.cancel()
	defer c.deliveryCancel()
	sources, err := chainSources(ChainCommandOptions{Directory: options.Directory})
	if err != nil {
		return err
	}
	names := make([]string, 0, len(c.Chains))
	for name := range c.Chains {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		chain := c.Chains[name]
		if chain.ChainId != chainID || chain.ValAddress != options.Valoper {
			continue
		}
		base := strings.SplitN(name, " / ", 2)[0]
		file := sources[base]
		if file == "" {
			file = options.ConfigFile
		}
		same := sources[base] != "" && strings.EqualFold(base, options.Name)
		if target != nil {
			same = file == target.filename
		}
		if same {
			if options.Force {
				continue
			}
			return fmt.Errorf("%s already watches this validator (%s)", file, stripControl(options.Valoper))
		}
		return fmt.Errorf("%s is already watched by %s (%s)", stripControl(options.Valoper), name, file)
	}
	return nil
}

func compactAlertFamilies(groups []string) string {
	if len(groups) == 8 {
		return "all"
	}
	if len(groups) < 5 {
		return strings.Join(groups, ",")
	}
	enabled := make(map[string]bool)
	for _, name := range groups {
		enabled[name] = true
	}
	var off []string
	for _, name := range []string{"missed_blocks", "stalled", "no_servers", "gov", "upgrade", "missed_proposal", "fibre", "bridge"} {
		if !enabled[name] {
			off = append(off, name)
		}
	}
	return "all except " + strings.Join(off, ",")
}

func yamlPath(node *yaml.Node, path string) *yaml.Node {
	for _, key := range strings.Split(path, ".") {
		if node == nil {
			return nil
		}
		node = lookupMapping(node, key)
	}
	return node
}

func yamlDocument(data []byte) (*yaml.Node, error) {
	// Decode maps just like applyAlertDefaults, including YAML merges and aliases.
	var raw yamlMap
	if err := unmarshalLenient(data, &raw); err != nil {
		return nil, err
	}
	if raw == nil {
		raw = make(yamlMap)
	}
	return settingDocument(raw), nil
}

func settingDocument(value interface{}) *yaml.Node {
	var node yaml.Node
	_ = node.Encode(value)
	return &node
}

func showChain(options ChainCommandOptions, requested string, output io.Writer) error {
	c, err := loadConfigQuiet(options.ConfigFile, options.Directory, options.Password)
	if err != nil {
		return err
	}
	defer c.cancel()
	defer c.deliveryCancel()
	sources, err := chainSources(options)
	if err != nil {
		return err
	}
	raw, err := yamlDocument(c.configData)
	if err != nil {
		return err
	}
	names := make(map[string]bool)
	for name := range c.Chains {
		names[strings.SplitN(name, " / ", 2)[0]] = true
	}
	var matches []string
	name := ""
	for candidate := range names {
		if candidate == requested {
			name = candidate
		}
		if strings.EqualFold(candidate, requested) {
			matches = append(matches, candidate)
		}
	}
	sort.Strings(matches)
	if name == "" {
		if len(matches) > 1 {
			return fmt.Errorf("%q matches %s; use the exact name", requested, strings.Join(matches, ", "))
		}
		if len(matches) == 0 {
			return fmt.Errorf("no chain %q; see tenderduty chain list", requested)
		}
		name = matches[0]
	}
	filename := sources[name]
	chainRaw := yamlPath(raw, "chains")
	if chainRaw != nil {
		chainRaw = lookupMapping(chainRaw, name)
	}
	if filename != "" {
		// #nosec G304 G703 -- path from the operator's chains.d
		data, err := os.ReadFile(filename)
		if err != nil {
			return err
		}
		chainRaw, err = yamlDocument(data)
		if err != nil {
			return err
		}
	} else {
		filename = options.ConfigFile
	}
	if chainRaw == nil {
		return fmt.Errorf("no configuration for %s", name)
	}
	var original ChainConfig
	if err := chainRaw.Decode(&original); err != nil {
		return err
	}
	effectiveName := name
	if len(original.Validators) > 0 {
		effectiveName += " / " + original.Validators[0].Name
	}
	chain := c.Chains[effectiveName]
	if chain == nil {
		return fmt.Errorf("no configuration for %s", effectiveName)
	}
	var registryErr error
	if chain.Testnet == nil {
		pathMux.Lock()
		_, known := cosmosPaths[chain.ChainId]
		pathMux.Unlock()
		if !known {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			registryErr = refreshRegistry(ctx)
			cancel()
		}
	}
	before := settingDocument(chain)
	// Use the monitor's defaults and shared notification merge, without contacting nodes.
	validateConfig(c)
	effective := settingDocument(chain)
	source := filename
	if relative, err := filepath.Rel(filepath.Dir(options.ConfigFile), filename); err == nil {
		source = filepath.ToSlash(relative)
	}
	table := tabwriter.NewWriter(output, 0, 0, 2, ' ', 0)
	fmt.Fprintf(table, "%s \u00b7 %s \u00b7 %s\n", name, chain.ChainId, filename)
	if len(original.Validators) == 0 {
		fmt.Fprintf(table, "Validator\t%s\n", chain.ValAddress)
	} else {
		fmt.Fprintln(table, "Validators")
		for _, validator := range original.Validators {
			fmt.Fprintf(table, "  %s\t%s\n", validator.Name, validator.ValAddress)
			for _, node := range validator.Nodes {
				fmt.Fprintf(table, "    %s\talert_if_down: %s\n", node.Url, yesNo(node.AlertIfDown))
			}
		}
	}
	fmt.Fprintln(table, "Nodes")
	for _, node := range original.Nodes {
		fmt.Fprintf(table, "  %s\talert_if_down: %s\n", node.Url, yesNo(node.AlertIfDown))
	}
	keys := chainSettingTypes()
	var ordered []string
	for key := range keys {
		if key != "chain_id" && key != "valoper_address" && legacyAlertSettings[strings.TrimPrefix(key, "alerts.")] == "" {
			ordered = append(ordered, key)
		}
	}
	group := func(key string) int {
		if strings.HasPrefix(key, "alerts.") {
			return 2
		}
		if strings.HasPrefix(key, "bridge.") {
			return 1
		}
		return 0
	}
	sort.Slice(ordered, func(i, j int) bool {
		a, b := group(ordered[i]), group(ordered[j])
		if a != b {
			return a < b
		}
		return ordered[i] < ordered[j]
	})
	alertsHeading := false
	for _, key := range ordered {
		from := "default"
		for old, replacement := range legacyAlertSettings {
			if strings.HasSuffix(old, "_alerts") && replacement == "now "+key {
				if yamlPath(chainRaw, "alerts."+old) != nil {
					from = source
				} else if yamlPath(raw, "alert_defaults."+old) != nil {
					from = "config.yml alert_defaults"
				}
			}
		}
		if yamlPath(chainRaw, key) != nil {
			from = source
		} else if strings.HasPrefix(key, "alerts.") && yamlPath(raw, "alert_defaults."+strings.TrimPrefix(key, "alerts.")) != nil {
			from = "config.yml alert_defaults"
		}
		prior, after := yamlPath(before, key), yamlPath(effective, key)
		if !strings.Contains(strings.TrimPrefix(key, "alerts."), ".") && prior != nil && after != nil && prior.Value != after.Value {
			from = "default"
		}
		parts := strings.Split(key, ".")
		if len(parts) == 3 && parts[0] == "alerts" {
			shared := yamlPath(raw, strings.Join(parts[1:], "."))
			if shared != nil && prior != nil && (prior.Tag == "!!null" || prior.Kind == yaml.ScalarNode && prior.Value == "") && after != nil && after.Value != "" {
				from = "config.yml"
			}
			// Mentions/default_severity follow the credential, even when explicitly set on the chain.
			credential := "api_key"
			switch parts[1] {
			case "discord", "slack":
				credential = "webhook"
			case "gotify":
				credential = "url"
			}
			cred := yamlPath(before, "alerts."+parts[1]+"."+credential)
			if shared != nil && cred != nil && cred.Value == "" && (parts[2] == "mentions" && parts[1] != "pagerduty" || parts[2] == "default_severity" && parts[1] == "pagerduty") {
				from = "config.yml"
			}
		}
		text := settingText(keys[key], yamlPath(effective, key))
		if len(parts) == 3 && parts[0] == "alerts" && parts[2] == "enabled" {
			shared, local := notificationSwitches(c, chain, parts[1])
			text = yesNo(enabledFor(shared, local))
			if !on(shared) || local == nil {
				from = "default"
				if yamlPath(raw, parts[1]+".enabled") != nil {
					from = "config.yml"
				}
			}
		}
		if text == "unset" || text == `""` {
			text = "-"
		}
		if key == "testnet" && chain.Testnet == nil {
			pathMux.Lock()
			entry, known := cosmosPaths[chain.ChainId]
			partial := registryPartial
			pathMux.Unlock()
			text = yesNo(entry.testnet)
			from = "from the chain registry"
			if !known {
				from = "not in the chain registry; set testnet=yes for a testnet"
				if registryErr != nil || partial {
					from = "chain registry unavailable; set testnet=yes for a testnet"
				}
			}
		}
		if secretSetting(key) && text != "-" {
			text = "***"
		}
		prefix := ""
		if group(key) == 2 {
			prefix = "  "
		}
		if strings.HasPrefix(key, "alerts.") && !alertsHeading {
			fmt.Fprintln(table, "\nSettings\tvalue\tfrom")
			alertsHeading = true
		}
		fmt.Fprintf(table, "%s%s\t%s\t%s\n", prefix, key, text, from)
	}
	if chain.Alerts.FibreAlerts {
		listen := c.FibreMetrics.Listen
		if listen == "" {
			listen = "-"
		}
		fmt.Fprintf(table, "\nFibre\notel_metrics.listen\t%s\ntoken: %s\n", listen, tokenState(c.FibreMetrics.Token))
	}
	if chain.Bridge.URL != "" {
		fmt.Fprintf(table, "\nBridge\nbridge.url\t%s\ntoken: %s\n", chain.Bridge.URL, tokenState(chain.Bridge.token))
	}
	if sources[name] == "" {
		fmt.Fprintf(table, "\nDefined in %s (read-only here); edit that file directly\n", options.ConfigFile)
	} else {
		fmt.Fprintf(table, "\nChange one: tenderduty chain set %s <key>=<value>\n", name)
	}
	var old []string
	for key, replacement := range legacyAlertSettings {
		if yamlPath(chainRaw, "alerts."+key) != nil {
			old = append(old, "alerts."+key+" ("+replacement+")")
		}
	}
	if len(old) > 0 {
		sort.Strings(old)
		fmt.Fprintf(table, "\nOld setting names in %s: %s\n", filename, strings.Join(old, "; "))
	}
	return table.Flush()
}

func yesNo(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}
func tokenState(value string) string {
	if value != "" {
		return "set"
	}
	return "none"
}
func secretSetting(key string) bool {
	for _, suffix := range []string{"api_key", "webhook", "token", "ping_url", "password", "secret"} {
		if strings.HasSuffix(key, suffix) {
			return true
		}
	}
	return false
}

// The shared switch gates delivery; an unset chain switch follows it.
func notificationSwitches(c *Config, chain *ChainConfig, channel string) (*bool, *bool) {
	switch channel {
	case "telegram":
		return c.Telegram.Enabled, chain.Alerts.Telegram.Enabled
	case "discord":
		return c.Discord.Enabled, chain.Alerts.Discord.Enabled
	case "slack":
		return c.Slack.Enabled, chain.Alerts.Slack.Enabled
	case "gotify":
		return c.Gotify.Enabled, chain.Alerts.Gotify.Enabled
	case "pagerduty":
		return c.Pagerduty.Enabled, chain.Alerts.Pagerduty.Enabled
	}
	return nil, nil
}
