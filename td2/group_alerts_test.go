package tenderduty

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	dash "github.com/kynraze/tenderduty/v2/td2/dashboard"
)

// redirectTransport sends telegram and pagerduty requests to the test server.
type redirectTransport struct {
	base   http.RoundTripper
	target *url.URL
}

func (r redirectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Host == "api.telegram.org" || req.URL.Host == "events.pagerduty.com" {
		clone := req.Clone(req.Context())
		clone.URL.Scheme, clone.URL.Host, clone.Host = r.target.Scheme, r.target.Host, r.target.Host
		req = clone
	}
	return r.base.RoundTrip(req)
}

type sentTelegram struct{ chat, text string }

type pagerdutyEvent struct{ Action, Key string }

type groupHarness struct {
	t      *testing.T
	c      *Config
	server *httptest.Server

	mux      sync.Mutex
	telegram []sentTelegram
	pd       []pagerdutyEvent
	discord  []DiscordMessage
	slack    []SlackMessage
	failTg   bool
	lookups  atomic.Int32
}

const (
	groupUnit       = 100 * time.Millisecond
	testToken       = "123:test"
	hostAIP         = "203.0.113.5"
	hostBIP         = "198.51.100.7"
	testNodeDownMin = 3
)

func newGroupHarness(t *testing.T, dns map[string]string) *groupHarness {
	t.Helper()
	h := &groupHarness{t: t}
	h.server = httptest.NewServer(http.HandlerFunc(h.serve))
	target, _ := url.Parse(h.server.URL)
	oldTransport, oldUnit, oldLookup := http.DefaultTransport, groupWaitUnit, lookupIPs
	// runs after auditConfig's cleanup stops the workers
	t.Cleanup(func() {
		http.DefaultTransport, groupWaitUnit, lookupIPs = oldTransport, oldUnit, oldLookup
		clearServerCache()
		h.server.Close()
	})
	http.DefaultTransport = redirectTransport{base: oldTransport, target: target}
	groupWaitUnit = groupUnit
	clearServerCache()
	lookupIPs = func(_ context.Context, host string) ([]net.IPAddr, error) {
		h.lookups.Add(1)
		ip, ok := dns[host]
		if !ok {
			return nil, errors.New("no such host")
		}
		return []net.IPAddr{{IP: net.ParseIP(ip)}}, nil
	}
	h.c = auditConfig(t)
	h.c.NodeDownMin, h.c.NodeDownSeverity = testNodeDownMin, "critical"
	h.c.GroupNodeAlerts, h.c.GroupWaitSeconds = true, 2
	h.c.Telegram.Enabled = yes(true)
	h.c.stateFile = filepath.Join(t.TempDir(), "state.json")
	return h
}

func clearServerCache() {
	serverCacheMux.Lock()
	serverCache = make(map[string]serverEntry)
	serverCacheMux.Unlock()
}

func (h *groupHarness) serve(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasSuffix(r.URL.Path, "/getMe"):
		_, _ = w.Write([]byte(`{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"td","username":"td"}}`))
	case strings.HasSuffix(r.URL.Path, "/sendMessage"):
		_ = r.ParseForm()
		h.mux.Lock()
		fail := h.failTg
		if !fail {
			h.telegram = append(h.telegram, sentTelegram{chat: r.Form.Get("chat_id"), text: r.Form.Get("text")})
		}
		h.mux.Unlock()
		if fail {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1,"date":1,"chat":{"id":1,"type":"channel"}}}`))
	case r.URL.Path == "/v2/enqueue":
		var event struct {
			Action string `json:"event_action"`
			Key    string `json:"dedup_key"`
		}
		_ = json.NewDecoder(r.Body).Decode(&event)
		h.mux.Lock()
		h.pd = append(h.pd, pagerdutyEvent{event.Action, event.Key})
		h.mux.Unlock()
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"status":"success","message":"ok","dedup_key":"x"}`))
	case r.URL.Path == "/discord":
		var msg DiscordMessage
		_ = json.NewDecoder(r.Body).Decode(&msg)
		h.mux.Lock()
		h.discord = append(h.discord, msg)
		h.mux.Unlock()
		w.WriteHeader(http.StatusNoContent)
	case r.URL.Path == "/slack":
		var msg SlackMessage
		_ = json.NewDecoder(r.Body).Decode(&msg)
		h.mux.Lock()
		h.slack = append(h.slack, msg)
		h.mux.Unlock()
	default:
		http.NotFound(w, r)
	}
}

func (h *groupHarness) chain(name, nodeURL, channel string) *ChainConfig {
	cc := &ChainConfig{name: name, ChainId: strings.ToLower(name) + "-1", ValAddress: strings.ToLower(name) + "valoper",
		Nodes: []*NodeConfig{{Url: nodeURL, AlertIfDown: true}}}
	cc.Alerts.Telegram = TeleConfig{ApiKey: testToken, Channel: channel}
	h.c.Chains[name] = cc
	return cc
}

func (h *groupHarness) withPagerduty() {
	h.c.Pagerduty.Enabled = yes(true)
	for _, cc := range h.c.Chains {
		cc.Alerts.Pagerduty.ApiKey = "pagerdutykey"
	}
}

func (h *groupHarness) withDiscordAndSlack() {
	h.c.Discord.Enabled, h.c.Slack.Enabled = yes(true), yes(true)
	for _, cc := range h.c.Chains {
		cc.Alerts.Discord.Webhook = h.server.URL + "/discord"
		cc.Alerts.Slack.Webhook = h.server.URL + "/slack"
	}
}

func (h *groupHarness) run() { h.c.startWorker(h.c.runNotifications) }

func (h *groupHarness) down(name string) {
	cc := h.c.Chains[name]
	id := cc.Nodes[0].Url
	h.c.alert(name, nodeDownMessage(id, cc.ChainId), "critical", false, &id)
}

func (h *groupHarness) up(name string) {
	cc := h.c.Chains[name]
	id := cc.Nodes[0].Url
	h.c.alert(name, nodeDownMessage(id, cc.ChainId), "info", true, &id)
}

func (h *groupHarness) telegramSent() []sentTelegram {
	h.mux.Lock()
	defer h.mux.Unlock()
	sent := append([]sentTelegram(nil), h.telegram...)
	sort.Slice(sent, func(i, j int) bool { return sent[i].text < sent[j].text })
	return sent
}

func (h *groupHarness) pagerduty() (triggers, resolves []string) {
	h.mux.Lock()
	defer h.mux.Unlock()
	for _, event := range h.pd {
		if event.Action == "resolve" {
			resolves = append(resolves, event.Key)
		} else {
			triggers = append(triggers, event.Key)
		}
	}
	return
}

func (h *groupHarness) waitFor(what string, ok func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			h.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (h *groupHarness) waitTelegram(n int) []sentTelegram {
	h.t.Helper()
	h.waitFor("telegram messages", func() bool { return len(h.telegramSent()) >= n })
	time.Sleep(4 * groupUnit)
	sent := h.telegramSent()
	if len(sent) != n {
		h.t.Fatalf("%d telegram messages, want %d: %+v", len(sent), n, sent)
	}
	return sent
}

func (h *groupHarness) drained() bool {
	alarms.notifyMux.RLock()
	defer alarms.notifyMux.RUnlock()
	return len(alarms.Outbox) == 0
}

func contains(t *testing.T, text string, want ...string) {
	t.Helper()
	for _, s := range want {
		if !strings.Contains(text, s) {
			t.Errorf("message lacks %q:\n%s", s, text)
		}
	}
}

func TestGroupedNodesOfOneServerAreOneMessage(t *testing.T) {
	h := newGroupHarness(t, map[string]string{"rpc-a": hostAIP, "10.0.0.5": hostAIP})
	h.chain("AtomOne", "https://rpc-a:443", "@one")
	h.chain("Realio", "http://10.0.0.5:26657", "@one")
	h.c.Chains["Realio"].ExtraInfo = "see runbook"
	h.c.Chains["AtomOne"].ExtraInfo = "see runbook"
	h.run()
	started := time.Now()
	h.down("AtomOne")
	h.down("Realio")
	sent := h.waitTelegram(1)
	if time.Since(started) < 3*groupUnit {
		t.Errorf("sent after %v, before the group wait", time.Since(started))
	}
	contains(t, sent[0].text, "<b>Server "+hostAIP+"</b> · 2 nodes down",
		"<b>AtomOne:</b> <code>https://rpc-a:443</code>", "<b>Realio:</b> <code>http://10.0.0.5:26657</code>",
		"down for &gt; 3 minutes", "<i>see runbook</i>")
	if sent[0].chat != "@one" {
		t.Errorf("sent to %q", sent[0].chat)
	}
	if strings.Contains(sent[0].text, testToken) {
		t.Error("the message holds the token")
	}
	h.waitFor("the outbox to drain", h.drained)
	alarms.notifyMux.RLock()
	defer alarms.notifyMux.RUnlock()
	if len(alarms.SentTgAlarms) != 2 || len(alarms.inFlight[tg]) != 0 {
		t.Errorf("sent %v, in flight %v", alarms.SentTgAlarms, alarms.inFlight[tg])
	}
	if len(alarms.Groups) != 1 {
		t.Fatalf("groups %+v", alarms.Groups)
	}
	for _, group := range alarms.Groups {
		if group.Server != hostAIP || len(group.Members) != 2 || group.Destination != tg {
			t.Errorf("group %+v", group)
		}
	}
}

func TestLoneNodeGetsItsOwnCardAfterTheWait(t *testing.T) {
	h := newGroupHarness(t, map[string]string{"rpc-a": hostAIP})
	h.chain("AtomOne", "https://rpc-a:443", "@one")
	h.run()
	started := time.Now()
	h.down("AtomOne")
	sent := h.waitTelegram(1)
	if time.Since(started) < 3*groupUnit {
		t.Errorf("sent after %v, before the group wait", time.Since(started))
	}
	contains(t, sent[0].text, "<b>AtomOne</b>", "<b>Node:</b> <code>https://rpc-a:443</code>")
	if strings.Contains(sent[0].text, "Server") {
		t.Errorf("a lone node got a group card:\n%s", sent[0].text)
	}
	h.waitFor("the outbox to drain", h.drained)
	alarms.notifyMux.RLock()
	defer alarms.notifyMux.RUnlock()
	if len(alarms.Groups) != 0 {
		t.Errorf("groups %+v", alarms.Groups)
	}
}

func TestTwoServersAreTwoMessages(t *testing.T) {
	h := newGroupHarness(t, map[string]string{"a1": hostAIP, "a2": hostAIP, "b1": hostBIP, "b2": hostBIP})
	h.chain("AlphaOne", "http://a1:26657", "@one")
	h.chain("AlphaTwo", "http://a2:26657", "@one")
	h.chain("BetaOne", "http://b1:26657", "@one")
	h.chain("BetaTwo", "http://b2:26657", "@one")
	// State writes can outlast the group window; queue every node before workers start.
	for _, name := range []string{"AlphaOne", "BetaOne", "AlphaTwo", "BetaTwo"} {
		h.down(name)
	}
	h.run()
	sent := h.waitTelegram(2)
	var alpha, beta string
	for _, s := range sent {
		switch {
		case strings.Contains(s.text, hostAIP):
			alpha = s.text
		case strings.Contains(s.text, hostBIP):
			beta = s.text
		}
	}
	contains(t, alpha, "Server "+hostAIP, "2 nodes down", "AlphaOne", "AlphaTwo")
	contains(t, beta, "Server "+hostBIP, "2 nodes down", "BetaOne", "BetaTwo")
	if strings.Contains(alpha, "Beta") || strings.Contains(beta, "Alpha") {
		t.Errorf("servers were mixed:\n%s\n%s", alpha, beta)
	}
}

func TestOneServerTwoChannelsIsOneMessageEach(t *testing.T) {
	h := newGroupHarness(t, map[string]string{"a1": hostAIP, "a2": hostAIP})
	h.chain("AlphaOne", "http://a1:26657", "@one")
	h.chain("AlphaTwo", "http://a2:26657", "@two")
	h.run()
	h.down("AlphaOne")
	h.down("AlphaTwo")
	sent := h.waitTelegram(2)
	chats := map[string]string{sent[0].chat: sent[0].text, sent[1].chat: sent[1].text}
	if chats["@one"] == "" || chats["@two"] == "" {
		t.Fatalf("messages went to %v", chats)
	}
	contains(t, chats["@one"], "AlphaOne")
	contains(t, chats["@two"], "AlphaTwo")
	for _, text := range chats {
		if strings.Contains(text, "Server") {
			t.Errorf("a channel with one node got a group card:\n%s", text)
		}
	}
}

func TestServerLabelGroupsNodesOnDifferentIPs(t *testing.T) {
	// the labelled nodes are never looked up
	h := newGroupHarness(t, map[string]string{})
	h.chain("AlphaOne", "http://a1:26657", "@one").Nodes[0].Server = "hetzner-1"
	h.chain("AlphaTwo", "http://other-host:26657", "@one").Nodes[0].Server = "hetzner-1"
	h.chain("Elsewhere", "http://c1:26657", "@one").Nodes[0].Server = "ovh-2"
	h.run()
	h.down("AlphaOne")
	h.down("AlphaTwo")
	h.down("Elsewhere")
	sent := h.waitTelegram(2)
	var grouped, single string
	for _, s := range sent {
		if strings.Contains(s.text, "Server") {
			grouped = s.text
		} else {
			single = s.text
		}
	}
	contains(t, grouped, "<b>Server hetzner-1</b> · 2 nodes down", "AlphaOne", "AlphaTwo")
	contains(t, single, "<b>Elsewhere</b>")
	if h.lookups.Load() != 0 {
		t.Errorf("%d lookups for labelled nodes", h.lookups.Load())
	}
}

func TestGroupedRecoveryIsQuietUntilTheLastNode(t *testing.T) {
	h := newGroupHarness(t, map[string]string{"a1": hostAIP, "a2": hostAIP})
	h.chain("AlphaOne", "http://a1:26657", "@one")
	h.chain("AlphaTwo", "http://a2:26657", "@one")
	h.withPagerduty()
	h.run()
	h.down("AlphaOne")
	h.down("AlphaTwo")
	h.waitTelegram(1)
	h.waitFor("pagerduty triggers", func() bool { triggers, _ := h.pagerduty(); return len(triggers) == 2 })
	triggers, _ := h.pagerduty()
	sort.Strings(triggers)
	if triggers[0] != "http://a1:26657" || triggers[1] != "http://a2:26657" {
		t.Fatalf("pagerduty triggers %v", triggers)
	}

	h.up("AlphaOne")
	h.waitFor("the first pagerduty resolve", func() bool { _, resolves := h.pagerduty(); return len(resolves) == 1 })
	h.waitFor("the outbox to drain", h.drained)
	if sent := h.telegramSent(); len(sent) != 1 {
		t.Fatalf("the first recovery was announced: %+v", sent)
	}
	alarms.notifyMux.RLock()
	remaining := len(alarms.SentTgAlarms)
	alarms.notifyMux.RUnlock()
	if remaining != 1 {
		t.Fatalf("%d alarms still sent to telegram after the first recovery", remaining)
	}

	h.up("AlphaTwo")
	sent := h.waitTelegram(2)
	var back string
	for _, s := range sent {
		if strings.Contains(s.text, "Back up") {
			back = s.text
		}
	}
	contains(t, back, "✅ <b>Server "+hostAIP+"</b> · Back up", "<b>AlphaOne:</b> <code>http://a1:26657</code>",
		"<b>AlphaTwo:</b> <code>http://a2:26657</code>", "all nodes back up", "Lasted")
	h.waitFor("the second pagerduty resolve", func() bool { _, resolves := h.pagerduty(); return len(resolves) == 2 })
	h.waitFor("the outbox to drain", h.drained)
	alarms.notifyMux.RLock()
	defer alarms.notifyMux.RUnlock()
	if len(alarms.Groups) != 0 || len(alarms.SentTgAlarms) != 0 || len(alarms.SentPdAlarms) != 0 {
		t.Errorf("groups %+v, sent to telegram %v, to pagerduty %v", alarms.Groups, alarms.SentTgAlarms, alarms.SentPdAlarms)
	}
}

func TestNodesRecoveringTogetherAreOneMessage(t *testing.T) {
	h := newGroupHarness(t, map[string]string{"a1": hostAIP, "a2": hostAIP, "a3": hostAIP})
	h.chain("AlphaOne", "http://a1:26657", "@one")
	h.chain("AlphaTwo", "http://a2:26657", "@one")
	h.chain("AlphaThree", "http://a3:26657", "@one")
	h.run()
	h.down("AlphaOne")
	h.down("AlphaTwo")
	h.down("AlphaThree")
	h.waitTelegram(1)
	h.waitFor("the outbox to drain", h.drained)
	h.up("AlphaOne")
	h.up("AlphaTwo")
	h.up("AlphaThree")
	sent := h.waitTelegram(2)
	backs := 0
	for _, s := range sent {
		if strings.Contains(s.text, "Back up") {
			backs++
			contains(t, s.text, "AlphaOne", "AlphaTwo", "AlphaThree")
		}
	}
	if backs != 1 {
		t.Fatalf("%d recovery messages: %+v", backs, sent)
	}
	h.waitFor("the outbox to drain", h.drained)
}

func TestGroupSurvivesARestart(t *testing.T) {
	auditConfig(t)
	dir := t.TempDir()
	configFile, stateFile := filepath.Join(dir, "config.yml"), filepath.Join(dir, "state.json")
	config := `node_down_alert_minutes: 3
group_node_alerts: yes
group_wait_seconds: 10
telegram:
  enabled: yes
  api_key: "` + testToken + `"
  channel: "@one"
chains:
  AlphaOne:
    chain_id: alphaone-1
    valoper_address: alphaonevaloper
    nodes:
      - url: http://a1:26657
        alert_if_down: yes
  AlphaTwo:
    chain_id: alphatwo-1
    valoper_address: alphatwovaloper
    nodes:
      - url: http://a2:26657
        alert_if_down: yes
`
	if err := os.WriteFile(configFile, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	h := &groupHarness{t: t}
	h.server = httptest.NewServer(http.HandlerFunc(h.serve))
	target, _ := url.Parse(h.server.URL)
	oldTransport, oldUnit, oldLookup := http.DefaultTransport, groupWaitUnit, lookupIPs
	var current *Config
	t.Cleanup(func() {
		if current != nil {
			current.cancel()
			current.deliveryCancel()
			current.workers.Wait()
		}
		http.DefaultTransport, groupWaitUnit, lookupIPs = oldTransport, oldUnit, oldLookup
		clearServerCache()
		h.server.Close()
	})
	http.DefaultTransport = redirectTransport{base: oldTransport, target: target}
	groupWaitUnit = 10 * time.Millisecond
	clearServerCache()
	lookupIPs = func(_ context.Context, host string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP(hostAIP)}}, nil
	}
	start := func() *Config {
		password := ""
		c, err := loadConfig(configFile, stateFile, filepath.Join(dir, "chains.d"), &password)
		if err != nil {
			t.Fatal(err)
		}
		if fatal, problems := validateConfig(c); fatal {
			t.Fatal(problems)
		}
		td, current = c, c
		h.c = c
		c.startWorker(c.runNotifications)
		return c
	}
	stop := func(c *Config) {
		c.cancel()
		c.deliveryCancel()
		c.workers.Wait()
		current = nil
	}

	c := start()
	h.down("AlphaOne")
	h.down("AlphaTwo")
	h.waitTelegram(1)
	h.waitFor("the outbox to drain", h.drained)
	alarms.notifyMux.Lock()
	for _, group := range alarms.Groups {
		group.Members[0].Raised = time.Now().Add(-12 * time.Minute)
		group.Members[1].Raised = time.Now().Add(-5 * time.Minute)
	}
	alarms.notifyMux.Unlock()
	if err := c.persistState(); err != nil {
		t.Fatal(err)
	}
	stop(c)

	c = start()
	alarms.notifyMux.RLock()
	groups := len(alarms.Groups)
	alarms.notifyMux.RUnlock()
	if groups != 1 {
		t.Fatalf("%d groups restored", groups)
	}
	h.up("AlphaOne")
	h.waitFor("the outbox to drain", h.drained)
	if sent := h.telegramSent(); len(sent) != 1 {
		t.Fatalf("a recovery after the restart was announced alone: %+v", sent)
	}
	h.up("AlphaTwo")
	sent := h.waitTelegram(2)
	var back string
	for _, s := range sent {
		if strings.Contains(s.text, "Back up") {
			back = s.text
		}
	}
	contains(t, back, "Server "+hostAIP, "Back up", "AlphaOne", "AlphaTwo", "Lasted:</b> 12m")
	h.waitFor("the outbox to drain", h.drained)
}

func TestGroupingOffSendsEveryNodeAtOnce(t *testing.T) {
	h := newGroupHarness(t, map[string]string{"a1": hostAIP, "a2": hostAIP})
	h.c.GroupNodeAlerts, h.c.GroupWaitSeconds = false, 600
	h.chain("AlphaOne", "http://a1:26657", "@one")
	h.chain("AlphaTwo", "http://a2:26657", "@one")
	h.run()
	started := time.Now()
	h.down("AlphaOne")
	h.down("AlphaTwo")
	h.waitFor("both messages", func() bool { return len(h.telegramSent()) == 2 })
	if time.Since(started) > 3*time.Second {
		t.Errorf("took %v with grouping off", time.Since(started))
	}
	for _, s := range h.telegramSent() {
		if strings.Contains(s.text, "Server") {
			t.Errorf("grouping is off:\n%s", s.text)
		}
	}
	if h.lookups.Load() != 0 {
		t.Errorf("%d lookups with grouping off", h.lookups.Load())
	}
}

func TestDiscordAndSlackGetOneMessageToo(t *testing.T) {
	h := newGroupHarness(t, map[string]string{"a1": hostAIP, "a2": hostAIP})
	h.c.Telegram.Enabled = yes(false)
	h.chain("AlphaOne", "http://a1:26657", "@one")
	h.chain("AlphaTwo", "http://a2:26657", "@one")
	h.withDiscordAndSlack()
	h.run()
	h.down("AlphaOne")
	h.down("AlphaTwo")
	h.waitFor("discord and slack", func() bool {
		h.mux.Lock()
		defer h.mux.Unlock()
		return len(h.discord) >= 1 && len(h.slack) >= 1
	})
	time.Sleep(4 * groupUnit)
	h.mux.Lock()
	defer h.mux.Unlock()
	if len(h.discord) != 1 || len(h.slack) != 1 {
		t.Fatalf("%d discord and %d slack messages", len(h.discord), len(h.slack))
	}
	d := h.discord[0]
	contains(t, d.Content, "Server "+hostAIP, "2 nodes down")
	if len(d.Embeds) != 1 || len(d.Embeds[0].Fields) != 3 {
		t.Fatalf("discord embed %+v", d.Embeds)
	}
	s := h.slack[0]
	contains(t, s.Text, "Server "+hostAIP)
	if len(s.Attachments) != 1 || len(s.Attachments[0].Fields) != 3 || !strings.Contains(s.Attachments[0].Title, "2 nodes down") {
		t.Fatalf("slack message %+v", s)
	}
}

func TestFailedGroupMessageReleasesEveryNode(t *testing.T) {
	h := newGroupHarness(t, map[string]string{"a1": hostAIP, "a2": hostAIP})
	h.failTg = true
	h.chain("AlphaOne", "http://a1:26657", "@one")
	h.chain("AlphaTwo", "http://a2:26657", "@one")
	h.run()
	h.down("AlphaOne")
	h.down("AlphaTwo")
	time.Sleep(6 * groupUnit)
	alarms.notifyMux.RLock()
	outbox, sent, flying, groups := len(alarms.Outbox), len(alarms.SentTgAlarms), len(alarms.inFlight[tg]), len(alarms.Groups)
	alarms.notifyMux.RUnlock()
	if outbox != 2 || sent != 0 || flying != 0 || groups != 0 {
		t.Fatalf("after the failure: outbox %d, sent %d, in flight %d, groups %d", outbox, sent, flying, groups)
	}
	h.mux.Lock()
	h.failTg = false
	h.mux.Unlock()
	h.waitFor("the retry", func() bool { return len(h.telegramSent()) >= 1 })
	sent2 := h.waitTelegram(1)
	contains(t, sent2[0].text, "2 nodes down")
	h.waitFor("the outbox to drain", h.drained)
}

func TestServerResolution(t *testing.T) {
	var calls atomic.Int32
	old := lookupIPs
	defer func() { lookupIPs = old; clearServerCache() }()
	clearServerCache()
	lookupIPs = func(_ context.Context, host string) ([]net.IPAddr, error) {
		calls.Add(1)
		switch host {
		case "multi":
			return []net.IPAddr{{IP: net.ParseIP("203.0.113.9")}, {IP: net.ParseIP("10.0.0.2")}, {IP: net.ParseIP("203.0.113.1")}}, nil
		case "slow":
			return nil, context.DeadlineExceeded
		}
		return nil, errors.New("no such host")
	}
	ctx := context.Background()
	if got := resolveServer(ctx, "https://multi:443"); got != "10.0.0.2" {
		t.Errorf("lowest address: %q", got)
	}
	if got := resolveServer(ctx, "http://multi:26657/path"); got != "10.0.0.2" || calls.Load() != 1 {
		t.Errorf("cached lookup: %q after %d lookups", got, calls.Load())
	}
	if got := resolveServer(ctx, "http://gone:26657"); got != "gone" {
		t.Errorf("failed lookup: %q", got)
	}
	if got := resolveServer(ctx, "http://slow:26657"); got != "slow" {
		t.Errorf("timed out lookup: %q", got)
	}
	// retry failed DNS lookups after expiry; cache successful ones for ten minutes
	serverCacheMux.Lock()
	multi, gone := serverCache["multi"], serverCache["gone"]
	serverCacheMux.Unlock()
	if left := time.Until(multi.expires); left < 9*time.Minute || left > serverCacheTime {
		t.Errorf("a good lookup is kept for %v", left)
	}
	if left := time.Until(gone.expires); left > serverFailedTime {
		t.Errorf("a failed lookup is kept for %v", left)
	}

	c := &Config{Chains: map[string]*ChainConfig{"Chain": {Nodes: []*NodeConfig{{Url: "http://multi:26657", Server: "hetzner-1"}, {Url: "http://multi:26658"}}}}}
	before := calls.Load()
	if got := c.nodeServer("Chain", "http://multi:26657"); got != "hetzner-1" || calls.Load() != before {
		t.Errorf("label: %q after %d lookups", got, calls.Load()-before)
	}
	if got := c.nodeServer("Chain", "http://multi:26658"); got != "10.0.0.2" {
		t.Errorf("unlabelled node: %q", got)
	}
	if got := c.nodeServer("Removed", "http://multi:26658"); got != "10.0.0.2" {
		t.Errorf("node of a removed chain: %q", got)
	}
}

func TestOnlyNodeDownAlertsWait(t *testing.T) {
	c := &Config{GroupNodeAlerts: true, GroupWaitSeconds: 60}
	queued := time.Now()
	down := queuedNotification{Destination: tg, pendingRecovery: pendingRecovery{Chain: "Chain", Message: nodeDownMessage("http://a1:26657", "chain-1")}, Queued: queued}
	if d := c.groupDelay(down); d < 59*time.Second || d > 60*time.Second {
		t.Errorf("node down waits %v", d)
	}
	cases := map[string]func(*queuedNotification){
		"an old state file":       func(q *queuedNotification) { q.Queued = time.Time{} },
		"a recovery":              func(q *queuedNotification) { q.Resolved = true },
		"pagerduty":               func(q *queuedNotification) { q.Destination = pd },
		"no RPC endpoints":        func(q *queuedNotification) { q.Message = "no RPC endpoints are working for chain-1" },
		"a stalled chain":         func(q *queuedNotification) { q.Message = "stalled: have not seen a new block on chain-1 in 10 minutes" },
		"grouping turned off now": func(q *queuedNotification) { c.GroupNodeAlerts = false },
	}
	for name, change := range cases {
		item := down
		change(&item)
		if d := c.groupDelay(item); d != 0 {
			t.Errorf("%s waits %v", name, d)
		}
		c.GroupNodeAlerts = true
	}
	// old state files have no queue timestamp
	var item queuedNotification
	if err := json.Unmarshal([]byte(`{"sequence":1,"destination":1,"chain":"Chain","message":"x","resolved":false}`), &item); err != nil || !item.Queued.IsZero() {
		t.Errorf("old queue entry: %+v, %v", item, err)
	}
	alarms := newAlarmCache()
	alarms.enqueue(&alertMsg{tg: true, chain: "Chain", message: down.Message})
	if len(alarms.Outbox) != 1 || alarms.Outbox[0].Queued.IsZero() {
		t.Errorf("outbox %+v", alarms.Outbox)
	}
}

func TestServerLabelDoesNotRestartAChain(t *testing.T) {
	c := auditConfig(t)
	c.updateChan = make(chan *dash.ChainStatus, 8)
	c.statsChan = make(chan *promUpdate, 8)
	chain := reloadTestChain()
	chain.blocksResults = []int{3, 0}
	c.Chains["Chain"] = chain
	if got := c.settings(); got.GroupNodeAlerts {
		t.Fatal("grouping starts on")
	}
	next := &Config{GroupNodeAlerts: true, GroupWaitSeconds: 30, Chains: map[string]*ChainConfig{"Chain": reloadTestChain()}}
	next.Chains["Chain"].Nodes[0].Server = "hetzner-1"
	if reason := restartReason(chain, next.Chains["Chain"]); reason != "" {
		t.Fatalf("a server label restarts the chain: %q", reason)
	}
	diff := c.applyReload(next)
	if len(diff.Restarted) != 0 || c.Chains["Chain"] != chain || !reflect.DeepEqual(chain.blocksResults, []int{3, 0}) {
		t.Fatalf("diff %+v", diff)
	}
	if chain.Nodes[0].serverLabel() != "hetzner-1" {
		t.Fatal("the node kept its old server")
	}
	if on, wait := c.groupSettings(); !on || wait != 30*groupWaitUnit {
		t.Fatalf("grouping %v after %v", on, wait)
	}
	next = &Config{Chains: map[string]*ChainConfig{"Chain": reloadTestChain()}}
	c.applyReload(next)
	if chain.Nodes[0].serverLabel() != "" {
		t.Fatal("the node kept a removed label")
	}
	if on, wait := c.groupSettings(); on || wait != defaultGroupWaitSeconds*groupWaitUnit {
		t.Fatalf("grouping %v, wait %v", on, wait)
	}
}

func TestGroupSettingsAreRead(t *testing.T) {
	dir := t.TempDir()
	read := func(extra string) *Config {
		oldAlarms := alarms
		defer func() { alarms = oldAlarms }()
		configFile := filepath.Join(dir, "config.yml")
		config := extra + `node_down_alert_minutes: 3
chains:
  Realio:
    chain_id: realio-1
    nodes:
      - url: http://shared:26657
        server: ovh-1
    validators:
      - name: RIO
        valoper_address: realiovaloper1first
        nodes:
          - url: http://own:26657
            alert_if_down: yes
            server: hetzner-1
`
		if err := os.WriteFile(configFile, []byte(config), 0600); err != nil {
			t.Fatal(err)
		}
		password := ""
		c, err := loadConfig(configFile, filepath.Join(dir, "state.json"), filepath.Join(dir, "chains.d"), &password)
		if err != nil {
			t.Fatal(err)
		}
		c.cancel()
		c.deliveryCancel()
		return c
	}
	c := read("")
	if fatal, _ := validateConfig(c); fatal {
		t.Fatal("config is invalid")
	}
	if c.GroupNodeAlerts || c.GroupWaitSeconds != 60 {
		t.Errorf("defaults: %v, %d", c.GroupNodeAlerts, c.GroupWaitSeconds)
	}
	nodes := c.Chains["Realio / RIO"].Nodes
	if len(nodes) != 2 || nodes[0].Server != "hetzner-1" || nodes[1].Server != "ovh-1" {
		t.Errorf("servers of the expanded nodes: %+v", nodes)
	}
	c = read("group_node_alerts: yes\ngroup_wait_seconds: 120\n")
	if fatal, _ := validateConfig(c); fatal || !c.GroupNodeAlerts || c.GroupWaitSeconds != 120 {
		t.Errorf("set: %v, %d", c.GroupNodeAlerts, c.GroupWaitSeconds)
	}
	c = read("group_node_alerts: no\ngroup_wait_seconds: 3\n")
	fatal, problems := validateConfig(c)
	if fatal || c.GroupNodeAlerts || c.GroupWaitSeconds != 10 || !strings.Contains(strings.Join(problems, "\n"), "group_wait_seconds") {
		t.Errorf("too short: %v, %d, %v", c.GroupNodeAlerts, c.GroupWaitSeconds, problems)
	}
}
