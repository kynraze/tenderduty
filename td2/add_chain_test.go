package tenderduty

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func addChainTestFetchers() addChainFetchers {
	return addChainFetchers{
		status: func(context.Context, string) (addChainStatus, error) {
			return addChainStatus{ChainID: "chain-1"}, nil
		},
		validator: func(context.Context, string, string) (*ValInfo, error) {
			return &ValInfo{Moniker: "Operator", Bonded: true, VotingPower: 123}, nil
		},
		registry: func(context.Context, string) (string, error) { return "Registry Name", nil },
	}
}

func TestAddChainDefaultPorts(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"https://rpc.example", "https://rpc.example:443"},
		{"http://rpc.example", "http://rpc.example:80"},
		{"https://rpc.example:26657/path", "https://rpc.example:26657/path"},
		{"https://[::1]/rpc", "https://[::1]:443/rpc"},
	} {
		t.Run(tc.input, func(t *testing.T) {
			fetch := addChainTestFetchers()
			fetch.status = func(_ context.Context, node string) (addChainStatus, error) {
				if node != tc.want {
					t.Errorf("node = %q, want %q", node, tc.want)
				}
				return addChainStatus{ChainID: "chain-1"}, nil
			}
			options := AddChainOptions{Valoper: "celestiavaloper1test", Node: tc.input, Directory: t.TempDir()}
			if err := addChain(context.Background(), options, &bytes.Buffer{}, fetch); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestAddChainRefusals(t *testing.T) {
	for _, kind := range []string{"catching up", "unreachable", "not found", "nil validator", "empty chain id"} {
		t.Run(kind, func(t *testing.T) {
			fetch := addChainTestFetchers()
			switch kind {
			case "catching up":
				fetch.status = func(context.Context, string) (addChainStatus, error) {
					return addChainStatus{ChainID: "chain-1", CatchingUp: true}, nil
				}
				fetch.validator = func(context.Context, string, string) (*ValInfo, error) {
					t.Fatal("staking query was made to a catching-up node")
					return nil, nil
				}
			case "unreachable":
				fetch.status = func(context.Context, string) (addChainStatus, error) {
					return addChainStatus{}, errors.New("unreachable")
				}
			case "not found":
				fetch.validator = func(context.Context, string, string) (*ValInfo, error) {
					return nil, errors.New("not found")
				}
			case "nil validator":
				fetch.validator = func(context.Context, string, string) (*ValInfo, error) { return nil, nil }
			case "empty chain id":
				fetch.status = func(context.Context, string) (addChainStatus, error) { return addChainStatus{}, nil }
			}
			dir := t.TempDir()
			err := addChain(context.Background(), AddChainOptions{Valoper: "cosmosvaloper1test", Node: "https://rpc.example", Directory: dir}, &bytes.Buffer{}, fetch)
			if err == nil {
				t.Fatal("unsafe input was accepted")
			}
			files, readErr := os.ReadDir(dir)
			if readErr != nil || len(files) != 0 {
				t.Fatalf("refusal wrote a file: %v, %v", files, readErr)
			}
		})
	}
}

func TestAddChainNamesAndOverwrite(t *testing.T) {
	for _, tc := range []struct{ name, registry, override, want string }{
		{"registry", "Registry Name", "", "Registry-Name"},
		{"chain id", "", "", "chain-1"},
		{"override", "Registry Name", "My Validator", "My-Validator"},
		{"portable registry name", "Chain / Mainnet", "", "Chain-Mainnet"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fetch := addChainTestFetchers()
			fetch.registry = func(context.Context, string) (string, error) {
				if tc.override != "" {
					t.Fatal("registry lookup was needed despite -name")
				}
				if tc.registry == "" {
					return "", errors.New("unavailable")
				}
				return tc.registry, nil
			}
			dir := filepath.Join(t.TempDir(), "chains.d")
			options := AddChainOptions{Valoper: "celestiavaloper1test", Node: "https://rpc.example", Name: tc.override, Directory: dir, PublicFallback: true}
			var output bytes.Buffer
			if err := addChain(context.Background(), options, &output, fetch); err != nil {
				t.Fatal(err)
			}
			filename := filepath.Join(dir, tc.want+".yml")
			original, err := os.ReadFile(filename)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(output.String(), "Operator on chain-1: active, not jailed, voting power 123\n") || !strings.Contains(output.String(), "Added "+tc.want+": "+filename+"\n") {
				t.Fatalf("missing operator feedback: %s", output.String())
			}
			options.Valoper = "celestiavaloper1other"
			if err := addChain(context.Background(), options, &output, fetch); err == nil || err.Error() != filename+" already watches chain-1 (Operator); to add this validator to it: tenderduty chain add celestiavaloper1other --node https://rpc.example --to "+quotedName(tc.want)+", or --force to replace the file" {
				t.Fatalf("existing file wasn't refused: %v", err)
			}
			after, _ := os.ReadFile(filename)
			if !bytes.Equal(original, after) {
				t.Fatal("refusal overwrote the file")
			}
			options.Force = true
			if err := addChain(context.Background(), options, &output, fetch); err != nil {
				t.Fatal(err)
			}
			after, _ = os.ReadFile(filename)
			if !strings.Contains(string(after), options.Valoper) {
				t.Fatal("-force didn't replace the file")
			}
		})
	}
}

func TestAddChainFileLoadsWithDefaults(t *testing.T) {
	oldAlarms := alarms
	defer func() { alarms = oldAlarms }()
	dir := t.TempDir()
	chains := filepath.Join(dir, "chains.d")
	configFile := filepath.Join(dir, "config.yml")
	configData := []byte("# Preserve this comment\nalert_defaults:\n  consecutive_enabled: yes\n  consecutive_missed: 9\n")
	if err := os.WriteFile(configFile, configData, 0600); err != nil {
		t.Fatal(err)
	}
	options := AddChainOptions{Valoper: "celestiavaloper1test", Node: "https://rpc.example", Directory: chains, PublicFallback: true}
	if err := addChain(context.Background(), options, &bytes.Buffer{}, addChainTestFetchers()); err != nil {
		t.Fatal(err)
	}
	password := ""
	c, err := loadConfig(configFile, filepath.Join(dir, "state.json"), chains, &password)
	if err != nil {
		t.Fatal(err)
	}
	defer c.cancel()
	defer c.deliveryCancel()
	fatal, problems := validateConfig(c)
	if fatal {
		t.Fatalf("generated config is invalid: %v", problems)
	}
	chain := c.Chains["Registry-Name"]
	if chain == nil || chain.ChainId != "chain-1" || chain.ValAddress != options.Valoper || chain.Alerts.ConsecutiveMissed != 9 || !chain.Alerts.ConsecutiveAlerts || !chain.PublicFallback {
		t.Fatalf("generated chain didn't load with defaults: %+v", chain)
	}
	if len(chain.Nodes) != 1 || chain.Nodes[0].Url != "https://rpc.example:443" || !chain.Nodes[0].AlertIfDown {
		t.Fatal("node settings didn't round trip")
	}
	after, _ := os.ReadFile(configFile)
	if !bytes.Equal(configData, after) {
		t.Fatal("shared config was rewritten")
	}
	data, _ := os.ReadFile(filepath.Join(chains, "Registry Name.yml"))
	if strings.Contains(string(data), "alerts:") || strings.Contains(string(data), "extra_info:") {
		t.Fatal("generated file overrides shared defaults")
	}
}

func TestRegistryNameUsesSharedLookup(t *testing.T) {
	for _, tc := range []struct{ document, want string }{
		{`{"chain":{"pretty_name":"Pretty Chain","chain_name":"plain"}}`, "Pretty Chain"},
		{`{"chain":{"chain_name":"plain"}}`, "plain"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/" {
					fmt.Fprint(w, `{"chains":[{"chain_id":"test-add-1","path":"test-add"}]}`)
					return
				}
				fmt.Fprint(w, tc.document)
			}))
			defer server.Close()
			useRegistries(t, server.URL+"/", unreachableRegistry)
			name, err := registryName(context.Background(), "test-add-1")
			if err != nil || name != tc.want {
				t.Fatalf("registry name = %q, %v", name, err)
			}
		})
	}
}

func TestAddChainRegistrySelection(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/celestia":
			fmt.Fprint(w, `{"chain":{"chain_id":"celestia-1","bech32_prefix":"celestia","chain_name":"celestia","pretty_name":"Celestia"}}`)
		case "/mocha":
			fmt.Fprint(w, `{"chain":{"chain_id":"mocha-4","bech32_prefix":"celestia","chain_name":"mocha","pretty_name":"Mocha"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	useRegistries(t, server.URL+"/", unreachableRegistry)
	fetch := addChainTestFetchers()
	fetch.status = func(context.Context, string) (addChainStatus, error) {
		return addChainStatus{ChainID: "mocha-4"}, nil
	}
	fetch.selectRegistry = selectedRegistryName
	options := AddChainOptions{Valoper: "celestiavaloper1test", Node: "https://rpc.example", RegistryChain: "celestia", Directory: t.TempDir()}
	if err := addChain(context.Background(), options, &bytes.Buffer{}, fetch); err == nil || !strings.Contains(err.Error(), "your node reports mocha-4") {
		t.Fatalf("wrong network accepted: %v", err)
	}
	options.RegistryChain = "mocha"
	if err := addChain(context.Background(), options, &bytes.Buffer{}, fetch); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(options.Directory, "Mocha.yml")); err != nil {
		t.Fatal(err)
	}
}

func TestAddChainCelestiaFibreHint(t *testing.T) {
	for _, tc := range []struct {
		chainID, registry string
		want              bool
	}{
		{"celestia", "", true},
		{"mocha-5", "", true},
		{"arabica-11", "", true},
		{"custom-1", "celestia", true},
		{"custom-1", "celestiatestnet3", true},
		{"osmosis-1", "", false},
	} {
		t.Run(tc.chainID+tc.registry, func(t *testing.T) {
			fetch := addChainTestFetchers()
			fetch.status = func(context.Context, string) (addChainStatus, error) {
				return addChainStatus{ChainID: tc.chainID}, nil
			}
			fetch.selectRegistry = func(context.Context, string, string, string) (string, error) {
				return "Celestia", nil
			}
			options := AddChainOptions{Valoper: "celestiavaloper1test", Node: "https://rpc.example", Name: "Example", RegistryChain: tc.registry, Directory: t.TempDir()}
			var output bytes.Buffer
			if err := addChain(context.Background(), options, &output, fetch); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(filepath.Join(options.Directory, "Example.yml"))
			if err != nil {
				t.Fatal(err)
			}
			block := "# alerts:\n#   fibre_enabled: yes          # needs Fibre registered; see docs/fibre.md\n#   fibre_severity: critical\n"
			if got := strings.Contains(strings.ReplaceAll(string(data), "\r\n", "\n"), block); got != tc.want {
				t.Fatalf("Fibre block = %v, want %v: %s", got, tc.want, data)
			}
			hint := "Fibre: tenderduty chain set Example alerts.fibre_enabled=yes (see docs/fibre.md)\n"
			if got := strings.Contains(output.String(), hint); got != tc.want {
				t.Fatalf("Fibre hint = %v, want %v: %s", got, tc.want, output.String())
			}
		})
	}
}
