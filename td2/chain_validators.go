package tenderduty

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
	rpchttp "github.com/tendermint/tendermint/rpc/client/http"
	"go.yaml.in/yaml/v3"
)

// chainValidatorEdit is the chain file that `chain add --to` extends.
type chainValidatorEdit struct {
	filename string
	mode     os.FileMode
	data     []byte
	config   ChainConfig
}

const labelHint = `validator name must not be empty or contain "/"; set one with --label`

func validateValidatorLabel(label string) error {
	if strings.TrimSpace(label) == "" || strings.Contains(label, "/") || strings.IndexFunc(label, isControl) >= 0 {
		return fmt.Errorf(labelHint)
	}
	return nil
}

func monikerLabel(moniker string) string {
	return strings.Join(strings.Fields(stripControl(moniker)), "-")
}

var (
	stakingDenomPattern = regexp.MustCompile(`^[au][a-z0-9]{2,}$`)
	tokenSymbolPattern  = regexp.MustCompile(`^[A-Za-z0-9]{1,12}$`)
)

// denomLabel turns a plain micro/atto denom into a token label.
func denomLabel(denom string) string {
	if !stakingDenomPattern.MatchString(denom) {
		return ""
	}
	return strings.ToUpper(denom[1:])
}

// tokenLabel prefers the bank symbol over the a/u prefix rule.
func tokenLabel(denom, symbol string) string {
	if tokenSymbolPattern.MatchString(symbol) {
		return strings.ToUpper(symbol)
	}
	return denomLabel(denom)
}

func stakingDenomLabel(ctx context.Context, fetch addChainFetchers, node, valoper string) string {
	if fetch.stakingToken == nil {
		return ""
	}
	label, err := fetch.stakingToken(ctx, node, valoper)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(label)
}

// registryStakingToken asks a multistaking chain (Realio) for the validator's token and returns its label.
func registryStakingToken(ctx context.Context, node, valoper string) (string, error) {
	client, err := rpchttp.New(node, "/websocket")
	if err != nil {
		return "", err
	}
	request := append(binary.AppendUvarint([]byte{0x0a}, uint64(len(valoper))), valoper...)
	resp, err := client.ABCIQuery(ctx, "/multistaking.v1.Query/ValidatorMultiStakingCoin", request)
	if err != nil {
		return "", err
	}
	if resp == nil || resp.Response.Code != 0 {
		return "", fmt.Errorf("multistaking query failed")
	}
	denom, err := firstStringField(resp.Response.Value)
	if err != nil {
		return "", err
	}
	symbol := ""
	if b, err := (&banktypes.QueryDenomMetadataRequest{Denom: denom}).Marshal(); err == nil {
		resp, err := client.ABCIQuery(ctx, "/cosmos.bank.v1beta1.Query/DenomMetadata", b)
		var metadata banktypes.QueryDenomMetadataResponse
		if err == nil && resp != nil && resp.Response.Code == 0 && metadata.Unmarshal(resp.Response.Value) == nil {
			symbol = metadata.Metadata.Symbol
		}
	}
	return tokenLabel(denom, symbol), nil
}

// firstStringField reads field 1 (a string) from a protobuf message.
func firstStringField(data []byte) (string, error) {
	for len(data) > 0 {
		tag, n := binary.Uvarint(data)
		if n <= 0 {
			break
		}
		data = data[n:]
		switch tag & 7 {
		case 0:
			_, n = binary.Uvarint(data)
			if n <= 0 {
				return "", fmt.Errorf("bad protobuf")
			}
			data = data[n:]
		case 2:
			size, n := binary.Uvarint(data)
			if n <= 0 || size > uint64(len(data)-n) {
				return "", fmt.Errorf("bad protobuf")
			}
			value := data[n : n+int(size)]
			data = data[n+int(size):]
			if tag>>3 == 1 {
				return string(value), nil
			}
		default:
			return "", fmt.Errorf("bad protobuf")
		}
	}
	return "", fmt.Errorf("no denom in response")
}

func loadValidatorTarget(options AddChainOptions) (*chainValidatorEdit, error) {
	directory := options.Directory
	if directory == "" {
		directory = "chains.d"
	}
	password := options.Password
	if password == nil {
		empty := ""
		password = &empty
	}
	name, filename, err := standaloneChain(ChainCommandOptions{ConfigFile: options.ConfigFile, Directory: directory, Password: password}, options.To)
	if err != nil {
		path := filepath.Join(directory, options.To+".yml")
		if _, statErr := os.Stat(path); os.IsNotExist(statErr) && !strings.Contains(err.Error(), "is defined in") && !strings.Contains(err.Error(), "matches") && !badChainFileName(options.To) {
			return nil, fmt.Errorf("%s does not exist; add the chain first without --to", path)
		}
		return nil, err
	}
	options.To = name
	info, err := os.Stat(filename)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, err
	}
	target := &chainValidatorEdit{filename: filename, mode: info.Mode().Perm(), data: data}
	if err := decodeConfig(data, &target.config); err != nil {
		return nil, fmt.Errorf("cannot read %s: %v", filename, err)
	}
	if len(target.config.Validators) == 0 && target.config.ValAddress == "" {
		return nil, fmt.Errorf("%s has no validator to add to", options.To)
	}
	for _, existing := range target.config.Validators {
		if existing.ValAddress == options.Valoper {
			return nil, fmt.Errorf("%s is already in %s as %s", options.Valoper, options.To, existing.Name)
		}
	}
	if target.config.ValAddress == options.Valoper {
		return nil, fmt.Errorf("%s is already in %s", options.Valoper, options.To)
	}
	if options.Label != "" {
		if err := target.checkLabel(options.To, options.Label); err != nil {
			return nil, err
		}
	}
	return target, nil
}

func (t *chainValidatorEdit) checkLabel(chain, label string) error {
	for _, existing := range t.config.Validators {
		if existing.Name == label {
			return fmt.Errorf("%s already has a validator named %s; set another with --label", chain, label)
		}
	}
	return nil
}

func scalarNode(value string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}
}

func newValidatorNode(name, valoper, node string) (*yaml.Node, error) {
	type nodeEntry struct {
		URL         string `yaml:"url"`
		AlertIfDown bool   `yaml:"alert_if_down"`
	}
	var entry yaml.Node
	err := entry.Encode(struct {
		Name    string      `yaml:"name"`
		Valoper string      `yaml:"valoper_address"`
		Nodes   []nodeEntry `yaml:"nodes"`
	}{name, valoper, []nodeEntry{{node, true}}})
	return &entry, err
}

// encodeChainDocument keeps the head comment on the first key, like chain set.
func encodeChainDocument(doc *yaml.Node, original []byte) ([]byte, error) {
	if doc.HeadComment != "" && len(doc.Content[0].Content) > 0 && !headCommentStandsAlone(original) {
		first := doc.Content[0].Content[0]
		first.HeadComment = strings.TrimSuffix(doc.HeadComment+"\n"+first.HeadComment, "\n")
		doc.HeadComment = ""
	}
	var output bytes.Buffer
	encoder := yaml.NewEncoder(&output)
	encoder.SetIndent(2)
	if err := encoder.Encode(doc); err != nil {
		return nil, err
	}
	if err := encoder.Close(); err != nil {
		return nil, err
	}
	result := output.Bytes()
	if runtime.GOOS == "windows" {
		result = bytes.ReplaceAll(result, []byte("\n"), []byte("\r\n"))
	}
	return result, nil
}

func parseChainDocument(data []byte) (*yaml.Node, *yaml.Node, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, nil, err
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, nil, fmt.Errorf("chain file must be a YAML mapping")
	}
	return &doc, doc.Content[0], nil
}

func saveChainEdit(options AddChainOptions, filename string, mode os.FileMode, data []byte) error {
	if options.ConfigFile != "" {
		password := options.Password
		if password == nil {
			empty := ""
			password = &empty
		}
		directory := options.Directory
		if directory == "" {
			directory = "chains.d"
		}
		if err := validateChainEdit(options.ConfigFile, directory, filename, data, password); err != nil {
			return err
		}
	}
	if err := writeChainFile(filename, data, mode, true); err != nil {
		return writeError(filename, err)
	}
	return nil
}

// appendChainValidator converts a single-validator file to a validators list.
func appendChainValidator(ctx context.Context, options AddChainOptions, output io.Writer, fetch addChainFetchers, node string, validator *ValInfo, target *chainValidatorEdit) error {
	label := options.Label
	if label == "" {
		label = stakingDenomLabel(ctx, fetch, node, options.Valoper)
	}
	if label == "" {
		label = monikerLabel(validator.Moniker)
	}
	if err := validateValidatorLabel(label); err != nil {
		return err
	}
	if err := target.checkLabel(options.To, label); err != nil {
		return err
	}
	doc, root, err := parseChainDocument(target.data)
	if err != nil {
		return err
	}
	entry, err := newValidatorNode(label, options.Valoper, node)
	if err != nil {
		return err
	}
	converted := ""
	if list := lookupMapping(root, "validators"); list != nil && len(target.config.Validators) > 0 {
		if list.Kind != yaml.SequenceNode {
			return fmt.Errorf("validators in %s must be a list", target.filename)
		}
		list.Content = append(list.Content, entry)
	} else {
		converted = stakingDenomLabel(ctx, fetch, node, target.config.ValAddress)
		if converted == "" {
			if first, err := fetch.validator(ctx, node, target.config.ValAddress); err == nil && first != nil {
				converted = monikerLabel(first.Moniker)
			}
		}
		if validateValidatorLabel(converted) != nil {
			converted = "main"
		}
		if converted == label {
			return fmt.Errorf("%s already has a validator named %s; set another with --label", options.To, label)
		}
		first := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		var moved []*yaml.Node
		position := -1
		kept := make([]*yaml.Node, 0, len(root.Content))
		for i := 0; i+1 < len(root.Content); i += 2 {
			key, value := root.Content[i], root.Content[i+1]
			switch key.Value {
			case "valoper_address", "valcons_override", "nodes":
				if position < 0 {
					position = len(kept)
				}
				moved = append(moved, key, value)
			default:
				kept = append(kept, key, value)
			}
		}
		first.Content = append(first.Content, scalarNode("name"), scalarNode(converted))
		for _, wanted := range []string{"valoper_address", "valcons_override", "nodes"} {
			for i := 0; i+1 < len(moved); i += 2 {
				if moved[i].Value == wanted {
					first.Content = append(first.Content, moved[i], moved[i+1])
				}
			}
		}
		list := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Content: []*yaml.Node{first, entry}}
		if position < 0 {
			position = len(kept)
		}
		root.Content = append(append(append([]*yaml.Node{}, kept[:position]...), scalarNode("validators"), list), kept[position:]...)
	}
	data, err := encodeChainDocument(doc, target.data)
	if err != nil {
		return err
	}
	if err := saveChainEdit(options, target.filename, target.mode, data); err != nil {
		return err
	}
	if converted != "" {
		fmt.Fprintf(output, "%s now lists validators; its alerts are now named \"%s / %s\"\n", options.To, options.To, converted)
	}
	fmt.Fprintf(output, "Added %s to %s: %s\n", label, options.To, target.filename)
	return nil
}

// removeChainValidator drops one entry from validators; false when the operator declined.
func removeChainValidator(options ChainCommandOptions, chain, filename, label string, yes bool, input io.Reader, output io.Writer) (bool, error) {
	info, err := os.Stat(filename)
	if err != nil {
		return false, err
	}
	data, err := os.ReadFile(filename)
	if err != nil {
		return false, err
	}
	doc, root, err := parseChainDocument(data)
	if err != nil {
		return false, err
	}
	list := lookupMapping(root, "validators")
	if list == nil || list.Kind != yaml.SequenceNode || len(list.Content) == 0 {
		return false, fmt.Errorf("%s has a single validator; remove the chain instead", chain)
	}
	names := make([]string, len(list.Content))
	found := -1
	for i, item := range list.Content {
		if name := lookupMapping(item, "name"); name != nil {
			names[i] = name.Value
		}
		if names[i] == label {
			found = i
		}
	}
	if found < 0 {
		return false, fmt.Errorf("%s has no validator named %s (has: %s)", chain, stripControl(label), strings.Join(names, ", "))
	}
	if len(names) == 1 {
		return false, fmt.Errorf("%s is the only validator in %s; remove the chain instead", label, chain)
	}
	if !yes {
		fmt.Fprintf(output, "Remove %s from %s? [y/N] ", label, filename)
		answer, err := bufio.NewReader(input).ReadString('\n')
		if err != nil && err != io.EOF {
			return false, err
		}
		answer = strings.ToLower(strings.TrimSpace(answer))
		if answer != "y" && answer != "yes" {
			fmt.Fprintln(output, "Cancelled")
			return false, nil
		}
	}
	list.Content = append(list.Content[:found], list.Content[found+1:]...)
	result, err := encodeChainDocument(doc, data)
	if err != nil {
		return false, err
	}
	edit := AddChainOptions{ConfigFile: options.ConfigFile, Directory: options.Directory, Password: options.Password}
	if err := saveChainEdit(edit, filename, info.Mode().Perm(), result); err != nil {
		return false, err
	}
	fmt.Fprintf(output, "Removed %s from %s (%s)\n", label, chain, filename)
	return true, nil
}
