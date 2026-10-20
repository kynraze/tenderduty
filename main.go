package main

import (
	"context"
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"golang.org/x/term"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	// FROM scratch image has no zoneinfo
	_ "time/tzdata"

	td2 "github.com/kynraze/tenderduty/v2/td2"
)

//go:embed examples/config.yml
var defaultConfig []byte

//go:embed examples/chains.d/Osmosis.yml
var defaultChainConfig []byte

//go:embed examples/chains.d/Celestia.yml
var defaultCelestiaConfig []byte

const topHelpText = `Tenderduty monitors Tendermint validators and alerts when they miss blocks, get jailed or a node goes down.

Usage:
  tenderduty [flags]
  tenderduty [command]

Available Commands:
  start       Run tenderduty (the same as no command)
  chain       Add, change, remove or list chains
  config      Show or change config.yml
  reload      Reload a running tenderduty
  help        Help about any command

Flags:
  -home string               directory for config.yml, chains.d and the state file, or env TENDERDUTY_HOME
                             (default: the current directory if it has config.yml, else /etc/tenderduty
                             with the state in /var/lib/tenderduty)
  -f string                  config file, or env CONFIG (default "config.yml", or <home>/config.yml)
  -cc string                 chain config directory (default "chains.d", or <home>/chains.d)
  -state string              state file (default ".tenderduty-state.json", or <home>/.tenderduty-state.json)
  -example-config            print an example config.yml and exit
  -example-chain             print an example chains.d/<Chain>.yml and exit
  -example-celestia          print an example chains.d/Celestia.yml with Fibre and exit
  -h, -help                  help for tenderduty

Remote config (see docs/remote.md):
  -encrypt                   encrypt -f to -encrypted-config
  -decrypt                   decrypt -encrypted-config to -f
  -encrypted-config string   encrypted config file (default "config.yml.asc")
  -password string           password for -encrypt/-decrypt, or env PASSWORD, prompts if unset

A relative -f, -cc or -state is relative to the current directory, not to -home.
With -home /etc/tenderduty, state defaults to /var/lib/tenderduty/.tenderduty-state.json.

Use "tenderduty [command] -h" for more information about a command.
`

const reloadHelpText = `Reload a running tenderduty: re-reads config.yml and chains.d without a restart.

Usage:
  tenderduty reload [flags]

Flags:
  -h, -help                  help for reload
` + td2.GlobalFlagsHelp

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

type unknownCommandError struct{ name, parent string }

func (e *unknownCommandError) Error() string {
	return fmt.Sprintf("unknown command %q for %q", e.name, e.parent)
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	var home, configFile, chainConfigDirectory, stateFile, encryptedFile, password string
	var dumpConfig, dumpChainConfig, dumpCelestiaConfig, encryptConfig, decryptConfig bool
	var addChain, node, name, registryChain string
	var settings repeatedSettings
	var publicFallback, force bool
	// usage is hand-written; flags only parse
	flags := flag.NewFlagSet("tenderduty", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&home, "home", "", "directory for config.yml, chains.d and state (in /var/lib/tenderduty for /etc/tenderduty), can also be set with the ENV var 'TENDERDUTY_HOME'")
	flags.StringVar(&configFile, "f", "", "configuration file to use, can also be set with the ENV var 'CONFIG'")
	flags.StringVar(&encryptedFile, "encrypted-config", "config.yml.asc", "encrypted config file, only valid with -encrypt or -decrypt flag")
	flags.StringVar(&password, "password", "", "password to use for encrypting/decrypting the config, if unset will prompt, also can use ENV var 'PASSWORD'")
	flags.StringVar(&stateFile, "state", "", "file for storing state between restarts")
	flags.StringVar(&chainConfigDirectory, "cc", "", "directory containing additional chain specific configurations")
	flags.BoolVar(&dumpConfig, "example-config", false, "print an example config.yml with the shared settings and exit")
	flags.BoolVar(&dumpChainConfig, "example-chain", false, "print an example chains.d/<Chain>.yml for one chain and exit")
	flags.BoolVar(&dumpCelestiaConfig, "example-celestia", false, "print an example chains.d/Celestia.yml with Fibre and exit")
	flags.BoolVar(&encryptConfig, "encrypt", false, "encrypt the file specified by -f to -encrypted-config")
	flags.BoolVar(&decryptConfig, "decrypt", false, "decrypt the file specified by -encrypted-config to -f")
	// deprecated, use `chain add`
	flags.StringVar(&addChain, "add-chain", "", "validator address to add from a synced RPC node")
	flags.StringVar(&registryChain, "chain", "", "registry entry with -add-chain")
	flags.Var(&settings, "set", "chain key=value override (repeatable)")
	flags.StringVar(&node, "node", "", "RPC URL to use with -add-chain")
	flags.StringVar(&name, "name", "", "override the chain registry name with -add-chain")
	flags.BoolVar(&publicFallback, "public-fallback", false, "enable registry RPC fallback for the added chain")
	flags.BoolVar(&force, "force", false, "replace an existing chain file with -add-chain")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, _ = io.WriteString(stdout, topHelpText)
			return 0
		}
		fmt.Fprintln(stderr, err)
		fmt.Fprintln(stderr, "Run 'tenderduty -h' for usage.")
		return 2
	}

	if dumpConfig {
		fmt.Fprintln(stdout, string(defaultConfig))
		return 0
	}

	if dumpChainConfig {
		fmt.Fprintln(stdout, string(defaultChainConfig))
		return 0
	}

	if dumpCelestiaConfig {
		fmt.Fprintln(stdout, string(defaultCelestiaConfig))
		return 0
	}

	if configFile == "" {
		configFile = os.Getenv("CONFIG")
	}
	if home == "" {
		home = os.Getenv(td2.HomeEnv)
	}

	if os.Getenv("PASSWORD") != "" {
		password = os.Getenv("PASSWORD")
	}

	if encryptConfig || decryptConfig {
		if password == "" {
			if !term.IsTerminal(int(syscall.Stdin)) {
				log.Println("no terminal to ask for the password; pass -password or env PASSWORD")
				return 1
			}
			fmt.Fprint(stdout, "Please enter the encryption password: ")
			pass, err := term.ReadPassword(int(syscall.Stdin))
			if err != nil {
				log.Println(err)
				return 1
			}
			fmt.Fprintln(stdout, "")
			password = string(pass)
			pass = nil
		}

		homeDir, e := td2.ResolveHome(home)
		if e != nil {
			return finish(e, stderr)
		}
		configFile, _, _ = td2.ResolvePaths(homeDir, configFile, "", "")
		if encryptConfig {
			e = td2.EncryptedConfig(configFile, encryptedFile, password, false)
		} else {
			e = td2.EncryptedConfig(configFile, encryptedFile, password, true)
		}
		if e != nil {
			log.Println(e)
			return 1
		}
		return 0
	}

	commandOptions := td2.ChainCommandOptions{
		Home: home, ConfigFile: configFile, Directory: chainConfigDirectory, StateFile: stateFile, Password: &password,
	}
	rest := flags.Args()
	if len(rest) > 0 && rest[0] == "start" {
		// same as no command; flags may follow it
		if err := flags.Parse(rest[1:]); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				_, _ = io.WriteString(stdout, topHelpText)
				return 0
			}
			return finish(&td2.UsageError{Err: err, Usage: "Run 'tenderduty -h' for usage.\n"}, stderr)
		}
		if flags.NArg() != 0 {
			return finish(&td2.UsageError{Err: td2.TakesError("start", "no arguments", flags.Args()), Usage: "Run 'tenderduty -h' for usage.\n"}, stderr)
		}
		rest = nil
	}
	if len(rest) > 0 {
		var err error
		switch rest[0] {
		case "chain":
			err = td2.ChainCommand(context.Background(), commandOptions, rest[1:], stdin, stdout)
		case "config":
			err = td2.ConfigCommand(context.Background(), commandOptions, rest[1:], stdin, stdout)
		case "reload":
			err = reloadCommand(home, configFile, stateFile, rest[1:], stdout)
		case "help":
			err = helpCommand(rest[1:], stdout)
		default:
			err = &unknownCommandError{name: rest[0], parent: "tenderduty"}
		}
		return finish(err, stderr)
	}
	if addChain != "" {
		fmt.Fprintln(stderr, "-add-chain is deprecated; use tenderduty chain add")
		aliasArgs := deprecatedAddArguments(addChain, node, name, registryChain, settings, publicFallback, force)
		return finish(td2.ChainCommand(context.Background(), commandOptions, aliasArgs, stdin, stdout), stderr)
	}

	homeDir, err := td2.ResolveHome(home)
	if err != nil {
		return finish(err, stderr)
	}
	searched := homeDir == "" && configFile == ""
	configFile, chainConfigDirectory, stateFile = td2.ResolvePaths(homeDir, configFile, chainConfigDirectory, stateFile)
	if err := missingConfig(configFile, searched); err != nil {
		return finish(err, stderr)
	}
	err = td2.Run(configFile, stateFile, chainConfigDirectory, &password)
	if err != nil {
		//#nosec G706 -- local startup error
		log.Println(err.Error())
		return 1
	}
	return 0
}

// missingConfig says where tenderduty looked and how to create a config.yml.
func missingConfig(configFile string, searched bool) error {
	if strings.HasPrefix(configFile, "http://") || strings.HasPrefix(configFile, "https://") {
		return nil
	}
	if _, err := os.Stat(configFile); !os.IsNotExist(err) {
		return nil
	}
	if searched && runtime.GOOS != "windows" {
		dir, _ := os.Getwd()
		return fmt.Errorf("no config.yml in %s or %s\nCreate one: tenderduty -example-config > config.yml (or %s), or pass -home or -f",
			dir, filepath.Dir(td2.SystemConfigFile), td2.SystemConfigFile)
	}
	return fmt.Errorf("%s not found\nCreate one: tenderduty -example-config > %s", configFile, configFile)
}

// finish returns the exit code, printing usage for usage errors.
func finish(err error, stderr io.Writer) int {
	if err == nil {
		return 0
	}
	var usage *td2.UsageError
	var unknown *unknownCommandError
	fmt.Fprintf(stderr, "Error: %s\n", err)
	switch {
	case errors.As(err, &usage):
		fmt.Fprint(stderr, "\n"+usage.Usage)
	case errors.As(err, &unknown):
		fmt.Fprintf(stderr, "Run '%s -h' for usage.\n", unknown.parent)
	}
	return 1
}

func reloadCommand(home, configFile, stateFile string, args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("reload", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&home, "home", home, "home directory")
	flags.StringVar(&stateFile, "state", stateFile, "state file")
	// accepted by every command; reload only signals the running process
	flags.StringVar(&configFile, "f", configFile, "config file")
	flags.String("cc", "", "chain directory")
	usageError := func(err error) error {
		_, usage, _ := strings.Cut(reloadHelpText, "\n\n")
		return &td2.UsageError{Err: err, Usage: usage}
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, err = io.WriteString(stdout, reloadHelpText)
			return err
		}
		return usageError(err)
	}
	if len(flags.Args()) != 0 {
		return usageError(td2.TakesError("reload", "no arguments", flags.Args()))
	}
	homeDir, err := td2.ResolveHome(home)
	if err != nil {
		return err
	}
	_, _, stateFile = td2.ResolvePaths(homeDir, configFile, "", stateFile)
	return td2.Reload(stateFile, stdout)
}

func helpCommand(topic []string, stdout io.Writer) error {
	text, ok := topHelpText, true
	switch {
	case len(topic) == 0 || (topic[0] == "help" || topic[0] == "start") && len(topic) == 1:
	case topic[0] == "chain":
		if text, ok = td2.ChainHelp(topic[1:]); !ok {
			return &unknownCommandError{name: topic[1], parent: "tenderduty chain"}
		}
	case topic[0] == "config":
		if text, ok = td2.ConfigHelp(topic[1:]); !ok {
			return &unknownCommandError{name: topic[1], parent: "tenderduty config"}
		}
	case topic[0] == "reload" && len(topic) == 1:
		text = reloadHelpText
	case topic[0] == "reload":
		return &unknownCommandError{name: topic[1], parent: "tenderduty reload"}
	default:
		return &unknownCommandError{name: topic[0], parent: "tenderduty"}
	}
	_, err := io.WriteString(stdout, text)
	return err
}

type repeatedSettings []string

func (s *repeatedSettings) String() string {
	return fmt.Sprint([]string(*s))
}
func (s *repeatedSettings) Set(value string) error {
	*s = append(*s, value)
	return nil
}

func deprecatedAddArguments(valoper, node, name, registry string, settings []string, publicFallback, force bool) []string {
	args := []string{"add", valoper, "--node", node}
	if name != "" {
		args = append(args, "--name", name)
	}
	if registry != "" {
		args = append(args, "--chain", registry)
	}
	if publicFallback {
		args = append(args, "--public-fallback")
	}
	if force {
		args = append(args, "--force")
	}
	for _, setting := range settings {
		args = append(args, "--set", setting)
	}
	return args
}
