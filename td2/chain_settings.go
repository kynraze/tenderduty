package tenderduty

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

func chainSettingTypes() map[string]reflect.Type {
	keys := make(map[string]reflect.Type)
	var walk func(reflect.Type, string)
	walk = func(t reflect.Type, prefix string) {
		for i := 0; i < t.NumField(); i++ {
			field := t.Field(i)
			key := strings.Split(field.Tag.Get("yaml"), ",")[0]
			if field.PkgPath != "" || key == "" || key == "-" || key == "nodes" || key == "validators" {
				continue
			}
			kind := field.Type
			if kind.Kind() == reflect.Ptr {
				kind = kind.Elem()
			}
			if kind.Kind() == reflect.Struct {
				walk(kind, prefix+key+".")
			} else {
				keys[prefix+key] = kind
			}
		}
	}
	walk(reflect.TypeOf(ChainConfig{}), "")
	return keys
}

func editDistance(a, b string) int {
	previous := make([]int, len(b)+1)
	for i := range previous {
		previous[i] = i
	}
	for i := 0; i < len(a); i++ {
		next := make([]int, len(b)+1)
		next[0] = i + 1
		for j := 0; j < len(b); j++ {
			cost := 0
			if a[i] != b[j] {
				cost = 1
			}
			next[j+1] = previous[j] + cost
			if previous[j+1]+1 < next[j+1] {
				next[j+1] = previous[j+1] + 1
			}
			if next[j]+1 < next[j+1] {
				next[j+1] = next[j] + 1
			}
		}
		previous = next
	}
	return previous[len(b)]
}

func settingValue(t reflect.Type, text string) (interface{}, bool) {
	switch t.Kind() {
	case reflect.String:
		return text, true
	case reflect.Bool:
		switch strings.ToLower(text) {
		case "yes", "true", "on":
			return true, true
		case "no", "false", "off":
			return false, true
		}
	case reflect.Int, reflect.Int64, reflect.Int32:
		value, err := strconv.ParseInt(text, 10, t.Bits())
		if err == nil {
			return value, true
		}
	case reflect.Slice:
		values := reflect.MakeSlice(t, 0, 0)
		if text == "" {
			return values.Interface(), true
		}
		for _, part := range strings.Split(text, ",") {
			value, ok := settingValue(t.Elem(), strings.TrimSpace(part))
			if !ok {
				return nil, false
			}
			values = reflect.Append(values, reflect.ValueOf(value).Convert(t.Elem()))
		}
		return values.Interface(), true
	}
	return nil, false
}

// settingExpects completes "<key> must be ..." for a value of type t.
func settingExpects(t reflect.Type) string {
	switch t.Kind() {
	case reflect.Bool:
		return "yes or no"
	case reflect.Int, reflect.Int64, reflect.Int32:
		return "a whole number"
	case reflect.Slice:
		switch t.Elem().Kind() {
		case reflect.Bool:
			return "a comma-separated list of yes/no values"
		case reflect.Int, reflect.Int64, reflect.Int32:
			return "a comma-separated list of whole numbers"
		}
		return "a comma-separated list"
	}
	return "text"
}

// settingChange holds a key and its displayed old and new values.
type settingChange struct {
	Key, Old, New string
	Changed       bool
}

type parsedSetting struct {
	key   string
	t     reflect.Type
	value interface{}
}

// parseSetting checks one key=value against the chain settings without touching any document.
func parseSetting(keys map[string]reflect.Type, assignment string) (parsedSetting, error) {
	key, text, ok := strings.Cut(assignment, "=")
	if !ok {
		return parsedSetting{}, fmt.Errorf("expected key=value, got %q", assignment)
	}
	key = strings.ToLower(key)
	t, ok := keys[key]
	if !ok {
		if key == "nodes" || strings.HasPrefix(key, "nodes.") {
			return parsedSetting{}, fmt.Errorf("%s is not settable; use chain node add or edit the file", key)
		}
		if key == "validators" || strings.HasPrefix(key, "validators.") {
			return parsedSetting{}, fmt.Errorf("%s is not settable; use chain add or edit the file", key)
		}
		names := make([]string, 0, len(keys))
		for name := range keys {
			names = append(names, name)
		}
		sort.Strings(names)
		// a cut-off key ("alerts.stalled") means one of the keys it starts
		var started []string
		for _, name := range names {
			if strings.HasPrefix(name, key) {
				started = append(started, name)
			}
		}
		if len(started) > 0 && len(started) <= 5 {
			return parsedSetting{}, fmt.Errorf("unknown setting %q; did you mean %s?", key, strings.Join(started, " or "))
		}
		closest, distance := "", int(^uint(0)>>1)
		for _, name := range names {
			if d := editDistance(key, name); d < distance {
				closest, distance = name, d
			}
		}
		return parsedSetting{}, fmt.Errorf("unknown setting %q; did you mean %s?", key, closest)
	}
	if strings.HasSuffix(key, ".min_severity") {
		if text != "info" && text != "warning" && text != "error" && text != "critical" {
			return parsedSetting{}, fmt.Errorf("%s must be info, warning, error or critical, got %q", key, text)
		}
	}
	value, ok := settingValue(t, text)
	if !ok {
		return parsedSetting{}, fmt.Errorf("%s must be %s, got %q", key, settingExpects(t), text)
	}
	return parsedSetting{key, t, value}, nil
}

// validateChainSettings checks assignments before writing or fetching.
func validateChainSettings(assignments []string) error {
	keys := chainSettingTypes()
	for _, assignment := range assignments {
		if _, err := parseSetting(keys, assignment); err != nil {
			return err
		}
	}
	return nil
}

// settingText formats booleans as yes/no and lists with commas.
func settingText(t reflect.Type, node *yaml.Node) string {
	if node == nil || node.Kind == yaml.ScalarNode && node.Tag == "!!null" {
		return "unset"
	}
	var text string
	switch node.Kind {
	case yaml.ScalarNode:
		text = node.Value
	case yaml.SequenceNode:
		items := make([]string, len(node.Content))
		for i, item := range node.Content {
			items[i] = item.Value
		}
		text = strings.Join(items, ",")
	default:
		return "unset"
	}
	if t.Kind() == reflect.Bool {
		if value, ok := settingValue(t, text); ok {
			text = map[bool]string{true: "yes", false: "no"}[value.(bool)]
		}
	}
	if text == "" {
		return `""`
	}
	return text
}

// lookupMapping returns the existing value for key, nil if absent.
func lookupMapping(node *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

func mappingValue(node *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	value := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	node.Content = append(node.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, value)
	return value
}

func applyChainSettings(data []byte, assignments []string) ([]byte, error) {
	result, _, err := applyChainSettingsDiff(data, assignments)
	return result, err
}

// applyChainSettingsDiff returns the original data when nothing changed.
func applyChainSettingsDiff(data []byte, assignments []string) ([]byte, []settingChange, error) {
	if len(assignments) == 0 {
		return data, nil, nil
	}
	var config ChainConfig
	if err := decodeConfig(data, &config); err != nil {
		return nil, nil, err
	}
	return applySettingsDiff(data, assignments, chainSettingTypes())
}

func applySettingsDiff(data []byte, assignments []string, keys map[string]reflect.Type) ([]byte, []settingChange, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, nil, err
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, nil, fmt.Errorf("settings file must be a YAML mapping")
	}
	var changes []settingChange
	anyChanged := false
	// a changed one-line value is replaced in its line, keeping blank lines and comment alignment
	lines := strings.Split(string(data), "\n")
	inPlace := true
	// a new key in an existing section goes in as one line after the section's last value
	type insertion struct {
		at   int
		text string
	}
	var inserts []insertion
	added := map[*yaml.Node]int{}
	for _, assignment := range assignments {
		setting, err := parseSetting(keys, assignment)
		if err != nil {
			return nil, nil, err
		}
		key := setting.key
		parts := strings.Split(key, ".")
		node := doc.Content[0]
		for _, part := range parts[:len(parts)-1] {
			if node.Kind != yaml.MappingNode {
				return nil, nil, fmt.Errorf("%s is not a mapping", part)
			}
			if lookupMapping(node, part) == nil {
				inPlace = false
			}
			node = mappingValue(node, part)
		}
		if node.Kind != yaml.MappingNode {
			return nil, nil, fmt.Errorf("parent of %s is not a mapping", key)
		}
		var replacement yaml.Node
		if err := replacement.Encode(setting.value); err != nil {
			return nil, nil, err
		}
		// config files say yes/no
		if setting.t.Kind() == reflect.Bool {
			replacement = yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: map[bool]string{true: "yes", false: "no"}[setting.value.(bool)]}
		}
		change := settingChange{Key: key, Old: settingText(setting.t, lookupMapping(node, parts[len(parts)-1])), New: settingText(setting.t, &replacement)}
		change.Changed = change.Old != change.New
		changes = append(changes, change)
		if !change.Changed {
			continue
		}
		anyChanged = true
		leaf := parts[len(parts)-1]
		existed := lookupMapping(node, leaf) != nil
		var at int
		var line string
		if !existed && inPlace {
			at, line, inPlace = keyLine(lines, node, leaf, &replacement, added)
			added[node] = at
		}
		target := mappingValue(node, leaf)
		// a text field written as a number (listen_port: 8891) stays unquoted
		if replacement.Kind == yaml.ScalarNode && replacement.Tag == "!!str" && target.Kind == yaml.ScalarNode && target.Style == 0 && target.Tag != "!!str" {
			var plain yaml.Node
			if yaml.Unmarshal([]byte(replacement.Value), &plain) == nil && len(plain.Content) == 1 && plain.Content[0].Tag == target.Tag {
				replacement.Tag, replacement.Style = target.Tag, 0
			}
		}
		// a file written with true/false keeps it
		if setting.t.Kind() == reflect.Bool && target.Kind == yaml.ScalarNode && (strings.EqualFold(target.Value, "true") || strings.EqualFold(target.Value, "false")) {
			replacement.Tag, replacement.Value = "!!bool", map[bool]string{true: "true", false: "false"}[setting.value.(bool)]
		}
		if inPlace && existed {
			inPlace = replaceInLine(lines, target, &replacement)
		} else if inPlace {
			inserts = append(inserts, insertion{at, line})
		}
		replacement.HeadComment, replacement.LineComment, replacement.FootComment = target.HeadComment, target.LineComment, target.FootComment
		*target = replacement
	}
	if !anyChanged {
		return data, changes, nil
	}
	if inPlace {
		eol := ""
		if bytes.Contains(data, []byte("\r\n")) {
			eol = "\r"
		}
		out := make([]string, 0, len(lines)+len(inserts))
		for i, line := range lines {
			out = append(out, line)
			for _, insert := range inserts {
				if insert.at == i+1 {
					out = append(out, insert.text+eol)
				}
			}
		}
		return []byte(strings.Join(out, "\n")), changes, nil
	}
	// keep the first key's comment from becoming a document comment
	if doc.HeadComment != "" && len(doc.Content[0].Content) > 0 && !headCommentStandsAlone(data) {
		first := doc.Content[0].Content[0]
		first.HeadComment = strings.TrimSuffix(doc.HeadComment+"\n"+first.HeadComment, "\n")
		doc.HeadComment = ""
	}
	var output bytes.Buffer
	encoder := yaml.NewEncoder(&output)
	encoder.SetIndent(2)
	if err := encoder.Encode(&doc); err != nil {
		return nil, nil, err
	}
	if err := encoder.Close(); err != nil {
		return nil, nil, err
	}
	result := output.Bytes()
	if bytes.HasPrefix(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")), []byte("---")) {
		result = append([]byte("---\n"), result...)
	}
	if bytes.Contains(data, []byte("\r\n")) {
		result = bytes.ReplaceAll(result, []byte("\n"), []byte("\r\n"))
	}
	return result, changes, nil
}

// keyLine renders key: value at the indent of parent's keys, and the line it goes after; false when that needs more than one line.
func keyLine(lines []string, parent *yaml.Node, key string, value *yaml.Node, added map[*yaml.Node]int) (int, string, bool) {
	n := len(parent.Content)
	if n < 2 || parent.Style&yaml.FlowStyle != 0 {
		return 0, "", false
	}
	at := added[parent]
	if last := parent.Content[n-1]; at == 0 && last.Line > 0 && last.Line <= len(lines) {
		oneLine := last.Kind == yaml.ScalarNode && last.Style&(yaml.LiteralStyle|yaml.FoldedStyle) == 0
		flowList := last.Kind == yaml.SequenceNode && last.Style&yaml.FlowStyle != 0 && strings.Contains(lines[last.Line-1], "]")
		if oneLine || flowList {
			at = last.Line
		}
	}
	v := *value
	if v.Kind == yaml.SequenceNode {
		v.Style = yaml.FlowStyle
	}
	rendered, err := yaml.Marshal(&v)
	text := strings.TrimSuffix(string(rendered), "\n")
	if at == 0 || err != nil || strings.Contains(text, "\n") {
		return 0, "", false
	}
	return at, strings.Repeat(" ", parent.Content[0].Column-1) + key + ": " + text, true
}

// replaceInLine swaps a one-line scalar for another in lines; false when the value spans lines or can't be located.
func replaceInLine(lines []string, old, next *yaml.Node) bool {
	// a one-line flow list like mentions: [] stays a flow list
	flow := old.Kind == yaml.SequenceNode && old.Style&yaml.FlowStyle != 0 && next.Kind == yaml.SequenceNode
	if !flow && (old.Kind != yaml.ScalarNode || next.Kind != yaml.ScalarNode) || old.Line < 1 || old.Line > len(lines) || old.Kind == yaml.ScalarNode && old.Value == "" && old.Style == 0 {
		return false
	}
	if flow {
		list := *next
		list.Style = yaml.FlowStyle
		next = &list
	}
	rendered, err := yaml.Marshal(next)
	text := strings.TrimSuffix(string(rendered), "\n")
	if err != nil || strings.Contains(text, "\n") {
		return false
	}
	line := lines[old.Line-1]
	start := old.Column - 1
	if start < 0 || start >= len(line) || strings.IndexFunc(line[:start], func(r rune) bool { return r > 127 }) >= 0 {
		return false
	}
	rest := strings.TrimRight(line[start:], "\r")
	end := len(rest)
	switch {
	case flow:
		end = strings.IndexByte(rest, ']') + 1
		if !strings.HasPrefix(rest, "[") || end == 0 {
			return false
		}
	case old.Style == 0:
		if i := strings.Index(rest, " #"); i >= 0 {
			end = i
		}
		end = len(strings.TrimRight(rest[:end], " \t"))
	case old.Style == yaml.DoubleQuotedStyle || old.Style == yaml.SingleQuotedStyle:
		quote := rest[0]
		end = -1
		for i := 1; i < len(rest) && end < 0; i++ {
			switch {
			case quote == '"' && rest[i] == '\\':
				i++
			case quote == '\'' && rest[i] == '\'' && i+1 < len(rest) && rest[i+1] == '\'':
				i++
			case rest[i] == quote:
				end = i + 1
			}
		}
		if end < 0 {
			return false
		}
	default:
		return false
	}
	lines[old.Line-1] = line[:start] + text + line[start+end:]
	return true
}

func headCommentStandsAlone(data []byte) bool {
	for _, line := range strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		return trimmed == ""
	}
	return false
}

// Stage the directory so the loader can validate it without changing live files.
func validateChainEdit(configFile, directory, filename string, data []byte, password *string) error {
	return validateChainEdits(configFile, directory, map[string][]byte{filename: data}, password)
}

func validateChainEdits(configFile, directory string, replacements map[string][]byte, password *string) error {
	stage, err := os.MkdirTemp("", "tenderduty-chain-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	entries, err := os.ReadDir(directory)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yml") {
			continue
		}
		if _, replaced := replacements[filepath.Join(directory, entry.Name())]; replaced {
			continue
		}
		contents, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(stage, entry.Name()), contents, 0600); err != nil {
			return err
		}
	}
	for filename, data := range replacements {
		if err := os.WriteFile(filepath.Join(stage, filepath.Base(filename)), data, 0600); err != nil {
			return err
		}
	}
	c, err := loadConfigQuiet(configFile, stage, password)
	if err != nil {
		return err
	}
	defer c.cancel()
	defer c.deliveryCancel()
	_, problems := validateConfig(c)
	return addedErrors(liveProblems(configFile, directory, password), problems)
}

func liveProblems(configFile, directory string, password *string) []string {
	c, err := loadConfigQuiet(configFile, directory, password)
	if err != nil {
		return nil
	}
	defer c.cancel()
	defer c.deliveryCancel()
	_, problems := validateConfig(c)
	return problems
}

// addedErrors blocks only errors an edit adds; an error already in the live config must not block the edit that fixes it.
func addedErrors(before, after []string) error {
	known := make(map[string]bool, len(before))
	for _, problem := range before {
		known[problem] = true
	}
	var added []string
	for _, problem := range after {
		if strings.HasPrefix(problem, "error") && !known[problem] {
			added = append(added, problem)
		}
	}
	if len(added) == 0 {
		return nil
	}
	return fmt.Errorf("configuration is invalid: %s", strings.Join(added, "; "))
}
