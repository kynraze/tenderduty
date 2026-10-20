package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	td2 "github.com/kynraze/tenderduty/v2/td2"
)

func runCLI(args ...string) (code int, stdout, stderr string) {
	var out, errs bytes.Buffer
	code = run(args, strings.NewReader(""), &out, &errs)
	return code, out.String(), errs.String()
}

func TestTopHelp(t *testing.T) {
	for _, args := range [][]string{{"-h"}, {"-help"}, {"--help"}, {"help"}} {
		code, stdout, stderr := runCLI(args...)
		if code != 0 || stderr != "" || stdout != topHelpText {
			t.Fatalf("%v: exit %d, stderr %q, stdout %q", args, code, stderr, stdout)
		}
	}
	for _, want := range []string{"\n  chain ", "\n  reload ", "\n  help ", "\nFlags:\n", "-example-chain", "-encrypted-config string", "\n  -home string ", "TENDERDUTY_HOME", "<home>/config.yml", "<home>/chains.d", "<home>/.tenderduty-state.json"} {
		if !strings.Contains(topHelpText, want) {
			t.Errorf("top help lacks %q", want)
		}
	}
	for _, hidden := range []string{"add-chain", "-chain", "-node", "-name", "-set", "-public-fallback", "-force"} {
		if strings.Contains(topHelpText, "\n  "+hidden+" ") || strings.Contains(topHelpText, "add-chain") {
			t.Errorf("top help shows the deprecated %s", hidden)
		}
	}
}

func TestVersion(t *testing.T) {
	code, stdout, stderr := runCLI("-version")
	if code != 0 || stderr != "" || stdout != "tenderduty "+td2.Version()+"\n" {
		t.Fatalf("exit %d, stderr %q, stdout %q", code, stderr, stdout)
	}
	if !strings.Contains(topHelpText, "-version                   print the version and exit") {
		t.Fatal("top help lacks -version")
	}
}

func TestCommandHelp(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"chain", "-h"}, "Usage:\n  tenderduty chain [command]"},
		{[]string{"help", "chain"}, "Usage:\n  tenderduty chain [command]"},
		{[]string{"chain", "add", "-h"}, "--node string"},
		{[]string{"help", "chain", "add"}, "--node string"},
		{[]string{"-cc", "x", "chain", "set", "--help"}, "tenderduty chain set <Name> [<Name> ...] key=value"},
		{[]string{"chain", "remove", "-h"}, "--yes"},
		{[]string{"chain", "list", "-h"}, "tenderduty chain list"},
		{[]string{"reload", "-h"}, "Global Flags:\n  -home string"},
		{[]string{"help", "reload"}, "Usage:\n  tenderduty reload [flags]"},
	} {
		code, stdout, stderr := runCLI(tc.args...)
		if code != 0 || stderr != "" || !strings.Contains(stdout, tc.want) || strings.Contains(stdout, "public-fallback") {
			t.Errorf("%v: exit %d, stderr %q, stdout %q", tc.args, code, stderr, stdout)
		}
	}
}

func TestUnknownCommand(t *testing.T) {
	code, stdout, stderr := runCLI("x")
	if code != 1 || stdout != "" || stderr != "Error: unknown command \"x\" for \"tenderduty\"\nRun 'tenderduty -h' for usage.\n" {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	code, _, stderr = runCLI("help", "x")
	if code != 1 || !strings.HasPrefix(stderr, "Error: unknown command \"x\" for \"tenderduty\"\n") {
		t.Fatalf("help x: exit %d, stderr %q", code, stderr)
	}
	code, _, stderr = runCLI("-nope")
	if code == 0 || !strings.Contains(stderr, "-nope") || !strings.Contains(stderr, "Run 'tenderduty -h' for usage.") {
		t.Fatalf("unknown flag: exit %d, stderr %q", code, stderr)
	}
}

func TestUsageErrorShowsTheUsage(t *testing.T) {
	dir := t.TempDir()
	code, stdout, stderr := runCLI("-cc", dir, "chain", "add", "celestiavaloper1test")
	if code != 1 || stdout != "" || !strings.HasPrefix(stderr, "Error: --node is required") || !strings.Contains(stderr, "\nUsage:\n  tenderduty chain add <valoper|valcons> --node <url> [flags]\n") {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	code, _, stderr = runCLI("-home", homeFixture(t), "chain", "set", "Celestia")
	if code != 1 || !strings.Contains(stderr, "chain set takes a chain name") || !strings.Contains(stderr, "\nUsage:\n  tenderduty chain set <Name>") {
		t.Fatalf("set: exit %d, stderr %q", code, stderr)
	}
	code, _, stderr = runCLI("chain", "bogus")
	if code != 1 || !strings.Contains(stderr, "unknown chain command \"bogus\"") || !strings.Contains(stderr, "tenderduty chain [command]") {
		t.Fatalf("chain bogus: exit %d, stderr %q", code, stderr)
	}
	code, _, stderr = runCLI("reload", "extra")
	if code != 1 || !strings.HasPrefix(stderr, "Error: reload takes no arguments, got 1: \"extra\"\n") || !strings.Contains(stderr, "\nUsage:\n  tenderduty reload [flags]\n") {
		t.Fatalf("reload extra: exit %d, stderr %q", code, stderr)
	}
}

func TestDeprecatedAddChainFlagsStillParse(t *testing.T) {
	code, _, stderr := runCLI("-cc", t.TempDir(), "-add-chain", "celestiavaloper1test", "-name", "Celestia", "-chain", "celestia", "-set", "a=b", "-public-fallback", "-force")
	if code != 1 || !strings.Contains(stderr, "-add-chain is deprecated; use tenderduty chain add") || !strings.Contains(stderr, "--node is required") || strings.Contains(stderr, "not defined") {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
}

func TestDeprecatedAddChainAliasArguments(t *testing.T) {
	got := deprecatedAddArguments("celestiavaloper1test", "https://rpc.example", "Celestia", "celestia", []string{"public_fallback=yes", "alerts.fibre_enabled=yes"}, true, true)
	want := []string{"add", "celestiavaloper1test", "--node", "https://rpc.example", "--name", "Celestia", "--chain", "celestia", "--public-fallback", "--force", "--set", "public_fallback=yes", "--set", "alerts.fibre_enabled=yes"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("alias lost flags: %v", got)
	}
	args := deprecatedAddArguments("celestiavaloper1test", "", "", "", nil, false, false)
	err := td2.ChainCommand(context.Background(), td2.ChainCommandOptions{Directory: t.TempDir()}, args, strings.NewReader(""), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "--node is required") {
		t.Fatalf("alias node requirement: %v", err)
	}
}

func homeFixture(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "chains.d"), 0700); err != nil {
		t.Fatal(err)
	}
	chain := "chain_id: chain-1\nvaloper_address: celestiavaloper1test\nnodes:\n  - url: https://rpc.example\n    alert_if_down: true\n"
	if err := os.WriteFile(filepath.Join(home, "chains.d", "Homed.yml"), []byte(chain), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.yml"), []byte("alert_defaults:\n  upgrade_enabled: yes\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return home
}

func TestHomeFlagAndEnvFindTheChains(t *testing.T) {
	home := homeFixture(t)
	other := t.TempDir()
	for name, tc := range map[string]struct {
		env  string
		args []string
	}{
		"flag before":     {"", []string{"-home", home, "chain", "list"}},
		"double dash":     {"", []string{"--home", home, "chain", "list"}},
		"flag after":      {"", []string{"chain", "list", "-home", home}},
		"env":             {home, []string{"chain", "list"}},
		"flag beats env":  {other, []string{"-home", home, "chain", "list"}},
		"after beats env": {other, []string{"chain", "list", "--home", home}},
	} {
		t.Setenv("TENDERDUTY_HOME", tc.env)
		code, stdout, stderr := runCLI(tc.args...)
		if code != 0 || !strings.Contains(stdout, "\nHomed ") {
			t.Errorf("%s: exit %d, stdout %q, stderr %q", name, code, stdout, stderr)
		}
	}
	// -cc given explicitly wins over the home, on its own
	t.Setenv("TENDERDUTY_HOME", "")
	code, stdout, _ := runCLI("-home", home, "-cc", filepath.Join(other, "chains.d"), "chain", "list")
	if code != 0 || strings.Contains(stdout, "Homed") {
		t.Errorf("explicit -cc: exit %d, stdout %q", code, stdout)
	}
}

func TestReloadHonoursHome(t *testing.T) {
	home := homeFixture(t)
	t.Setenv("TENDERDUTY_HOME", "")
	want := filepath.Join(home, ".tenderduty-state.json")
	for _, args := range [][]string{{"-home", home, "reload"}, {"reload", "--home", home}, {"reload", "-home", home, "-f", "x.yml", "-cc", "y"}} {
		code, _, stderr := runCLI(args...)
		if code != 1 || !strings.Contains(stderr, "no process for state file "+want) {
			t.Errorf("%v: exit %d, stderr %q", args, code, stderr)
		}
	}
	t.Setenv("TENDERDUTY_HOME", home)
	if _, _, stderr := runCLI("reload"); !strings.Contains(stderr, want) {
		t.Errorf("env: stderr %q", stderr)
	}
	if _, _, stderr := runCLI("-state", "mine.json", "reload"); !strings.Contains(stderr, "no process for state file mine.json") {
		t.Errorf("explicit -state: stderr %q", stderr)
	}
}

func TestMissingHomeIsAnError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "x")
	for _, args := range [][]string{{"-home", missing}, {"-home", missing, "chain", "list"}, {"-home", missing, "reload"}} {
		code, stdout, stderr := runCLI(args...)
		if code != 1 || stdout != "" || stderr != "Error: home directory "+missing+" does not exist\n" {
			t.Errorf("%v: exit %d, stdout %q, stderr %q", args, code, stdout, stderr)
		}
	}
}

func TestHelpGroupsRemoteConfig(t *testing.T) {
	mainFlags, remoteFlags, ok := strings.Cut(topHelpText, "\nRemote config (see docs/remote.md):\n")
	if !ok {
		t.Fatal("missing Remote config block")
	}
	for _, flag := range []string{"-encrypt ", "-decrypt ", "-encrypted-config string", "-password string"} {
		if strings.Contains(mainFlags, flag) || !strings.Contains(remoteFlags, flag) {
			t.Errorf("%s is not grouped under Remote config", flag)
		}
	}
	want := "directory for config.yml, chains.d and the state file, or env TENDERDUTY_HOME (default: the current directory if it has config.yml, else /etc/tenderduty with the state in /var/lib/tenderduty)"
	if !strings.Contains(strings.Join(strings.Fields(mainFlags), " "), want) {
		t.Error("home help lacks the system layout default")
	}
	for _, line := range strings.Split(mainFlags, "\n") {
		if len(line) > 110 {
			t.Errorf("help line exceeds 110 columns: %s", line)
		}
	}
	readme, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ReplaceAll(string(readme), "\r\n", "\n"), "$ tenderduty -h\n"+topHelpText+"```") {
		t.Error("README Options block differs from help")
	}
}

func systemLayoutFixture(t *testing.T) (string, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("system layout is Unix only")
	}
	system := homeFixture(t)
	state := filepath.Join(t.TempDir(), ".tenderduty-state.json")
	oldConfig, oldDirectory, oldState := td2.SystemConfigFile, td2.SystemChainDirectory, td2.SystemStateFile
	t.Cleanup(func() {
		td2.SystemConfigFile, td2.SystemChainDirectory, td2.SystemStateFile = oldConfig, oldDirectory, oldState
	})
	td2.SystemConfigFile = filepath.Join(system, "config.yml")
	td2.SystemChainDirectory = filepath.Join(system, "chains.d")
	td2.SystemStateFile = state
	return system, state
}

func TestChainCLISystemLayout(t *testing.T) {
	system, _ := systemLayoutFixture(t)
	local := homeFixture(t)
	empty := t.TempDir()
	for _, tc := range []struct {
		name, cwd, homeEnv, configEnv string
		args                          []string
		wantFile                      string
	}{
		{name: "system", cwd: empty, args: []string{"chain", "show", "Homed"}, wantFile: filepath.Join(system, "chains.d", "Homed.yml")},
		{name: "cwd wins", cwd: local, args: []string{"chain", "show", "Homed"}, wantFile: filepath.Join("chains.d", "Homed.yml")},
		{name: "cc before", cwd: empty, args: []string{"-cc", filepath.Join(local, "chains.d"), "chain", "show", "Homed"}, wantFile: filepath.Join(local, "chains.d", "Homed.yml")},
		{name: "cc after", cwd: empty, args: []string{"chain", "show", "Homed", "-cc", filepath.Join(local, "chains.d")}, wantFile: filepath.Join(local, "chains.d", "Homed.yml")},
		{name: "f before", cwd: empty, args: []string{"-f", filepath.Join(local, "config.yml"), "chain", "list"}},
		{name: "f after", cwd: empty, args: []string{"chain", "list", "-f", filepath.Join(local, "config.yml")}},
		{name: "CONFIG", cwd: empty, configEnv: filepath.Join(local, "config.yml"), args: []string{"chain", "list"}},
		{name: "home before", cwd: empty, args: []string{"-home", local, "chain", "show", "Homed"}, wantFile: filepath.Join(local, "chains.d", "Homed.yml")},
		{name: "home after", cwd: empty, args: []string{"chain", "show", "Homed", "-home", local}, wantFile: filepath.Join(local, "chains.d", "Homed.yml")},
		{name: "home env", cwd: empty, homeEnv: local, args: []string{"chain", "show", "Homed"}, wantFile: filepath.Join(local, "chains.d", "Homed.yml")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(tc.cwd)
			t.Setenv(td2.HomeEnv, tc.homeEnv)
			t.Setenv("CONFIG", tc.configEnv)
			code, stdout, stderr := runCLI(tc.args...)
			if code != 0 || stderr != "" {
				t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
			}
			if tc.wantFile == "" {
				if !strings.Contains(stdout, "No chains yet.") {
					t.Fatalf("explicit config did not skip fallback: %s", stdout)
				}
			} else if !strings.Contains(stdout, tc.wantFile) {
				t.Fatalf("missing chain file %q: %s", tc.wantFile, stdout)
			}
		})
	}
}

func TestReloadCLISystemLayout(t *testing.T) {
	_, state := systemLayoutFixture(t)
	local := homeFixture(t)
	empty := t.TempDir()
	for _, tc := range []struct {
		name, cwd, homeEnv, configEnv string
		args                          []string
		wantState                     string
	}{
		{name: "system", cwd: empty, args: []string{"reload"}, wantState: state},
		{name: "cwd wins", cwd: local, args: []string{"reload"}, wantState: ".tenderduty-state.json"},
		{name: "cc before", cwd: empty, args: []string{"-cc", "other.d", "reload"}, wantState: state},
		{name: "cc after", cwd: empty, args: []string{"reload", "-cc", "other.d"}, wantState: state},
		{name: "f before", cwd: empty, args: []string{"-f", "other.yml", "reload"}, wantState: ".tenderduty-state.json"},
		{name: "f after", cwd: empty, args: []string{"reload", "-f", "other.yml"}, wantState: ".tenderduty-state.json"},
		{name: "CONFIG", cwd: empty, configEnv: "other.yml", args: []string{"reload"}, wantState: ".tenderduty-state.json"},
		{name: "home before", cwd: empty, args: []string{"-home", local, "reload"}, wantState: filepath.Join(local, ".tenderduty-state.json")},
		{name: "home after", cwd: empty, args: []string{"reload", "-home", local}, wantState: filepath.Join(local, ".tenderduty-state.json")},
		{name: "home env", cwd: empty, homeEnv: local, args: []string{"reload"}, wantState: filepath.Join(local, ".tenderduty-state.json")},
		{name: "state before", cwd: empty, args: []string{"-state", "mine.json", "reload"}, wantState: "mine.json"},
		{name: "state after", cwd: empty, args: []string{"reload", "-state", "mine.json"}, wantState: "mine.json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(tc.cwd)
			t.Setenv(td2.HomeEnv, tc.homeEnv)
			t.Setenv("CONFIG", tc.configEnv)
			code, _, stderr := runCLI(tc.args...)
			if code != 1 || !strings.Contains(stderr, "no process for state file "+tc.wantState+");") {
				t.Fatalf("exit %d, stderr %q, want state %q", code, stderr, tc.wantState)
			}
		})
	}
}

func TestExampleCelestia(t *testing.T) {
	code, stdout, stderr := runCLI("-example-celestia")
	if code != 0 || stderr != "" || stdout != string(defaultCelestiaConfig)+"\n" {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "chain_id: celestia") || !strings.Contains(stdout, "  fibre_enabled: no") || !strings.Contains(stdout, "  token_file:") {
		t.Fatalf("Celestia example is missing its chain or Fibre settings: %s", stdout)
	}
	code, stdout, stderr = runCLI("-example-chain")
	if code != 0 || stderr != "" || stdout != string(defaultChainConfig)+"\n" {
		t.Fatalf("Osmosis example: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
}
