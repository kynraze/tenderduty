package tenderduty

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const unreachableRegistry = "http://127.0.0.1:1/"

// useRegistries sets both URLs and clears the registry map and node cache.
func useRegistries(t *testing.T, mainnet, testnet string) {
	t.Helper()
	oldMainnet, oldTestnet := registryJson, testnetRegistryJson
	registryJson, testnetRegistryJson = mainnet, testnet
	pathMux.Lock()
	oldPaths := cosmosPaths
	cosmosPaths = make(map[string]registryPath)
	pathMux.Unlock()
	registryMux.Lock()
	oldCache := registryCache
	registryCache = make(map[string]registryNodes)
	registryMux.Unlock()
	t.Cleanup(func() {
		registryJson, testnetRegistryJson = oldMainnet, oldTestnet
		pathMux.Lock()
		cosmosPaths = oldPaths
		pathMux.Unlock()
		registryMux.Lock()
		registryCache = oldCache
		registryMux.Unlock()
		for _, id := range []string{"osmosis-1", "both-1", "mocha-4", "osmo-test-5"} {
			reportNetworks.Delete(id)
		}
	})
}

// registryServer serves known paths and returns 404 otherwise.
func registryServer(t *testing.T, pages map[string]string) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page, ok := pages[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(page))
	}))
	t.Cleanup(server.Close)
	return server.URL + "/"
}

func downRegistry(t *testing.T) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	return server.URL + "/"
}

func mainnetRegistry(t *testing.T) string {
	return registryServer(t, map[string]string{
		"/":        `{"chains":[{"chain_id":"osmosis-1","path":"osmosis"},{"chain_id":"both-1","path":"both"}]}`,
		"/osmosis": `{"chain":{"chain_id":"osmosis-1","network_type":"mainnet","pretty_name":"Osmosis","bech32_prefix":"osmo"}}`,
		"/both":    `{"chain":{"chain_id":"both-1","network_type":"mainnet","pretty_name":"Both Mainnet","bech32_prefix":"both"}}`,
	})
}

func testnetRegistry(t *testing.T) string {
	return registryServer(t, map[string]string{
		"/":                 `{"chains":[{"chain_id":"mocha-4","path":"celestiatestnet3"},{"chain_id":"both-1","path":"bothtestnet"},{"chain_id":"osmo-test-5","path":"osmosistestnet"}]}`,
		"/celestiatestnet3": `{"chain":{"chain_id":"mocha-4","network_type":"testnet","pretty_name":"Mocha Testnet","bech32_prefix":"celestia","best_apis":{"rpc":[{"address":"https://mocha-best.example/"}]},"apis":{"rpc":[{"address":"https://mocha-best.example"},{"address":"http://mocha-other.example:26657"}]}}}`,
		"/bothtestnet":      `{"chain":{"chain_id":"both-1","network_type":"testnet","pretty_name":"Both Testnet","bech32_prefix":"both"}}`,
		"/osmosistestnet":   `{"chain":{"chain_id":"osmo-test-5","network_type":"testnet","pretty_name":"Osmosis Testnet","bech32_prefix":"osmo"}}`,
	})
}

func addChainTestnetFetchers(chainID string) addChainFetchers {
	fetch := addChainTestFetchers()
	fetch.status = func(context.Context, string) (addChainStatus, error) {
		return addChainStatus{ChainID: chainID}, nil
	}
	fetch.registry = registryName
	fetch.selectRegistry = selectedRegistryName
	return fetch
}

func TestTestnetOnlyChainIsNamedFromRegistry(t *testing.T) {
	useRegistries(t, mainnetRegistry(t), testnetRegistry(t))
	dir := t.TempDir()
	var output bytes.Buffer
	options := AddChainOptions{Valoper: "celestiavaloper1test", Node: "https://rpc.example", Directory: dir}
	if err := addChain(context.Background(), options, &output, addChainTestnetFetchers("mocha-4")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "Mocha-Testnet.yml")); err != nil {
		t.Fatalf("file isn't named from the registry: %v", err)
	}
	if strings.Contains(output.String(), "Not in the chain registry") {
		t.Fatalf("registered chain got the unregistered hint: %s", output.String())
	}
	if known, _ := reportNetworks.Load("mocha-4"); known != true {
		t.Fatal("testnet wasn't marked for reports")
	}
}

func TestChainFlagTakesTestnetPath(t *testing.T) {
	useRegistries(t, mainnetRegistry(t), testnetRegistry(t))
	dir := t.TempDir()
	options := AddChainOptions{Valoper: "celestiavaloper1test", Node: "https://rpc.example", RegistryChain: "celestiatestnet3", Directory: dir}
	if err := addChain(context.Background(), options, &bytes.Buffer{}, addChainTestnetFetchers("mocha-4")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "Mocha-Testnet.yml")); err != nil {
		t.Fatal(err)
	}
	if known, _ := reportNetworks.Load("mocha-4"); known != true {
		t.Fatal("testnet wasn't marked for reports")
	}
	if err := addChain(context.Background(), options, &bytes.Buffer{}, addChainTestnetFetchers("chain-1")); err == nil || !strings.Contains(err.Error(), "your node reports chain-1") {
		t.Fatalf("wrong chain id accepted: %v", err)
	}
	options.Valoper = "osmovaloper1test"
	if err := addChain(context.Background(), options, &bytes.Buffer{}, addChainTestnetFetchers("mocha-4")); err == nil || !strings.Contains(err.Error(), "prefix does not match") {
		t.Fatalf("wrong prefix accepted: %v", err)
	}
	options.RegistryChain = "nowhere"
	if err := addChain(context.Background(), options, &bytes.Buffer{}, addChainTestnetFetchers("mocha-4")); err == nil || err.Error() != `chain registry has no entry "nowhere" (mainnet or testnet)` {
		t.Fatalf("missing path accepted: %v", err)
	}
}

func TestChainInBothRegistriesUsesMainnet(t *testing.T) {
	useRegistries(t, mainnetRegistry(t), testnetRegistry(t))
	name, err := registryName(context.Background(), "both-1")
	if err != nil || name != "Both Mainnet" {
		t.Fatalf("name = %q, %v", name, err)
	}
	if known, _ := reportNetworks.Load("both-1"); known != false {
		t.Fatal("mainnet entry was marked testnet")
	}
	// mainnet entries replace testnet duplicates and survive mainnet outages
	pathMux.Lock()
	cosmosPaths["both-1"] = registryPath{path: "bothtestnet", testnet: true}
	pathMux.Unlock()
	if err := refreshRegistry(context.Background()); err != nil {
		t.Fatal(err)
	}
	pathMux.Lock()
	entry := cosmosPaths["both-1"]
	pathMux.Unlock()
	if entry.testnet || entry.path != "both" {
		t.Fatalf("entry = %+v", entry)
	}
	registryJson = downRegistry(t)
	if err := refreshRegistry(context.Background()); err != nil {
		t.Fatal(err)
	}
	pathMux.Lock()
	entry = cosmosPaths["both-1"]
	pathMux.Unlock()
	if entry.testnet {
		t.Fatalf("testnet list replaced the mainnet entry: %+v", entry)
	}
}

func TestRegistryKeepsWhatLoaded(t *testing.T) {
	ctx := context.Background()
	t.Run("testnet down", func(t *testing.T) {
		useRegistries(t, mainnetRegistry(t), downRegistry(t))
		if name, err := registryName(ctx, "osmosis-1"); err != nil || name != "Osmosis" {
			t.Fatalf("name = %q, %v", name, err)
		}
		if _, err := registryName(ctx, "mocha-4"); err == nil || errors.Is(err, errNotInRegistry) {
			t.Fatalf("testnet chain resolved without its registry: %v", err)
		}
		if _, known := reportNetworks.Load("mocha-4"); known {
			t.Fatal("network stored while the testnet registry is down")
		}
	})
	t.Run("mainnet down", func(t *testing.T) {
		useRegistries(t, downRegistry(t), testnetRegistry(t))
		if name, err := registryName(ctx, "mocha-4"); err != nil || name != "Mocha Testnet" {
			t.Fatalf("name = %q, %v", name, err)
		}
	})
	t.Run("mainnet unreachable", func(t *testing.T) {
		useRegistries(t, unreachableRegistry, testnetRegistry(t))
		if name, err := registryName(ctx, "osmo-test-5"); err != nil || name != "Osmosis Testnet" {
			t.Fatalf("name = %q, %v", name, err)
		}
	})
	t.Run("both down", func(t *testing.T) {
		useRegistries(t, downRegistry(t), unreachableRegistry)
		err := refreshRegistry(ctx)
		if err == nil || errors.Is(err, errNotInRegistry) {
			t.Fatalf("refresh = %v", err)
		}
		if _, err := registryName(ctx, "mocha-4"); err == nil || errors.Is(err, errNotInRegistry) {
			t.Fatalf("registry name = %v", err)
		}
	})
}

func TestReportNetworkLookupBeforeSnapshotDue(t *testing.T) {
	for _, tc := range []struct {
		chainID string
		testnet bool
	}{
		{chainID: "mocha-4", testnet: true},
		{chainID: "missing-1", testnet: false},
	} {
		t.Run(tc.chainID, func(t *testing.T) {
			useRegistries(t, mainnetRegistry(t), testnetRegistry(t))
			reportNetworks.Delete(tc.chainID)
			t.Cleanup(func() { reportNetworks.Delete(tc.chainID) })
			now := time.Now()
			cc := &ChainConfig{ChainId: tc.chainID}
			c := &Config{
				Chains: map[string]*ChainConfig{tc.chainID: cc},
				reportData: reportHistory{Snapshots: map[string][]reportSnapshot{
					tc.chainID: {{At: now.Add(-30 * time.Minute)}},
				}},
			}
			c.collectReportSnapshots(now, false)
			if cc.reportIsTestnet() != tc.testnet {
				t.Fatalf("testnet = %v, want %v", cc.reportIsTestnet(), tc.testnet)
			}
			if known, ok := reportNetworks.Load(tc.chainID); !ok || known != tc.testnet {
				t.Fatalf("cached network = %v, %v; want %v", known, ok, tc.testnet)
			}
		})
	}
}

func TestPublicFallbackUsesTestnetNodes(t *testing.T) {
	useRegistries(t, mainnetRegistry(t), testnetRegistry(t))
	nodes, err := registryRpcs(context.Background(), "mocha-4")
	if err != nil || len(nodes) != 2 || nodes[0] != "https://mocha-best.example:443" || nodes[1] != "http://mocha-other.example:26657" {
		t.Fatalf("nodes %v, %v", nodes, err)
	}
}

func TestAddChainNotInRegistryHint(t *testing.T) {
	useRegistries(t, mainnetRegistry(t), testnetRegistry(t))
	parent := t.TempDir()
	dir := filepath.Join(parent, "chains.d")
	configFile := filepath.Join(parent, "config.yml")
	if err := os.WriteFile(configFile, []byte("alert_defaults:\n  upgrade_enabled: yes\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	options := AddChainOptions{Valoper: "celestiavaloper1test", Node: "https://rpc.example", Directory: dir}
	if err := addChain(context.Background(), options, &output, addChainTestnetFetchers("newchain-1")); err != nil {
		t.Fatal(err)
	}
	want := "Not in the chain registry (mainnet or testnet), so the file is named after the chain id.\n  If it is a testnet: tenderduty chain set newchain-1 testnet=yes\n  Other name: rename " + filepath.Join(dir, "newchain-1.yml") + ", then tenderduty reload\n"
	text := output.String()
	if !strings.HasSuffix(text, want) || !strings.Contains(text[:strings.Index(text, want)], "Added newchain-1: ") || strings.Contains(text, "Could not read registry name") {
		t.Fatalf("output = %q", text)
	}
	if _, err := os.Stat(filepath.Join(dir, "newchain-1.yml")); err != nil {
		t.Fatal(err)
	}

	// the suggested command works on the written file
	password := ""
	command := ChainCommandOptions{ConfigFile: configFile, Directory: dir, StateFile: filepath.Join(parent, "state.json"), Password: &password}
	if err := ChainCommand(context.Background(), command, []string{"set", "newchain-1", "testnet=yes"}, strings.NewReader(""), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "newchain-1.yml"))
	var chain ChainConfig
	if err := decodeConfig(data, &chain); err != nil || chain.Testnet == nil || !*chain.Testnet {
		t.Fatalf("testnet wasn't set: %v\n%s", err, data)
	}

	// an explicit name skips registry naming
	output.Reset()
	options.Name = "Chosen"
	if err := addChain(context.Background(), options, &output, addChainTestnetFetchers("newchain-1")); err != nil || strings.Contains(output.String(), "Not in the chain registry") {
		t.Fatalf("named add: %v, %s", err, output.String())
	}
	useRegistries(t, unreachableRegistry, unreachableRegistry)
	output.Reset()
	options.Name = ""
	options.Force = true
	if err := addChain(context.Background(), options, &output, addChainTestnetFetchers("newchain-1")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Could not read registry name") || strings.Contains(output.String(), "Not in the chain registry") {
		t.Fatalf("unreachable registry output = %q", output.String())
	}
}

func TestRegistryAssetDenoms(t *testing.T) {
	for _, tc := range []struct {
		name    string
		body    string
		base    string
		display string
	}{
		{
			name:    "objects",
			body:    `{"chain":{"chain_id":"mocha-5","network_type":"testnet","assets":[{"name":"Celestia","symbol":"TIA","denom":"utia","decimals":6,"base":{"denom":"utia","exponent":0},"display":{"denom":"tia","exponent":6},"denom_units":[{"denom":"utia","exponent":0},{"denom":"tia","exponent":6}]}]}}`,
			base:    "utia",
			display: "tia",
		},
		{
			name:    "strings",
			body:    `{"chain":{"chain_id":"mocha-5","network_type":"testnet"},"assets":[{"base":"uatom","display":"atom"}]}`,
			base:    "uatom",
			display: "atom",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Cleanup(func() { reportNetworks.Delete("mocha-5") })
			base := registryServer(t, map[string]string{"/chain": tc.body})
			chain, err := fetchRegistryFrom(context.Background(), base, "chain")
			if err != nil {
				t.Fatal(err)
			}
			if len(chain.Assets) != 1 {
				t.Fatalf("assets = %+v", chain.Assets)
			}
			if string(chain.Assets[0].Base) != tc.base || string(chain.Assets[0].Display) != tc.display {
				t.Fatalf("asset = %+v", chain.Assets[0])
			}
			if known, _ := reportNetworks.Load("mocha-5"); known != true {
				t.Fatal("testnet wasn't marked for reports")
			}
		})
	}
}

func TestDashboardNetwork(t *testing.T) {
	c := auditConfig(t)
	useRegistries(t, mainnetRegistry(t), testnetRegistry(t))
	for _, id := range []string{"mocha-4", "missing-dashboard"} {
		id := id
		reportNetworks.Delete(id)
		dashboardNetworkLookups.Delete(id)
		t.Cleanup(func() { reportNetworks.Delete(id); dashboardNetworkLookups.Delete(id) })
	}
	cc := &ChainConfig{name: "Chain", ChainId: "mocha-4"}
	c.Chains[cc.name] = cc
	if cc.dashboardStatus().Testnet {
		t.Fatal("unknown network must default to mainnet")
	}
	cc.lookupDashboardNetwork(context.Background())
	if !cc.dashboardStatus().Testnet {
		t.Fatal("registry testnet missing from dashboard")
	}
	mainnet := false
	cc.Testnet = &mainnet
	if cc.dashboardStatus().Testnet {
		t.Fatal("mainnet override ignored")
	}
	testnet := true
	cc.ChainId = "missing-dashboard"
	cc.Testnet = &testnet
	cc.lookupDashboardNetwork(context.Background())
	if !cc.dashboardStatus().Testnet {
		t.Fatal("testnet override ignored")
	}
	if _, started := dashboardNetworkLookups.Load(cc.ChainId); started {
		t.Fatal("override must skip registry")
	}
	cc.Testnet = nil
	cc.lookupDashboardNetwork(context.Background())
	if cc.dashboardStatus().Testnet {
		t.Fatal("missing registry entry must stay mainnet")
	}
	reportNetworks.Delete(cc.ChainId)
	cc.lookupDashboardNetwork(context.Background())
	if _, known := reportNetworks.Load(cc.ChainId); known {
		t.Fatal("registry lookup repeated")
	}
}
