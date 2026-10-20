package tenderduty

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/cosmos/cosmos-sdk/types/bech32"
	query "github.com/cosmos/cosmos-sdk/types/query"
	staking "github.com/cosmos/cosmos-sdk/x/staking/types"
	rpchttp "github.com/tendermint/tendermint/rpc/client/http"
	"go.yaml.in/yaml/v3"
)

type AddChainOptions struct {
	Valoper, Node, Name, Directory string
	To, Label                      string
	RegistryChain, ConfigFile      string
	Settings                       []string
	Password                       *string
	PublicFallback, Force          bool
}

type addChainStatus struct {
	ChainID    string
	CatchingUp bool
}

type addChainFetchers struct {
	status         func(context.Context, string) (addChainStatus, error)
	validator      func(context.Context, string, string) (*ValInfo, error)
	registry       func(context.Context, string) (string, error)
	selectRegistry func(context.Context, string, string, string) (string, error)
	// stakingToken is the multistaking token label, empty when absent.
	stakingToken func(ctx context.Context, node, valoper string) (string, error)
	// valoperPrefix is asked after a failed lookup, empty when unknown.
	valoperPrefix func(ctx context.Context, node, chainID string) string
	// fibreProvider returns the Fibre host the validator registered.
	fibreProvider func(ctx context.Context, node, valcons string) (string, bool, error)
}

const addChainTimeout = 45 * time.Second

var errBadChainName = errors.New("--name must be a file name without periods, path separators or reserved punctuation")

func badChainFileName(name string) bool {
	return name == "" || strings.ContainsAny(name, ".<>:\"/\\|?*\x00\r\n")
}

func isControl(r rune) bool { return r < 32 || r == 127 || r >= 0x80 && r <= 0x9f }

// stripControl removes terminal control characters from node or registry text.
func stripControl(text string) string {
	return strings.Map(func(r rune) rune {
		if isControl(r) {
			return -1
		}
		return r
	}, text)
}

// rootCause removes error wrappers.
func rootCause(err error) string {
	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &netErr) && netErr.Timeout() {
		return fmt.Sprintf("timed out after %s", addChainTimeout)
	}
	for {
		var dns *net.DNSError
		if errors.As(err, &dns) {
			return stripControl(fmt.Sprintf("lookup %s: %s", dns.Name, dns.Err))
		}
		next := errors.Unwrap(err)
		if next == nil {
			return stripControl(err.Error())
		}
		err = next
	}
}

// groupDigits writes 1100105 as 1,100,105.
func groupDigits(n int64) string {
	digits := strconv.FormatInt(n, 10)
	sign := ""
	if n < 0 {
		sign, digits = "-", digits[1:]
	}
	for i := len(digits) - 3; i > 0; i -= 3 {
		digits = digits[:i] + "," + digits[i:]
	}
	return sign + digits
}

// prefixHint names the chain's own prefix when the address starts differently.
func prefixHint(address, valoperPrefix string) string {
	cut := strings.LastIndex(address, "1")
	if valoperPrefix == "" || cut < 0 {
		return ""
	}
	want := valoperPrefix
	if strings.HasSuffix(address[:cut], "valcons") {
		want = strings.TrimSuffix(want, "valoper") + "valcons"
	}
	if address[:cut] == want {
		return ""
	}
	return " (this chain's validators start with " + stripControl(want) + ")"
}

func validatorMissing(ctx context.Context, fetch addChainFetchers, node, chainID string, missing *validatorNotFoundError) error {
	if missing.activeOnly {
		return fmt.Errorf("validator %s not found in the active set of %s; use its valoper address for an inactive validator", missing.address, stripControl(chainID))
	}
	hint := ""
	if fetch.valoperPrefix != nil {
		hint = prefixHint(missing.address, fetch.valoperPrefix(ctx, node, chainID))
	}
	return fmt.Errorf("validator %s not found on %s%s", missing.address, stripControl(chainID), hint)
}

// The registry knows the prefix; a chain outside it shows it in its bonded validators' addresses.
func registryValoperPrefix(ctx context.Context, node, chainID string) string {
	if chain, err := fetchRegistryChain(ctx, chainID); err == nil && chain.Bech32Prefix != "" {
		return chain.Bech32Prefix + "valoper"
	}
	client, err := rpchttp.New(node, "/websocket")
	if err != nil {
		return ""
	}
	request := staking.QueryValidatorsRequest{Status: "BOND_STATUS_BONDED", Pagination: &query.PageRequest{Limit: 1}}
	b, err := request.Marshal()
	if err != nil {
		return ""
	}
	resp, err := client.ABCIQuery(ctx, "/cosmos.staking.v1beta1.Query/Validators", b)
	if err != nil || resp == nil || resp.Response.Code != 0 {
		return ""
	}
	validators := &staking.QueryValidatorsResponse{}
	if validators.Unmarshal(resp.Response.Value) != nil || len(validators.Validators) == 0 {
		return ""
	}
	prefix, _, _ := strings.Cut(validators.Validators[0].OperatorAddress, "1")
	return prefix
}

// AddChain writes a standalone chain file.
func AddChain(ctx context.Context, options AddChainOptions, output io.Writer) error {
	return addChain(ctx, options, output, addChainFetchers{
		status:         nodeChainStatus,
		validator:      registryValidator,
		registry:       registryName,
		selectRegistry: selectedRegistryName,
		valoperPrefix:  registryValoperPrefix,
		stakingToken:   registryStakingToken,
		fibreProvider:  registryFibreProvider,
	})
}

func registryFibreProvider(ctx context.Context, node, valcons string) (string, bool, error) {
	client, err := rpchttp.New(node, "/websocket")
	if err != nil {
		return "", false, err
	}
	return getFibreProvider(ctx, client, valcons)
}

func nodeChainStatus(ctx context.Context, node string) (addChainStatus, error) {
	client, err := rpchttp.New(node, "/websocket")
	if err != nil {
		return addChainStatus{}, err
	}
	status, err := client.Status(ctx)
	if err != nil {
		return addChainStatus{}, err
	}
	return addChainStatus{status.NodeInfo.Network, status.SyncInfo.CatchingUp}, nil
}

// getVal already accepts other operator prefixes and valcons.
func registryValidator(ctx context.Context, node, address string) (*ValInfo, error) {
	client, err := rpchttp.New(node, "/websocket")
	if err != nil {
		return nil, err
	}
	pub, _, moniker, jailed, bonded, err := getVal(ctx, client, address)
	if err != nil {
		return nil, err
	}
	if len(pub) != 20 {
		return nil, fmt.Errorf("validator %s has no valid consensus address", address)
	}
	power, err := getVotingPower(ctx, client.Remote(), pub)
	if err != nil {
		return nil, fmt.Errorf("could not read voting power: %w", err)
	}
	if strings.Contains(address, "valcons") && power.rank == 0 {
		return nil, &validatorNotFoundError{address: address, activeOnly: true}
	}
	valcons := address
	if prefix, _, ok := strings.Cut(address, "valoper1"); ok {
		valcons, _ = bech32.ConvertAndEncode(prefix+"valcons", pub[:20])
	}
	return &ValInfo{Moniker: moniker, Jailed: jailed, Bonded: bonded, VotingPower: power.power, Valcons: valcons}, nil
}

func registryName(ctx context.Context, chainID string) (string, error) {
	chain, err := fetchRegistryChain(ctx, chainID)
	if err != nil {
		return "", err
	}
	if chain.PrettyName != "" {
		return chain.PrettyName, nil
	}
	return chain.ChainName, nil
}

var errNodeRequired = errors.New("--node is required: tenderduty watches your own validator node; public RPC is only a fallback")

func addChain(ctx context.Context, options AddChainOptions, output io.Writer, fetch addChainFetchers) error {
	if options.ConfigFile != "" {
		var err error
		options.ConfigFile, err = requireChainConfig(options.ConfigFile, "")
		if err != nil {
			return err
		}
	}
	// local input first, so a typo never waits on the network
	if options.Valoper == "" {
		return fmt.Errorf("chain add needs a validator address")
	}
	if options.Node == "" {
		return errNodeRequired
	}
	if !validHTTPURL(options.Node) {
		return fmt.Errorf("--node must be an http:// or https:// URL, got %q", options.Node)
	}
	if err := validateChainSettings(options.Settings); err != nil {
		return err
	}
	if options.Name != "" {
		if badChainFileName(strings.TrimSpace(options.Name)) {
			return errBadChainName
		}
		options.Name = chainFileName(options.Name)
		if options.Name == "" {
			return errBadChainName
		}
	}
	if options.Name != "" && badChainFileName(options.Name) {
		return errBadChainName
	}
	if options.To == "" && options.Label != "" {
		return fmt.Errorf("--label requires --to")
	}
	var target *chainValidatorEdit
	if options.To != "" {
		for _, conflict := range []struct {
			key string
			set bool
		}{
			{"name", options.Name != ""}, {"chain", options.RegistryChain != ""},
			{"force", options.Force}, {"set", len(options.Settings) != 0},
		} {
			if conflict.set {
				return fmt.Errorf("--to cannot be used with --%s", conflict.key)
			}
		}
		if options.Label != "" {
			if err := validateValidatorLabel(options.Label); err != nil {
				return err
			}
		}
		var err error
		target, err = loadValidatorTarget(options)
		if err != nil {
			return err
		}
	}
	ctx, cancel := context.WithTimeout(ctx, addChainTimeout)
	defer cancel()
	node := withDefaultPort(strings.TrimRight(options.Node, "/"))
	status, err := fetch.status(ctx, node)
	if err != nil {
		return fmt.Errorf("could not reach node %s: %s", strings.TrimRight(options.Node, "/"), rootCause(err))
	}
	if status.CatchingUp {
		return fmt.Errorf("node is catching up; use a synced node")
	}
	if status.ChainID == "" {
		return fmt.Errorf("node returned an empty chain id")
	}
	if target != nil {
		options.To = strings.TrimSuffix(filepath.Base(target.filename), ".yml")
	}
	if target != nil && status.ChainID != target.config.ChainId {
		return fmt.Errorf("node reports chain id %s, but %s has %s", stripControl(status.ChainID), options.To, stripControl(target.config.ChainId))
	}
	if err := duplicateChainValidator(options, status.ChainID, target); err != nil {
		return err
	}
	selectedName := ""
	if options.RegistryChain != "" {
		if fetch.selectRegistry == nil {
			return fmt.Errorf("registry selection is unavailable")
		}
		selectedName, err = fetch.selectRegistry(ctx, options.RegistryChain, status.ChainID, options.Valoper)
		if err != nil {
			return err
		}
	}
	name := options.Name
	notInRegistry := false
	if target == nil {
		if name == "" {
			if selectedName != "" {
				name = selectedName
			} else {
				name, err = fetch.registry(ctx, status.ChainID)
			}
			notInRegistry = errors.Is(err, errNotInRegistry)
			if err != nil && !notInRegistry {
				fmt.Fprintf(output, "Could not read registry name: %v; using chain id\n", stripControl(err.Error()))
			}
			if name == "" {
				name = status.ChainID
			}
		}
		name = chainFileName(name)
		name = strings.TrimSpace(name)
		if badChainFileName(name) {
			return errBadChainName
		}
		options.Name = name
		if err := duplicateChainValidator(options, status.ChainID, nil); err != nil {
			return err
		}
	}
	validator, err := fetch.validator(ctx, node, options.Valoper)
	var missing *validatorNotFoundError
	if err == nil && validator == nil {
		missing = &validatorNotFoundError{address: options.Valoper}
		err = missing
	}
	if errors.As(err, &missing) {
		return validatorMissing(ctx, fetch, node, status.ChainID, missing)
	}
	if err != nil {
		return fmt.Errorf("could not check validator %s on %s: %s", options.Valoper, stripControl(status.ChainID), rootCause(err))
	}
	jail := "not jailed"
	if validator.Jailed {
		jail = "jailed"
	}
	active := "active"
	if !validator.Bonded {
		active = "inactive (not bonded)"
	}
	fmt.Fprintf(output, "%s on %s: %s, %s, voting power %s\n", stripControl(validator.Moniker), stripControl(status.ChainID), active, jail, groupDigits(validator.VotingPower))
	if strings.Contains(options.Valoper, "valcons") {
		fmt.Fprintln(output, "Consensus address verified in the active set; use valoper for its staking moniker and jailed status.")
	}
	if target != nil {
		return appendChainValidator(ctx, options, output, fetch, node, validator, target)
	}
	if options.Directory == "" {
		options.Directory = "chains.d"
	}
	if err := makeChainDirectory(options.Directory); err != nil {
		return err
	}
	matches, err := matchingChainNames(options.Directory, name)
	if err != nil {
		return err
	}
	for _, existing := range matches {
		if existing != name {
			if !options.Force || len(matches) > 1 {
				return fmt.Errorf("%s already exists as %s", existing, filepath.Join(options.Directory, existing+".yml"))
			}
			name = existing
		}
	}
	// Marshal only what this command owns; alert_defaults supplies the rest.
	fileConfig := struct {
		ChainID string `yaml:"chain_id"`
		Valoper string `yaml:"valoper_address"`
		Nodes   []struct {
			URL         string `yaml:"url"`
			AlertIfDown bool   `yaml:"alert_if_down"`
		} `yaml:"nodes"`
		PublicFallback bool `yaml:"public_fallback,omitempty"`
	}{ChainID: status.ChainID, Valoper: options.Valoper, PublicFallback: options.PublicFallback}
	fileConfig.Nodes = append(fileConfig.Nodes, struct {
		URL         string `yaml:"url"`
		AlertIfDown bool   `yaml:"alert_if_down"`
	}{node, true})
	data, err := yaml.Marshal(fileConfig)
	if err != nil {
		return err
	}
	data = append([]byte("# Generated by tenderduty chain add.\n# Add overrides under alerts; shared settings come from alert_defaults.\n"), data...)
	filename := filepath.Join(options.Directory, name+".yml")
	fibreExample := status.ChainID == "celestia" || strings.HasPrefix(status.ChainID, "mocha-") ||
		strings.HasPrefix(status.ChainID, "arabica-") || options.RegistryChain == "celestia" ||
		strings.HasPrefix(options.RegistryChain, "celestiatestnet")
	if fibreExample {
		data = append(data, []byte(`
alerts:
  # Fibre: alert when the validator is not a registered Fibre provider, its Fibre host is unreachable,
  # or the host's certificate is not this validator's. With otel_metrics in config.yml, also alerts on
  # what Fibre reports. Run tenderduty on a different machine from Fibre. See docs/fibre.md.
  fibre_enabled: no
  # Quiet hours hold warnings back, use critical to hear about a dead Fibre at night.
  fibre_severity: warning
  # Hostname of this chain's Fibre server, as Fibre reports it. Only needed when Fibre doesn't send
  # service.namespace=<chain_id>, or one tenderduty watches several validators on this chain.
  fibre_instance: ""
  # Alert when Fibre's storage is at this percent of its budget or more, 1 to 100, 0 is off.
  fibre_storage_percent: 0
  # Alert when Fibre has stored no upload for this many minutes, 0 is off. Only for steady uploads.
  fibre_idle_minutes: 0
  fibre_endorsement_alert: no
  fibre_endorsement_blobs: 50
  fibre_endorsement_minutes: 120

  # Bridge alerts, need bridge.url below. See docs/bridge.md.
  bridge_enabled: no
  bridge_severity: warning
  bridge_lag_blocks: 20
  bridge_lag_minutes: 5

bridge:
  url: ""          # e.g. http://<bridge-ip>:26658, empty is off
  token_file: ""   # file with the read token
`)...)
	}
	data, err = applyChainSettings(data, options.Settings)
	if err != nil {
		return err
	}
	if options.ConfigFile != "" {
		password := options.Password
		if password == nil {
			empty := ""
			password = &empty
		}
		if err := validateChainEdit(options.ConfigFile, options.Directory, filename, data, password); err != nil {
			return err
		}
	}
	if runtime.GOOS == "windows" {
		data = bytes.ReplaceAll(bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n")), []byte("\n"), []byte("\r\n"))
	}
	if err := writeChainFile(filename, data, 0600, options.Force); err != nil {
		if os.IsExist(err) {
			return existingChainError(ctx, options, name, filename, status.ChainID, node, fetch)
		}
		return writeError(filename, err)
	}
	fmt.Fprintf(output, "Added %s: %s\n", name, filename)
	if fibreExample {
		quoted := name
		if strings.ContainsAny(name, " \t") {
			quoted = `"` + name + `"`
		}
		// only a hint: the validator may not run Fibre, or be someone else's
		if host, found, err := fetchFibreProvider(ctx, fetch, node, validator.Valcons); err == nil && found {
			fmt.Fprintf(output, "Registered Fibre provider at %s; for Fibre alerts: tenderduty chain fibre %s\n", stripControl(host), quoted)
		} else {
			fmt.Fprintf(output, "Fibre: once registered, tenderduty chain fibre %s (see docs/fibre.md)\n", quoted)
		}
	}
	if notInRegistry {
		quoted := name
		if strings.ContainsAny(name, " \t") {
			quoted = `"` + name + `"`
		}
		fmt.Fprintf(output, "Not in the chain registry (mainnet or testnet), so the file is named after the chain id.\n  If it is a testnet: tenderduty chain set %s testnet=yes\n  Other name: rename %s, then tenderduty reload\n", quoted, filename)
	}
	return nil
}

func fetchFibreProvider(ctx context.Context, fetch addChainFetchers, node, valcons string) (string, bool, error) {
	if fetch.fibreProvider == nil || valcons == "" {
		return "", false, nil
	}
	return fetch.fibreProvider(ctx, node, valcons)
}

// existingChainError suggests --to for another validator on the same chain.
func existingChainError(ctx context.Context, options AddChainOptions, name, filename, chainID, node string, fetch addChainFetchers) error {
	plain := fmt.Errorf("%s already exists; add --force to replace it", filename)
	// #nosec G304 G703 -- path from the operator's chains.d
	data, err := os.ReadFile(filename)
	if err != nil {
		return plain
	}
	var existing ChainConfig
	if decodeConfig(data, &existing) != nil || existing.ChainId != chainID {
		return plain
	}
	first := existing.ValAddress
	if len(existing.Validators) > 0 {
		first = existing.Validators[0].ValAddress
	}
	if existing.ValAddress == options.Valoper {
		first = options.Valoper
	}
	for _, validator := range existing.Validators {
		if validator.ValAddress == options.Valoper {
			first = options.Valoper
		}
	}
	if first == options.Valoper {
		return fmt.Errorf("%s already watches this validator (%s)", filename, stripControl(options.Valoper))
	}
	who := stripControl(first)
	if first != "" {
		if info, err := fetch.validator(ctx, node, first); err == nil && info != nil && strings.TrimSpace(stripControl(info.Moniker)) != "" {
			who = stripControl(info.Moniker)
		}
	}
	quoted := name
	if strings.ContainsAny(name, " \t") {
		quoted = `"` + name + `"`
	}
	return fmt.Errorf("%s already watches %s (%s); to add this validator to it: tenderduty chain add %s --node %s --to %s, or --force to replace the file", filename, stripControl(chainID), who, options.Valoper, options.Node, quoted)
}

// The node supplies the chain ID; --chain does not select a public node.
func selectedRegistryName(ctx context.Context, name, chainID, address string) (string, error) {
	if name == "" || strings.Trim(name, "abcdefghijklmnopqrstuvwxyz0123456789_-") != "" {
		return "", fmt.Errorf("--chain must be a registry entry name")
	}
	chain, err := fetchRegistryPath(ctx, name)
	var status registryStatusError
	if errors.As(err, &status) && int(status) == http.StatusNotFound {
		return "", fmt.Errorf("chain registry has no entry %q (mainnet or testnet)", name)
	}
	if err != nil {
		return "", fmt.Errorf("could not read the chain registry: %s", rootCause(err))
	}
	if chain.ChainID != chainID {
		return "", fmt.Errorf("registry entry %s has chain id %s, but your node reports %s", name, stripControl(chain.ChainID), stripControl(chainID))
	}
	prefix, _, ok := strings.Cut(address, "1")
	if !ok || (chain.Bech32Prefix != "" && prefix != chain.Bech32Prefix+"valoper" && prefix != chain.Bech32Prefix+"valcons") {
		return "", fmt.Errorf("validator prefix does not match registry entry %s", name)
	}
	if chain.PrettyName != "" {
		return chain.PrettyName, nil
	}
	return chain.ChainName, nil
}
