package tenderduty

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestChainAddDuplicateAcrossFiles(t *testing.T) {
	for _, target := range []string{"", "Other"} {
		t.Run("to="+target, func(t *testing.T) {
			options, filename, original := chainCommandFixture(t)
			if target != "" {
				other := bytes.ReplaceAll(original, []byte("celestiavaloper1test"), []byte("othervaloper1test"))
				if err := os.WriteFile(filepath.Join(options.Directory, target+".yml"), other, 0600); err != nil {
					t.Fatal(err)
				}
			}
			add := AddChainOptions{ConfigFile: options.ConfigFile, Directory: options.Directory, Password: options.Password, Valoper: "celestiavaloper1test", Node: "https://rpc.example", Name: "Different"}
			if target != "" {
				add.Name, add.To = "", target
			}
			fetch := addChainTestFetchers()
			fetch.validator = func(context.Context, string, string) (*ValInfo, error) {
				t.Fatal("duplicate queried staking")
				return nil, nil
			}
			err := addChain(context.Background(), add, &bytes.Buffer{}, fetch)
			if err == nil || !strings.Contains(err.Error(), "celestiavaloper1test is already watched by Celestia ("+filename+")") {
				t.Fatalf("duplicate accepted: %v", err)
			}
			data, _ := os.ReadFile(filename)
			if !bytes.Equal(data, original) {
				t.Fatal("duplicate changed existing file")
			}
		})
	}
	options, _, _ := chainCommandFixture(t)
	add := AddChainOptions{ConfigFile: options.ConfigFile, Directory: options.Directory, Password: options.Password, Valoper: "celestiavaloper1test", Node: "https://rpc.example", Name: "Celestia", Force: true}
	if err := addChain(context.Background(), add, &bytes.Buffer{}, addChainTestFetchers()); err != nil {
		t.Fatal(err)
	}
}

func TestChainNamesWithSpaces(t *testing.T) {
	options, _, _ := chainCommandFixture(t)
	add := AddChainOptions{ConfigFile: options.ConfigFile, Directory: options.Directory, Password: options.Password, Valoper: "newvaloper1test", Node: "https://rpc.example", Name: "Atom One"}
	var output bytes.Buffer
	if err := addChain(context.Background(), add, &output, addChainTestFetchers()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(options.Directory, "Atom-One.yml")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Added Atom-One:") {
		t.Fatal(&output)
	}
	options.reload = func(string) (int, error) { return 0, &notRunningError{} }
	output.Reset()
	if err := ChainCommand(context.Background(), options, []string{"rename", "Atom-One", "New Name"}, strings.NewReader(""), &output); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(options.Directory, "New-Name.yml")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Renamed Atom-One to New-Name") || strings.Count(output.String(), "not running") != 1 || !strings.Contains(output.String(), "Not applied yet: tenderduty is not running; history stays under Atom-One unless you rename it back") {
		t.Fatal(&output)
	}
}

func TestChainMissingConfigPreflight(t *testing.T) {
	options, _, _ := chainCommandFixture(t)
	if err := os.Remove(options.ConfigFile); err != nil {
		t.Fatal(err)
	}
	options.Home = filepath.Dir(options.ConfigFile)
	for _, args := range [][]string{{"add", "valoper1test", "--node", "https://rpc.example"}, {"list"}, {"set", "Celestia", "public_fallback=yes"}, {"show", "Celestia"}, {"rename", "Celestia", "New"}, {"remove", "Celestia", "--yes"}, {"node", "add", "Celestia", "https://rpc.example"}} {
		err := ChainCommand(context.Background(), options, args, strings.NewReader(""), &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), "no config.yml in "+options.Home+"; create one first:") || !strings.Contains(err.Error(), "tenderduty -home "+options.Home+" -example-config > "+options.ConfigFile) {
			t.Fatalf("%v: %v", args, err)
		}
	}
	fetch := addChainTestFetchers()
	fetch.status = func(context.Context, string) (addChainStatus, error) {
		t.Fatal("missing config contacted node")
		return addChainStatus{}, nil
	}
	err := addChain(context.Background(), AddChainOptions{ConfigFile: options.ConfigFile, Valoper: "valoper1test", Node: "https://rpc.example"}, &bytes.Buffer{}, fetch)
	if err == nil || !strings.Contains(err.Error(), "create one first") {
		t.Fatalf("missing config: %v", err)
	}
	if err := os.WriteFile(options.ConfigFile+".asc", []byte("encrypted"), 0600); err != nil {
		t.Fatal(err)
	}
	if file, err := requireChainConfig(options.ConfigFile, options.Home); err != nil || file != options.ConfigFile+".asc" {
		t.Fatalf("encrypted config: %s, %v", file, err)
	}
}

func TestChainShowSettingsAndSources(t *testing.T) {
	useRegistries(t, mainnetRegistry(t), testnetRegistry(t))
	options, filename, original := chainCommandFixture(t)
	shared := "alert_defaults:\n  stalled_minutes: 17\ntelegram:\n  api_key: telegram-secret\ndiscord:\n  webhook: https://discord.example/secret\n"
	if err := os.WriteFile(options.ConfigFile, []byte(shared), 0600); err != nil {
		t.Fatal(err)
	}
	data := append(original, []byte("public_fallback: yes\n")...)
	if err := os.WriteFile(filename, data, 0600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := ChainCommand(context.Background(), options, []string{"show", "celestia"}, strings.NewReader(""), &output); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	for _, want := range []string{"Celestia \u00b7 chain-1 \u00b7 " + filename, "Validator", "celestiavaloper1test", "https://rpc.example", "alert_if_down: yes", "Settings", "value", "from", "not in the chain registry; set testnet=yes for a testnet", "Change one: tenderduty chain set Celestia"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q: %s", want, text)
		}
	}
	for _, tc := range []struct{ key, value, source string }{{"public_fallback", "yes", "chains.d/Celestia.yml"}, {"alerts.stalled_minutes", "17", "config.yml alert_defaults"}, {"alerts.bridge_enabled", "no", "default"}, {"alerts.telegram.api_key", "***", "config.yml"}, {"alerts.discord.webhook", "***", "config.yml"}} {
		found := false
		for _, line := range strings.Split(text, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), tc.key+" ") && strings.Contains(line, tc.value) && strings.HasSuffix(line, tc.source) {
				found = true
			}
		}
		if !found {
			t.Fatalf("missing %v: %s", tc, text)
		}
	}
	for _, secret := range []string{"telegram-secret", "https://discord.example/secret"} {
		if strings.Contains(text, secret) {
			t.Fatal("secret printed")
		}
	}
	after, _ := os.ReadFile(filename)
	if !bytes.Equal(after, data) {
		t.Fatal("show wrote config")
	}
}

func TestChainShowMultistakingAndConfigChain(t *testing.T) {
	options, filename, _ := chainCommandFixture(t)
	multi := "chain_id: chain-1\nvalidators:\n  - name: RST\n    valoper_address: realiovaloper1rst\n  - name: RIO\n    valoper_address: realiovaloper1rio\nnodes:\n  - url: https://rpc.example\n    alert_if_down: no\n"
	if err := os.WriteFile(filename, []byte(multi), 0600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := ChainCommand(context.Background(), options, []string{"show", "Celestia"}, strings.NewReader(""), &output); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Validators", "RST", "RIO", "realiovaloper1rst", "realiovaloper1rio", "alert_if_down: no"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("missing %s: %s", want, &output)
		}
	}
	if err := os.WriteFile(options.ConfigFile, []byte("chains:\n  Inline:\n    chain_id: inline-1\n    valoper_address: inlinevaloper1test\n    public_fallback: yes\n"), 0600); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if err := ChainCommand(context.Background(), options, []string{"show", "inline"}, strings.NewReader(""), &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Inline") || !strings.Contains(output.String(), options.ConfigFile) {
		t.Fatal(&output)
	}
	output.Reset()
	if err := ChainCommand(context.Background(), options, []string{"list"}, strings.NewReader(""), &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Inline (config.yml)") || strings.Contains(output.String(), "FILE") {
		t.Fatal(&output)
	}
}

func TestChainSetAssignmentSyntax(t *testing.T) {
	options, filename, _ := chainCommandFixture(t)
	for _, args := range [][]string{{"set", "Celestia", "public_fallback", "=", "yes"}, {"set", "Celestia", "Public_Fallback", "=yes"}} {
		err := ChainCommand(context.Background(), options, args, strings.NewReader(""), &bytes.Buffer{})
		if err == nil || err.Error() != `write key=value without spaces around "="` {
			t.Fatalf("%v: %v", args, err)
		}
	}
	if err := ChainCommand(context.Background(), options, []string{"set", "Celestia", "Public_Fallback=yes"}, strings.NewReader(""), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filename)
	if !bytes.Contains(data, []byte("public_fallback: yes")) || bytes.Contains(data, []byte("Public_Fallback")) {
		t.Fatal(string(data))
	}
	err := ChainCommand(context.Background(), options, []string{"set", "Celestia", "public_fallbac=yes"}, strings.NewReader(""), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "? Every setting: tenderduty chain show Celestia") {
		t.Fatal(err)
	}
}

func TestCompactChainAlerts(t *testing.T) {
	all := []string{"missed_blocks", "stalled", "no_servers", "gov", "upgrade", "missed_proposal", "fibre", "bridge"}
	for _, tc := range []struct {
		n    int
		want string
	}{{8, "all"}, {6, "all except fibre,bridge"}, {4, "missed_blocks,stalled,no_servers,gov"}, {0, ""}} {
		if got := compactAlertFamilies(all[:tc.n]); got != tc.want {
			t.Fatalf("%d: %q, want %q", tc.n, got, tc.want)
		}
	}
}

func TestDuplicateValidatorFromMultistakingAndSharedConfig(t *testing.T) {
	for _, multi := range []bool{false, true} {
		options, filename, _ := chainCommandFixture(t)
		existing := "Celestia"
		if multi {
			data := "chain_id: chain-1\nvalidators:\n  - name: RST\n    valoper_address: watchedvaloper1test\nnodes:\n  - url: https://rpc.example\n"
			if err := os.WriteFile(filename, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			existing += " / RST"
		} else {
			data := "chains:\n  Inline:\n    chain_id: chain-1\n    valoper_address: watchedvaloper1test\n    public_fallback: yes\n"
			if err := os.WriteFile(options.ConfigFile, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			existing, filename = "Inline", options.ConfigFile
		}
		fetch := addChainTestFetchers()
		fetch.validator = func(context.Context, string, string) (*ValInfo, error) {
			t.Fatal("duplicate queried validator")
			return nil, nil
		}
		fetch.registry = func(context.Context, string) (string, error) { return "New Registry Name", nil }
		add := AddChainOptions{ConfigFile: options.ConfigFile, Directory: options.Directory, Password: options.Password, Valoper: "watchedvaloper1test", Node: "https://rpc.example"}
		err := addChain(context.Background(), add, &bytes.Buffer{}, fetch)
		if err == nil || !strings.Contains(err.Error(), "is already watched by "+existing+" ("+filename+")") {
			t.Fatalf("duplicate accepted: %v", err)
		}
	}
}

func TestChainShowFibreBridgeTokens(t *testing.T) {
	options, filename, _ := chainCommandFixture(t)
	if err := os.WriteFile(options.ConfigFile, []byte("otel_metrics:\n  listen: 127.0.0.1:4318\n  token: fibre-secret-value\n"), 0600); err != nil {
		t.Fatal(err)
	}
	tokenFile := filepath.Join(filepath.Dir(filename), "bridge.token")
	if err := os.WriteFile(tokenFile, []byte("bridge-secret-value"), 0600); err != nil {
		t.Fatal(err)
	}
	data := "chain_id: chain-1\nvaloper_address: celestiavaloper1test\nnodes:\n  - url: https://rpc.example\nalerts:\n  fibre_enabled: yes\nbridge:\n  url: http://127.0.0.1:26658\n  token_file: " + filepath.ToSlash(tokenFile) + "\n"
	if err := os.WriteFile(filename, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := ChainCommand(context.Background(), options, []string{"show", "Celestia"}, strings.NewReader(""), &output); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Fibre\n", "Bridge\n", "127.0.0.1:4318", "http://127.0.0.1:26658", filepath.ToSlash(tokenFile)} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("missing %s: %s", want, &output)
		}
	}
	if strings.Count(output.String(), "token: set") != 2 || strings.Contains(output.String(), "secret-value") {
		t.Fatal(&output)
	}
}

func TestChainShowTestnetOrigin(t *testing.T) {
	useRegistries(t, mainnetRegistry(t), testnetRegistry(t))
	for _, tc := range []struct{ id, override, value, from string }{
		{"mocha-4", "", "yes", "from the chain registry"},
		{"osmosis-1", "", "no", "from the chain registry"},
		{"unknown-1", "", "no", "not in the chain registry; set testnet=yes for a testnet"},
		{"mocha-4", "testnet: no\n", "no", "chains.d/Celestia.yml"},
	} {
		options, filename, original := chainCommandFixture(t)
		data := bytes.ReplaceAll(original, []byte("chain-1"), []byte(tc.id))
		data = append(data, []byte(tc.override)...)
		if err := os.WriteFile(filename, data, 0600); err != nil {
			t.Fatal(err)
		}
		var output bytes.Buffer
		if err := showChain(options, "Celestia", &output); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, line := range strings.Split(output.String(), "\n") {
			if fields := strings.Fields(line); len(fields) >= 3 && fields[0] == "testnet" && fields[1] == tc.value && strings.HasSuffix(line, tc.from) {
				found = true
			}
		}
		if !found {
			t.Fatalf("%+v: %s", tc, &output)
		}
	}
}

func TestChainShowLegacyNames(t *testing.T) {
	useRegistries(t, mainnetRegistry(t), testnetRegistry(t))
	for _, legacy := range []bool{false, true} {
		options, filename, original := chainCommandFixture(t)
		if legacy {
			original = bytes.Replace(original, []byte("alerts:\n"), []byte("alerts:\n  discord_alerts: true\n  percentage_missed: 10\n"), 1)
			if err := os.WriteFile(filename, original, 0600); err != nil {
				t.Fatal(err)
			}
		}
		var output bytes.Buffer
		if err := showChain(options, "Celestia", &output); err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(output.String(), "\n") {
			for key := range legacyAlertSettings {
				if strings.HasPrefix(strings.TrimSpace(line), "alerts."+key+" ") {
					t.Fatalf("legacy row: %s", line)
				}
			}
		}
		if strings.Contains(output.String(), "Old setting names in ") != legacy {
			t.Fatal(&output)
		}
		if legacy && !strings.Contains(output.String(), "alerts.discord_alerts (now alerts.discord.enabled)") {
			t.Fatal(&output)
		}
		if !strings.Contains(output.String(), "alerts.discord.enabled") {
			t.Fatal(&output)
		}
	}
}
