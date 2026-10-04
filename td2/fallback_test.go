package tenderduty

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRegistryRpcs(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/atomone" {
			http.NotFound(w, r)
			return
		}
		_, _ = fmt.Fprint(w, `{"chain":{"best_apis":{"rpc":[{"address":"https://best.example/"}]},"apis":{"rpc":[{"address":"https://best.example"},{"address":"http://other.example:26657"}]}}}`)
	}))
	defer server.Close()
	useRegistries(t, server.URL+"/", unreachableRegistry)
	pathMux.Lock()
	cosmosPaths = mainnetPaths(map[string]string{"atomone-1": "atomone"})
	pathMux.Unlock()
	registryMux.Lock()
	delete(registryCache, "atomone-1")
	registryMux.Unlock()

	nodes, err := registryRpcs(context.Background(), "atomone-1")
	if err != nil || len(nodes) != 2 || nodes[0] != "https://best.example:443" || nodes[1] != "http://other.example:26657" {
		t.Fatalf("nodes %v, %v", nodes, err)
	}
	if _, err := registryRpcs(context.Background(), "unknown-1"); err == nil {
		t.Fatal("a chain missing from the registry gave nodes")
	}
}

func TestRegistryRpcsDropOtherSchemes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"chain":{"best_apis":{"rpc":[{"address":"file:///etc/passwd"},{"address":"https://good.example"},{"address":"tcp://10.0.0.1:26657"}]},"apis":{"rpc":[{"address":"ws://other.example:26657"},{"address":"unix:///var/run/x.sock"},{"address":"gopher://old.example"},{"address":"https://"},{"address":"HTTP://upper.example:26657"},{"address":"javascript:alert(1)"},{"address":"://broken"}]}}}`)
	}))
	defer server.Close()
	useRegistries(t, server.URL+"/", unreachableRegistry)
	pathMux.Lock()
	cosmosPaths = mainnetPaths(map[string]string{"schemes-1": "schemes"})
	pathMux.Unlock()
	registryMux.Lock()
	delete(registryCache, "schemes-1")
	registryMux.Unlock()

	nodes, err := registryRpcs(context.Background(), "schemes-1")
	if err != nil || len(nodes) != 2 || nodes[0] != "https://good.example:443" || nodes[1] != "HTTP://upper.example:26657" {
		t.Fatalf("nodes %v, %v", nodes, err)
	}
}
