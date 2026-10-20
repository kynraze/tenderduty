package tenderduty

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
	"golang.org/x/term"
)

var restartSettings = []string{
	"enable_dashboard", "listen_port", "prometheus_enabled", "prometheus_listen_port",
	"otel_metrics.listen", "otel_metrics.tls_cert", "otel_metrics.tls_key", "hide_logs", "healthcheck",
}

func configField(v reflect.Value, key string) reflect.Value {
	for _, part := range strings.Split(key, ".") {
		t := v.Type()
		for i := 0; i < t.NumField(); i++ {
			if strings.Split(t.Field(i).Tag.Get("yaml"), ",")[0] == part {
				v = v.Field(i)
				break
			}
		}
	}
	return v
}

func configSettingTypes() (map[string]reflect.Type, []string) {
	keys := make(map[string]reflect.Type)
	var order []string
	var walk func(reflect.Type, string)
	walk = func(t reflect.Type, prefix string) {
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			key := strings.Split(f.Tag.Get("yaml"), ",")[0]
			if f.PkgPath != "" || key == "" || key == "-" || key == "chains" {
				continue
			}
			key = prefix + key
			kind := f.Type
			if kind.Kind() == reflect.Ptr {
				kind = kind.Elem()
			}
			if kind.Kind() == reflect.Struct {
				walk(kind, key+".")
			} else {
				keys[key] = kind
				order = append(order, key)
			}
		}
	}
	walk(reflect.TypeOf(Config{}), "")
	return keys, order
}

func restartSetting(key string) bool {
	for _, restart := range restartSettings {
		if key == restart || strings.HasPrefix(key, restart+".") {
			return true
		}
	}
	return false
}

const configHelpText = `Show or change config.yml. set keeps its comments and key order.

Usage:
  tenderduty config show [section]
  tenderduty config set key=value [key=value ...]

Examples:
  tenderduty config show telegram
  tenderduty config set reports.daily=09:00 reports.timezone=Asia/Jakarta
  tenderduty config set otel_metrics.listen=10.0.0.5:4318 otel_metrics.token=generate
  echo "$BOT_TOKEN" | tenderduty config set telegram.api_key=-

Use =- to read a value from stdin, or token=generate for a random token.

Flags:
  -h, -help                  help for config
` + GlobalFlagsHelp

func ConfigHelp(args []string) (string, bool) {
	if len(args) == 0 || len(args) == 1 && (args[0] == "show" || args[0] == "set") {
		return configHelpText, true
	}
	return "", false
}

func ConfigCommand(ctx context.Context, options ChainCommandOptions, args []string, input io.Reader, output io.Writer) error {
	_ = ctx
	previous := cliMode
	cliMode = true
	defer func() { cliMode = previous }()
	if len(args) == 0 || isHelpFlag(args[0]) {
		_, err := io.WriteString(output, configHelpText)
		return err
	}
	command := args[0]
	if command == "help" {
		if help, ok := ConfigHelp(args[1:]); ok {
			_, err := io.WriteString(output, help)
			return err
		}
	}
	if command != "show" && command != "set" {
		return &UsageError{Err: fmt.Errorf("unknown config command %q", command), Usage: configHelpText}
	}
	flags := flag.NewFlagSet("config "+command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&options.Home, "home", options.Home, "home directory")
	flags.StringVar(&options.ConfigFile, "f", options.ConfigFile, "configuration file")
	flags.StringVar(&options.Directory, "cc", options.Directory, "chain directory")
	flags.StringVar(&options.StateFile, "state", options.StateFile, "state file")
	if err := parseCommandFlags(flags, args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, err = io.WriteString(output, configHelpText)
			return err
		}
		return &UsageError{Err: err, Usage: configHelpText}
	}
	args = flags.Args()
	home, err := ResolveHome(options.Home)
	if err != nil {
		return err
	}
	options.ConfigFile, options.Directory, options.StateFile = ResolvePaths(home, options.ConfigFile, options.Directory, options.StateFile)
	options.ConfigFile, err = requireChainConfig(options.ConfigFile, options.Home)
	if err != nil {
		return err
	}
	if options.Password == nil {
		password := ""
		options.Password = &password
	}
	if command == "show" {
		if len(args) > 1 {
			return &UsageError{Err: fmt.Errorf("show takes at most one section"), Usage: configHelpText}
		}
		section := ""
		if len(args) == 1 {
			section = strings.ToLower(args[0])
		}
		return showConfig(options, section, output)
	}
	if len(args) == 0 {
		return &UsageError{Err: fmt.Errorf("set needs at least one key=value"), Usage: configHelpText}
	}
	return setConfig(options, args, input, output)
}

func showConfig(options ChainCommandOptions, section string, output io.Writer) error {
	keys, order := configSettingTypes()
	sections := []string{"general", "pagerduty", "discord", "telegram", "slack", "gotify", "quiet_hours", "healthcheck", "reports", "otel_metrics", "alert_defaults"}
	for _, key := range order {
		top, _, nested := strings.Cut(key, ".")
		if !nested {
			continue
		}
		found := false
		for _, name := range sections {
			if name == top {
				found = true
			}
		}
		if !found {
			sections = append(sections, top)
		}
	}
	if section != "" {
		found := false
		for _, name := range sections {
			if section == name {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("unknown section %q; sections: %s", section, strings.Join(sections, ", "))
		}
	}
	c, err := loadConfigForEdit(options.ConfigFile, options.Directory, options.Password)
	if err != nil {
		return fmt.Errorf("cannot load config: %w", err)
	}
	defer c.cancel()
	defer c.deliveryCancel()
	validateConfig(c)
	order, sections = configDisplayOrder(c.configData, keys, order, sections)
	sources, err := chainSources(options)
	if err != nil {
		return err
	}
	own := make(map[string][]string)
	var shared yaml.Node
	if err := yaml.Unmarshal(c.configData, &shared); err != nil {
		return err
	}
	originals := make(map[string]*yaml.Node)
	for name, file := range sources {
		data, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		doc, err := yamlDocument(data)
		if err != nil {
			return err
		}
		originals[name] = doc
	}
	inline := configYAMLPath(&shared, "chains")
	if inline != nil {
		for i := 0; i+1 < len(inline.Content); i += 2 {
			name := inline.Content[i].Value
			if originals[name] == nil {
				originals[name] = inline.Content[i+1]
			}
		}
	}
	for name, doc := range originals {
		for key, kind := range keys {
			if !strings.HasPrefix(key, "alert_defaults.") {
				continue
			}
			node := yamlPath(doc, "alerts."+strings.TrimPrefix(key, "alert_defaults."))
			if node == nil {
				continue
			}
			text := settingText(kind, node)
			if text == "unset" || text == `""` {
				text = "-"
			}
			if secretSetting(key) {
				text = "***"
				if node.Value == "" {
					text = "-"
				}
			}
			own[key] = append(own[key], fmt.Sprintf("%s (%s)", name, text))
		}
	}
	example := ""
	fmt.Fprintln(output, options.ConfigFile)
	for _, group := range sections {
		if section != "" && section != group {
			continue
		}
		fmt.Fprintf(output, "\n%s:\n", group)
		for _, key := range order {
			top, _, nested := strings.Cut(key, ".")
			if group == "general" && nested || group != "general" && top != group {
				continue
			}
			value := configField(reflect.ValueOf(c).Elem(), key)
			var node yaml.Node
			effective := value.Interface()
			if err := node.Encode(effective); err != nil {
				return err
			}
			text := settingText(keys[key], &node)
			if node.Kind == yaml.MappingNode {
				node.Style = yaml.FlowStyle
				data, err := yaml.Marshal(&node)
				if err != nil {
					return err
				}
				text = strings.TrimSpace(string(data))
			}
			if text == "unset" || text == `""` {
				text = "-"
			}
			if secretSetting(key) {
				text = "-"
				if !value.IsZero() {
					text = "***"
				}
			}
			label := key
			if group != "general" {
				label = strings.TrimPrefix(key, group+".")
			}
			if example == "" {
				value := "<value>"
				if keys[key].Kind() == reflect.Bool {
					value = "yes"
					if text == "yes" {
						value = "no"
					}
				}
				example = key + "=" + value
			}
			fmt.Fprintf(output, "  %s: %s", label, text)
			if restartSetting(key) {
				fmt.Fprint(output, " (restart to change)")
			}
			if names := own[key]; len(names) > 0 {
				sort.Strings(names)
				extra := ""
				if len(names) > 5 {
					extra = fmt.Sprintf(", +%d more", len(names)-5)
					names = names[:5]
				}
				fmt.Fprintf(output, " · own value on: %s%s", strings.Join(names, ", "), extra)
			}
			fmt.Fprintln(output)
		}
	}
	fmt.Fprintf(output, "\nChange one: tenderduty config set %s\n", example)
	return nil
}

func configDisplayOrder(data []byte, keys map[string]reflect.Type, order, sections []string) ([]string, []string) {
	var doc yaml.Node
	if yaml.Unmarshal(data, &doc) != nil || len(doc.Content) != 1 {
		return order, sections
	}
	seen := make(map[string]bool)
	groups := map[string]bool{"general": true}
	result := make([]string, 0, len(order))
	grouped := []string{"general"}
	var walk func(*yaml.Node, string)
	walk = func(node *yaml.Node, prefix string) {
		if node.Kind != yaml.MappingNode {
			return
		}
		for i := 0; i+1 < len(node.Content); i += 2 {
			key := prefix + node.Content[i].Value
			if _, ok := keys[key]; ok {
				result = append(result, key)
				seen[key] = true
			} else {
				walk(node.Content[i+1], key+".")
			}
		}
	}
	walk(doc.Content[0], "")
	for _, key := range order {
		if !seen[key] {
			result = append(result, key)
		}
	}
	for i := 0; i+1 < len(doc.Content[0].Content); i += 2 {
		name := doc.Content[0].Content[i].Value
		for _, section := range sections {
			if section == name && !groups[name] {
				grouped = append(grouped, name)
				groups[name] = true
			}
		}
	}
	for _, section := range sections {
		if !groups[section] {
			grouped = append(grouped, section)
		}
	}
	return result, grouped
}

func configYAMLPath(doc *yaml.Node, key string) *yaml.Node {
	if doc.Kind == yaml.DocumentNode {
		if len(doc.Content) == 0 {
			return nil
		}
		doc = doc.Content[0]
	}
	for _, part := range strings.Split(key, ".") {
		if doc == nil || doc.Kind != yaml.MappingNode {
			return nil
		}
		doc = lookupMapping(doc, part)
	}
	return doc
}

func setConfig(options ChainCommandOptions, args []string, input io.Reader, output io.Writer) error {
	if strings.HasSuffix(options.ConfigFile, ".asc") || strings.HasPrefix(options.ConfigFile, "http://") || strings.HasPrefix(options.ConfigFile, "https://") {
		return fmt.Errorf("config.yml is encrypted/remote; decrypt it (tenderduty -decrypt), edit, then encrypt again")
	}
	keys, _ := configSettingTypes()
	generated := make(map[string]string)
	reader := bufio.NewReader(input)
	settings := make([]string, 0, len(args))
	for _, arg := range args {
		key, value, ok := strings.Cut(arg, "=")
		if !ok || strings.TrimSpace(key) != key || strings.TrimSpace(value) != value || key == "" {
			return fmt.Errorf(`write key=value without spaces around "="`)
		}
		key = strings.ToLower(key)
		if key == "servers" || strings.HasPrefix(key, "servers.") {
			return fmt.Errorf("edit servers in config.yml by hand")
		}
		if key == "chains" || strings.HasPrefix(key, "chains.") {
			return fmt.Errorf("chains settings are managed with tenderduty chain set")
		}
		if _, ok := keys[key]; !ok {
			_, err := parseSetting(keys, arg)
			return err
		}
		if value == "-" {
			if file, ok := input.(*os.File); ok && term.IsTerminal(int(file.Fd())) {
				data, err := term.ReadPassword(int(file.Fd()))
				if err != nil {
					return err
				}
				value = string(data)
			} else {
				line, err := reader.ReadString('\n')
				if err != nil && err != io.EOF {
					return err
				}
				value = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
			}
		} else if strings.HasSuffix(key, ".token") && value == "generate" {
			data := make([]byte, 32)
			if _, err := rand.Read(data); err != nil {
				return err
			}
			value = hex.EncodeToString(data)
			generated[key] = value
		}
		settings = append(settings, key+"="+value)
	}
	data, err := os.ReadFile(options.ConfigFile)
	if err != nil {
		return err
	}
	candidate, changes, err := applySettingsDiff(data, settings, keys)
	if err != nil {
		if strings.HasPrefix(err.Error(), "yaml:") {
			return fmt.Errorf("%s: %w", filepath.Base(options.ConfigFile), err)
		}
		return err
	}
	changed := false
	var restart []string
	for _, change := range changes {
		if change.Changed {
			changed = true
			if restartSetting(change.Key) {
				restart = append(restart, change.Key)
			}
		}
	}
	if changed {
		stage, err := os.MkdirTemp("", "tenderduty-config-*")
		if err != nil {
			return err
		}
		defer os.RemoveAll(stage)
		file := filepath.Join(stage, "config.yml")
		if err := os.WriteFile(file, candidate, 0600); err != nil {
			return err
		}
		c, err := loadConfigForEdit(file, options.Directory, options.Password)
		if err != nil {
			return err
		}
		_, problems := validateConfig(c)
		c.cancel()
		c.deliveryCancel()
		live := liveProblems(options.ConfigFile, options.Directory, options.Password)
		if err := addedErrors(live, problems); err != nil {
			return err
		}
		defer func() {
			for _, problem := range problems {
				if strings.HasPrefix(problem, "error") {
					fmt.Fprintln(output, "Still to fix:", strings.TrimPrefix(problem, "error: "))
				}
			}
			printAddedWarnings(output, live, problems)
		}()
		info, err := os.Stat(options.ConfigFile)
		if err != nil {
			return err
		}
		if err := writeChainFile(options.ConfigFile, candidate, info.Mode().Perm(), true); err != nil {
			return writeError(options.ConfigFile, err)
		}
	}
	for _, change := range changes {
		if token := generated[change.Key]; token != "" {
			fmt.Fprintf(output, "%s: generated, copy it to Fibre/bridge now: %s\n", change.Key, token)
			delete(generated, change.Key)
			continue
		}
		if secretSetting(change.Key) {
			if change.Old == "unset" || change.Old == `""` {
				change.Old = "-"
			} else {
				change.Old = "***"
			}
			if change.New == `""` {
				change.New = "-"
			} else {
				change.New = "***"
			}
		}
		if change.Changed {
			fmt.Fprintf(output, "%s: %s -> %s\n", change.Key, change.Old, change.New)
		} else {
			fmt.Fprintf(output, "%s: already %s\n", change.Key, change.New)
		}
	}
	if changed {
		if len(restart) > 0 {
			fmt.Fprintf(output, "Saved; restart tenderduty to apply %s\n", strings.Join(restart, ", "))
		} else {
			reloadAfterEdit(options.StateFile, output, options.reload)
		}
	}
	return nil
}
