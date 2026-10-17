package tenderduty

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
)

type ChainCommandOptions struct {
	Home, ConfigFile, Directory, StateFile string // "" means not given
	Password                               *string
	reload                                 func(stateFile string) (int, error) // nil: signal the running process
}

// UsageError carries the usage text to print after Err.
type UsageError struct {
	Err   error
	Usage string
}

func (e *UsageError) Error() string { return e.Err.Error() }
func (e *UsageError) Unwrap() error { return e.Err }

// GlobalFlagsHelp lists the flags every command takes.
const GlobalFlagsHelp = `
Global Flags:
  -home string               directory for config.yml, chains.d and the state file, or env TENDERDUTY_HOME
  -f string                  config file, or env CONFIG (default <home>/config.yml)
  -cc string                 chain config directory (default <home>/chains.d)
  -state string              state file (default <home>/.tenderduty-state.json)
`

const chainHelpText = `Add, change, remove or list the chain files in chains.d.

Usage:
  tenderduty chain [command]

Available Commands:
  add         Add a chain from your own node
  set         Change a chain's settings
  bridge      Check and configure a bridge
  fibre       Configure Fibre metrics and alerts
  rename      Rename a chain and its history
  node        Add or remove a node
  remove      Remove a chain
  show        Show a chain and its effective settings
  list        List chains

Flags:
  -h, -help                  help for chain
` + GlobalFlagsHelp + `
Use "tenderduty chain [command] -h" for more information about a command.
`

var chainSubHelpText = map[string]string{
	"bridge": `Check a Celestia bridge node, then save its URL and token file in chains.d/<Name>.yml.
Nothing is saved when the check fails, unless --force.

Usage:
  tenderduty chain bridge <Name> <url> [flags]

Examples:
  tenderduty chain bridge Celestia http://bridge:26658
  tenderduty chain bridge Celestia http://bridge:26658 --token-file /etc/tenderduty/bridge.token --alert yes

Flags:
  --token-file string        bridge auth token file (celestia bridge auth read)
  --alert yes|no             bridge alerts (alerts.bridge_enabled)
  --force                    save even if the check fails
  --endpoint string          tenderduty URL for the printed --metrics.endpoint, e.g. behind NAT (default: from fibre_metrics.listen)
  -h, -help                  help for bridge
` + GlobalFlagsHelp,
	"fibre": `Turn on Fibre alerts for a chain and print the fibre start flags.

Usage:
  tenderduty chain fibre <Name> [flags]

Examples:
  tenderduty chain fibre Celestia
  tenderduty chain fibre Celestia --instance fibre-host
  tenderduty chain fibre Celestia --off

Flags:
  --instance string          Fibre instance name (alerts.fibre_instance)
  --off                      turn Fibre alerts off
  --endpoint string          tenderduty URL for the printed --otel-endpoint, e.g. behind NAT (default: from fibre_metrics.listen)
  -h, -help                  help for fibre
` + GlobalFlagsHelp,
	"show": `Show a chain, its validators, nodes and effective settings with their sources.

Usage:
  tenderduty chain show <Name>

Flags:
  -h, -help                  help for show
` + GlobalFlagsHelp,
	"rename": `Rename a chain file. History follows when tenderduty is running and reloads.

Usage:
  tenderduty chain rename <Old> <New>

Examples:
  tenderduty chain rename akash Akash

Flags:
  -h, -help                  help for rename
` + GlobalFlagsHelp,
	"node": `Add or remove a node in a chain file.

Usage:
  tenderduty chain node add <Name> <url> [--alert yes|no]
  tenderduty chain node remove <Name> <url>

Examples:
  tenderduty chain node add Akash http://backup:26657
  tenderduty chain node add Akash http://sentry:26657 --alert yes
  tenderduty chain node remove Akash http://old-node:26657

Flags:
  --alert yes|no             alert when this node is down (default: no, yes for a chain's first node)
  -h, -help                  help for node
` + GlobalFlagsHelp,
	"add": `Add a chain from your own node. Checks the node and the validator, then writes chains.d/<Name>.yml.

Usage:
  tenderduty chain add <valoper|valcons> --node <url> [flags]

Examples:
  tenderduty chain add realiovaloper1... --node http://node-b:26657 --to Realio
  tenderduty chain add celestiavaloper1... --node http://localhost:26657
  tenderduty chain add atonevaloper1... --node https://rpc.example.com --set public_fallback=yes --set alerts.fibre_enabled=yes

Flags:
  --node string              your synced RPC node, required
  --chain string             chain registry entry, e.g. osmosis or celestiatestnet3
  --name string              file and chain name (default: registry name or chain id)
  --set key=value            chain setting, repeatable
  --force                    replace an existing file
  --to string                add the validator to an existing chain file (multistaking)
  --label string             validator name with --to (default: its multistaking token symbol, e.g. RST, else the moniker)
  -h, -help                  help for add
` + GlobalFlagsHelp + `
Notes:
  The name and mainnet/testnet come from the chain registry (cosmos.directory, testcosmos.directory).
  A chain in neither is named after its chain id and counted as mainnet in reports;
  for a testnet add --set testnet=yes.
`,
	"set": `Change a chain's settings. Edits chains.d/<Name>.yml and keeps its comments and key order.
See every setting and its current value: tenderduty chain show <Name>

Usage:
  tenderduty chain set <Name> [<Name> ...] key=value [key=value ...]
  tenderduty chain set --all key=value [key=value ...]

Examples:
  tenderduty chain set Celestia Akash public_fallback=yes
  tenderduty chain set --all public_fallback=yes
  tenderduty chain set Celestia alerts.fibre_enabled=yes
  tenderduty chain set Celestia alerts.telegram.mentions=alice,bob alerts.telegram.min_severity=warning

Flags:
  --all                      change every chain
  -h, -help                  help for set
` + GlobalFlagsHelp,
	"remove": `Remove a chain. Deletes chains.d/<Name>.yml after asking, the last chain can't be removed.

Usage:
  tenderduty chain remove <Name> [--yes]
  tenderduty chain remove <Name> --validator <name> [--yes]

Examples:
  tenderduty chain remove Celestia
  tenderduty chain remove Celestia --yes

Flags:
  --yes                      skip the confirmation
  --validator string         remove one validator from a multistaking chain
  -h, -help                  help for remove
` + GlobalFlagsHelp,
	"list": `List chains: name, chain id, validator, node count, public fallback, enabled alerts.
Use tenderduty chain show <Name> for settings and source files.

Usage:
  tenderduty chain list

Flags:
  -h, -help                  help for list
` + GlobalFlagsHelp,
}

// ChainHelp returns the help for a chain command, false if the command does not exist.
func ChainHelp(args []string) (string, bool) {
	if len(args) == 0 {
		return chainHelpText, true
	}
	text, ok := chainSubHelpText[args[0]]
	return text, ok && len(args) == 1
}

func usageOf(help string) string {
	_, usage, _ := strings.Cut(help, "\n\n")
	return usage
}

func isHelpFlag(arg string) bool {
	return arg == "-h" || arg == "-help" || arg == "--h" || arg == "--help"
}

type settingFlags []string

func (s *settingFlags) String() string {
	return strings.Join(*s, ", ")
}
func (s *settingFlags) Set(value string) error {
	*s = append(*s, value)
	return nil
}

// Go's flag package stops at the first positional argument; move flags ahead of them.
func parseCommandFlags(flags *flag.FlagSet, args []string) error {
	var options, positional []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			positional = append(positional, arg)
			continue
		}
		key := strings.TrimLeft(strings.SplitN(arg, "=", 2)[0], "-")
		f := flags.Lookup(key)
		if f == nil {
			if isHelpFlag(arg) {
				return flag.ErrHelp
			}
			return fmt.Errorf("unknown flag %s", arg)
		}
		options = append(options, arg)
		boolean, isBool := f.Value.(interface{ IsBoolFlag() bool })
		if !strings.Contains(arg, "=") && !(isBool && boolean.IsBoolFlag()) {
			i++
			if i >= len(args) {
				return fmt.Errorf("%s needs a value", arg)
			}
			options = append(options, args[i])
		}
	}
	return flags.Parse(append(options, positional...))
}

func ChainCommand(ctx context.Context, options ChainCommandOptions, args []string, input io.Reader, output io.Writer) error {
	// Shared flags may precede the command.
	var leading []string
	for len(args) > 0 && strings.HasPrefix(args[0], "-") && !isHelpFlag(args[0]) {
		key := strings.TrimLeft(strings.SplitN(args[0], "=", 2)[0], "-")
		if key != "home" && key != "f" && key != "cc" && key != "state" {
			break
		}
		arg := args[0]
		leading = append(leading, arg)
		args = args[1:]
		if !strings.Contains(arg, "=") {
			if len(args) == 0 {
				return &UsageError{Err: fmt.Errorf("%s needs a value", arg), Usage: usageOf(chainHelpText)}
			}
			leading = append(leading, args[0])
			args = args[1:]
		}
	}
	if len(args) > 0 {
		args = append(append([]string(nil), args...), leading...)
	}
	previous := cliMode
	cliMode = true
	defer func() { cliMode = previous }()
	if len(args) == 0 {
		_, err := io.WriteString(output, chainHelpText)
		return err
	}
	command := args[0]
	if command == "help" || isHelpFlag(command) {
		text, ok := ChainHelp(args[1:])
		if !ok {
			return &UsageError{Err: fmt.Errorf("unknown command %q for \"tenderduty chain\"", args[1]), Usage: usageOf(chainHelpText)}
		}
		_, err := io.WriteString(output, text)
		return err
	}
	usageError := func(err error) error {
		return &UsageError{Err: err, Usage: usageOf(chainSubHelpText[command])}
	}
	flags := flag.NewFlagSet("chain "+command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&options.Home, "home", options.Home, "home directory")
	flags.StringVar(&options.ConfigFile, "f", options.ConfigFile, "shared configuration file")
	flags.StringVar(&options.Directory, "cc", options.Directory, "chain directory")
	flags.StringVar(&options.StateFile, "state", options.StateFile, "state file")
	add := AddChainOptions{}
	var settings settingFlags
	var yes, all bool
	var alert string
	var validatorLabel string
	var tokenFile, instance, endpoint string
	var off, force bool
	switch command {
	case "add":
		flags.StringVar(&add.To, "to", "", "existing chain file")
		flags.StringVar(&add.Label, "label", "", "validator name")
		flags.StringVar(&add.Node, "node", "", "your validator's synced RPC URL (required)")
		flags.StringVar(&add.Name, "name", "", "chain file name")
		flags.StringVar(&add.RegistryChain, "chain", "", "registry entry name")
		flags.Var(&settings, "set", "key=value override (repeatable)")
		flags.BoolVar(&add.PublicFallback, "public-fallback", false, "allow registry RPC fallback")
		flags.BoolVar(&add.Force, "force", false, "replace existing file")
	case "remove":
		flags.BoolVar(&yes, "yes", false, "skip confirmation")
		flags.StringVar(&validatorLabel, "validator", "", "validator name")
	case "set":
		flags.BoolVar(&all, "all", false, "every chain file")
	case "node":
		flags.StringVar(&alert, "alert", "", "alert when the node is down")
	case "bridge":
		flags.StringVar(&tokenFile, "token-file", "", "bridge token file")
		flags.StringVar(&alert, "alert", "", "enable bridge alerts")
		flags.BoolVar(&force, "force", false, "save even if unreachable")
		flags.StringVar(&endpoint, "endpoint", "", "metrics endpoint")
	case "fibre":
		flags.StringVar(&instance, "instance", "", "Fibre instance name")
		flags.BoolVar(&off, "off", false, "disable Fibre alerts")
		flags.StringVar(&endpoint, "endpoint", "", "metrics endpoint")
	case "rename", "list", "show":
	default:
		return &UsageError{Err: fmt.Errorf("unknown chain command %q", command), Usage: usageOf(chainHelpText)}
	}
	if err := parseCommandFlags(flags, args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, err := io.WriteString(output, chainSubHelpText[command])
			return err
		}
		return usageError(err)
	}
	provided := make(map[string]bool)
	flags.Visit(func(f *flag.Flag) { provided[f.Name] = true })
	if command == "add" {
		if provided["to"] {
			for _, key := range []string{"name", "chain", "force", "set"} {
				if provided[key] {
					return usageError(fmt.Errorf("--to cannot be used with --%s", key))
				}
			}
			if add.To == "" {
				return usageError(fmt.Errorf("--to needs a chain name"))
			}
		} else if provided["label"] {
			return usageError(fmt.Errorf("--label requires --to"))
		}
		if provided["label"] {
			if err := validateValidatorLabel(add.Label); err != nil {
				return usageError(err)
			}
		}
	}
	if command == "remove" && provided["validator"] && validatorLabel == "" {
		return usageError(fmt.Errorf("--validator needs a validator name"))
	}
	args = flags.Args()
	// typos first, before asking for a config.yml
	switch {
	case command == "add" && len(args) != 1:
		return usageError(argsError("add", "one validator address", args))
	case command == "add" && add.Node == "":
		return usageError(errNodeRequired)
	}
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
	switch command {
	case "bridge":
		if len(args) != 2 {
			return usageError(argsError("bridge", "a chain name and URL", args))
		}
		if provided["token-file"] && tokenFile == "" {
			return usageError(fmt.Errorf("--token-file needs a path"))
		}
		if provided["alert"] && alert != "yes" && alert != "no" {
			return usageError(fmt.Errorf("--alert is yes or no"))
		}
		return setupBridge(ctx, options, args[0], args[1], tokenFile, alert, endpoint, force, output)
	case "fibre":
		if len(args) != 1 {
			return usageError(argsError("fibre", "one chain name", args))
		}
		return setupFibre(ctx, options, args[0], instance, endpoint, off, provided["instance"], output)
	case "add":
		if len(args) != 1 {
			return usageError(argsError("add", "one validator address", args))
		}
		add.Valoper, add.Directory, add.Settings = args[0], options.Directory, settings
		add.ConfigFile, add.Password = options.ConfigFile, options.Password
		if err := AddChain(ctx, add, output); err != nil {
			if add.Node == "" {
				return usageError(err)
			}
			return err
		}
	case "set":
		changed, err := setChains(options, args, all, output)
		if err != nil {
			var usage *UsageError
			if errors.As(err, &usage) {
				return usageError(usage.Err)
			}
			return err
		}
		if !changed {
			return nil
		}
	case "rename":
		if len(args) != 2 {
			return usageError(argsError("rename", "old and new chain names", args))
		}
		return renameChainFile(options, args[0], args[1], output)
	case "node":
		if len(args) != 3 || (args[0] != "add" && args[0] != "remove") {
			return usageError(argsError("node", "add|remove, a chain name and URL", args))
		}
		if alert != "" && (args[0] != "add" || alert != "yes" && alert != "no") {
			return usageError(fmt.Errorf("--alert is yes or no, with node add"))
		}
		if err := editChainNode(ctx, options, args[0], args[1], args[2], alert, output); err != nil {
			return err
		}
	case "remove":
		if len(args) != 1 {
			return usageError(argsError("remove", "one chain name", args))
		}
		name, filename, err := standaloneChain(options, args[0])
		if err != nil {
			return err
		}
		if validatorLabel != "" {
			changed, err := removeChainValidator(options, name, filename, validatorLabel, yes, input, output)
			if err != nil || !changed {
				return err
			}
			reloadAfterEdit(options.StateFile, output, options.reload)
			return nil
		}
		if only, err := onlyChain(options, name); err == nil && only {
			return fmt.Errorf("%s is the only chain configured; tenderduty needs at least one, add another chain first", name)
		}
		if !yes {
			fmt.Fprintf(output, "Remove %s? [y/N] ", filename)
			answer, err := bufio.NewReader(input).ReadString('\n')
			if err != nil && err != io.EOF {
				return err
			}
			answer = strings.ToLower(strings.TrimSpace(answer))
			if answer != "y" && answer != "yes" {
				fmt.Fprintln(output, "Cancelled")
				return nil
			}
		}
		if err := os.Remove(filename); err != nil {
			return writeError(filename, err)
		}
		fmt.Fprintf(output, "Removed %s (%s)\n", name, filename)
	case "show":
		if len(args) != 1 {
			return usageError(argsError("show", "one chain name", args))
		}
		return showChain(options, args[0], output)
	case "list":
		if len(args) != 0 {
			return usageError(argsError("list", "no arguments", args))
		}
		return listChains(options, output)
	}
	reloadAfterEdit(options.StateFile, output, options.reload)
	return nil
}

func standaloneChain(options ChainCommandOptions, name string) (string, string, error) {
	if badChainFileName(name) {
		return "", "", fmt.Errorf("chain name must be a filename without periods or path separators")
	}
	matches, err := matchingChainNames(options.Directory, name)
	if err != nil {
		return "", "", err
	}
	for _, match := range matches {
		if match == name {
			return match, filepath.Join(options.Directory, match+".yml"), nil
		}
	}
	if len(matches) > 1 {
		return "", "", fmt.Errorf("%q matches %s; use the exact name", name, strings.Join(matches, ", "))
	}
	if len(matches) == 1 {
		return matches[0], filepath.Join(options.Directory, matches[0]+".yml"), nil
	}
	c, err := loadConfigQuiet(options.ConfigFile, options.Directory, options.Password)
	if err == nil {
		defer c.cancel()
		defer c.deliveryCancel()
		for key := range c.Chains {
			if strings.EqualFold(key, name) {
				return "", "", fmt.Errorf("%s is defined in %s; edit that file directly", key, options.ConfigFile)
			}
		}
	}
	return "", "", fmt.Errorf("no chain %q in %s%s; see tenderduty chain list", name, options.Directory, chainNames(options.Directory))
}

func matchingChainNames(directory, name string) ([]string, error) {
	entries, err := os.ReadDir(directory)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	var matches []string
	for _, entry := range entries {
		base, ok := strings.CutSuffix(entry.Name(), ".yml")
		if ok && !entry.IsDir() && strings.EqualFold(base, name) {
			matches = append(matches, base)
		}
	}
	return matches, nil
}

// chainNames lists up to five chain files, empty when none exist.
func chainNames(directory string) string {
	entries, _ := os.ReadDir(directory)
	var names []string
	for _, entry := range entries {
		if base, ok := strings.CutSuffix(entry.Name(), ".yml"); ok && !entry.IsDir() && base != "" && !strings.Contains(base, ".") {
			names = append(names, base)
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return ""
	}
	more := ""
	if len(names) > 5 {
		more = fmt.Sprintf(" and %d more", len(names)-5)
		names = names[:5]
	}
	return " (chains: " + strings.Join(names, ", ") + more + ")"
}

// onlyChain finds the sole chain; the caller handles an unloadable config.
func onlyChain(options ChainCommandOptions, name string) (bool, error) {
	c, err := loadConfigQuiet(options.ConfigFile, options.Directory, options.Password)
	if err != nil {
		return false, err
	}
	defer c.cancel()
	defer c.deliveryCancel()
	for chain := range c.Chains {
		if strings.SplitN(chain, " / ", 2)[0] != name {
			return false, nil
		}
	}
	return true, nil
}

func listChains(options ChainCommandOptions, output io.Writer) error {
	c, err := loadConfigQuiet(options.ConfigFile, options.Directory, options.Password)
	if errors.Is(err, errNoChains) {
		_, err = fmt.Fprintln(output, "No chains yet. Add one: tenderduty chain add <valoper> --node <url>")
		return err
	}
	if err != nil {
		return err
	}
	defer c.cancel()
	defer c.deliveryCancel()
	if fatal, problems := validateConfig(c); fatal {
		return fmt.Errorf("configuration is invalid: %s", strings.Join(problems, "; "))
	}
	table := tabwriter.NewWriter(output, 0, 0, 2, ' ', 0)
	fmt.Fprintln(table, "NAME\tCHAIN ID\tVALIDATOR\tNODES\tFALLBACK\tALERTS")
	sources, err := chainSources(options)
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
		address := chain.ValAddress
		if len(address) > 24 {
			address = address[:16] + "..." + address[len(address)-6:]
		}
		groups := enabledAlertFamilies(chain.Alerts)
		source := sources[strings.SplitN(name, " / ", 2)[0]]
		if source == "" {
			name += " (config.yml)"
		}
		fallback, alertList := "no", compactAlertFamilies(groups)
		if chain.PublicFallback {
			fallback = "yes"
		}
		if alertList == "" {
			alertList = "-"
		}
		fmt.Fprintf(table, "%s\t%s\t%s\t%d\t%s\t%s\n", name, chain.ChainId, address, len(chain.Nodes), fallback, alertList)
	}
	return table.Flush()
}

func writeChainFile(filename string, data []byte, mode os.FileMode, replace bool) error {
	file, err := os.CreateTemp(filepath.Dir(filename), ".tenderduty-chain-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	// keep the owner when the service runs as another user
	mode, uid, gid := chainFileOwner(filename, replace, mode)
	if err := chownTemp(file, uid, gid); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Chmod(mode); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if !replace {
		// A hard link will not overwrite a competing add.
		return os.Link(file.Name(), filename)
	}
	return os.Rename(file.Name(), filename)
}

func argsError(command, want string, args []string) error {
	return TakesError("chain "+command, want, args)
}

// TakesError reports a command given the wrong arguments: `reload takes no arguments, got 1: "extra"`.
func TakesError(command, want string, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("%s takes %s, got none", command, want)
	}
	quoted := make([]string, len(args))
	for i, arg := range args {
		quoted[i] = fmt.Sprintf("%q", arg)
	}
	return fmt.Errorf("%s takes %s, got %d: %s", command, want, len(args), strings.Join(quoted, " "))
}

// writeError names the destination folder and how to fix it.
func writeError(filename string, err error) error {
	if errors.Is(err, os.ErrPermission) {
		return fmt.Errorf("cannot write %s: permission denied; run as the owner of %s or with sudo", filename, filepath.Dir(filename))
	}
	return fmt.Errorf("cannot write %s: %w", filename, err)
}

func enabledAlertFamilies(a AlertConfig) []string {
	var groups []string
	for _, family := range []struct {
		name string
		on   bool
	}{
		{"missed_blocks", a.MissedBlocksAlerts},
		{"stalled", a.StalledAlerts},
		{"no_servers", a.AlertIfNoServers},
		{"gov", a.GovAlerts},
		{"upgrade", a.UpgradeAlerts},
		{"missed_proposal", a.ProposalAlerts},
		{"fibre", a.FibreAlerts},
		{"bridge", a.BridgeAlerts},
	} {
		if family.on {
			groups = append(groups, family.name)
		}
	}
	return groups
}
