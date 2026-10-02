package tenderduty

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// the public fallback picks RPC nodes from the chain registry. The cosmos.directory proxy can't be used directly, it
// doesn't pass websockets through and tenderduty needs them for blocks and votes.

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

// registryRpcs lists a chain's RPC nodes from the registry, its best rated ones first.
func registryRpcs(ctx context.Context, chainId string) ([]string, error) {
	registryMux.Lock()
	cached, ok := registryCache[chainId]
	registryMux.Unlock()
	if ok && time.Since(cached.at) < registryRefresh {
		return cached.nodes, nil
	}
	pathMux.Lock()
	path := cosmosPaths[chainId]
	pathMux.Unlock()
	if path == "" {
		return nil, fmt.Errorf("%s is not in the chain registry", chainId)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, registryJson+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("registry returned status %d", resp.StatusCode)
	}
	type apis struct {
		Rpc []struct {
			Address string `json:"address"`
		} `json:"rpc"`
	}
	result := struct {
		Chain struct {
			BestApis apis `json:"best_apis"`
			Apis     apis `json:"apis"`
		} `json:"chain"`
	}{}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&result); err != nil {
		return nil, err
	}
	seen := make(map[string]bool)
	nodes := make([]string, 0)
	for _, rpc := range append(result.Chain.BestApis.Rpc, result.Chain.Apis.Rpc...) {
		node := withDefaultPort(strings.TrimRight(rpc.Address, "/"))
		if node != "" && !seen[node] {
			seen[node] = true
			nodes = append(nodes, node)
		}
	}
	registryMux.Lock()
	registryCache[chainId] = registryNodes{nodes: nodes, at: time.Now()}
	registryMux.Unlock()
	return nodes, nil
}

// websocketWorks checks a node accepts the websocket tenderduty subscribes on.
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

// publicFallback finds a registry node with the right chain id, synced, and a working websocket. The last one that
// worked is tried first. tryUrl is newRpc's check, it also switches the chain to the node when it passes.
func (cc *ChainConfig) publicFallback(tryUrl func(string) (string, bool, bool)) (string, bool) {
	nodes, err := registryRpcs(td.context(), cc.ChainId)
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
		if len(tried) >= fallbackCandidates || td.context().Err() != nil {
			break
		}
		if tried[node] {
			continue
		}
		tried[node] = true
		if !websocketWorks(td.context(), node) {
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
