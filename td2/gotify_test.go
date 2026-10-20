package tenderduty

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

const gotifyTestToken = "A1b2-gotify-app-token"

type gotifyRequest struct {
	path, key, query string
	msg              GotifyMessage
}

// newGotifyServer records each POST /message and answers with status.
func newGotifyServer(t *testing.T, status int) (*httptest.Server, *[]gotifyRequest) {
	t.Helper()
	var got []gotifyRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m GotifyMessage
		_ = json.NewDecoder(r.Body).Decode(&m)
		got = append(got, gotifyRequest{path: r.URL.Path, key: r.Header.Get("X-Gotify-Key"), query: r.URL.RawQuery, msg: m})
		w.WriteHeader(status)
		// echoes the token back, the sender must not pass it on
		_, _ = fmt.Fprintf(w, "rejected %s", r.Header.Get("X-Gotify-Key"))
	}))
	t.Cleanup(server.Close)
	return server, &got
}

func gotifyChain(name string) *ChainConfig {
	return &ChainConfig{name: name, ChainId: strings.ToLower(name) + "-1", ValAddress: "valoper", PublicFallback: true, observedBlock: true}
}

func TestGotifySendsAlertToMessageEndpoint(t *testing.T) {
	auditConfig(t)
	server, got := newGotifyServer(t, http.StatusOK)
	msg := &alertMsg{chain: "Devnet1", severity: "critical", message: "missed blocks", gotify: true, gotifyURL: server.URL + "/", gotifyToken: gotifyTestToken}
	if err := notifyGotify(msg); err != nil {
		t.Fatal(err)
	}
	if len(*got) != 1 {
		t.Fatalf("%d requests sent", len(*got))
	}
	req := (*got)[0]
	if req.path != "/message" || req.key != gotifyTestToken || req.query != "" {
		t.Fatalf("request %+v", req)
	}
	if !strings.Contains(req.msg.Title, "Devnet1") || req.msg.Message == "" || req.msg.Priority != 8 {
		t.Fatalf("body %+v", req.msg)
	}
}

func TestGotifyPriorityBySeverity(t *testing.T) {
	for _, tc := range []struct {
		severity string
		resolved bool
		want     int
	}{
		{"critical", false, 8}, {"error", false, 7}, {"warning", false, 5}, {"info", false, 2}, {"critical", true, 2},
	} {
		msg := &alertMsg{chain: "Devnet1", severity: tc.severity, resolved: tc.resolved, message: "missed blocks"}
		if got := buildGotifyMessage(msg).Priority; got != tc.want {
			t.Fatalf("%s resolved=%v: priority %d, want %d", tc.severity, tc.resolved, got, tc.want)
		}
	}
}

func TestGotifyErrorHidesToken(t *testing.T) {
	auditConfig(t)
	server, _ := newGotifyServer(t, http.StatusInternalServerError)
	msg := &alertMsg{chain: "Devnet1", severity: "critical", message: "missed blocks", gotify: true, gotifyURL: server.URL, gotifyToken: gotifyTestToken}
	err := notifyGotify(msg)
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("error %v", err)
	}
	if strings.Contains(err.Error(), gotifyTestToken) {
		t.Fatalf("the error leaks the token: %v", err)
	}
}

func TestGotifySkipsDisabledAndBelowMinSeverity(t *testing.T) {
	c := auditConfig(t)
	server, got := newGotifyServer(t, http.StatusOK)
	cc := gotifyChain("Devnet1")
	c.Chains[cc.name] = cc
	c.Gotify = GotifyConfig{Enabled: yes(true), URL: server.URL, Token: gotifyTestToken}
	cc.Alerts.Gotify.MinSeverity = "critical"
	if msg := c.makeAlert("Devnet1", "missed blocks", "warning", false, nil); msg == nil || msg.gotify {
		t.Fatalf("a warning reached gotify below min_severity: %+v", msg)
	} else if err := notifyGotify(msg); err != nil || len(*got) != 0 {
		t.Fatalf("sent %d, err %v", len(*got), err)
	}
	c.Gotify.Enabled = nil
	if msg := c.makeAlert("Devnet1", "missed blocks", "critical", false, nil); msg == nil || msg.gotify {
		t.Fatalf("disabled gotify still routes: %+v", msg)
	}
	c.Gotify.Enabled = yes(true)
	if msg := c.makeAlert("Devnet1", "missed blocks", "critical", false, nil); msg == nil || !msg.gotify {
		t.Fatalf("critical alert not routed to gotify: %+v", msg)
	}
}

func TestGotifyConfigValidation(t *testing.T) {
	c := auditConfig(t)
	bad := gotifyChain("Bad")
	bad.Alerts.Gotify = GotifyConfig{URL: "gotify.example", Token: gotifyTestToken}
	noToken := gotifyChain("NoToken")
	noToken.Alerts.Gotify = GotifyConfig{URL: "https://gotify.example"}
	c.Chains[bad.name], c.Chains[noToken.name] = bad, noToken
	c.Gotify = GotifyConfig{Enabled: yes(true)}
	if fatal, problems := validateConfig(c); fatal {
		t.Fatalf("fatal: %v", problems)
	} else if n := strings.Count(strings.Join(problems, "\n"), "gotify alerts need"); n != 2 {
		t.Fatalf("%d gotify warnings, want 2: %v", n, problems)
	}
	if enabledFor(c.Gotify.Enabled, bad.Alerts.Gotify.Enabled) || enabledFor(c.Gotify.Enabled, noToken.Alerts.Gotify.Enabled) {
		t.Fatal("gotify stayed on for a chain without a valid URL and token")
	}

	// the shared URL and token fill in, a chain can still override them
	good := gotifyChain("Good")
	good.Alerts.Gotify = GotifyConfig{URL: "https://other.example", Token: "other-token"}
	c.Chains[good.name] = good
	if fatal, problems := validateConfig(c); fatal {
		t.Fatalf("fatal: %v", problems)
	} else if strings.Contains(strings.Join(problems, "\n"), "gotify alerts need") {
		t.Fatalf("valid override warned: %v", problems)
	}
	if !enabledFor(c.Gotify.Enabled, good.Alerts.Gotify.Enabled) || good.Alerts.Gotify.Token != "other-token" {
		t.Fatalf("chain override lost: %+v", good.Alerts.Gotify)
	}
}

func TestGotifySecretIsMaskedInConfigShow(t *testing.T) {
	options, _, _ := chainCommandFixture(t)
	config := "gotify:\n  enabled: yes\n  url: https://gotify.example\n  token: gotify-secret\n"
	if err := os.WriteFile(options.ConfigFile, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	if err := ConfigCommand(context.Background(), options, []string{"show", "gotify"}, strings.NewReader(""), &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "token: ***") || strings.Contains(output.String(), "gotify-secret") {
		t.Fatalf("show: %s", output.String())
	}
}

func TestGotifyReportsAreNotSent(t *testing.T) {
	c := auditConfig(t)
	c.Gotify = GotifyConfig{Enabled: yes(true), URL: "https://gotify.example", Token: gotifyTestToken}
	cc := gotifyChain("Devnet1")
	cc.Alerts.Gotify = GotifyConfig{URL: "https://gotify.example", Token: gotifyTestToken}
	c.Chains[cc.name] = cc
	if destinations := c.reportDestinations(); len(destinations) != 0 {
		t.Fatalf("reports go to %+v", destinations)
	}
}
