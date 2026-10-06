package tenderduty

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// altValopers is used to get a bech32 prefix for chains using non-standard naming
var altValopers = &valoperOverrides{
	Prefixes: map[string]string{
		"ival":    "ica",         // Iris hub
		"crocncl": "crocnclcons", // Crypto.org
	},
}

type valoperOverrides struct {
	sync.RWMutex
	Prefixes    map[string]string `json:"prefixes"`
	LastUpdated time.Time         `json:"last_updated"`
}

func (vo *valoperOverrides) getAltPrefix(oper string) (prefix string, ok bool) {
	split := strings.Split(oper, "1")
	if len(split) == 0 {
		return "", false
	}
	vo.RLock()
	defer vo.RUnlock()
	return altValopers.Prefixes[split[0]], altValopers.Prefixes[split[0]] != ""
}

// cosmosPaths holds a mapping useful for finding public nodes from the cosmos directory from eco_stake
// it will be refreshed periodically.
var cosmosPaths = mainnetPaths(map[string]string{
	"Antora":                     "idep",
	"Oraichain":                  "oraichain",
	"agoric-3":                   "agoric",
	"akashnet-2":                 "akash",
	"arkh":                       "arkh",
	"axelar-dojo-1":              "axelar",
	"bitcanna-1":                 "bitcanna",
	"bitsong-2b":                 "bitsong",
	"bostrom":                    "bostrom",
	"carbon-1":                   "carbon",
	"cerberus-chain-1":           "cerberus",
	"cheqd-mainnet-1":            "cheqd",
	"chihuahua-1":                "chihuahua",
	"colosseum-1":                "firmachain",
	"columbus-5":                 "terra",
	"comdex-1":                   "comdex",
	"core-1":                     "persistence",
	"cosmoshub-4":                "cosmoshub",
	"crescent-1":                 "crescent",
	"cronosmainnet_25-1":         "cronos",
	"crypto-org-chain-mainnet-1": "cryptoorgchain",
	"cudos-1":                    "cudos",
	"darchub":                    "konstellation",
	"desmos-mainnet":             "desmos",
	"dig-1":                      "dig",
	"echelon_3000-3":             "echelon",
	"emoney-3":                   "emoney",
	"evmos_9001-2":               "evmos",
	"fetchhub-4":                 "fetchhub",
	"galaxy-1":                   "galaxy",
	"genesis_29-2":               "genesisl1",
	"gravity-bridge-3":           "gravitybridge",
	"impacthub-3":                "impacthub",
	"injective-1":                "injective",
	"iov-mainnet-ibc":            "starname",
	"irishub-1":                  "irisnet",
	"juno-1":                     "juno",
	"kava_2222-10":               "kava",
	"kichain-2":                  "kichain",
	"laozi-mainnet":              "bandchain",
	"likecoin-mainnet-2":         "likecoin",
	"logos_7002-1":               "logos",
	"lum-network-1":              "lumnetwork",
	"mainnet-3":                  "decentr",
	"mantle-1":                   "assetmantle",
	"meme-1":                     "meme",
	"microtick-1":                "microtick",
	"morocco-1":                  "chronicnetwork",
	"mythos_7001-1":              "mythos",
	"nomic-stakenet-2":           "nomic",
	"octa":                       "octa",
	"odin-mainnet-freya":         "odin",
	"osmosis-1":                  "osmosis",
	"panacea-3":                  "panacea",
	"phoenix-1":                  "terra2",
	"pio-mainnet-1":              "provenance",
	"regen-1":                    "regen",
	"secret-4":                   "secretnetwork",
	"sentinelhub-2":              "sentinel",
	"shentu-2.2":                 "shentu",
	"sifchain-1":                 "sifchain",
	"sommelier-3":                "sommelier",
	"stargaze-1":                 "stargaze",
	"thorchain-mainnet-v1":       "thorchain",
	"titan-1":                    "rizon",
	"umee-1":                     "umee",
	"vidulum-1":                  "vidulum",
})

type registryPath struct {
	path    string
	testnet bool
}

func mainnetPaths(paths map[string]string) map[string]registryPath {
	entries := make(map[string]registryPath, len(paths))
	for id, path := range paths {
		entries[id] = registryPath{path: path}
	}
	return entries
}

var pathMux sync.Mutex

var (
	registryJson        = "https://chains.cosmos.directory/"
	testnetRegistryJson = "https://chains.testcosmos.directory/"
)

// a trimmed down version only holding the info we need to create a lookup map
type registryResults struct {
	Chains []struct {
		Path    string `json:"path"`
		ChainId string `json:"chain_id"`
	} `json:"chains"`
}

func fetchRegistryList(ctx context.Context, url string) (*registryResults, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("registry returned status %d", res.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	chains := &registryResults{}
	err = json.Unmarshal(body, chains)
	if err != nil {
		return nil, err
	}
	if len(chains.Chains) == 0 {
		return nil, errors.New("response had no chains")
	}
	return chains, nil
}

// refreshRegistry maps chain IDs to mainnet and testnet paths for public node lookup.
// Mainnet wins duplicate IDs; a failed registry refresh keeps its entries.
func refreshRegistry(ctx context.Context) error {
	mainnet, mainErr := fetchRegistryList(ctx, registryJson)
	testnet, testErr := fetchRegistryList(ctx, testnetRegistryJson)
	if mainErr != nil && testErr != nil {
		return fmt.Errorf("mainnet registry: %w; testnet registry: %w", mainErr, testErr)
	}
	pathMux.Lock()
	defer pathMux.Unlock()
	if cosmosPaths == nil {
		cosmosPaths = make(map[string]registryPath)
	}
	if testnet != nil {
		for _, c := range testnet.Chains {
			if known, ok := cosmosPaths[c.ChainId]; !ok || known.testnet {
				cosmosPaths[c.ChainId] = registryPath{path: c.Path, testnet: true}
			}
		}
	}
	if mainnet != nil {
		for _, c := range mainnet.Chains {
			cosmosPaths[c.ChainId] = registryPath{path: c.Path}
		}
	}
	return nil
}
