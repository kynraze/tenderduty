package tenderduty

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cosmos/cosmos-sdk/types/bech32"
	"github.com/gorilla/websocket"
	rpchttp "github.com/tendermint/tendermint/rpc/client/http"
)

func auditConfig(t *testing.T) *Config {
	t.Helper()
	oldConfig, oldAlarms := td, alarms
	ctx, cancel := context.WithCancel(context.Background())
	delivery, cancelDelivery := context.WithCancel(context.Background())
	c := &Config{ctx: ctx, cancel: cancel, deliveryCtx: delivery, deliveryCancel: cancelDelivery, alertChan: make(chan *alertMsg, 16), Chains: make(map[string]*ChainConfig)}
	td, alarms = c, newAlarmCache()
	t.Cleanup(func() { cancel(); cancelDelivery(); c.workers.Wait(); td, alarms = oldConfig, oldAlarms })
	return c
}

func TestFailedRefreshRetainsLastKnownSigningState(t *testing.T) {
	auditConfig(t)
	address, _ := bech32.ConvertAndEncode("cosmosvalcons", make([]byte, 20))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			ID interface{} `json:"id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"jsonrpc": "2.0", "id": request.ID, "error": map[string]interface{}{"code": -32601, "message": "query unavailable"}})
	}))
	defer server.Close()
	client, _ := rpchttp.New(server.URL, "/websocket")
	updated := time.Now().Add(-time.Minute)
	chain := &ChainConfig{ValAddress: address, client: client, valInfo: &ValInfo{Moniker: address, Bonded: true, Valcons: address, Tombstoned: true, Missed: 123, Window: 1000, SigningUpdatedAt: updated}}
	if chain.GetValInfo(false) == nil {
		t.Fatal("query failure was accepted")
	}
	info, _ := chain.validatorState()
	if !info.Tombstoned || info.Missed != 123 || info.Window != 1000 || !info.SigningStale || !info.SigningUpdatedAt.Equal(updated) {
		t.Fatalf("last known signing state was lost: %+v", info)
	}
}

func rpcStatus(w http.ResponseWriter, r *http.Request) {
	var request struct {
		ID interface{} `json:"id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&request)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"jsonrpc": "2.0", "id": request.ID, "result": map[string]interface{}{"node_info": map[string]interface{}{"network": "test-chain"}, "sync_info": map[string]interface{}{"catching_up": false}}})
}

func TestRPCFailoverUsesIndependentTimeouts(t *testing.T) {
	auditConfig(t)
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))
	defer slow.Close()
	defer close(release)
	good := httptest.NewServer(http.HandlerFunc(rpcStatus))
	defer good.Close()
	chain := &ChainConfig{ChainId: "test-chain", Nodes: []*NodeConfig{{Url: slow.URL}, {Url: good.URL, down: true}}}
	if err := chain.newRpc(); err != nil {
		t.Fatal(err)
	}
	if chain.clientSnapshot().Remote() != good.URL || chain.hasNoNodes() || chain.Nodes[1].snapshot().down {
		t.Fatal("healthy backup was not selected")
	}
}

func TestReconnectRotatesPastRejectedWebsocket(t *testing.T) {
	c := auditConfig(t)
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/websocket" {
			http.Error(w, "subscriptions disabled", http.StatusServiceUnavailable)
			return
		}
		rpcStatus(w, r)
	}))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(rpcStatus))
	defer good.Close()
	chain := &ChainConfig{ChainId: "test-chain", Nodes: []*NodeConfig{{Url: bad.URL}, {Url: good.URL}}, valInfo: &ValInfo{Conspub: make([]byte, 20)}}
	c.Chains["Chain"] = chain
	if err := chain.newRpc(); err != nil {
		t.Fatal(err)
	}
	chain.WsRun()
	if err := chain.newRpc(); err != nil {
		t.Fatal(err)
	}
	if chain.clientSnapshot().Remote() != good.URL {
		t.Fatal("reconnect reused the rejected endpoint")
	}
}

func TestCommitFlagsAndVoteHeights(t *testing.T) {
	for _, flag := range []int{1, 2, 3} {
		block := rawBlock{}
		block.Block.LastCommit.Signatures = []signature{{ValidatorAddress: "ABCD", BlockIDFlag: flag}}
		if block.find("ABCD") != (flag == 2) {
			t.Fatalf("incorrect result for flag %d", flag)
		}
	}
	tracker := signingTracker{}
	tracker.consume(StatusUpdate{Height: 101, Status: StatusPrecommit})
	if status, ok := tracker.consume(StatusUpdate{Height: 100, Final: true, Status: Statusmissed}); !ok || status != Statusmissed {
		t.Fatal("future vote contaminated the previous block")
	}
	if status, ok := tracker.consume(StatusUpdate{Height: 101, Final: true, Status: Statusmissed}); !ok || status != StatusPrecommit {
		t.Fatal("matching vote was discarded")
	}
	if _, ok := tracker.consume(StatusUpdate{Height: 101, Final: true, Status: StatusSigned}); ok {
		t.Fatal("duplicate block was counted twice")
	}
	tracker.consume(StatusUpdate{Height: 102, Status: StatusProposed})
	if status, _ := tracker.consume(StatusUpdate{Height: 102, Final: true, Status: Statusmissed}); status != Statusmissed {
		t.Fatal("proposal alone was counted as signed")
	}
}

func TestBlockCommitHeightAndCanceledWorkers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	blocks := make(chan *WsReply)
	results := make(chan StatusUpdate)
	done := make(chan error, 1)
	go func() { done <- handleBlocks(ctx, blocks, results, "ABCD") }()
	reply := &WsReply{}
	reply.Result.Data.Value = json.RawMessage(`{"block":{"header":{"height":"101","proposer_address":"OTHER"},"last_commit":{"height":"100","signatures":[{"block_id_flag":3,"validator_address":"ABCD"}]}}}`)
	blocks <- reply
	select {
	case update := <-results:
		if update.Height != 100 || update.HeadHeight != 101 || update.Status != Statusmissed {
			t.Fatalf("incorrect commit: %+v", update)
		}
	case <-time.After(time.Second):
		t.Fatal("block was not processed")
	}
	blocks <- reply
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("blocked block worker ignored cancellation")
	}
	ctx, cancel = context.WithCancel(context.Background())
	votes := make(chan *WsReply)
	finished := make(chan struct{})
	go func() { handleVotes(ctx, votes, results, "ABCD"); close(finished) }()
	reply.Result.Data.Value = json.RawMessage(`{"Vote":{"height":"100","type":2,"validator_address":"ABCD"}}`)
	votes <- reply
	cancel()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("blocked vote worker ignored cancellation")
	}
}

func TestWebsocketCancellationWaitsForConnectionWorkers(t *testing.T) {
	c := auditConfig(t)
	accepted := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for i := 0; i < 2; i++ {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
		close(accepted)
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer server.Close()
	client, _ := rpchttp.New(server.URL, "/websocket")
	chain := &ChainConfig{client: client, valInfo: &ValInfo{Conspub: make([]byte, 20)}}
	done := make(chan struct{})
	go func() { chain.WsRun(); close(done) }()
	select {
	case <-accepted:
	case <-time.After(time.Second):
		t.Fatal("subscriptions not established")
	}
	c.cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("connection workers did not stop")
	}
}

func notificationConfig(t *testing.T, endpoint string) *Config {
	c := auditConfig(t)
	c.Discord.Enabled = true
	chain := &ChainConfig{name: "Chain", ChainId: "test-chain", ValAddress: "validator"}
	chain.Alerts.Discord.Enabled = true
	chain.Alerts.Discord.Webhook = endpoint
	c.Chains["Chain"] = chain
	c.stateFile = filepath.Join(t.TempDir(), "state.json")
	return c
}

func TestNotificationQueuePreservesIncidentOrder(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var mux sync.Mutex
	var order []bool
	var c *Config
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var msg DiscordMessage
		_ = json.NewDecoder(r.Body).Decode(&msg)
		data, err := os.ReadFile(c.stateFile)
		if err != nil || !strings.Contains(string(data), "outbox") {
			t.Error("notification was sent before its queue entry reached disk")
		}
		mux.Lock()
		order = append(order, strings.Contains(msg.Content, "Resolved"))
		first := len(order) == 1
		mux.Unlock()
		if first {
			close(entered)
			<-release
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	c = notificationConfig(t, server.URL)
	c.alert("Chain", "missed blocks", "critical", false, nil)
	key := alarms.Outbox[0].workerKey()
	done := make(chan struct{})
	go func() { c.deliverQueue(key); close(done) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("trigger not delivered")
	}
	c.alert("Chain", "missed blocks", "info", true, nil)
	c.alert("Chain", "missed blocks", "critical", false, nil)
	close(release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ordered queue did not drain")
	}
	mux.Lock()
	defer mux.Unlock()
	if len(order) != 3 || order[0] || !order[1] || order[2] {
		t.Fatalf("incorrect delivery order: %v", order)
	}
	if len(alarms.Outbox) != 0 {
		t.Fatal("acknowledged notifications remain queued")
	}
}

func TestRecoveredUnsentIncidentIsNotDelivered(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(http.StatusNoContent) }))
	defer server.Close()
	c := notificationConfig(t, server.URL)
	c.alert("Chain", "missed blocks", "critical", false, nil)
	c.alert("Chain", "missed blocks", "info", true, nil)
	key := alarms.Outbox[0].workerKey()
	c.deliverQueue(key)
	if calls != 0 || len(alarms.Outbox) != 0 {
		t.Fatal("obsolete trigger was delivered after recovery")
	}
}

func TestPersistentQueueResumesWithCurrentCredentials(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(http.StatusNoContent) }))
	defer server.Close()
	c := notificationConfig(t, server.URL)
	c.alert("Chain", "missed blocks", "critical", false, nil)
	data, err := os.ReadFile(c.stateFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), server.URL) {
		t.Fatal("notification queue persisted credentials or destination URL")
	}
	var saved savedState
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	alarms = newAlarmCache()
	alarms.Outbox, alarms.NextNotification = saved.Alarms.Outbox, saved.Alarms.NextNotification
	alarms.AllAlarms = saved.Alarms.AllAlarms
	c.deliverQueue(alarms.Outbox[0].workerKey())
	if calls != 1 || len(alarms.Outbox) != 0 {
		t.Fatal("persistent queue did not resume")
	}
}

func TestQueueDoesNotSendWhenStateCannotBeSaved(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(http.StatusNoContent) }))
	defer server.Close()
	c := notificationConfig(t, server.URL)
	c.stateFile = filepath.Join(t.TempDir(), "missing", "state.json")
	c.alert("Chain", "missed blocks", "critical", false, nil)
	time.AfterFunc(100*time.Millisecond, c.cancel)
	c.deliverQueue(alarms.Outbox[0].workerKey())
	if calls != 0 || len(alarms.Outbox) != 1 {
		t.Fatal("unpersisted notification was sent or dropped")
	}
}

func TestHeartbeatRejectsHTTPFailuresAndHonorsCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer server.Close()
	if pingURL(context.Background(), server.Client(), server.URL) == nil {
		t.Fatal("503 was reported as a successful heartbeat")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if pingURL(ctx, server.Client(), server.URL) == nil {
		t.Fatal("canceled heartbeat was sent")
	}
}

func TestFailedDeliveryRemainsQueuedAcrossRestart(t *testing.T) {
	failed := make(chan struct{})
	var mux sync.Mutex
	success := false
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.Lock()
		calls++
		ok := success
		mux.Unlock()
		if !ok {
			w.WriteHeader(http.StatusServiceUnavailable)
			close(failed)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	c := notificationConfig(t, server.URL)
	c.alert("Chain", "missed blocks", "critical", false, nil)
	key := alarms.Outbox[0].workerKey()
	done := make(chan struct{})
	go func() { c.deliverQueue(key); close(done) }()
	select {
	case <-failed:
	case <-time.After(time.Second):
		t.Fatal("failed delivery did not run")
	}
	c.cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("retry worker did not stop")
	}
	data, err := os.ReadFile(c.stateFile)
	if err != nil {
		t.Fatal(err)
	}
	var saved savedState
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if len(saved.Alarms.Outbox) != 1 || len(saved.Alarms.SentDiAlarms) != 0 {
		t.Fatal("failed notification lost its retry state")
	}
	alarms = newAlarmCache()
	alarms.Outbox = saved.Alarms.Outbox
	alarms.NextNotification = saved.Alarms.NextNotification
	alarms.AllAlarms = saved.Alarms.AllAlarms
	c.ctx, c.cancel = context.WithCancel(context.Background())
	mux.Lock()
	success = true
	mux.Unlock()
	c.deliverQueue(key)
	mux.Lock()
	defer mux.Unlock()
	if calls != 2 || len(alarms.Outbox) != 0 {
		t.Fatal("failed delivery was not resumed")
	}
}

func TestLegacyRecoveryMigrationPreservesActiveIncident(t *testing.T) {
	var order []bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var msg DiscordMessage
		_ = json.NewDecoder(r.Body).Decode(&msg)
		order = append(order, strings.Contains(msg.Content, "Resolved"))
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	c := notificationConfig(t, server.URL)
	key := "Chain\x00missed blocks"
	alarms.AllAlarms["Chain"] = map[string]time.Time{"missed blocks": time.Now()}
	alarms.SentDiAlarms[key] = time.Now()
	alarms.PendingRecoveries[key] = &pendingRecovery{Chain: "Chain", Message: "missed blocks", ID: "validator"}
	c.restoreRecoveries()
	if !alarmIsActive("Chain", "missed blocks") {
		t.Fatal("recovery migration changed active monitoring state")
	}
	c.alert("Chain", "missed blocks", "critical", false, nil)
	if len(alarms.Outbox) != 2 {
		t.Fatalf("unexpected migrated queue: %+v", alarms.Outbox)
	}
	c.restoreRecoveries()
	if len(alarms.Outbox) != 2 {
		t.Fatal("migration reordered or duplicated the queue")
	}
	c.deliverQueue(alarms.Outbox[0].workerKey())
	if len(order) != 2 || !order[0] || order[1] {
		t.Fatalf("incorrect migrated order: %v", order)
	}
}

func TestStaleSigningInfoCannotClearPercentageAlert(t *testing.T) {
	c := auditConfig(t)
	chain := &ChainConfig{name: "Chain", ChainId: "test-chain", valInfo: &ValInfo{Moniker: "validator", Bonded: true, Valcons: "consensus", Missed: 0, Window: 100, SigningStale: true}}
	chain.Alerts.PercentageAlerts, chain.Alerts.Window = true, 10
	c.Chains["Chain"] = chain
	message := "validator has missed > 10% of the slashing window's blocks on test-chain"
	alarms.AllAlarms["Chain"] = map[string]time.Time{message: time.Now()}
	c.startWorker(chain.watch)
	select {
	case alert := <-c.alertChan:
		t.Fatalf("stale data changed the alert: %+v", alert)
	case <-time.After(2200 * time.Millisecond):
	}
	c.cancel()
	c.workers.Wait()
	if !alarmIsActive("Chain", message) {
		t.Fatal("stale data cleared the percentage alert")
	}
}

func TestReconnectGapDoesNotCountUnobservedBlocksAsConsecutive(t *testing.T) {
	c := auditConfig(t)
	chain := &ChainConfig{lastBlockNum: 100, statConsecutiveMiss: 10, blocksResults: make([]int, showBLocks), valInfo: &ValInfo{Bonded: true}}
	results := make(chan StatusUpdate)
	done := make(chan struct{})
	go func() { chain.processResults(c.ctx, results); close(done) }()
	results <- StatusUpdate{Height: 103, HeadHeight: 104, Final: true, Status: Statusmissed}
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
		chain.stateMux.RLock()
		height := chain.lastBlockNum
		chain.stateMux.RUnlock()
		if height == 104 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	c.cancel()
	<-done
	_, _, consecutive := chain.blockState()
	blocks := chain.blocksSnapshot()
	if consecutive != 1 || blocks[1] != -1 || blocks[2] != -1 || blocks[3] != -1 {
		t.Fatal("unknown reconnect gap contaminated consecutive misses")
	}
}

func TestRejectedSubscriptionClosesConnectionWorkers(t *testing.T) {
	auditConfig(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"id":1,"error":{"code":-32603,"message":"subscription limit"}}`))
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer server.Close()
	client, _ := rpchttp.New(server.URL, "/websocket")
	chain := &ChainConfig{client: client, valInfo: &ValInfo{Conspub: make([]byte, 20)}}
	done := make(chan struct{})
	go func() { chain.WsRun(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		td.cancel()
		<-done
		t.Fatal("rejected subscription did not reconnect")
	}
}

func TestRuntimeShutdownSavesStateAfterMonitoringStops(t *testing.T) {
	oldConfig, oldAlarms := td, alarms
	defer func() { td, alarms = oldConfig, oldAlarms }()
	address, _ := bech32.ConvertAndEncode("cosmosvalcons", make([]byte, 20))
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-entered:
		default:
			close(entered)
		}
		<-release
		w.WriteHeader(http.StatusNoContent)
	}))
	defer destination.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/websocket" {
			var request struct {
				ID     interface{} `json:"id"`
				Method string      `json:"method"`
			}
			_ = json.NewDecoder(r.Body).Decode(&request)
			if request.Method == "status" {
				_ = json.NewEncoder(w).Encode(map[string]interface{}{"jsonrpc": "2.0", "id": request.ID, "result": map[string]interface{}{"node_info": map[string]interface{}{"network": "test-chain"}, "sync_info": map[string]interface{}{"catching_up": false}}})
			} else {
				_ = json.NewEncoder(w).Encode(map[string]interface{}{"jsonrpc": "2.0", "id": request.ID, "error": map[string]interface{}{"code": -32601, "message": "unsupported"}})
			}
			return
		}
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for i := 0; i < 2; i++ {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"result":{"data":{"type":"tendermint/event/NewBlock","value":{"block":{"header":{"height":"101"},"last_commit":{"height":"100","signatures":[{"validator_address":"0000000000000000000000000000000000000000","block_id_flag":3}]}}}}}}`))
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer server.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	directory := t.TempDir()
	stateFile := filepath.Join(directory, "state.json")
	configFile := filepath.Join(directory, "config.yml")
	config := fmt.Sprintf("enable_dashboard: yes\nhide_logs: yes\nlisten_port: %d\nnode_down_alert_minutes: 3\ndiscord:\n  enabled: yes\n  webhook: %s\nchains:\n  Chain:\n    chain_id: test-chain\n    valoper_address: %s\n    alerts:\n      consecutive_enabled: yes\n      consecutive_missed: 1\n      discord:\n        enabled: yes\n    nodes:\n      - url: %s\n", port, destination.URL, address, server.URL)
	if err := os.WriteFile(configFile, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	password := ""
	go func() {
		done <- runContext(ctx, configFile, stateFile, filepath.Join(directory, "chains.d"), &password)
	}()
	client := &http.Client{Timeout: time.Second}
	ready := false
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		response, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/ready", port))
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				ready = true
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	select {
	case <-entered:
	case <-time.After(4 * time.Second):
		cancel()
		releaseOnce.Do(func() { close(release) })
		<-done
		t.Fatal("notification request did not start")
	}
	cancel()
	select {
	case <-done:
		releaseOnce.Do(func() { close(release) })
		t.Fatal("shutdown completed before the active notification request")
	case <-time.After(100 * time.Millisecond):
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("runtime shutdown blocked")
	}
	if !ready {
		t.Fatal("monitoring never became ready")
	}
	data, err := os.ReadFile(stateFile)
	if err != nil {
		t.Fatal(err)
	}
	var saved savedState
	_ = json.Unmarshal(data, &saved)
	if saved.Heights["Chain"] != 101 || saved.Blocks["Chain"][0] != int(Statusmissed) || len(saved.Alarms.Outbox) != 0 || len(saved.Alarms.SentDiAlarms) != 1 {
		t.Fatal("final monitoring state was not saved")
	}
}
