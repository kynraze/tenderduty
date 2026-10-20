package tenderduty

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	dash "github.com/kynraze/tenderduty/v2/td2/dashboard"
	"github.com/prometheus/client_golang/prometheus"
	rpchttp "github.com/tendermint/tendermint/rpc/client/http"
)

func reloadTestChain() *ChainConfig {
	return &ChainConfig{
		name: "Chain", ChainId: "chain-1", ValAddress: "cosmosvaloper1test",
		Nodes:  []*NodeConfig{{Url: "http://localhost:26657", AlertIfDown: true}},
		Alerts: AlertConfig{ConsecutiveAlerts: true, ConsecutiveMissed: 5},
	}
}

func TestReloadDiff(t *testing.T) {
	old := map[string]*ChainConfig{
		"removed": reloadTestChain(), "nodes": reloadTestChain(), "validator": reloadTestChain(),
		"identity": reloadTestChain(), "updated": reloadTestChain(), "unchanged": reloadTestChain(),
	}
	next := map[string]*ChainConfig{
		"added": reloadTestChain(), "nodes": reloadTestChain(), "validator": reloadTestChain(),
		"identity": reloadTestChain(), "updated": reloadTestChain(), "unchanged": reloadTestChain(),
	}
	next["nodes"].Nodes[0].Url = "https://localhost:443"
	next["validator"].ValAddress = "cosmosvaloper1other"
	next["identity"].ChainId = "chain-2"
	next["updated"].Alerts.ConsecutiveMissed = 7
	old["unchanged"].Nodes[0].markDown("down", false)
	old["unchanged"].blocksResults = []int{3, 0}
	got := diffChains(old, next)
	want := chainDiff{
		Added: []string{"added"}, Removed: []string{"removed"}, Updated: []string{"updated"}, Unchanged: []string{"unchanged"},
		Restarted: map[string]string{"nodes": "nodes changed", "validator": "validator changed", "identity": "chain_id changed"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("diff = %+v, want %+v", got, want)
	}
}

func TestReloadInvalidKeepsOldStateAndReportsOncePerDestination(t *testing.T) {
	c := auditConfig(t)
	received := make(chan string, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received <- string(body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	c.Discord = DiscordConfig{Enabled: yes(true), Webhook: server.URL, MinSeverity: "critical"}
	chain := reloadTestChain()
	chain.Alerts.Discord = c.Discord
	c.Chains["Chain"] = chain
	other := reloadTestChain()
	other.Alerts.Discord = c.Discord
	c.Chains["Other"] = other
	message := "validator has missed 5 blocks on chain-1"
	alarms.AllAlarms["Chain"] = map[string]time.Time{message: time.Now()}
	before := alarms
	dir := t.TempDir()
	filename := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(filename, []byte("chains:\n  Broken:\n    nodes: []\n"), 0600); err != nil {
		t.Fatal(err)
	}
	password := ""
	if err := c.reload(filename, filepath.Join(dir, "chains.d"), &password); err == nil {
		t.Fatal("invalid config was accepted")
	}
	c.workers.Wait()
	if c.Chains["Chain"] != chain || alarms != before || !alarmIsActive("Chain", message) || c.liveSettings != nil {
		t.Fatal("failed reload changed running state")
	}
	if len(received) != 1 {
		t.Fatalf("got %d notices for one destination", len(received))
	}
	body := <-received
	if !strings.Contains(body, "Config reload failed") || !strings.Contains(body, "requires chain_id and valoper_address") {
		t.Fatalf("notice didn't report the problems: %s", body)
	}
	if strings.Contains(body, "**Chain**") || strings.Contains(body, "**Other**") {
		t.Fatalf("config-wide notice headed by a chain: %s", body)
	}
	if err := os.WriteFile(filename, []byte("chains: ["), 0600); err != nil {
		t.Fatal(err)
	}
	if err := c.reload(filename, filepath.Join(dir, "chains.d"), &password); err == nil {
		t.Fatal("broken YAML was accepted")
	}
	c.workers.Wait()
	if c.Chains["Chain"] != chain || alarms != before || len(received) != 1 {
		t.Fatal("parse failure changed state or failed to notify")
	}
}

func TestReloadSettingsPreserveChainAndListeners(t *testing.T) {
	c := auditConfig(t)
	c.updateChan = make(chan *dash.ChainStatus, 8)
	c.statsChan = make(chan *promUpdate, 8)
	c.EnableDash, c.Listen, c.Prom, c.PrometheusListenPort = true, "8888", true, "28686"
	c.FibreMetrics = FibreMetricsConfig{Listen: "127.0.0.1:4318", Token: "old"}
	chain := reloadTestChain()
	chain.blocksResults, chain.statTotalSigns = []int{3, 0}, 17
	c.Chains["Chain"] = chain
	next := &Config{
		Listen: "9999", PrometheusListenPort: "127.0.0.1:9000",
		FibreMetrics: FibreMetricsConfig{Listen: "127.0.0.1:4319", Token: "new", TLSCert: "cert", TLSKey: "key"},
		Chains:       map[string]*ChainConfig{"Chain": reloadTestChain()},
	}
	next.Chains["Chain"].Alerts.ConsecutiveMissed = 7
	next.Chains["Chain"].ExtraInfo = "new notes"
	next.Chains["Chain"].PublicFallback = true
	next.NodeDownMin = 9
	next.AlertDefaults.ConsecutiveMissed = 7
	restarts := keepListeners(c, next)
	if len(restarts) != 7 {
		t.Fatalf("restart settings: %v", restarts)
	}
	c.applyReload(next)
	if c.Chains["Chain"] != chain || chain.statTotalSigns != 17 || !reflect.DeepEqual(chain.blocksResults, []int{3, 0}) {
		t.Fatal("settings update replaced monitoring state")
	}
	if chain.alertSettings().ConsecutiveMissed != 7 || chain.extraInfo() != "new notes" || !chain.publicFallbackEnabled() {
		t.Fatal("chain settings didn't apply")
	}
	s := c.settings()
	if !s.EnableDash || s.Listen != "8888" || !s.Prom || s.PrometheusListenPort != "28686" || s.FibreMetrics.Listen != "127.0.0.1:4318" || s.FibreMetrics.TLSCert != "" {
		t.Fatal("restart-only settings were applied")
	}
	if s.FibreMetrics.Token != "new" || s.NodeDownMin != 9 || s.AlertDefaults.ConsecutiveMissed != 7 {
		t.Fatal("shared settings were not published")
	}
}

func TestReloadRemovedChainResolvesAndDeletesSeries(t *testing.T) {
	c := auditConfig(t)
	delivered := make(chan string, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		delivered <- string(body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	c.updateChan = make(chan *dash.ChainStatus, 8)
	c.statsChan = make(chan *promUpdate, 8)
	chain := reloadTestChain()
	c.Chains["Chain"] = chain
	c.Discord.Enabled = yes(true)
	chain.Alerts.Discord.Webhook = server.URL
	message, id := "validator has missed 5 blocks on chain-1", "consensus-consecutive"
	c.alert("Chain", message, "critical", false, &id)
	alarms.notifyMux.Lock()
	alarms.SentDiAlarms["Chain\x00"+message] = time.Now()
	alarms.notifyMux.Unlock()
	c.applyReload(&Config{Chains: map[string]*ChainConfig{}})
	if c.Chains["Chain"] != nil || alarmIsActive("Chain", message) {
		t.Fatal("removed chain or its alarm is still active")
	}
	r, ok := storedResolution("Chain", message)
	if !ok || r.label != "No longer watched" || r.text != message+"\n"+removedNote {
		t.Fatalf("resolution = %+v, %v", r, ok)
	}
	update, stat := <-c.updateChan, <-c.statsChan
	if update.MsgType != "remove" || update.Name != "Chain" || !stat.remove || stat.name != "Chain" {
		t.Fatal("removed chain wasn't deleted from consumers")
	}
	for _, item := range alarms.Outbox {
		if item.Resolved && item.Chain == "Chain" {
			if item.ID != id || item.snapshot == nil || !item.snapshot.resolved {
				t.Fatal("recovery lost its key or old destinations")
			}
			c.deliverQueue(item.workerKey())
			if len(delivered) != 1 || !strings.Contains(<-delivered, "No longer watched") {
				t.Fatal("recovery wasn't delivered to the old destination")
			}
			return
		}
	}
	t.Fatal("no recovery was queued")
}

func TestReloadConcurrentReaders(t *testing.T) {
	c := auditConfig(t)
	c.updateChan = make(chan *dash.ChainStatus, 8)
	c.statsChan = make(chan *promUpdate, 8)
	c.Chains["Chain"] = reloadTestChain()
	var readers sync.WaitGroup
	readers.Add(1)
	go func() {
		defer readers.Done()
		for i := 0; i < 500; i++ {
			_ = c.makeAlert("Chain", "reading", "info", false, nil)
			_ = c.healthStatus()
			_ = c.fibreChainFor("host")
			_ = c.statusSummary()
		}
	}()
	for i := 0; i < 100; i++ {
		chain := reloadTestChain()
		chain.Alerts.ConsecutiveMissed = i + 1
		chain.ExtraInfo = fmt.Sprint(i)
		c.applyReload(&Config{NodeDownMin: i + 1, Chains: map[string]*ChainConfig{"Chain": chain}})
	}
	readers.Wait()
}

func TestChainCancellationClosesWebsocket(t *testing.T) {
	c := auditConfig(t)
	opened, closed := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		defer close(closed)
		close(opened)
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer server.Close()
	client, err := rpchttp.New(server.URL, "/websocket")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(c.context())
	defer cancel()
	chain := &ChainConfig{ctx: ctx, cancel: cancel, client: client, valInfo: &ValInfo{Conspub: make([]byte, 20)}}
	chain.workers.Add(1)
	go func() { defer chain.workers.Done(); chain.WsRun() }()
	select {
	case <-opened:
	case <-time.After(3 * time.Second):
		t.Fatal("websocket didn't open")
	}
	done := make(chan struct{})
	go func() { stopChain(chain); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("chain worker didn't stop")
	}
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("websocket wasn't closed")
	}
}

func TestReloadDeletesPrometheusLabels(t *testing.T) {
	gauge := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "test_reload_signed", Help: "test"}, []string{"name", "chain_id", "moniker"})
	registry := prometheus.NewRegistry()
	registry.MustRegister(gauge)
	m := metrics{metricSigned: gauge}
	m.setStat(&promUpdate{metric: metricSigned, name: "removed", chainId: "chain-1", moniker: "old", counter: 3})
	m.setStat(&promUpdate{metric: metricSigned, name: "kept", chainId: "chain-1", moniker: "other", counter: 7})
	m.setStat(&promUpdate{name: "removed", remove: true})
	all, err := registry.Gather()
	if err != nil || len(all) != 1 || len(all[0].Metric) != 1 || all[0].Metric[0].Gauge.GetValue() != 7 {
		t.Fatalf("series deletion removed the wrong metrics: %v, %v", all, err)
	}
}

func TestReloadEncryptedConfigReusesPassword(t *testing.T) {
	c := auditConfig(t)
	chain := reloadTestChain()
	chain.blocksResults = []int{3, 0}
	c.Chains["Chain"] = chain
	before := alarms
	password := "R3load-7cW9!uQ6#sP2"
	encrypted, err := encrypt([]byte(`alert_defaults:
  consecutive_enabled: yes
  consecutive_missed: 9
chains:
  Chain:
    chain_id: chain-1
    valoper_address: cosmosvaloper1test
    nodes:
      - url: http://localhost:26657
        alert_if_down: yes
`), password)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(encrypted)
	}))
	defer server.Close()
	dir := filepath.Join(t.TempDir(), "chains.d")
	for i := 0; i < 2; i++ {
		if err := c.reload(server.URL, dir, &password); err != nil {
			t.Fatal(err)
		}
	}
	if password != "R3load-7cW9!uQ6#sP2" || alarms != before || c.Chains["Chain"] != chain || chain.alertSettings().ConsecutiveMissed != 9 || !reflect.DeepEqual(chain.blocksResults, []int{3, 0}) {
		t.Fatal("encrypted reload lost its password, settings or running state")
	}
}

func TestReloadNodeChangeCarriesHistory(t *testing.T) {
	old, next := reloadTestChain(), reloadTestChain()
	old.blocksResults = []int{3, 0}
	old.statTotalSigns, old.statConsecutiveMiss = 19, 1
	old.lastBlockNum, old.lastBlockTime = 100, time.Now()
	next.Nodes[0].Url = "https://other.example:443"
	carryChainHistory(old, next)
	if !reflect.DeepEqual(old.blocksResults, next.blocksResults) || next.statTotalSigns != 19 || next.statConsecutiveMiss != 1 || next.lastBlockNum != 100 || next.lastBlockTime != old.lastBlockTime {
		t.Fatal("node restart lost validator history")
	}
	next.blocksResults[0] = 0
	if old.blocksResults[0] != 3 {
		t.Fatal("replacement shares mutable history")
	}
	other := reloadTestChain()
	other.ValAddress = "cosmosvaloper1other"
	carryChainHistory(old, other)
	if other.statTotalSigns != 0 || len(other.blocksResults) != 0 {
		t.Fatal("another validator inherited history")
	}
}

func TestRetiredChainCannotRaiseNewAlarms(t *testing.T) {
	c := auditConfig(t)
	chain := reloadTestChain()
	c.Chains["Chain"] = chain
	var recorder sync.WaitGroup
	recorder.Add(1)
	go func() {
		defer recorder.Done()
		for i := 0; i < 500; i++ {
			c.alert("Chain", "alarm", "critical", false, nil)
		}
	}()
	stopChain(chain)
	c.removeChainAlarms("Chain")
	recorder.Wait()
	if alarmIsActive("Chain", "alarm") {
		t.Fatal("a worker raised an alarm after chain retirement")
	}
}

func TestReloadKeepsOriginalAlarmKey(t *testing.T) {
	c := auditConfig(t)
	c.Chains["Chain"] = reloadTestChain()
	original, newID := "old-validator-consecutive", "new-validator-consecutive"
	c.alert("Chain", "same message", "critical", false, &original)
	c.alert("Chain", "same message", "critical", false, &newID)
	if got := c.alarmID("Chain", "same message"); got != original {
		t.Fatalf("surviving alarm changed key to %q", got)
	}
}
