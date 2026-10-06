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
		case "yes", "true":
			return true, true
		case "no", "false":
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
	t, ok := keys[key]
	if !ok {
		if key == "nodes" || strings.HasPrefix(key, "nodes.") || key == "validators" || strings.HasPrefix(key, "validators.") {
			return parsedSetting{}, fmt.Errorf("%s is not settable; use chain add or edit the file", key)
		}
		names := make([]string, 0, len(keys))
		for name := range keys {
			names = append(names, name)
		}
		sort.Strings(names)
		closest, distance := "", int(^uint(0)>>1)
		for _, name := range names {
			if d := editDistance(key, name); d < distance {
				closest, distance = name, d
			}
		}
		return parsedSetting{}, fmt.Errorf("unknown setting %q; did you mean %s?", key, closest)
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
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, nil, err
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, nil, fmt.Errorf("chain file must be a YAML mapping")
	}
	keys := chainSettingTypes()
	var changes []settingChange
	anyChanged := false
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
			node = mappingValue(node, part)
		}
		if node.Kind != yaml.MappingNode {
			return nil, nil, fmt.Errorf("parent of %s is not a mapping", key)
		}
		var replacement yaml.Node
		if err := replacement.Encode(setting.value); err != nil {
			return nil, nil, err
		}
		change := settingChange{Key: key, Old: settingText(setting.t, lookupMapping(node, parts[len(parts)-1])), New: settingText(setting.t, &replacement)}
		change.Changed = change.Old != change.New
		changes = append(changes, change)
		if !change.Changed {
			continue
		}
		anyChanged = true
		target := mappingValue(node, parts[len(parts)-1])
		replacement.HeadComment, replacement.LineComment, replacement.FootComment = target.HeadComment, target.LineComment, target.FootComment
		*target = replacement
	}
	if !anyChanged {
		return data, changes, nil
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
	if bytes.Contains(data, []byte("\r\n")) {
		result = bytes.ReplaceAll(result, []byte("\n"), []byte("\r\n"))
	}
	return result, changes, nil
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
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yml") || entry.Name() == filepath.Base(filename) {
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
	if err := os.WriteFile(filepath.Join(stage, filepath.Base(filename)), data, 0600); err != nil {
		return err
	}
	c, err := loadConfigQuiet(configFile, stage, password)
	if err != nil {
		return err
	}
	defer c.cancel()
	defer c.deliveryCancel()
	if fatal, problems := validateConfig(c); fatal {
		return fmt.Errorf("configuration is invalid: %s", strings.Join(problems, "; "))
	}
	return nil
}
