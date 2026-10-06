package tenderduty

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReloadKeepsFibreTokenWhileTheListenerIsOpen(t *testing.T) {
	c := auditConfig(t)
	chain := reloadTestChain()
	c.Chains["Chain"] = chain
	c.FibreMetrics = FibreMetricsConfig{Listen: "0.0.0.0:4318", Token: "secret"}
	dir := t.TempDir()
	filename, chains := filepath.Join(dir, "config.yml"), filepath.Join(dir, "chains.d")
	write := func(fibre string) {
		t.Helper()
		config := "fibre_metrics:\n" + fibre + "chains:\n  Chain:\n    chain_id: chain-1\n    valoper_address: cosmosvaloper1test\n    nodes:\n      - url: http://localhost:26657\n        alert_if_down: yes\n"
		if err := os.WriteFile(filename, []byte(config), 0600); err != nil {
			t.Fatal(err)
		}
	}
	password := ""
	handler := c.fibreMetricsHandler()
	unauthorized := func() int {
		request := httptest.NewRequest(http.MethodPost, "/v1/metrics", strings.NewReader(""))
		request.Header.Set("Content-Type", "application/x-protobuf")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response.Code
	}

	// the running listener stays on 0.0.0.0 and still needs a token
	write("  listen: 127.0.0.1:4318\n")
	err := c.reload(filename, chains, &password)
	c.workers.Wait()
	if err == nil || !strings.Contains(err.Error(), "Not reloaded: fibre_metrics.listen changes only on restart") {
		t.Fatalf("a reload that drops the token was accepted: %v", err)
	}
	if c.liveSettings != nil || c.settings().FibreMetrics.Token != "secret" || c.Chains["Chain"] != chain {
		t.Fatal("the refused reload changed the running config")
	}
	if got := unauthorized(); got != http.StatusUnauthorized {
		t.Fatalf("the listener answered %d without the token after a refused reload", got)
	}

	// token changes apply on reload
	write("  listen: 127.0.0.1:4318\n  token: other\n")
	if err := c.reload(filename, chains, &password); err != nil {
		t.Fatal(err)
	}
	s := c.settings()
	if s.FibreMetrics.Token != "other" || s.FibreMetrics.Listen != "0.0.0.0:4318" {
		t.Fatalf("token %q, listen %q", s.FibreMetrics.Token, s.FibreMetrics.Listen)
	}

	write("  listen: 0.0.0.0:4318\n  allow_without_token: yes\n")
	if err := c.reload(filename, chains, &password); err != nil {
		t.Fatalf("dropping the token with allow_without_token was refused: %v", err)
	}
	s = c.settings()
	if s.FibreMetrics.Token != "" || !s.FibreMetrics.AllowWithoutToken || s.FibreMetrics.Listen != "0.0.0.0:4318" {
		t.Fatalf("reloaded settings: %+v", s.FibreMetrics)
	}
	if got := unauthorized(); got != http.StatusOK {
		t.Fatalf("the existing handler answered %d without a token after the allowed reload, want 200", got)
	}

	write("  listen: 127.0.0.1:4318\n")
	if err := c.reload(filename, chains, &password); err == nil {
		t.Fatal("the old allow_without_token allowed a reload without the new opt-in")
	}
	if c.settings() != s {
		t.Fatal("the refused reload changed the running settings")
	}
}

func TestReloadMayDropFibreTokenOnLoopback(t *testing.T) {
	old := &Config{FibreMetrics: FibreMetricsConfig{Listen: "127.0.0.1:4318", Token: "secret"}}
	for name, tc := range map[string]struct {
		old     FibreMetricsConfig
		token   string
		refused bool
	}{
		"loopback listener":    {FibreMetricsConfig{Listen: "127.0.0.1:4318"}, "", false},
		"localhost":            {FibreMetricsConfig{Listen: "localhost:4318"}, "", false},
		"receiver off":         {FibreMetricsConfig{}, "", false},
		"everywhere":           {FibreMetricsConfig{Listen: ":4318", Token: "secret"}, "", true},
		"private address":      {FibreMetricsConfig{Listen: "10.0.0.5:4318", Token: "secret"}, "", true},
		"everywhere, kept":     {FibreMetricsConfig{Listen: ":4318", Token: "secret"}, "new", false},
		"unparsable listen":    {FibreMetricsConfig{Listen: "nonsense", Token: "secret"}, "", true},
		"ipv6 everywhere":      {FibreMetricsConfig{Listen: "[::]:4318", Token: "secret"}, "", true},
		"ipv6 loopback, empty": {FibreMetricsConfig{Listen: "[::1]:4318"}, "", false},
	} {
		old.FibreMetrics = tc.old
		next := &Config{FibreMetrics: FibreMetricsConfig{Listen: "127.0.0.1:4318", Token: tc.token}}
		err := fibreTokenKept(old, next)
		if (err != nil) != tc.refused {
			t.Errorf("%s: error %v, refused want %v", name, err, tc.refused)
		}
		if err != nil {
			want := fmt.Sprintf("Not reloaded: fibre_metrics.listen changes only on restart, and dropping the token now would leave %s open without one. Restart tenderduty to apply.", tc.old.Listen)
			if err.Error() != want {
				t.Errorf("%s: message %q, want %q", name, err, want)
			}
		}
	}
}
