package tenderduty

import (
	"bytes"
	"fmt"
	"io"

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
