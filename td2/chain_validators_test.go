package tenderduty

import (
	"bytes"
	"context"
	"errors"
	"os"
	"sort"
	"strings"
	"testing"
)

func validatorFetchers(tokens map[string]string) addChainFetchers {
	fetch := addChainTestFetchers()
	fetch.validator = func(_ context.Context, _, valoper string) (*ValInfo, error) {
		return &ValInfo{Moniker: "Moniker " + valoper[len(valoper)-3:], Bonded: true, VotingPower: 250039}, nil
	}
	fetch.stakingToken = func(_ context.Context, _, valoper string) (string, error) {
		if label, ok := tokens[valoper]; ok {
			return label, nil
		}
		return "", errors.New("no multistaking")
	}
	return fetch
}

func addToOptions(options ChainCommandOptions, valoper string) AddChainOptions {
	return AddChainOptions{Valoper: valoper, Node: "http://node-b:26657", To: "Celestia", Directory: options.Directory, ConfigFile: options.ConfigFile, Password: options.Password}
}

func loadedChainNames(t *testing.T, options ChainCommandOptions) map[string]*ChainConfig {
	t.Helper()
	c, err := loadConfigQuiet(options.ConfigFile, options.Directory, options.Password)
	if err != nil {
		t.Fatal(err)
	}
	defer c.cancel()
	defer c.deliveryCancel()
	if fatal, problems := validateConfig(c); fatal {
		t.Fatalf("invalid: %v", problems)
	}
	return c.Chains
}

func chainNamesOf(chains map[string]*ChainConfig) string {
	names := make([]string, 0, len(chains))
	for name := range chains {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}

func TestAddToConvertsSingleValidator(t *testing.T) {
	options, filename, _ := chainCommandFixture(t)
	fetch := validatorFetchers(map[string]string{"celestiavaloper1test": "RST", "celestiavaloper1new": "RIO"})
	var output bytes.Buffer
	if err := addChain(context.Background(), addToOptions(options, "celestiavaloper1new"), &output, fetch); err != nil {
		t.Fatal(err)
	}
	want := "Moniker new on chain-1: active, not jailed, voting power 250,039\n" +
		"Celestia now lists validators; its alerts are now named \"Celestia / RST\"\n" +
		"Added RIO to Celestia: " + filename + "\n"
	if output.String() != want {
		t.Fatalf("output:\n%s\nwant:\n%s", output.String(), want)
	}
	data, _ := os.ReadFile(filename)
	text := string(data)
	for _, keep := range []string{"# Operator settings", "# Keep this comment", "chain_id: chain-1", "extra_info: Operator"} {
		if !strings.Contains(text, keep) {
			t.Fatalf("lost %q:\n%s", keep, text)
		}
	}
	if strings.Contains(text, "\nvaloper_address:") || strings.Contains(text, "\nnodes:") {
		t.Fatalf("top level keys remain:\n%s", text)
	}
	chains := loadedChainNames(t, options)
	if chainNamesOf(chains) != "Celestia / RIO,Celestia / RST" {
		t.Fatalf("chains: %s", chainNamesOf(chains))
	}
	first, second := chains["Celestia / RST"], chains["Celestia / RIO"]
	if first.ValAddress != "celestiavaloper1test" || len(first.Nodes) != 1 || !strings.Contains(first.Nodes[0].Url, "https://rpc.example") || !first.Nodes[0].AlertIfDown {
		t.Fatalf("first validator lost its nodes: %+v", first)
	}
	if second.ValAddress != "celestiavaloper1new" || len(second.Nodes) != 1 || !strings.Contains(second.Nodes[0].Url, "node-b:26657") || second.ChainId != "chain-1" {
		t.Fatalf("new validator: %+v", second)
	}
}

func TestAddToAppendsToValidatorsFile(t *testing.T) {
	options, filename, _ := chainCommandFixture(t)
	fetch := validatorFetchers(map[string]string{"celestiavaloper1test": "RST", "celestiavaloper1new": "RIO"})
	if err := addChain(context.Background(), addToOptions(options, "celestiavaloper1new"), &bytes.Buffer{}, fetch); err != nil {
		t.Fatal(err)
	}
	third := addToOptions(options, "celestiavaloper1abc")
	third.Label = "ABC"
	var output bytes.Buffer
	if err := addChain(context.Background(), third, &output, fetch); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "now lists validators") || !strings.Contains(output.String(), "Added ABC to Celestia: "+filename+"\n") {
		t.Fatalf("output: %s", output.String())
	}
	if got := chainNamesOf(loadedChainNames(t, options)); got != "Celestia / ABC,Celestia / RIO,Celestia / RST" {
		t.Fatalf("chains: %s", got)
	}
}

func TestAddToLabelFallbacks(t *testing.T) {
	options, _, _ := chainCommandFixture(t)
	fetch := validatorFetchers(nil)
	fetch.validator = func(_ context.Context, _, valoper string) (*ValInfo, error) {
		if valoper == "celestiavaloper1test" {
			return nil, errors.New("not found")
		}
		return &ValInfo{Moniker: " Big \t Val\x1b ", Bonded: true}, nil
	}
	if err := addChain(context.Background(), addToOptions(options, "celestiavaloper1new"), &bytes.Buffer{}, fetch); err != nil {
		t.Fatal(err)
	}
	if got := chainNamesOf(loadedChainNames(t, options)); got != "Celestia / Big-Val,Celestia / main" {
		t.Fatalf("chains: %s", got)
	}
}

func TestAddToRefusals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*AddChainOptions, *addChainFetchers, ChainCommandOptions)
		want   string
	}{
		{"duplicate valoper", func(o *AddChainOptions, _ *addChainFetchers, _ ChainCommandOptions) {
			o.Valoper = "celestiavaloper1test"
		}, "celestiavaloper1test is already in Celestia"},
		{"chain id", func(_ *AddChainOptions, f *addChainFetchers, _ ChainCommandOptions) {
			f.status = func(context.Context, string) (addChainStatus, error) { return addChainStatus{ChainID: "other-1"}, nil }
		}, "node reports chain id other-1, but Celestia has chain-1"},
		{"missing file", func(o *AddChainOptions, _ *addChainFetchers, c ChainCommandOptions) {
			o.To = "Nope"
		}, "Nope.yml does not exist; add the chain first without --to"},
		{"no validator", func(o *AddChainOptions, _ *addChainFetchers, c ChainCommandOptions) {
			o.To = "Empty"
			if err := os.WriteFile(c.Directory+"/Empty.yml", []byte("chain_id: chain-1\npublic_fallback: yes\n"), 0600); err != nil {
				t.Fatal(err)
			}
		}, "Empty has no validator to add to"},
		{"with name", func(o *AddChainOptions, _ *addChainFetchers, _ ChainCommandOptions) { o.Name = "X" }, "--to cannot be used with --name"},
		{"with force", func(o *AddChainOptions, _ *addChainFetchers, _ ChainCommandOptions) { o.Force = true }, "--to cannot be used with --force"},
		{"with set", func(o *AddChainOptions, _ *addChainFetchers, _ ChainCommandOptions) {
			o.Settings = []string{"testnet=yes"}
		}, "--to cannot be used with --set"},
		{"label without to", func(o *AddChainOptions, _ *addChainFetchers, _ ChainCommandOptions) {
			o.To, o.Label = "", "X"
		}, "--label requires --to"},
		{"label with slash", func(o *AddChainOptions, _ *addChainFetchers, _ ChainCommandOptions) { o.Label = "A/B" }, `validator name must not be empty or contain "/"; set one with --label`},
		{"empty moniker", func(_ *AddChainOptions, f *addChainFetchers, _ ChainCommandOptions) {
			f.validator = func(context.Context, string, string) (*ValInfo, error) { return &ValInfo{Moniker: "  "}, nil }
		}, `validator name must not be empty`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			options, filename, original := chainCommandFixture(t)
			add := addToOptions(options, "celestiavaloper1new")
			fetch := validatorFetchers(map[string]string{})
			tc.change(&add, &fetch, options)
			err := addChain(context.Background(), add, &bytes.Buffer{}, fetch)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
			after, _ := os.ReadFile(filename)
			if !bytes.Equal(original, after) {
				t.Fatal("refusal changed the file")
			}
		})
	}
}

func TestAddToDuplicateLabel(t *testing.T) {
	options, _, _ := chainCommandFixture(t)
	fetch := validatorFetchers(map[string]string{"celestiavaloper1test": "RST", "celestiavaloper1new": "RIO"})
	if err := addChain(context.Background(), addToOptions(options, "celestiavaloper1new"), &bytes.Buffer{}, fetch); err != nil {
		t.Fatal(err)
	}
	again := addToOptions(options, "celestiavaloper1other")
	again.Label = "RIO"
	err := addChain(context.Background(), again, &bytes.Buffer{}, fetch)
	if err == nil || err.Error() != "Celestia already has a validator named RIO; set another with --label" {
		t.Fatalf("got %v", err)
	}
	dup := addToOptions(options, "celestiavaloper1new")
	err = addChain(context.Background(), dup, &bytes.Buffer{}, fetch)
	if err == nil || err.Error() != "celestiavaloper1new is already in Celestia as RIO" {
		t.Fatalf("got %v", err)
	}
}

func TestChainAddToUsageErrors(t *testing.T) {
	options, _, _ := chainCommandFixture(t)
	for args, want := range map[string]string{
		"add celestiavaloper1x --node http://n:26657 --to Celestia --name X":    "--to cannot be used with --name",
		"add celestiavaloper1x --node http://n:26657 --label X":                 "--label requires --to",
		"add celestiavaloper1x --node http://n:26657 --to Celestia --label a/b": `validator name must not be empty or contain "/"`,
	} {
		err := ChainCommand(context.Background(), options, strings.Fields(args), strings.NewReader(""), &bytes.Buffer{})
		var usage *UsageError
		if !errors.As(err, &usage) || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: got %v", args, err)
		}
	}
}

func multiValidatorFile(t *testing.T, options ChainCommandOptions, names ...string) string {
	t.Helper()
	text := "chain_id: chain-2\npublic_fallback: yes\nvalidators:\n"
	for _, name := range names {
		text += "  - name: " + name + "\n    valoper_address: cosmosvaloper1" + strings.ToLower(name) + "\n"
	}
	filename := options.Directory + "/Multi.yml"
	if err := os.WriteFile(filename, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	return filename
}

func TestChainRemoveValidator(t *testing.T) {
	for _, tc := range []struct {
		answer  string
		yes     bool
		removed bool
	}{{"n\n", false, false}, {"", false, false}, {"y\n", false, true}, {"", true, true}} {
		options, _, _ := chainCommandFixture(t)
		filename := multiValidatorFile(t, options, "RST", "RIO")
		args := []string{"remove", "Multi", "--validator", "RIO"}
		if tc.yes {
			args = append(args, "--yes")
		}
		var output bytes.Buffer
		if err := ChainCommand(context.Background(), options, args, strings.NewReader(tc.answer), &output); err != nil {
			t.Fatal(err)
		}
		names := chainNamesOf(loadedChainNames(t, options))
		if tc.removed {
			if names != "Celestia,Multi / RST" || !strings.Contains(output.String(), "Removed RIO from Multi ("+filename+")\n") {
				t.Fatalf("names %s, output %s", names, output.String())
			}
		} else if names != "Celestia,Multi / RIO,Multi / RST" || !strings.Contains(output.String(), "Remove RIO from "+filename+"? [y/N] ") {
			t.Fatalf("names %s, output %s", names, output.String())
		}
	}
}

func TestChainRemoveValidatorRefusals(t *testing.T) {
	options, celestia, _ := chainCommandFixture(t)
	multi := multiValidatorFile(t, options, "RST", "RIO")
	for _, tc := range []struct{ chain, validator, want string }{
		{"Multi", "X", "Multi has no validator named X (has: RST, RIO)"},
		{"Celestia", "RST", "Celestia has a single validator; remove the chain instead"},
	} {
		err := ChainCommand(context.Background(), options, []string{"remove", tc.chain, "--validator", tc.validator, "--yes"}, strings.NewReader(""), &bytes.Buffer{})
		if err == nil || err.Error() != tc.want {
			t.Fatalf("got %v, want %q", err, tc.want)
		}
	}
	if err := ChainCommand(context.Background(), options, []string{"remove", "Multi", "--validator", "RIO", "--yes"}, strings.NewReader(""), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	// one entry is left: still a validators list, and the last one stays
	data, _ := os.ReadFile(multi)
	if !strings.Contains(string(data), "validators:") {
		t.Fatalf("converted back:\n%s", data)
	}
	err := ChainCommand(context.Background(), options, []string{"remove", "Multi", "--validator", "RST", "--yes"}, strings.NewReader(""), &bytes.Buffer{})
	if err == nil || err.Error() != "RST is the only validator in Multi; remove the chain instead" {
		t.Fatalf("got %v", err)
	}
	if _, err := os.Stat(celestia); err != nil {
		t.Fatal(err)
	}
}

func quotedName(name string) string {
	if strings.ContainsAny(name, " \t") {
		return `"` + name + `"`
	}
	return name
}

func TestAddExistingFileSays(t *testing.T) {
	options, filename, original := chainCommandFixture(t)
	add := func(chainID, valoper string, fetch addChainFetchers) error {
		fetch.status = func(context.Context, string) (addChainStatus, error) { return addChainStatus{ChainID: chainID}, nil }
		return addChain(context.Background(), AddChainOptions{Valoper: valoper, Node: "http://node-b:26657", Name: "Celestia", Directory: options.Directory, ConfigFile: options.ConfigFile, Password: options.Password}, &bytes.Buffer{}, fetch)
	}
	fetch := validatorFetchers(nil)
	err := add("chain-1", "celestiavaloper1new", fetch)
	want := filename + " already watches chain-1 (Moniker est); to add this validator to it: tenderduty chain add celestiavaloper1new --node http://node-b:26657 --to Celestia, or --force to replace the file"
	if err == nil || err.Error() != want {
		t.Fatalf("got %v, want %q", err, want)
	}
	// moniker unreadable: the address
	fetch.validator = func(_ context.Context, _, valoper string) (*ValInfo, error) {
		if valoper == "celestiavaloper1test" {
			return nil, errors.New("down")
		}
		return &ValInfo{Moniker: "New"}, nil
	}
	if err := add("chain-1", "celestiavaloper1new", fetch); err == nil || !strings.Contains(err.Error(), "already watches chain-1 (celestiavaloper1test);") {
		t.Fatalf("got %v", err)
	}
	if err := add("chain-1", "celestiavaloper1test", validatorFetchers(nil)); err == nil || err.Error() != filename+" already watches this validator (celestiavaloper1test)" {
		t.Fatalf("got %v", err)
	}
	if err := add("other-1", "celestiavaloper1new", fetch); err == nil || err.Error() != filename+" already exists; add --force to replace it" {
		t.Fatalf("got %v", err)
	}
	after, _ := os.ReadFile(filename)
	if !bytes.Equal(original, after) {
		t.Fatal("refusal changed the file")
	}
}

func TestTokenLabel(t *testing.T) {
	for _, tc := range []struct{ denom, symbol, want string }{
		{"arst", "RST", "RST"},
		{"ario", "rio", "RIO"},
		{"erc20:0xb841F365D5221Bed66d60E69094418D8C2aa5A44", "DSTRX", "DSTRX"},
		{"uatom", "", "ATOM"},
		{"arst", "bad symbol!", "RST"},
		{"erc20:0xb841F365D5221Bed66d60E69094418D8C2aa5A44", "bad/symbol", ""},
		{"erc20:0x1", "", ""},
		{"x", "", ""},
	} {
		if got := tokenLabel(tc.denom, tc.symbol); got != tc.want {
			t.Errorf("tokenLabel(%q, %q) = %q, want %q", tc.denom, tc.symbol, got, tc.want)
		}
	}
}

func TestFirstStringField(t *testing.T) {
	got, err := firstStringField([]byte{0x0a, 0x04, 'a', 'r', 's', 't'})
	if err != nil || got != "arst" {
		t.Fatalf("got %q, %v", got, err)
	}
	for _, bad := range [][]byte{nil, {0x0a, 0x09, 'a'}, {0x0d}} {
		if _, err := firstStringField(bad); err == nil {
			t.Fatalf("%v accepted", bad)
		}
	}
}
