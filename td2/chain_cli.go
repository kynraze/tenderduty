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

const chainHelpText = `Add, change, remove or list the chain files in chains.d.

Usage:
  tenderduty chain [command]

Available Commands:
  add         Add a chain from your own node
  set         Change a chain's settings
  remove      Remove a chain
  list        List chains

Flags:
  -home, -f, -cc, -state    as for tenderduty

Use "tenderduty chain [command] -h" for more information about a command.
`

var chainSubHelpText = map[string]string{
	"add": `Add a chain from your own node. Checks the node and the validator, then writes chains.d/<Name>.yml.

Usage:
  tenderduty chain add <valoper|valcons> --node <url> [flags]

Examples:
  tenderduty chain add realiovaloper1... --node http://node-b:26657 --to Realio
  tenderduty chain add celestiavaloper1... --node http://localhost:26657
  tenderduty chain add atonevaloper1... --node https://rpc.example.com --set public_fallback=yes --set alerts.fibre_enabled=yes

Flags:
  --node string      your synced RPC node, required
  --chain string     chain registry entry, e.g. osmosis or celestiatestnet3
  --name string      file and chain name (default: registry name or chain id)
  --set key=value    chain setting, repeatable
  --force            replace an existing file
  --to string        add the validator to an existing chain file (multistaking)
  --label string     validator name with --to (default: its multistaking token symbol, e.g. RST, else the moniker)
  -home, -f, -cc, -state    as for tenderduty

Notes:
  The name and mainnet/testnet come from the chain registry (cosmos.directory, testcosmos.directory).
  A chain in neither is named after its chain id and counted as mainnet in reports;
  for a testnet add --set testnet=yes.
`,
	"set": `Change a chain's settings. Edits chains.d/<Name>.yml and keeps its comments and key order.

Usage:
  tenderduty chain set <Name> key=value [key=value ...]

Examples:
  tenderduty chain set Celestia alerts.fibre_enabled=yes
  tenderduty chain set Celestia alerts.telegram.mentions=alice,bob alerts.telegram.min_severity=warning

Flags:
  -home, -f, -cc, -state    as for tenderduty
`,
	"remove": `Remove a chain. Deletes chains.d/<Name>.yml after asking, the last chain can't be removed.

Usage:
  tenderduty chain remove <Name> [--yes]
  tenderduty chain remove <Name> --validator <name> [--yes]

Examples:
  tenderduty chain remove Celestia
  tenderduty chain remove Celestia --yes

Flags:
  --yes              skip the confirmation
  --validator string  remove one validator from a multistaking chain
  -home, -f, -cc, -state    as for tenderduty
`,
	"list": `List chains: name, chain id, validator, node count, public fallback, enabled alerts and source file.

Usage:
  tenderduty chain list

Flags:
  -home, -f, -cc, -state    as for tenderduty
`,
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
	var yes bool
	var validatorLabel string
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
	case "set", "list":
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
	home, err := ResolveHome(options.Home)
	if err != nil {
		return err
	}
	options.ConfigFile, options.Directory, options.StateFile = ResolvePaths(home, options.ConfigFile, options.Directory, options.StateFile)
	if options.Password == nil {
		password := ""
		options.Password = &password
	}
	switch command {
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
		if len(args) < 2 {
			return usageError(argsError("set", "a chain name and at least one key=value", args))
		}
		filename, err := standaloneChain(options, args[0])
		if err != nil {
			return err
		}
		data, err := os.ReadFile(filename)
		if err != nil {
			return err
		}
		candidate, changes, err := applyChainSettingsDiff(data, args[1:])
		if err != nil {
			return err
		}
		changed := false
		for _, change := range changes {
			changed = changed || change.Changed
		}
		printChanges := func() {
			for _, change := range changes {
				if change.Changed {
					fmt.Fprintf(output, "%s: %s -> %s\n", change.Key, change.Old, change.New)
				} else {
					fmt.Fprintf(output, "%s: already %s\n", change.Key, change.New)
				}
			}
		}
		if !changed {
			printChanges()
			return nil
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
		printChanges()
		fmt.Fprintf(output, "Updated %s\n", filename)
	case "remove":
		if len(args) != 1 {
			return usageError(argsError("remove", "one chain name", args))
		}
		filename, err := standaloneChain(options, args[0])
		if err != nil {
			return err
		}
		if validatorLabel != "" {
			changed, err := removeChainValidator(options, args[0], filename, validatorLabel, yes, input, output)
			if err != nil || !changed {
				return err
			}
			reloadAfterEdit(options.StateFile, output, options.reload)
			return nil
		}
		if only, err := onlyChain(options, args[0]); err == nil && only {
			return fmt.Errorf("%s is the only chain configured; tenderduty needs at least one, add another chain first", args[0])
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
		fmt.Fprintf(output, "Removed %s (%s)\n", args[0], filename)
	case "list":
		if len(args) != 0 {
			return usageError(argsError("list", "no arguments", args))
		}
		return listChains(options, output)
	}
	reloadAfterEdit(options.StateFile, output, options.reload)
	return nil
}

func standaloneChain(options ChainCommandOptions, name string) (string, error) {
	if name == "" || strings.ContainsAny(name, ".<>:\"/\\|?*\x00\r\n") {
		return "", fmt.Errorf("chain name must be a filename without periods or path separators")
	}
	filename := filepath.Join(options.Directory, name+".yml")
	if _, err := os.Stat(filename); err == nil {
		return filename, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	c, err := loadConfigQuiet(options.ConfigFile, options.Directory, options.Password)
	if err == nil {
		defer c.cancel()
		defer c.deliveryCancel()
		if c.Chains[name] != nil {
			return "", fmt.Errorf("%s is defined in %s; edit that file directly", name, options.ConfigFile)
		}
	}
	return "", fmt.Errorf("no chain %q in %s%s; see tenderduty chain list", name, options.Directory, chainNames(options.Directory))
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
	fmt.Fprintln(table, "NAME\tCHAIN ID\tVALIDATOR\tNODES\tFALLBACK\tALERTS\tFILE")
	sources := make(map[string]string)
	entries, err := os.ReadDir(options.Directory)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".yml") {
			sources[strings.Split(entry.Name(), ".")[0]] = filepath.Join(options.Directory, entry.Name())
		}
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
			source = options.ConfigFile
		}
		fallback, alertList := "no", strings.Join(groups, ",")
		if chain.PublicFallback {
			fallback = "yes"
		}
		if alertList == "" {
			alertList = "-"
		}
		fmt.Fprintf(table, "%s\t%s\t%s\t%d\t%s\t%s\t%s\n", name, chain.ChainId, address, len(chain.Nodes), fallback, alertList, source)
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
