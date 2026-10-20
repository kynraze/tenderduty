package tenderduty

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"go.yaml.in/yaml/v3"
)

func setChains(options ChainCommandOptions, args []string, all bool, output io.Writer) (bool, error) {
	var names, settings []string
	for i, arg := range args {
		_, known := chainSettingTypes()[strings.ToLower(arg)]
		if arg == "=" || known && i+1 < len(args) && strings.HasPrefix(args[i+1], "=") {
			return false, fmt.Errorf(`write key=value without spaces around "="`)
		}
		if strings.Contains(arg, "=") {
			settings = append(settings, arg)
		} else {
			names = append(names, arg)
		}
	}
	// a setting key without "=" is a typo, not a chain
	for _, arg := range names {
		if _, known := chainSettingTypes()[strings.ToLower(arg)]; known {
			if _, _, err := standaloneChain(options, arg); err != nil {
				return false, fmt.Errorf("expected key=value, got %q", arg)
			}
		}
	}
	if len(settings) == 0 {
		return false, &UsageError{Err: argsError("set", "a chain name and at least one key=value", args)}
	}
	if (all && len(names) > 0) || (!all && len(names) == 0) {
		return false, &UsageError{Err: fmt.Errorf("set needs chain names or --all, and at least one key=value")}
	}
	if all {
		entries, err := os.ReadDir(options.Directory)
		if err != nil {
			return false, err
		}
		for _, entry := range entries {
			if name, ok := strings.CutSuffix(entry.Name(), ".yml"); ok && !entry.IsDir() && !badChainFileName(name) {
				names = append(names, name)
			}
		}
	}
	type edit struct {
		name, filename string
		changes        []settingChange
	}
	var edits []edit
	replacements := make(map[string][]byte)
	seen := make(map[string]bool)
	for _, arg := range names {
		name, filename, err := standaloneChain(options, arg)
		if err != nil {
			return false, err
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		// #nosec G304 G703 -- path from the operator's chains.d
		data, err := os.ReadFile(filename)
		if err != nil {
			return false, err
		}
		candidate, changes, err := applyChainSettingsDiff(data, settings)
		if err != nil {
			if strings.Contains(err.Error(), "unknown setting") && !strings.Contains(err.Error(), "; see ") {
				err = fmt.Errorf("%w Every setting: tenderduty chain show %s", err, name)
			}
			return false, err
		}
		for _, change := range changes {
			if change.Changed {
				replacements[filename] = candidate
				break
			}
		}
		edits = append(edits, edit{name, filename, changes})
	}
	if len(replacements) > 0 {
		if err := validateChainEdits(options.ConfigFile, options.Directory, replacements, options.Password); err != nil {
			return false, err
		}
		for _, edit := range edits {
			data, changed := replacements[edit.filename]
			if !changed {
				continue
			}
			info, err := os.Stat(edit.filename)
			if err != nil {
				return false, err
			}
			if err := writeChainFile(edit.filename, data, info.Mode().Perm(), true); err != nil {
				return false, writeError(edit.filename, err)
			}
		}
	}
	for _, edit := range edits {
		for _, change := range edit.changes {
			prefix := ""
			if len(edits) > 1 {
				prefix = edit.name + ": "
			}
			if change.Changed {
				fmt.Fprintf(output, "%s%s: %s -> %s\n", prefix, change.Key, change.Old, change.New)
			} else {
				fmt.Fprintf(output, "%s%s: already %s\n", prefix, change.Key, change.New)
			}
		}
	}
	if len(replacements) > 0 {
		if len(edits) == 1 {
			fmt.Fprintf(output, "Updated %s\n", edits[0].filename)
		} else {
			fmt.Fprintf(output, "Updated %d chain files\n", len(replacements))
		}
	}
	return len(replacements) > 0, nil
}

func renameChainFile(options ChainCommandOptions, old, next string, output io.Writer) error {
	old, filename, err := standaloneChain(options, old)
	if err != nil {
		return err
	}
	if badChainFileName(strings.TrimSpace(next)) {
		return fmt.Errorf("the new name must be a file name without periods, path separators or reserved punctuation")
	}
	next = chainFileName(next)
	if badChainFileName(next) {
		return fmt.Errorf("the new name must be a file name without periods, path separators or reserved punctuation")
	}
	matches, err := matchingChainNames(options.Directory, next)
	if err != nil {
		return err
	}
	for _, match := range matches {
		if match != old {
			return fmt.Errorf("%s already exists as %s", match, filepath.Join(options.Directory, match+".yml"))
		}
	}
	if old == next {
		fmt.Fprintf(output, "%s is already named %s\n", old, next)
		return nil
	}
	target := filepath.Join(options.Directory, next+".yml")
	// Two steps also handle case-only renames on Windows.
	temp, err := os.CreateTemp(options.Directory, ".tenderduty-rename-*")
	if err != nil {
		return err
	}
	temporary := temp.Name()
	if err := temp.Close(); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	defer func() {
		if temporary != "" {
			_ = os.Remove(temporary)
		}
	}()
	if err := os.Rename(filename, temporary); err != nil {
		return writeError(filename, err)
	}
	if err := os.Rename(temporary, target); err != nil {
		if rollback := os.Rename(temporary, filename); rollback != nil {
			saved := temporary
			temporary = ""
			return fmt.Errorf("rename failed: %v; restore %s to %s: %w", err, saved, filename, rollback)
		}
		return writeError(target, err)
	}
	fmt.Fprintf(output, "Renamed %s to %s (%s)\n", old, next, target)
	reload := options.reload
	if reload == nil {
		reload = reloadProcess
	}
	pid, err := reload(options.StateFile)
	var stopped *notRunningError
	if errors.As(err, &stopped) {
		fmt.Fprintf(output, "Not applied yet: tenderduty is not running; history stays under %s unless you rename it back\n", old)
	} else {
		reloadAfterEdit(options.StateFile, output, func(string) (int, error) { return pid, err })
	}
	return nil
}

func normalChainNode(url string) string {
	return withDefaultPort(strings.TrimRight(url, "/"))
}

func editChainNode(ctx context.Context, options ChainCommandOptions, action, name, url, alert string, output io.Writer) error {
	name, filename, err := standaloneChain(options, name)
	if err != nil {
		return err
	}
	if !validHTTPURL(url) {
		return fmt.Errorf("node must be an http:// or https:// URL, got %q", url)
	}
	url = normalChainNode(url)
	// #nosec G304 G703 -- path from the operator's chains.d
	data, err := os.ReadFile(filename)
	if err != nil {
		return err
	}
	var config ChainConfig
	if err := decodeConfig(data, &config); err != nil {
		return err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return err
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return fmt.Errorf("chain file must be a YAML mapping")
	}
	nodes := mappingValue(doc.Content[0], "nodes")
	if nodes.Kind == yaml.MappingNode && len(nodes.Content) == 0 {
		nodes.Kind, nodes.Tag = yaml.SequenceNode, "!!seq"
	}
	if nodes.Kind != yaml.SequenceNode {
		return fmt.Errorf("nodes must be a list")
	}
	found := -1
	for i, node := range nodes.Content {
		existing := lookupMapping(node, "url")
		if existing != nil && normalChainNode(existing.Value) == url {
			found = i
			break
		}
	}
	if action == "add" {
		if found >= 0 {
			return fmt.Errorf("%s already has node %s", name, url)
		}
		ctx, cancel := context.WithTimeout(ctx, addChainTimeout)
		defer cancel()
		status, err := nodeChainStatus(ctx, url)
		if err != nil {
			return fmt.Errorf("could not reach node %s: %s", url, rootCause(err))
		}
		if status.ChainID != config.ChainId {
			return fmt.Errorf("node reports chain id %s, but %s has %s", stripControl(status.ChainID), name, stripControl(config.ChainId))
		}
		if status.CatchingUp {
			return fmt.Errorf("node is catching up; use a synced node")
		}
		scalar := func(value string) *yaml.Node {
			return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}
		}
		// extra nodes are usually backups; the first one is the validator's own
		if alert == "" {
			alert = "no"
			if len(nodes.Content) == 0 {
				alert = "yes"
			}
		}
		nodes.Content = append(nodes.Content, &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: []*yaml.Node{scalar("url"), scalar(url), scalar("alert_if_down"), scalar(alert)}})
	} else {
		if found < 0 {
			return fmt.Errorf("%s has no node %s", name, url)
		}
		if len(nodes.Content) == 1 {
			return fmt.Errorf("%s has only this node; add another first", name)
		}
		nodes.Content = append(nodes.Content[:found], nodes.Content[found+1:]...)
	}
	if doc.HeadComment != "" && len(doc.Content[0].Content) > 0 && !headCommentStandsAlone(data) {
		first := doc.Content[0].Content[0]
		first.HeadComment = strings.TrimSuffix(doc.HeadComment+"\n"+first.HeadComment, "\n")
		doc.HeadComment = ""
	}
	var buffer bytes.Buffer
	encoder := yaml.NewEncoder(&buffer)
	encoder.SetIndent(2)
	if err := encoder.Encode(&doc); err != nil {
		return err
	}
	if err := encoder.Close(); err != nil {
		return err
	}
	candidate := buffer.Bytes()
	if bytes.Contains(data, []byte("\r\n")) {
		candidate = bytes.ReplaceAll(candidate, []byte("\n"), []byte("\r\n"))
	}
	if err := validateChainEdit(options.ConfigFile, options.Directory, filename, candidate, options.Password); err != nil {
		return err
	}
	info, err := os.Stat(filename)
	if err != nil {
		return err
	}
	if err := writeChainFile(filename, candidate, info.Mode().Perm(), true); err != nil {
		return writeError(filename, err)
	}
	if action == "add" {
		fmt.Fprintf(output, "Added node %s to %s, alert_if_down: %s\n", url, name, alert)
	} else {
		fmt.Fprintf(output, "Removed node %s from %s\n", url, name)
	}
	return nil
}
