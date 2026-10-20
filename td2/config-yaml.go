package tenderduty

import (
	"bytes"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

// decodeConfig rejects unknown fields and additional documents in configuration files.
func decodeConfig(data []byte, target interface{}) error {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(target); err != nil {
		if err == io.EOF {
			return nil // empty or comment only file
		}
		return err
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); err != io.EOF {
		if err != nil {
			return err
		}
		return fmt.Errorf("configuration must contain exactly one YAML document")
	}
	return nil
}

// unmarshalLenient decodes like the old yaml library did: a repeated key keeps its last value instead of failing.
func unmarshalLenient(data []byte, out interface{}) error {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return err
	}
	if doc.Kind == 0 {
		return nil
	}
	dropDuplicateKeys(&doc)
	return doc.Decode(out)
}

func dropDuplicateKeys(node *yaml.Node) {
	if node.Kind == yaml.MappingNode {
		last := make(map[string]int)
		for i := 0; i+1 < len(node.Content); i += 2 {
			last[node.Content[i].Value] = i
		}
		content := node.Content[:0]
		for i := 0; i+1 < len(node.Content); i += 2 {
			if last[node.Content[i].Value] == i {
				content = append(content, node.Content[i], node.Content[i+1])
			}
		}
		node.Content = content
	}
	for _, child := range node.Content {
		dropDuplicateKeys(child)
	}
}

func unknownConfigWarnings(data []byte, target interface{}) []string {
	var doc yaml.Node
	if yaml.Unmarshal(data, &doc) != nil {
		return nil
	}
	var warnings []string
	active := make(map[*yaml.Node]bool)
	var walk func(*yaml.Node, reflect.Type, string)
	walk = func(node *yaml.Node, t reflect.Type, prefix string) {
		if active[node] {
			return
		}
		active[node] = true
		defer delete(active, node)
		for t.Kind() == reflect.Ptr {
			t = t.Elem()
		}
		if node.Kind == yaml.AliasNode {
			walk(node.Alias, t, prefix)
			return
		}
		if node.Kind == yaml.DocumentNode {
			for _, child := range node.Content {
				walk(child, t, prefix)
			}
			return
		}
		if node.Kind == yaml.SequenceNode && (t.Kind() == reflect.Slice || t.Kind() == reflect.Array) {
			for _, child := range node.Content {
				walk(child, t.Elem(), prefix)
			}
			return
		}
		if node.Kind != yaml.MappingNode {
			return
		}
		if t.Kind() == reflect.Map {
			for i := 0; i+1 < len(node.Content); i += 2 {
				walk(node.Content[i+1], t.Elem(), prefix+node.Content[i].Value+".")
			}
			return
		}
		if t.Kind() != reflect.Struct {
			return
		}
		fields := make(map[string]reflect.Type)
		for i := 0; i < t.NumField(); i++ {
			field := t.Field(i)
			key := strings.Split(field.Tag.Get("yaml"), ",")[0]
			if field.PkgPath == "" && key != "" && key != "-" {
				fields[key] = field.Type
			}
		}
		names := make([]string, 0, len(fields))
		for key := range fields {
			names = append(names, key)
		}
		sort.Strings(names)
		seen := make(map[string]bool)
		for i := 0; i+1 < len(node.Content); i += 2 {
			key := node.Content[i].Value
			if key == "<<" {
				walk(node.Content[i+1], t, prefix)
				continue
			}
			if kind, ok := fields[key]; ok {
				walk(node.Content[i+1], kind, prefix+key+".")
				continue
			}
			if seen[key] {
				continue
			}
			seen[key] = true
			closest, distance := "", int(^uint(0)>>1)
			for _, name := range names {
				if d := editDistance(key, name); d < distance {
					closest, distance = name, d
				}
			}
			warning := "warning: " + prefix + key + " is not a setting"
			if closest != "" && distance <= len(key)/2+1 {
				warning += "; did you mean " + prefix + closest + "?"
			}
			warnings = append(warnings, warning)
		}
	}
	walk(&doc, reflect.TypeOf(target), "")
	return warnings
}

func configDecodeProblems(err error) []string {
	if typed, ok := err.(*yaml.TypeError); ok {
		var problems []string
		for _, problem := range typed.Errors {
			if strings.Contains(problem, "field ") && strings.Contains(problem, " not found in type ") {
				continue
			}
			problems = append(problems, problem)
		}
		return problems
	}
	return []string{err.Error()}
}
