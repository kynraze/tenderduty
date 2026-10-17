package tenderduty

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
)

func logoPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	var data bytes.Buffer
	if err := png.Encode(&data, image.NewNRGBA(image.Rect(0, 0, width, height))); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}

func useLogoPrefix(t *testing.T, prefix string) {
	t.Helper()
	old := registryLogoPrefix
	registryLogoPrefix = prefix
	t.Cleanup(func() { registryLogoPrefix = old })
}

func TestDashboardLogoAllowlist(t *testing.T) {
	for _, address := range []string{
		"http://raw.githubusercontent.com/cosmos/chain-registry/a.png",
		"https://example.com/cosmos/chain-registry/a.png",
		"https://raw.githubusercontent.com.evil.test/cosmos/chain-registry/a.png",
		"https://raw.githubusercontent.com/other/chain-registry/a.png",
		"https://raw.githubusercontent.com/cosmos/chain-registry-evil/a.png",
		"https://raw.githubusercontent.com@evil.test/cosmos/chain-registry/a.png",
		"",
	} {
		if data := fetchDashboardLogo(context.Background(), address); data != nil {
			t.Fatalf("accepted %q", address)
		}
	}
}

func TestDashboardLogoValidation(t *testing.T) {
	pngData := logoPNG(t, 32, 32)
	for _, tc := range []struct {
		name   string
		body   []byte
		status int
		valid  bool
	}{
		{"png", pngData, 200, true},
		{"at cap", append(append([]byte(nil), pngData...), make([]byte, (256<<10)-len(pngData))...), 200, true},
		{"over cap", append(append([]byte(nil), pngData...), make([]byte, (256<<10)+1-len(pngData))...), 200, false},
		{"svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg"></svg>`), 200, false},
		{"gif", []byte("GIF89a\x01\x00\x01\x00\x80\x00\x00\x00\x00\x00\xff\xff\xff"), 200, false},
		{"width", logoPNG(t, 4097, 1), 200, false},
		{"height", logoPNG(t, 1, 4097), 200, false},
		{"max width", logoPNG(t, 4096, 1), 200, true},
		{"max height", logoPNG(t, 1, 4096), 200, true},
		{"status", pngData, 404, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write(tc.body)
			}))
			defer server.Close()
			useLogoPrefix(t, server.URL+"/")
			data := fetchDashboardLogo(context.Background(), server.URL+"/logo.png")
			if (len(data) > 0) != tc.valid || (tc.valid && !bytes.Equal(data, tc.body)) {
				t.Fatalf("got %d bytes, valid = %v", len(data), tc.valid)
			}
		})
	}
}

func TestDashboardLogoRejectsRedirectOutsideAllowlist(t *testing.T) {
	var requests atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
	}))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/logo.png", http.StatusFound)
	}))
	defer server.Close()
	useLogoPrefix(t, server.URL+"/")
	if fetchDashboardLogo(context.Background(), server.URL+"/logo.png") != nil || requests.Load() != 0 {
		t.Fatal("followed an untrusted redirect")
	}
}

func TestDashboardLogoSharedCacheAndDedupe(t *testing.T) {
	c := auditConfig(t)
	const id = "dashboard-logo-test-1"
	pngData := logoPNG(t, 32, 32)
	var registryRequests, logoRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/logo.png" {
			logoRequests.Add(1)
			_, _ = w.Write(pngData)
			return
		}
		registryRequests.Add(1)
		_, _ = fmt.Fprintf(w, `{"chain":{"chain_id":%q,"logo_URIs":{"png":%q}}}`, id, registryLogoPrefix+"logo.png")
	}))
	defer server.Close()
	useLogoPrefix(t, server.URL+"/")
	useRegistries(t, server.URL+"/", unreachableRegistry)
	pathMux.Lock()
	cosmosPaths[id] = registryPath{path: "chain"}
	pathMux.Unlock()
	t.Cleanup(func() {
		dashboardLogoLookups.Delete(id)
		dashboardLogos.Delete(id)
		reportNetworks.Delete(id)
	})
	first := &ChainConfig{name: "First", ChainId: id}
	second := &ChainConfig{name: "Second", ChainId: id}
	c.Chains[first.name], c.Chains[second.name] = first, second
	if first.dashboardStatus().Logo || c.dashboardLogo(first.name) != nil {
		t.Fatal("logo set before caching")
	}
	var workers sync.WaitGroup
	for i := 0; i < 8; i++ {
		workers.Go(func() { first.lookupDashboardLogo(context.Background()) })
	}
	workers.Wait()
	second.lookupDashboardLogo(context.Background())
	if registryRequests.Load() != 1 || logoRequests.Load() != 1 {
		t.Fatalf("registry requests = %d, logo requests = %d", registryRequests.Load(), logoRequests.Load())
	}
	c.HideLogs = true
	for _, cc := range []*ChainConfig{first, second} {
		if !cc.dashboardStatus().Logo || !bytes.Equal(c.dashboardLogo(cc.name), pngData) {
			t.Fatalf("missing shared logo for %s", cc.name)
		}
	}
	if c.dashboardLogo("Unknown") != nil || (&ChainConfig{ChainId: "uncached"}).dashboardStatus().Logo {
		t.Fatal("unknown chain has a logo")
	}
}

func TestDashboardLogoRetriesWhileRegistryDown(t *testing.T) {
	const id = "dashboard-logo-down-1"
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer server.Close()
	useRegistries(t, server.URL+"/", unreachableRegistry)
	pathMux.Lock()
	cosmosPaths[id] = registryPath{path: "down"}
	pathMux.Unlock()
	t.Cleanup(func() { dashboardLogoLookups.Delete(id) })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { (&ChainConfig{ChainId: id}).lookupDashboardLogo(ctx); close(done) }()
	for requests.Load() == 0 {
		runtime.Gosched()
	}
	cancel()
	<-done
	// stopped while waiting: a restarted chain looks again
	if _, started := dashboardLogoLookups.Load(id); started {
		t.Fatal("lookup still marked after the chain stopped")
	}
}

func TestDashboardLogoFailureIsNotRetried(t *testing.T) {
	const id = "dashboard-logo-failure-1"
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.NotFound(w, r)
	}))
	defer server.Close()
	useRegistries(t, server.URL+"/", unreachableRegistry)
	pathMux.Lock()
	cosmosPaths[id] = registryPath{path: "missing"}
	pathMux.Unlock()
	t.Cleanup(func() { dashboardLogoLookups.Delete(id) })
	cc := &ChainConfig{ChainId: id}
	cc.lookupDashboardLogo(context.Background())
	cc.lookupDashboardLogo(context.Background())
	if requests.Load() != 1 || cachedDashboardLogo(id) != nil {
		t.Fatal("failed lookup was retried or cached")
	}
}
