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
	old := registryJson
	registryJson = server.URL + "/"
	defer func() { registryJson = old }()
	pathMux.Lock()
	cosmosPaths = map[string]string{"atomone-1": "atomone"}
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
