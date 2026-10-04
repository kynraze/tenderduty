package tenderduty

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// The cosmos.directory proxy can't be used directly: it doesn't pass websockets through.

const (
	fallbackCandidates = 10               // how many registry nodes are tried before giving up
	registryRefresh    = 30 * time.Minute // how long a chain's node list from the registry is kept
)

type registryNodes struct {
	nodes []string
	at    time.Time
}

var (
	registryMux   sync.Mutex
	registryCache = make(map[string]registryNodes) // by chain id
)

// Best rated first.
func registryRpcs(ctx context.Context, chainId string) ([]string, error) {
	registryMux.Lock()
	cached, ok := registryCache[chainId]
	registryMux.Unlock()
	if ok && time.Since(cached.at) < registryRefresh {
		return cached.nodes, nil
	}
	chain, err := fetchRegistryChain(ctx, chainId)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool)
	nodes := make([]string, 0)
	for _, rpc := range append(chain.BestApis.Rpc, chain.Apis.Rpc...) {
		node := withDefaultPort(strings.TrimRight(rpc.Address, "/"))
		if node != "" && registryWebAddress(node) && !seen[node] {
			seen[node] = true
			nodes = append(nodes, node)
		}
	}
	registryMux.Lock()
	registryCache[chainId] = registryNodes{nodes: nodes, at: time.Now()}
	registryMux.Unlock()
	return nodes, nil
}

// registryWebAddress keeps http and https; the registry is third-party and these addresses get dialed.
func registryWebAddress(address string) bool {
	u, err := url.Parse(address)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

type registryAPIs struct {
	Rpc []struct {
		Address string `json:"address"`
	} `json:"rpc"`
}

type registryChain struct {
	NetworkType  string       `json:"network_type"`
	ChainID      string       `json:"chain_id"`
	Bech32Prefix string       `json:"bech32_prefix"`
	PrettyName   string       `json:"pretty_name"`
	ChainName    string       `json:"chain_name"`
	BestApis     registryAPIs `json:"best_apis"`
	Apis         registryAPIs `json:"apis"`
}

var errNotInRegistry = errors.New("not in the chain registry")

type registryStatusError int

func (e registryStatusError) Error() string {
	return fmt.Sprintf("registry returned status %d", int(e))
}

// Shared with chain add.
func fetchRegistryChain(ctx context.Context, chainID string) (registryChain, error) {
	pathMux.Lock()
	entry := cosmosPaths[chainID]
	pathMux.Unlock()
	if entry.path == "" {
		if err := refreshRegistry(ctx); err != nil {
			return registryChain{}, err
		}
		pathMux.Lock()
		entry = cosmosPaths[chainID]
		pathMux.Unlock()
	}
	if entry.path == "" {
		return registryChain{}, fmt.Errorf("%s is %w", chainID, errNotInRegistry)
	}
	base := registryJson
	if entry.testnet {
		base = testnetRegistryJson
	}
	return fetchRegistryFrom(ctx, base, entry.path)
}

// Mainnet first, the testnet registry when the mainnet one has no such path.
func fetchRegistryPath(ctx context.Context, path string) (registryChain, error) {
	chain, err := fetchRegistryFrom(ctx, registryJson, path)
	var status registryStatusError
	if errors.As(err, &status) && int(status) == http.StatusNotFound {
		return fetchRegistryFrom(ctx, testnetRegistryJson, path)
	}
	return chain, err
}

func fetchRegistryFrom(ctx context.Context, base, path string) (registryChain, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	if err != nil {
		return registryChain{}, err
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return registryChain{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return registryChain{}, registryStatusError(resp.StatusCode)
	}
	result := struct {
		Chain registryChain `json:"chain"`
	}{}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&result); err != nil {
		return registryChain{}, err
	}
	reportNetworks.Store(result.Chain.ChainID, result.Chain.NetworkType == "testnet")
	return result.Chain, nil
}

func websocketWorks(ctx context.Context, node string) bool {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	conn, err := newClientContext(ctx, node, true)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// The last working node is tried first. tryUrl also switches the chain to the node when it passes.
func (cc *ChainConfig) publicFallback(tryUrl func(string) (string, bool, bool)) (string, bool) {
	nodes, err := registryRpcs(cc.context(), cc.ChainId)
	if err != nil {
		l(cc.ChainId, "⛑ no public endpoints:", err)
		return "", false
	}
	cc.stateMux.RLock()
	last := cc.publicNode
	cc.stateMux.RUnlock()
	if last != "" {
		nodes = append([]string{last}, nodes...)
	}
	tried := make(map[string]bool)
	for _, node := range nodes {
		if len(tried) >= fallbackCandidates || cc.context().Err() != nil {
			break
		}
		if tried[node] {
			continue
		}
		tried[node] = true
		if !websocketWorks(cc.context(), node) {
			continue
		}
		if _, failed, _ := tryUrl(node); failed {
			continue
		}
		cc.stateMux.Lock()
		cc.publicNode = node
		cc.stateMux.Unlock()
		return node, true
	}
	return "", false
}
