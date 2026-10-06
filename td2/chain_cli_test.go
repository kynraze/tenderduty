package tenderduty

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func chainCommandFixture(t *testing.T) (ChainCommandOptions, string, []byte) {
	t.Helper()
	dir := t.TempDir()
	options := ChainCommandOptions{ConfigFile: filepath.Join(dir, "config.yml"), Directory: filepath.Join(dir, "chains.d"), StateFile: filepath.Join(dir, "state.json")}
	password := ""
	options.Password = &password
	if err := os.Mkdir(options.Directory, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(options.ConfigFile, []byte("alert_defaults:\n  upgrade_enabled: yes\n"), 0600); err != nil {
		t.Fatal(err)
	}
	filename := filepath.Join(options.Directory, "Celestia.yml")
	data := []byte("# Operator settings\nchain_id: chain-1\nvaloper_address: celestiavaloper1test\nnodes:\n  - url: https://rpc.example\n    alert_if_down: true\nalerts:\n  fibre_enabled: false # Keep this comment\n  telegram:\n    min_severity: critical\nextra_info: Operator\n")
	if err := os.WriteFile(filename, data, 0600); err != nil {
		t.Fatal(err)
	}
	return options, filename, data
}

func TestChainSettingsGolden(t *testing.T) {
	input, err := os.ReadFile("testdata/chain_set_input.yml")
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("testdata/chain_set_expected.yml")
	if err != nil {
		t.Fatal(err)
	}
	got, err := applyChainSettings(input, []string{"alerts.fibre_enabled=yes", "alerts.telegram.min_severity=warning", "alerts.consecutive_missed=9"})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("golden mismatch:\n%s\nwant:\n%s", got, want)
	}
}

func TestChainSettingsKeepTheTopComment(t *testing.T) {
	for _, tc := range []struct{ name, input, want string }{
		{"attached", "# top\nchain_id: a\nextra_info: x\n", "# top\nchain_id: a\nextra_info: changed\n"},
		{"apart", "# top\n\nchain_id: a\nextra_info: x\n", "# top\n\nchain_id: a\nextra_info: changed\n"},
		{"crlf", "# top\r\nchain_id: a\r\nextra_info: x\r\n", "# top\r\nchain_id: a\r\nextra_info: changed\r\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := applyChainSettings([]byte(tc.input), []string{"extra_info=changed"})
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestChainSettingsRefusals(t *testing.T) {
	for _, tc := range []struct{ setting, want string }{
		{"alerts.fibre_enabld=yes", "did you mean alerts.fibre_enabled"},
		{"alerts.consecutive_missed=lots", `alerts.consecutive_missed must be a whole number, got "lots"`},
		{"alerts.fibre_enabled=maybe", `alerts.fibre_enabled must be yes or no, got "maybe"`},
		{"alerts.telegram.admins=1,x", `alerts.telegram.admins must be a comma-separated list of whole numbers, got "1,x"`},
		{"nodes=rpc.example", "not settable"},
		{"nodes.url=rpc.example", "not settable"},
		{"validators=x", "not settable"},
		{"extra_info", "expected key=value"},
	} {
		t.Run(tc.setting, func(t *testing.T) {
			_, err := applyChainSettings([]byte("chain_id: test\n"), []string{tc.setting})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %s", err, tc.want)
			}
		})
	}
}

func TestChainSettingsListsAndStrings(t *testing.T) {
	data, err := applyChainSettings([]byte("chain_id: test\n"), []string{"alerts.telegram.admins=12,34", "alerts.telegram.mentions=alice,bob", "extra_info=yes", "alerts.telegram.enabled=no"})
	if err != nil {
		t.Fatal(err)
	}
	var chain ChainConfig
	if err := decodeConfig(data, &chain); err != nil {
		t.Fatal(err)
	}
	if chain.ExtraInfo != "yes" || !reflect.DeepEqual(chain.Alerts.Telegram.Admins, []int64{12, 34}) || !reflect.DeepEqual(chain.Alerts.Telegram.Mentions, []string{"alice", "bob"}) || chain.Alerts.Telegram.Enabled == nil || *chain.Alerts.Telegram.Enabled {
		t.Fatalf("bad typed values: %s", data)
	}
}

func TestChainSetValidationAndComments(t *testing.T) {
	options, filename, original := chainCommandFixture(t)
	var output bytes.Buffer
	err := ChainCommand(context.Background(), options, []string{"set", "Celestia", "chain_id="}, strings.NewReader(""), &output)
	if err == nil || !strings.Contains(err.Error(), "requires chain_id") {
		t.Fatalf("invalid edit accepted: %v", err)
	}
	after, _ := os.ReadFile(filename)
	if !bytes.Equal(after, original) {
		t.Fatal("failed validation changed the file")
	}
	err = ChainCommand(context.Background(), options, []string{"set", "Celestia", "alerts.fibre_enabled=yes", "alerts.telegram.min_severity=warning"}, strings.NewReader(""), &output)
	if err != nil {
		t.Fatal(err)
	}
	after, _ = os.ReadFile(filename)
	if !bytes.Contains(after, []byte("fibre_enabled: true # Keep this comment")) || !bytes.Contains(after, []byte("min_severity: warning")) || !bytes.HasPrefix(after, []byte("# Operator settings")) {
		t.Fatalf("comments or settings missing: %s", after)
	}
	for _, want := range []string{"alerts.fibre_enabled: no -> yes\n", "alerts.telegram.min_severity: critical -> warning\n", "Updated " + filename + "\n", "Not applied yet: tenderduty is not running; it loads the change when it starts\n"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("missing %q: %s", want, output.String())
		}
	}
}

func TestChainRemoveConfirmation(t *testing.T) {
	for _, tc := range []struct {
		answer       string
		yes, removed bool
	}{
		{"n\n", false, false}, {"", false, false}, {"yes\n", false, true}, {"", true, true},
	} {
		options, filename, data := chainCommandFixture(t)
		// another chain, the last one can't be removed
		if err := os.WriteFile(filepath.Join(options.Directory, "Other.yml"), bytes.Replace(data, []byte("chain-1"), []byte("chain-2"), 1), 0600); err != nil {
			t.Fatal(err)
		}
		args := []string{"remove", "Celestia"}
		if tc.yes {
			args = append(args, "--yes")
		}
		var output bytes.Buffer
		if err := ChainCommand(context.Background(), options, args, strings.NewReader(tc.answer), &output); err != nil {
			t.Fatal(err)
		}
		_, err := os.Stat(filename)
		if os.IsNotExist(err) != tc.removed {
			t.Fatalf("removed=%t, stat=%v", tc.removed, err)
		}
		if tc.removed && !strings.Contains(output.String(), "Removed Celestia ("+filename+")\nNot applied yet: tenderduty is not running") {
			t.Fatalf("missing removal and apply lines: %s", output.String())
		}
	}
}

func TestChainRemoveRefusesTheLastChain(t *testing.T) {
	options, filename, data := chainCommandFixture(t)
	var output bytes.Buffer
	err := ChainCommand(context.Background(), options, []string{"remove", "Celestia", "--yes"}, strings.NewReader(""), &output)
	want := "Celestia is the only chain configured; tenderduty needs at least one, add another chain first"
	if err == nil || err.Error() != want {
		t.Fatalf("got %v, want %q", err, want)
	}
	after, readErr := os.ReadFile(filename)
	if readErr != nil || !bytes.Equal(data, after) {
		t.Fatalf("the chain file was touched: %v", readErr)
	}
}

func TestChainRemoveSharedConfigRefused(t *testing.T) {
	options, _, _ := chainCommandFixture(t)
	data := []byte("# Shared chain\nchains:\n  Shared:\n    chain_id: shared-1\n    valoper_address: cosmosvaloper1test\n    public_fallback: true\n")
	if err := os.WriteFile(options.ConfigFile, data, 0600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	err := ChainCommand(context.Background(), options, []string{"remove", "Shared", "--yes"}, strings.NewReader(""), &output)
	if err == nil || !strings.Contains(err.Error(), "is defined in") {
		t.Fatalf("shared chain was not refused: %v", err)
	}
	after, _ := os.ReadFile(options.ConfigFile)
	if !bytes.Equal(data, after) {
		t.Fatal("shared config changed")
	}
}

func TestChainList(t *testing.T) {
	options, filename, _ := chainCommandFixture(t)
	var output bytes.Buffer
	// the loader prints to stdout, which is what the operator sees next to the table
	stdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	err = ChainCommand(context.Background(), options, []string{"list"}, strings.NewReader(""), &output)
	os.Stdout = stdout
	_ = w.Close()
	printed, _ := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if len(printed) != 0 {
		t.Fatalf("the command printed besides its output: %q", printed)
	}
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "NAME ") || !strings.HasPrefix(lines[1], "Celestia ") || strings.Contains(output.String(), "Reading Chain Config File") {
		t.Fatalf("output is not only the table: %q", output.String())
	}
	for _, want := range []string{"chain-1", "celestiavaloper1test", "missed_blocks,stalled,no_servers,gov,upgrade", filename} {
		if !strings.Contains(lines[1], want) {
			t.Fatalf("missing %q: %s", want, output.String())
		}
	}
}

func TestCommandFlagsAfterPositionals(t *testing.T) {
	flags := flag.NewFlagSet("test", flag.ContinueOnError)
	var node, config, state string
	var settings settingFlags
	flags.StringVar(&node, "node", "", "")
	flags.StringVar(&config, "f", "", "")
	flags.StringVar(&state, "state", "", "")
	flags.Var(&settings, "set", "")
	if err := parseCommandFlags(flags, []string{"valoper1test", "--node", "https://rpc.example", "--set", "extra_info=hello world", "-f=config.yml", "-state", "state.json"}); err != nil {
		t.Fatal(err)
	}
	if node != "https://rpc.example" || config != "config.yml" || state != "state.json" || len(settings) != 1 || !reflect.DeepEqual(flags.Args(), []string{"valoper1test"}) {
		t.Fatal("interspersed flags lost")
	}
}

func TestChainAddNodeRequiredAndSettings(t *testing.T) {
	fetch := addChainTestFetchers()
	options, _, _ := chainCommandFixture(t)
	add := AddChainOptions{Valoper: "celestiavaloper1test", Name: "New", Directory: options.Directory, ConfigFile: options.ConfigFile, Password: options.Password}
	err := addChain(context.Background(), add, &bytes.Buffer{}, fetch)
	if err == nil || !strings.Contains(err.Error(), "--node is required") {
		t.Fatalf("missing node accepted: %v", err)
	}
	add.Node = "https://rpc.example"
	add.Settings = []string{"public_fallback=yes", "alerts.fibre_enabled=yes"}
	if err := addChain(context.Background(), add, &bytes.Buffer{}, fetch); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(options.Directory, "New.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var chain ChainConfig
	if err := yaml.Unmarshal(data, &chain); err != nil {
		t.Fatal(err)
	}
	if !chain.PublicFallback || !chain.Alerts.FibreAlerts {
		t.Fatalf("--set not applied: %s", data)
	}
}

func TestChainHelpTexts(t *testing.T) {
	for _, command := range []string{"add", "set", "remove", "list"} {
		text, ok := ChainHelp([]string{command})
		if !ok || !strings.Contains(text, "\nUsage:\n  tenderduty chain "+command) || !strings.Contains(text, "-home, -f, -cc, -state    as for tenderduty") {
			t.Fatalf("help for %s: %q", command, text)
		}
		if strings.Contains(text, "public-fallback") {
			t.Fatalf("help for %s shows a deprecated flag", command)
		}
		var output bytes.Buffer
		options, _, _ := chainCommandFixture(t)
		if err := ChainCommand(context.Background(), options, []string{command, "-h"}, strings.NewReader(""), &output); err != nil || output.String() != text {
			t.Fatalf("%s -h: %v, %q", command, err, output.String())
		}
	}
	if text, ok := ChainHelp([]string{"add"}); !ok || !strings.Contains(text, "--node string") || !strings.Contains(text, "\nNotes:\n") || !strings.Contains(text, "--set testnet=yes") {
		t.Fatalf("chain add help lacks --node: %q", text)
	}
	if text, ok := ChainHelp(nil); !ok || !strings.Contains(text, "  add         Add a chain from your own node\n") {
		t.Fatalf("chain help lacks its commands: %q", text)
	}
	for _, args := range [][]string{{"nope"}, {"add", "nope"}} {
		if _, ok := ChainHelp(args); ok {
			t.Fatalf("help for %v", args)
		}
	}
}

func TestChainUsageErrors(t *testing.T) {
	options, _, _ := chainCommandFixture(t)
	for _, tc := range []struct {
		args       []string
		err, usage string
	}{
		{[]string{"add"}, "chain add takes one validator address, got none", "tenderduty chain add <valoper|valcons> --node <url> [flags]"},
		{[]string{"add", "celestiavaloper1test"}, "--node is required", "tenderduty chain add <valoper|valcons>"},
		{[]string{"add", "chain", "celestiavaloper1test", "--node", "http://localhost:26657"}, "chain add takes one validator address, got 2: \"chain\" \"celestiavaloper1test\"", "tenderduty chain add <valoper|valcons>"},
		{[]string{"add", "celestiavaloper1test", "--bogus"}, "unknown flag --bogus", "tenderduty chain add <valoper|valcons>"},
		{[]string{"set", "Celestia"}, "chain set takes a chain name and at least one key=value, got 1: \"Celestia\"", "tenderduty chain set <Name> key=value"},
		{[]string{"remove"}, "chain remove takes one chain name, got none", "tenderduty chain remove <Name> [--yes]"},
		{[]string{"list", "extra"}, "chain list takes no arguments, got 1: \"extra\"", "tenderduty chain list"},
		{[]string{"remove", "Celestia", "--yes=maybe"}, "invalid boolean value", "tenderduty chain remove"},
		{[]string{"bogus"}, "unknown chain command \"bogus\"", "tenderduty chain [command]"},
	} {
		var output bytes.Buffer
		err := ChainCommand(context.Background(), options, tc.args, strings.NewReader(""), &output)
		var usage *UsageError
		if !errors.As(err, &usage) || !strings.Contains(err.Error(), tc.err) || !strings.Contains(usage.Usage, tc.usage) || strings.Contains(usage.Usage, "Usage of") {
			t.Fatalf("%v: %v", tc.args, err)
		}
		if output.Len() != 0 {
			t.Fatalf("%v printed Go's flag usage: %q", tc.args, output.String())
		}
	}
}

func TestDeprecatedPublicFallbackFlagStillParses(t *testing.T) {
	options, _, _ := chainCommandFixture(t)
	err := ChainCommand(context.Background(), options, []string{"add", "celestiavaloper1test", "--public-fallback"}, strings.NewReader(""), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "--node is required") {
		t.Fatalf("--public-fallback not parsed: %v", err)
	}
}

func TestChainSetDisablesDefaultAlerts(t *testing.T) {
	options, _, _ := chainCommandFixture(t)
	var output bytes.Buffer
	if err := ChainCommand(context.Background(), options, []string{"list"}, strings.NewReader(""), &output); err != nil {
		t.Fatal(err)
	}
	for _, group := range []string{"gov", "upgrade"} {
		if !strings.Contains(output.String(), group) {
			t.Fatalf("default %s missing from list: %s", group, output.String())
		}
	}
	output.Reset()
	if err := ChainCommand(context.Background(), options, []string{"set", "Celestia", "alerts.gov_enabled=no", "alerts.upgrade_enabled=no"}, strings.NewReader(""), &output); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"gov_enabled", "upgrade_enabled"} {
		if !strings.Contains(output.String(), "alerts."+key+": unset -> no\n") {
			t.Fatalf("missing setting change for %s: %s", key, output.String())
		}
	}
	c, err := loadConfigQuiet(options.ConfigFile, options.Directory, options.Password)
	if err != nil {
		t.Fatal(err)
	}
	defer c.cancel()
	defer c.deliveryCancel()
	a := c.Chains["Celestia"].alertSettings()
	if a.GovAlerts || a.UpgradeAlerts {
		t.Fatalf("disabled alerts returned after loading: %+v", a)
	}
	output.Reset()
	if err := ChainCommand(context.Background(), options, []string{"list"}, strings.NewReader(""), &output); err != nil {
		t.Fatal(err)
	}
	for _, group := range []string{"gov", "upgrade"} {
		if strings.Contains(output.String(), group) {
			t.Fatalf("disabled %s still in list: %s", group, output.String())
		}
	}
}
