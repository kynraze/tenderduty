package tenderduty

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// failingFetchers fails the test on any network access.
func failingFetchers(t *testing.T) addChainFetchers {
	t.Helper()
	return addChainFetchers{
		status: func(context.Context, string) (addChainStatus, error) {
			t.Fatal("the node was asked before the local input was checked")
			return addChainStatus{}, nil
		},
		validator: func(context.Context, string, string) (*ValInfo, error) {
			t.Fatal("the validator was asked for before the local input was checked")
			return nil, nil
		},
		registry: func(context.Context, string) (string, error) {
			t.Fatal("the registry was asked before the local input was checked")
			return "", nil
		},
		selectRegistry: func(context.Context, string, string, string) (string, error) {
			t.Fatal("--chain was checked before the local input was checked")
			return "", nil
		},
		valoperPrefix: func(context.Context, string, string) string {
			t.Fatal("the prefix was looked up before the local input was checked")
			return ""
		},
	}
}

func TestAddChainChecksLocalInputBeforeTheNetwork(t *testing.T) {
	const badName = "--name must be a file name without periods, path separators or reserved punctuation"
	for _, tc := range []struct {
		name    string
		options AddChainOptions
		want    string
	}{
		{"address", AddChainOptions{Node: "https://rpc.example"}, "chain add needs a validator address"},
		{"address before node", AddChainOptions{Node: "notaurl"}, "chain add needs a validator address"},
		{"node missing", AddChainOptions{Valoper: "celestiavaloper1test"}, "--node is required: tenderduty watches your own validator node; public RPC is only a fallback"},
		{"node not a url", AddChainOptions{Valoper: "celestiavaloper1test", Node: "notaurl"}, `--node must be an http:// or https:// URL, got "notaurl"`},
		{"node scheme", AddChainOptions{Valoper: "celestiavaloper1test", Node: "ftp://rpc.example"}, `--node must be an http:// or https:// URL, got "ftp://rpc.example"`},
		{"node before set", AddChainOptions{Valoper: "celestiavaloper1test", Node: "notaurl", Settings: []string{"nope=1"}, Name: "a/b"}, `--node must be an http:// or https:// URL, got "notaurl"`},
		{"set unknown key", AddChainOptions{Valoper: "celestiavaloper1test", Node: "https://rpc.example", Settings: []string{"alerts.fibre_enabld=yes"}}, `unknown setting "alerts.fibre_enabld"; did you mean alerts.fibre_enabled?`},
		{"set bad value", AddChainOptions{Valoper: "celestiavaloper1test", Node: "https://rpc.example", Settings: []string{"public_fallback=yes", "alerts.consecutive_missed=lots"}}, `alerts.consecutive_missed must be a whole number, got "lots"`},
		{"set before name", AddChainOptions{Valoper: "celestiavaloper1test", Node: "https://rpc.example", Settings: []string{"public_fallback=maybe"}, Name: "a/b"}, `public_fallback must be yes or no, got "maybe"`},
		{"path traversal", AddChainOptions{Valoper: "valoper", Node: "https://rpc.example", Name: "../outside"}, badName},
		{"name", AddChainOptions{Valoper: "celestiavaloper1test", Node: "https://rpc.example", Name: "a/b"}, badName},
		{"name with period", AddChainOptions{Valoper: "celestiavaloper1test", Node: "https://rpc.example", Name: "My.Chain"}, badName},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.options.Directory = t.TempDir()
			err := addChain(context.Background(), tc.options, &bytes.Buffer{}, failingFetchers(t))
			if err == nil || err.Error() != tc.want {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
			if files, _ := os.ReadDir(tc.options.Directory); len(files) != 0 {
				t.Fatalf("a refused add wrote %v", files)
			}
		})
	}
}

func TestAddChainNodeErrorsKeepTheRootCause(t *testing.T) {
	refused := fmt.Errorf("post failed: %w", &url.Error{Op: "Post", URL: "http://127.0.0.1:1", Err: &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", errors.New("connection refused"))}})
	slow := fmt.Errorf("post failed: %w", &url.Error{Op: "Post", URL: "http://127.0.0.1:1", Err: context.DeadlineExceeded})
	for _, tc := range []struct {
		name string
		node string
		err  error
		want string
	}{
		{"refused", "http://127.0.0.1:1", refused, "could not reach node http://127.0.0.1:1: connection refused"},
		{"timeout", "http://127.0.0.1:1/", slow, "could not reach node http://127.0.0.1:1: timed out after 45s"},
		{"dns", "https://nope.example", &net.DNSError{Name: "nope.example", Err: "no such host"}, "could not reach node https://nope.example: lookup nope.example: no such host"},
		{"plain", "https://rpc.example", errors.New("boom"), "could not reach node https://rpc.example: boom"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fetch := addChainTestFetchers()
			fetch.status = func(context.Context, string) (addChainStatus, error) { return addChainStatus{}, tc.err }
			fetch.selectRegistry = func(context.Context, string, string, string) (string, error) {
				t.Fatal("--chain was checked before the node answered")
				return "", nil
			}
			options := AddChainOptions{Valoper: "celestiavaloper1test", Node: tc.node, RegistryChain: "celestia", Directory: t.TempDir()}
			if err := addChain(context.Background(), options, &bytes.Buffer{}, fetch); err == nil || err.Error() != tc.want {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
		})
	}
}

func TestAddChainValidatorNotFound(t *testing.T) {
	for _, tc := range []struct {
		name, address, prefix string
		err                   error
		want                  string
	}{
		{"other prefix", "cosmosvaloper1abc", "celestiavaloper", &validatorNotFoundError{address: "cosmosvaloper1abc"}, "validator cosmosvaloper1abc not found on mocha-5 (this chain's validators start with celestiavaloper)"},
		{"same prefix", "celestiavaloper1abc", "celestiavaloper", &validatorNotFoundError{address: "celestiavaloper1abc"}, "validator celestiavaloper1abc not found on mocha-5"},
		{"unknown prefix", "cosmosvaloper1abc", "", &validatorNotFoundError{address: "cosmosvaloper1abc"}, "validator cosmosvaloper1abc not found on mocha-5"},
		{"no validator", "cosmosvaloper1abc", "celestiavaloper", nil, "validator cosmosvaloper1abc not found on mocha-5 (this chain's validators start with celestiavaloper)"},
		{"wrapped", "cosmosvaloper1abc", "", fmt.Errorf("lookup: %w", &validatorNotFoundError{address: "cosmosvaloper1abc"}), "validator cosmosvaloper1abc not found on mocha-5"},
		{"consensus prefix", "cosmosvalcons1abc", "celestiavaloper", &validatorNotFoundError{address: "cosmosvalcons1abc"}, "validator cosmosvalcons1abc not found on mocha-5 (this chain's validators start with celestiavalcons)"},
		{"inactive consensus", "celestiavalcons1abc", "celestiavaloper", &validatorNotFoundError{address: "celestiavalcons1abc", activeOnly: true}, "validator celestiavalcons1abc not found in the active set of mocha-5; use its valoper address for an inactive validator"},
		{"other failure", "celestiavaloper1abc", "celestiavaloper", errors.New("boom"), "could not check validator celestiavaloper1abc on mocha-5: boom"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fetch := addChainTestFetchers()
			fetch.status = func(context.Context, string) (addChainStatus, error) { return addChainStatus{ChainID: "mocha-5"}, nil }
			fetch.validator = func(context.Context, string, string) (*ValInfo, error) { return nil, tc.err }
			fetch.valoperPrefix = func(context.Context, string, string) string { return tc.prefix }
			options := AddChainOptions{Valoper: tc.address, Node: "https://rpc.example", Directory: t.TempDir()}
			if err := addChain(context.Background(), options, &bytes.Buffer{}, fetch); err == nil || err.Error() != tc.want {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
		})
	}
}

func TestRegistryValoperPrefix(t *testing.T) {
	useRegistries(t, mainnetRegistry(t), testnetRegistry(t))
	if got := registryValoperPrefix(context.Background(), "http://127.0.0.1:1", "osmosis-1"); got != "osmovaloper" {
		t.Fatalf("prefix = %q", got)
	}
	if got := registryValoperPrefix(context.Background(), "http://127.0.0.1:1", "mocha-4"); got != "celestiavaloper" {
		t.Fatalf("testnet prefix = %q", got)
	}
}

func TestAddChainRegistryEntryIsCheckedBeforeTheValidator(t *testing.T) {
	useRegistries(t, mainnetRegistry(t), testnetRegistry(t))
	fetch := addChainTestnetFetchers("mocha-4")
	fetch.validator = func(context.Context, string, string) (*ValInfo, error) {
		t.Fatal("the validator was asked for although --chain is wrong")
		return nil, nil
	}
	options := AddChainOptions{Valoper: "celestiavaloper1test", Node: "https://rpc.example", RegistryChain: "doesnotexist", Directory: t.TempDir()}
	want := `chain registry has no entry "doesnotexist" (mainnet or testnet)`
	if err := addChain(context.Background(), options, &bytes.Buffer{}, fetch); err == nil || err.Error() != want {
		t.Fatalf("got %v, want %q", err, want)
	}
	options.RegistryChain = "osmosis"
	if err := addChain(context.Background(), options, &bytes.Buffer{}, fetch); err == nil || !strings.Contains(err.Error(), "registry entry osmosis has chain id osmosis-1, but your node reports mocha-4") {
		t.Fatalf("chain id mismatch: %v", err)
	}
	options.RegistryChain = "celestiatestnet3"
	options.Valoper = "osmovaloper1test"
	if err := addChain(context.Background(), options, &bytes.Buffer{}, fetch); err == nil || !strings.Contains(err.Error(), "validator prefix does not match registry entry celestiatestnet3") {
		t.Fatalf("prefix mismatch: %v", err)
	}
	useRegistries(t, unreachableRegistry, unreachableRegistry)
	options.RegistryChain = "osmosis"
	if err := addChain(context.Background(), options, &bytes.Buffer{}, fetch); err == nil || !strings.HasPrefix(err.Error(), "could not read the chain registry: ") || strings.Contains(err.Error(), "Get ") {
		t.Fatalf("unreachable registry: %v", err)
	}
}

func TestAddChainReportsStatusAndPower(t *testing.T) {
	for _, tc := range []struct {
		validator ValInfo
		want      string
	}{
		{ValInfo{Moniker: "LinkPool", Bonded: true, VotingPower: 1100105}, "LinkPool on chain-1: active, not jailed, voting power 1,100,105"},
		{ValInfo{Moniker: "Idle", VotingPower: 999}, "Idle on chain-1: inactive (not bonded), not jailed, voting power 999"},
		{ValInfo{Moniker: "Out", Jailed: true, VotingPower: 0}, "Out on chain-1: inactive (not bonded), jailed, voting power 0"},
		{ValInfo{Moniker: "Stuck", Bonded: true, Jailed: true, VotingPower: 12000}, "Stuck on chain-1: active, jailed, voting power 12,000"},
	} {
		fetch := addChainTestFetchers()
		fetch.validator = func(context.Context, string, string) (*ValInfo, error) {
			validator := tc.validator
			return &validator, nil
		}
		dir := t.TempDir()
		var output bytes.Buffer
		options := AddChainOptions{Valoper: "celestiavaloper1test", Node: "https://rpc.example", Directory: dir}
		if err := addChain(context.Background(), options, &output, fetch); err != nil {
			t.Fatal(err)
		}
		want := tc.want + "\nAdded Registry-Name: " + filepath.Join(dir, "Registry-Name.yml") + "\n"
		if output.String() != want {
			t.Fatalf("got %q, want %q", output.String(), want)
		}
	}
	fetch := addChainTestFetchers()
	var output bytes.Buffer
	options := AddChainOptions{Valoper: "celestiavalcons1test", Node: "https://rpc.example", Directory: t.TempDir()}
	if err := addChain(context.Background(), options, &output, fetch); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[1], "Consensus address verified in the active set;") || !strings.HasPrefix(lines[2], "Added ") {
		t.Fatalf("output = %q", output.String())
	}
}

func TestAddChainStripsControlCharactersFromChainText(t *testing.T) {
	const esc = "\x1b[31m"
	fetch := addChainTestFetchers()
	fetch.status = func(context.Context, string) (addChainStatus, error) {
		return addChainStatus{ChainID: "chain\x1b[2J-1"}, nil
	}
	fetch.validator = func(context.Context, string, string) (*ValInfo, error) {
		return &ValInfo{Moniker: "Evil" + esc + "Pool\r\x07\u009b\x7f", Bonded: true, VotingPower: 5}, nil
	}
	fetch.registry = func(context.Context, string) (string, error) {
		return "", errors.New("bad\x1b]0;title\x07 reply")
	}
	var output bytes.Buffer
	options := AddChainOptions{Valoper: "celestiavaloper1test", Node: "https://rpc.example", Directory: t.TempDir()}
	if err := addChain(context.Background(), options, &output, fetch); err != nil {
		t.Fatal(err)
	}
	for _, r := range output.String() {
		if r != '\n' && isControl(r) {
			t.Fatalf("a control character reached the output: %q", output.String())
		}
	}
	if !strings.Contains(output.String(), "Evil[31mPool on chain[2J-1: active, not jailed, voting power 5\n") || !strings.Contains(output.String(), "Could not read registry name: bad]0;title reply; using chain id\n") {
		t.Fatalf("output = %q", output.String())
	}
	if files, _ := os.ReadDir(options.Directory); len(files) != 1 || strings.ContainsAny(files[0].Name(), "\x1b\x07") {
		t.Fatalf("files = %v", files)
	}
	for name, change := range map[string]func(*addChainFetchers){
		"node": func(f *addChainFetchers) {
			f.status = func(context.Context, string) (addChainStatus, error) {
				return addChainStatus{}, errors.New("bad\x1b[2Jnode")
			}
		},
		"validator": func(f *addChainFetchers) {
			f.validator = func(context.Context, string, string) (*ValInfo, error) { return nil, errors.New("bad\x1b[2Jvalidator") }
		},
		"not found": func(f *addChainFetchers) {
			f.validator = func(context.Context, string, string) (*ValInfo, error) { return nil, nil }
			f.valoperPrefix = func(context.Context, string, string) string { return "x\x1b[2Jvaloper" }
		},
	} {
		fetch := addChainTestFetchers()
		fetch.status = func(context.Context, string) (addChainStatus, error) {
			return addChainStatus{ChainID: "chain\x1b-1"}, nil
		}
		change(&fetch)
		err := addChain(context.Background(), AddChainOptions{Valoper: "cosmosvaloper1test", Node: "https://rpc.example", Directory: t.TempDir()}, &bytes.Buffer{}, fetch)
		if err == nil || strings.ContainsAny(err.Error(), "\x1b") {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if got := stripControl("a\tb\nc\x00d\x7fe\u0080f\u009fg h é"); got != "abcdefg h é" {
		t.Fatalf("stripControl = %q", got)
	}
}

func TestGroupDigits(t *testing.T) {
	for n, want := range map[int64]string{0: "0", 7: "7", 999: "999", 1000: "1,000", 1100105: "1,100,105", -1234567: "-1,234,567", 123456789012: "123,456,789,012"} {
		if got := groupDigits(n); got != want {
			t.Errorf("groupDigits(%d) = %q, want %q", n, got, want)
		}
	}
}

func fakeReloader(pid int, err error, calls *int) func(string) (int, error) {
	return func(string) (int, error) {
		*calls++
		return pid, err
	}
}

func TestChainSetPrintsTheDiffAndSkipsNoOps(t *testing.T) {
	options, filename, _ := chainCommandFixture(t)
	calls := 0
	options.reload = fakeReloader(4242, nil, &calls)
	var output bytes.Buffer
	args := []string{"set", "Celestia", "alerts.consecutive_missed=9", "alerts.fibre_enabled=no", "alerts.telegram.mentions=alice,bob"}
	if err := ChainCommand(context.Background(), options, args, strings.NewReader(""), &output); err != nil {
		t.Fatal(err)
	}
	want := "alerts.consecutive_missed: unset -> 9\nalerts.fibre_enabled: already no\nalerts.telegram.mentions: unset -> alice,bob\nUpdated " + filename + "\nApplied: reloaded tenderduty (pid 4242)\n"
	if output.String() != want || calls != 1 {
		t.Fatalf("output = %q, reloads = %d", output.String(), calls)
	}
	old := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := os.Chtimes(filename, old, old); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filename)
	output.Reset()
	args = []string{"set", "Celestia", "alerts.consecutive_missed=9", "alerts.fibre_enabled=false", "alerts.telegram.mentions=alice,bob"}
	if err := ChainCommand(context.Background(), options, args, strings.NewReader(""), &output); err != nil {
		t.Fatal(err)
	}
	want = "alerts.consecutive_missed: already 9\nalerts.fibre_enabled: already no\nalerts.telegram.mentions: already alice,bob\n"
	after, _ := os.ReadFile(filename)
	info, _ := os.Stat(filename)
	if output.String() != want || calls != 1 || !bytes.Equal(before, after) || !info.ModTime().Equal(old) {
		t.Fatalf("output = %q, reloads = %d, rewritten = %t", output.String(), calls, !info.ModTime().Equal(old))
	}
	output.Reset()
	args = []string{"set", "Celestia", "alerts.consecutive_missed=9", "alerts.consecutive_missed=12", "extra_info=Operator"}
	if err := ChainCommand(context.Background(), options, args, strings.NewReader(""), &output); err != nil {
		t.Fatal(err)
	}
	want = "alerts.consecutive_missed: already 9\nalerts.consecutive_missed: 9 -> 12\nextra_info: already Operator\nUpdated " + filename + "\nApplied: reloaded tenderduty (pid 4242)\n"
	if output.String() != want || calls != 2 {
		t.Fatalf("output = %q, reloads = %d", output.String(), calls)
	}
}

func TestChainSetBadValueNamesKeyAndValue(t *testing.T) {
	options, filename, original := chainCommandFixture(t)
	calls := 0
	options.reload = fakeReloader(1, nil, &calls)
	err := ChainCommand(context.Background(), options, []string{"set", "Celestia", "alerts.fibre_enabled=yes", "alerts.stalled_minutes=abc"}, strings.NewReader(""), &bytes.Buffer{})
	if err == nil || err.Error() != `alerts.stalled_minutes must be a whole number, got "abc"` {
		t.Fatalf("got %v", err)
	}
	after, _ := os.ReadFile(filename)
	if !bytes.Equal(original, after) || calls != 0 {
		t.Fatalf("a refused set changed the file or reloaded (%d)", calls)
	}
}

func TestUnknownChainNamesTheChains(t *testing.T) {
	options, _, data := chainCommandFixture(t)
	for _, command := range [][]string{{"set", "Nope", "extra_info=x"}, {"remove", "Nope", "--yes"}} {
		err := ChainCommand(context.Background(), options, command, strings.NewReader(""), &bytes.Buffer{})
		want := fmt.Sprintf("no chain %q in %s (chains: Celestia); see tenderduty chain list", "Nope", options.Directory)
		if err == nil || err.Error() != want {
			t.Fatalf("%v: got %v, want %q", command, err, want)
		}
	}
	if err := os.WriteFile(filepath.Join(options.Directory, "Osmosis.yml"), bytes.Replace(data, []byte("chain-1"), []byte("chain-2"), 1), 0600); err != nil {
		t.Fatal(err)
	}
	err := ChainCommand(context.Background(), options, []string{"set", "Nope", "extra_info=x"}, strings.NewReader(""), &bytes.Buffer{})
	if want := fmt.Sprintf("no chain %q in %s (chains: Celestia, Osmosis); see tenderduty chain list", "Nope", options.Directory); err == nil || err.Error() != want {
		t.Fatalf("got %v, want %q", err, want)
	}
	for i := 1; i <= 6; i++ {
		if err := os.WriteFile(filepath.Join(options.Directory, fmt.Sprintf("c%d.yml", i)), bytes.Replace(data, []byte("chain-1"), []byte(fmt.Sprintf("chain-c%d", i)), 1), 0600); err != nil {
			t.Fatal(err)
		}
	}
	err = ChainCommand(context.Background(), options, []string{"remove", "Nope", "--yes"}, strings.NewReader(""), &bytes.Buffer{})
	if want := fmt.Sprintf("no chain %q in %s (chains: Celestia, Osmosis, c1, c2, c3 and 3 more); see tenderduty chain list", "Nope", options.Directory); err == nil || err.Error() != want {
		t.Fatalf("got %v, want %q", err, want)
	}
	empty := options
	empty.Directory = t.TempDir()
	err = ChainCommand(context.Background(), empty, []string{"set", "Nope", "extra_info=x"}, strings.NewReader(""), &bytes.Buffer{})
	if want := fmt.Sprintf("no chain %q in %s; see tenderduty chain list", "Nope", empty.Directory); err == nil || err.Error() != want {
		t.Fatalf("got %v, want %q", err, want)
	}
}

func TestChainListIsAnAlignedTable(t *testing.T) {
	options, _, data := chainCommandFixture(t)
	other := bytes.Replace(data, []byte("chain-1"), []byte("chain-two-2"), 1)
	other = bytes.Replace(other, []byte("celestiavaloper1test"), []byte("osmovaloper1abcdefghijklmnopqrstuvwxyz"), 1)
	other = append(other, []byte("public_fallback: true\n")...)
	otherFile := filepath.Join(options.Directory, "Osmosis.yml")
	if err := os.WriteFile(otherFile, other, 0600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := ChainCommand(context.Background(), options, []string{"list"}, strings.NewReader(""), &output); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(output.String(), "\n"), "\n")
	if len(lines) != 3 || strings.Contains(output.String(), "\t") || strings.Contains(output.String(), "=") {
		t.Fatalf("output is not a plain table: %q", output.String())
	}
	if !regexp.MustCompile(`^NAME +CHAIN ID +VALIDATOR +NODES +FALLBACK +ALERTS$`).MatchString(lines[0]) {
		t.Fatalf("header = %q", lines[0])
	}
	column := strings.Index(lines[0], "CHAIN ID")
	for i, want := range [][]string{{"Celestia", "chain-1", "celestiavaloper1test", "1", "no", "all except missed_proposal,fibre,bridge"}, {"Osmosis", "chain-two-2", "osmovaloper1abcd...uvwxyz", "1", "yes", "all except missed_proposal,fibre,bridge"}} {
		line := lines[i+1]
		if got := strings.Fields(line); strings.Join(got, " ") != strings.Join(want, " ") {
			t.Fatalf("row %d = %v, want %v", i, got, want)
		}
		if !strings.HasPrefix(line[column:], want[1]) || line[column-1] != ' ' {
			t.Fatalf("row %d is not aligned with the header: %q", i, line)
		}
	}
}

func TestChainListWithoutChains(t *testing.T) {
	options, filename, _ := chainCommandFixture(t)
	if err := os.Remove(filename); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := ChainCommand(context.Background(), options, []string{"list"}, strings.NewReader(""), &output); err != nil {
		t.Fatal(err)
	}
	if want := "No chains yet. Add one: tenderduty chain add <valoper> --node <url>\n"; output.String() != want {
		t.Fatalf("got %q, want %q", output.String(), want)
	}
}

func TestBareChainCommandPrintsHelp(t *testing.T) {
	options, _, _ := chainCommandFixture(t)
	var output bytes.Buffer
	if err := ChainCommand(context.Background(), options, nil, strings.NewReader(""), &output); err != nil || output.String() != chainHelpText {
		t.Fatalf("bare chain: %v, %q", err, output.String())
	}
	if err := ChainCommand(context.Background(), options, []string{"bogus"}, strings.NewReader(""), &output); err == nil {
		t.Fatal("an unknown subcommand was accepted")
	}
}

func TestReloadAfterEditSaysWhatHappened(t *testing.T) {
	for _, tc := range []struct {
		name string
		pid  int
		err  error
		want string
	}{
		{"running", 1234, nil, "Applied: reloaded tenderduty (pid 1234)\n"},
		{"not running", 0, &notRunningError{"state.json"}, "Not applied yet: tenderduty is not running; it loads the change when it starts\n"},
		{"not running, wrapped", 0, fmt.Errorf("reload: %w", &notRunningError{"state.json"}), "Not applied yet: tenderduty is not running; it loads the change when it starts\n"},
		{"unsupported", 0, errReloadUnsupported, "Not applied yet: reload needs Linux/macOS/Docker; restart tenderduty to load the change\n"},
		{"failed", 0, errors.New("could not reload pid 7: operation not permitted"), "Saved, but reload failed: could not reload pid 7: operation not permitted; run tenderduty reload\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			var output bytes.Buffer
			reloadAfterEdit("state.json", &output, fakeReloader(tc.pid, tc.err, &calls))
			if output.String() != tc.want || calls != 1 {
				t.Fatalf("got %q after %d reloads, want %q", output.String(), calls, tc.want)
			}
		})
	}
}

func TestChainRemoveReloadFailureKeepsTheRemoval(t *testing.T) {
	options, filename, data := chainCommandFixture(t)
	if err := os.WriteFile(filepath.Join(options.Directory, "Other.yml"), bytes.Replace(data, []byte("chain-1"), []byte("chain-2"), 1), 0600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	options.reload = fakeReloader(0, errors.New("could not reload pid 7: operation not permitted"), &calls)
	var output bytes.Buffer
	if err := ChainCommand(context.Background(), options, []string{"remove", "Celestia", "--yes"}, strings.NewReader(""), &output); err != nil {
		t.Fatal(err)
	}
	want := "Removed Celestia (" + filename + ")\nSaved, but reload failed: could not reload pid 7: operation not permitted; run tenderduty reload\n"
	if output.String() != want {
		t.Fatalf("got %q, want %q", output.String(), want)
	}
}

func TestNotRunningMessageNamesTheStateFile(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state.json")
	_, err := readPID(state)
	if want := "tenderduty is not running (no process for state file " + state + "); start it, or pass the -state file it uses"; err == nil || err.Error() != want {
		t.Fatalf("got %v, want %q", err, want)
	}
}

func TestTakesError(t *testing.T) {
	if err := TakesError("reload", "no arguments", []string{"extra"}); err.Error() != `reload takes no arguments, got 1: "extra"` {
		t.Fatal(err)
	}
	if err := TakesError("reload", "no arguments", []string{"a", "b"}); err.Error() != `reload takes no arguments, got 2: "a" "b"` {
		t.Fatal(err)
	}
}
