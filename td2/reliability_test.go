package tenderduty

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/cosmos/cosmos-sdk/types/bech32"
	slashing "github.com/cosmos/cosmos-sdk/x/slashing/types"
	"github.com/gorilla/websocket"
	dash "github.com/kynraze/tenderduty/v2/td2/dashboard"
	rpchttp "github.com/tendermint/tendermint/rpc/client/http"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func fastWatch(t *testing.T) {
	t.Helper()
	oldStartup, oldInterval := watchStartupInterval, watchInterval
	watchStartupInterval, watchInterval = 10*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { watchStartupInterval, watchInterval = oldStartup, oldInterval })
}

func auditConfig(t *testing.T) *Config {
	t.Helper()
	resolutions.Range(func(key, _ interface{}) bool { resolutions.Delete(key); return true })
	oldConfig, oldAlarms := td, alarms
	ctx, cancel := context.WithCancel(context.Background())
	delivery, cancelDelivery := context.WithCancel(context.Background())
	c := &Config{ctx: ctx, cancel: cancel, deliveryCtx: delivery, deliveryCancel: cancelDelivery, alertChan: make(chan *alertMsg, 16), Chains: make(map[string]*ChainConfig)}
	// made before the cleanup below so workers stop before the dir is removed
	c.stateFile = filepath.Join(t.TempDir(), "state.json")
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
	if r.URL.Path == "/websocket" {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err == nil {
			_ = conn.Close()
		}
		return
	}
	var request struct {
		ID interface{} `json:"id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&request)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"jsonrpc": "2.0", "id": request.ID, "result": map[string]interface{}{"node_info": map[string]interface{}{"network": "test-chain"}, "sync_info": map[string]interface{}{"catching_up": false}}})
}

func TestRPCFailoverUsesIndependentTimeouts(t *testing.T) {
	old := rpcEndpointTimeout
	rpcEndpointTimeout = 500 * time.Millisecond
	t.Cleanup(func() { rpcEndpointTimeout = old })
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

func TestNodeRefusingWebsocketCountsAsDown(t *testing.T) {
	auditConfig(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/websocket" {
			http.Error(w, "no", http.StatusBadRequest)
			return
		}
		rpcStatus(w, r)
	}))
	defer server.Close()
	chain := &ChainConfig{ChainId: "test-chain", Nodes: []*NodeConfig{{Url: server.URL}}}
	if err := chain.newRpc(); err == nil {
		t.Fatal("node refusing websocket connections was accepted")
	}
	if !chain.hasNoNodes() {
		t.Fatal("chain with no usable endpoints was not marked as having no nodes")
	}
	if !chain.Nodes[0].snapshot().down {
		t.Fatal("node refusing websocket connections was not marked down")
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
		if block.find("ABCD") != (flag != blockIDFlagAbsent) {
			t.Fatalf("incorrect result for flag %d", flag)
		}
	}
	tracker := signingTracker{}
	tracker.consume(StatusUpdate{Height: 101, Status: StatusPrecommit})
	if status, _, ok := tracker.consume(StatusUpdate{Height: 100, Final: true, Status: Statusmissed}); !ok || status != Statusmissed {
		t.Fatal("future vote contaminated the previous block")
	}
	if status, _, ok := tracker.consume(StatusUpdate{Height: 101, Final: true, Status: Statusmissed}); !ok || status != StatusPrecommit {
		t.Fatal("matching vote was discarded")
	}
	if _, _, ok := tracker.consume(StatusUpdate{Height: 101, Final: true, Status: StatusSigned}); ok {
		t.Fatal("duplicate block was counted twice")
	}
	tracker.consume(StatusUpdate{Height: 102, Status: StatusProposed})
	if status, _, _ := tracker.consume(StatusUpdate{Height: 102, Final: true, Status: Statusmissed}); status != Statusmissed {
		t.Fatal("proposal alone was counted as signed")
	}
}

func TestBlockCommitHeightAndCanceledWorkers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	blocks := make(chan *WsReply)
	results := make(chan StatusUpdate)
	done := make(chan error, 1)
	go func() { done <- handleBlocks(ctx, blocks, make(chan *WsReply), results, "ABCD") }()
	reply := &WsReply{}
	reply.Result.Data.Value = json.RawMessage(`{"block":{"header":{"height":"101","proposer_address":"OTHER"},"last_commit":{"height":"100","signatures":[{"block_id_flag":1,"validator_address":"ABCD"}]}}}`)
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
	c.Discord.Enabled = yes(true)
	chain := &ChainConfig{name: "Chain", ChainId: "test-chain", ValAddress: "validator"}
	chain.Alerts.Discord.Enabled = yes(true)
	chain.Alerts.Discord.Webhook = endpoint
	c.Chains["Chain"] = chain
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

func TestQueueSendsWhenStateCannotBeSaved(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(http.StatusNoContent) }))
	defer server.Close()
	c := notificationConfig(t, server.URL)
	c.stateFile = filepath.Join(t.TempDir(), "missing", "state.json")
	c.alert("Chain", "missed blocks", "critical", false, nil)
	time.AfterFunc(time.Second, c.cancel)
	c.deliverQueue(alarms.Outbox[0].workerKey())
	if calls != 1 || len(alarms.Outbox) != 0 {
		t.Fatal("a state file problem held back the notification")
	}
}

func TestStateIsWrittenInPlaceWhenItCannotBeReplaced(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	c := auditConfig(t)
	directory := t.TempDir()
	c.stateFile = filepath.Join(directory, "state.json")
	if err := os.WriteFile(c.stateFile, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(directory, 0500); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(directory, 0700) }()
	c.Chains["Chain"] = &ChainConfig{lastBlockNum: 42}
	if err := c.persistState(); err != nil {
		t.Fatal(err)
	}
	var saved savedState
	data, err := os.ReadFile(c.stateFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &saved); err != nil || saved.Heights["Chain"] != 42 {
		t.Fatal("state was not written in place")
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

func TestStaleSigningInfoCannotClearSigningAlert(t *testing.T) {
	fastWatch(t)
	c := auditConfig(t)
	chain := &ChainConfig{name: "Chain", ChainId: "test-chain", valInfo: &ValInfo{Moniker: "validator", Bonded: true, Valcons: "consensus", Missed: 0, Window: 100, SigningStale: true}}
	chain.Alerts.MissedBlocksAlerts = true
	c.Chains["Chain"] = chain
	message := "validator is close to jail on test-chain"
	alarms.AllAlarms["Chain"] = map[string]time.Time{message: time.Now()}
	c.startWorker(chain.watch)
	select {
	case alert := <-c.alertChan:
		t.Fatalf("stale data changed the alert: %+v", alert)
	case <-time.After(250 * time.Millisecond):
	}
	c.cancel()
	c.workers.Wait()
	if !alarmIsActive("Chain", message) {
		t.Fatal("stale data cleared the signing alert")
	}
}

// a gap shouldn't count unseen blocks as missed, or reset the counter (that would clear an active alarm)
func TestReconnectGapKeepsConsecutiveMisses(t *testing.T) {
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
	if consecutive != 11 || blocks[0] != int(Statusmissed) || blocks[1] != -1 || blocks[2] != -1 || blocks[3] != -1 {
		t.Fatalf("reconnect gap changed consecutive misses to %v", consecutive)
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
	fastWatch(t)
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
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"result":{"data":{"type":"tendermint/event/NewBlock","value":{"block":{"header":{"height":"101"},"last_commit":{"height":"100","signatures":[{"validator_address":"0000000000000000000000000000000000000000","block_id_flag":1}]}}}}}}`))
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

func TestBrokenStateFileDoesNotStopStartup(t *testing.T) {
	oldAlarms := alarms
	defer func() { alarms = oldAlarms }()
	for name, contents := range map[string]string{"empty": "", "truncated": `{"alarms":{"sent_`, "invalid": "not json"} {
		t.Run(name, func(t *testing.T) {
			directory := t.TempDir()
			configFile, stateFile := filepath.Join(directory, "config.yml"), filepath.Join(directory, "state.json")
			config := "node_down_alert_minutes: 3\nchains:\n  Juno:\n    chain_id: juno-1\n    valoper_address: junovaloper1example\n    nodes:\n      - url: http://localhost:26657\n"
			if err := os.WriteFile(configFile, []byte(config), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(stateFile, []byte(contents), 0600); err != nil {
				t.Fatal(err)
			}
			password := ""
			c, err := loadConfig(configFile, stateFile, filepath.Join(directory, "chains.d"), &password)
			if err != nil {
				t.Fatalf("broken state file stopped startup: %v", err)
			}
			c.cancel()
			c.deliveryCancel()
		})
	}
}

func TestSavedConsecutiveMissesSurviveGapsInHistory(t *testing.T) {
	oldAlarms := alarms
	defer func() { alarms = oldAlarms }()
	directory := t.TempDir()
	configFile, stateFile := filepath.Join(directory, "config.yml"), filepath.Join(directory, "state.json")
	config := "node_down_alert_minutes: 3\nchains:\n  Juno:\n    chain_id: juno-1\n    valoper_address: junovaloper1example\n    nodes:\n      - url: http://localhost:26657\n"
	if err := os.WriteFile(configFile, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	blocks := make([]int, showBLocks)
	for i := range blocks {
		blocks[i] = int(StatusSigned)
	}
	blocks[0], blocks[1], blocks[2] = int(Statusmissed), -1, int(Statusmissed)
	data, _ := json.Marshal(savedState{Blocks: map[string][]int{"Juno": blocks}, Consecutive: map[string]float64{"Juno": 12}})
	if err := os.WriteFile(stateFile, data, 0600); err != nil {
		t.Fatal(err)
	}
	password := ""
	c, err := loadConfig(configFile, stateFile, filepath.Join(directory, "chains.d"), &password)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { c.cancel(); c.deliveryCancel() }()
	if _, _, missed := c.Chains["Juno"].blockState(); missed != 12 {
		t.Fatalf("restored %v consecutive misses instead of 12", missed)
	}
}

func TestRestoredBlockTimeDoesNotRaiseStallAlarm(t *testing.T) {
	fastWatch(t)
	c := auditConfig(t)
	chain := &ChainConfig{name: "Juno", ChainId: "juno-1", observedBlock: true, lastBlockTime: time.Now().Add(-time.Hour),
		valInfo: &ValInfo{Moniker: "validator", Bonded: true, Valcons: "junovalcons1example"}}
	chain.Alerts.StalledAlerts, chain.Alerts.Stalled = true, 1
	c.Chains["Juno"] = chain
	done := make(chan struct{})
	go func() { chain.watch(); close(done) }()
	defer func() { c.cancel(); <-done }()
	select {
	case alert := <-c.alertChan:
		t.Fatalf("restart raised an alert from a saved block time: %+v", alert)
	case <-time.After(300 * time.Millisecond):
	}
}

// watchFor runs watch on the chain and returns the alerts it raised in that time.
func watchFor(t *testing.T, c *Config, chain *ChainConfig, wait time.Duration) []*alertMsg {
	t.Helper()
	return watchUntil(t, c, chain, wait, nil)
}

// watchUntil stops early once enough is raised; a fixed wait is too short on a loaded race run.
func watchUntil(t *testing.T, c *Config, chain *ChainConfig, wait time.Duration, enough func([]*alertMsg) bool) []*alertMsg {
	t.Helper()
	fastWatch(t)
	ctx, cancel := context.WithCancel(context.Background())
	c.ctx = ctx
	c.Chains[chain.name] = chain
	done := make(chan struct{})
	go func() { chain.watch(); close(done) }()
	var raised []*alertMsg
	deadline := time.After(wait)
	for {
		select {
		case alert := <-c.alertChan:
			raised = append(raised, alert)
			if enough != nil && enough(raised) {
				cancel()
				<-done
				return raised
			}
		case <-deadline:
			cancel()
			<-done
			return raised
		}
	}
}

func TestNoRpcAlarmHasItsOwnDedupKey(t *testing.T) {
	c := auditConfig(t)
	chain := &ChainConfig{name: "Juno", ChainId: "juno-1", ValAddress: "junovaloper1example", noNodes: true}
	chain.Alerts.AlertIfNoServers = true
	raised := watchFor(t, c, chain, 200*time.Millisecond)
	if len(raised) != 1 || raised[0].uniqueId != "junovaloper1examplenorpc" {
		t.Fatalf("no RPC alarm used the wrong dedup key: %+v", raised)
	}
}

func TestRestoredDownNodeIsCheckedBeforeAlerting(t *testing.T) {
	c := auditConfig(t)
	c.NodeDownMin = 1
	node := &NodeConfig{Url: "http://node:26657", AlertIfDown: true, down: true, downSince: time.Now().Add(-time.Hour)}
	chain := &ChainConfig{name: "Juno", ChainId: "juno-1", ValAddress: "junovaloper1example", Nodes: []*NodeConfig{node},
		valInfo: &ValInfo{Moniker: "validator", Bonded: true, Valcons: "junovalcons1example"}}
	if raised := watchFor(t, c, chain, 300*time.Millisecond); len(raised) != 0 {
		t.Fatalf("node restored as down alerted before it was checked: %+v", raised)
	}
	node.markDown("still down", false)
	if raised := watchFor(t, c, chain, 300*time.Millisecond); len(raised) != 1 || raised[0].resolved {
		t.Fatalf("node that is still down did not alert: %+v", raised)
	}
}

func TestChainResetToLowerHeightIsTracked(t *testing.T) {
	tracker := signingTracker{finalized: 1_000_000}
	if _, _, ok := tracker.consume(StatusUpdate{Height: 999_990, Final: true, Status: StatusSigned}); ok {
		t.Fatal("block from an endpoint a few blocks behind was counted")
	}
	if status, _, ok := tracker.consume(StatusUpdate{Height: 5, Final: true, Status: StatusSigned}); !ok || status != StatusSigned {
		t.Fatal("blocks after a chain reset were ignored")
	}
}

func TestRestoredAlarmsNoLongerMonitoredAreCleared(t *testing.T) {
	c := auditConfig(t)
	chain := &ChainConfig{name: "Juno", ChainId: "juno-1", ValAddress: "junovaloper1example", observedBlock: true, lastBlockTime: time.Now(),
		valInfo: &ValInfo{Moniker: "validator", Bonded: true, Valcons: "junovalcons1example"}}
	chain.Alerts.ConsecutiveAlerts, chain.Alerts.ConsecutiveMissed = true, 10
	orphaned := map[string]string{
		"validator has missed 5 blocks on juno-1":                                               "junovalcons1exampleconsecutive", // threshold changed
		"stalled: have not seen a new block on juno-1 in 10 minutes":                            "junovalcons1example",            // alert disabled
		"Severity: critical\nRPC node http://old:26657 has been down for > 3 minutes on juno-1": "http://old:26657",
	}
	alarms.AllAlarms["Juno"] = make(map[string]time.Time)
	for message := range orphaned {
		alarms.AllAlarms["Juno"][message] = time.Now()
	}
	watchUntil(t, c, chain, 2*time.Second, func(raised []*alertMsg) bool {
		alert := raised[len(raised)-1]
		if id, ok := orphaned[alert.message]; ok && alert.resolved && alert.uniqueId == id {
			delete(orphaned, alert.message)
		}
		return len(orphaned) == 0
	})
	if len(orphaned) != 0 || alarms.getCount("Juno") != 0 {
		t.Fatalf("restored alarms were not cleared: %v", orphaned)
	}
}

func TestRefiredAlarmDropsItsPendingRecovery(t *testing.T) {
	c := notificationConfig(t, "http://127.0.0.1:1")
	key := "Chain\x00missed blocks"
	c.alert("Chain", "missed blocks", "critical", false, nil)
	alarms.SentDiAlarms[key] = time.Now()
	c.alert("Chain", "missed blocks", "info", true, nil)
	if alarms.PendingRecoveries[key] == nil {
		t.Fatal("recovery was not recorded")
	}
	c.alert("Chain", "missed blocks", "critical", false, nil)
	if alarms.PendingRecoveries[key] != nil {
		t.Fatal("alarm fired again but kept its old recovery")
	}
}

func TestRPCPrefersConfiguredOrder(t *testing.T) {
	c := auditConfig(t)
	first := httptest.NewServer(http.HandlerFunc(rpcStatus))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(rpcStatus))
	defer second.Close()
	chain := &ChainConfig{ChainId: "test-chain", Nodes: []*NodeConfig{{Url: first.URL}, {Url: second.URL}}}
	c.Chains["Chain"] = chain
	for i := 0; i < 3; i++ {
		if err := chain.newRpc(); err != nil {
			t.Fatal(err)
		}
		if chain.clientSnapshot().Remote() != first.URL {
			t.Fatal("reconnect moved away from the preferred node")
		}
		chain.setRpcSkip(first.URL, false)
	}
	chain.Nodes[0].markDown("down", false)
	if err := chain.newRpc(); err != nil {
		t.Fatal(err)
	}
	if chain.clientSnapshot().Remote() != second.URL {
		t.Fatal("known down node was tried before a healthy one")
	}
	chain.Nodes[1].markDown("down", false)
	if err := chain.newRpc(); err != nil {
		t.Fatal(err)
	}
	if chain.clientSnapshot().Remote() != first.URL || chain.Nodes[0].snapshot().down {
		t.Fatal("down nodes were not retried in order")
	}
}

func TestInactiveAtStartupAlerts(t *testing.T) {
	c := auditConfig(t)
	chain := &ChainConfig{name: "Juno", ChainId: "juno-1", ValAddress: "junovaloper1example", observedBlock: true, lastBlockTime: time.Now(),
		valInfo: &ValInfo{Moniker: "validator", Valcons: "junovalcons1example"}}
	chain.Alerts.MissedBlocksAlerts, chain.Alerts.AlertIfInactive = true, true
	if raised := watchFor(t, c, chain, 300*time.Millisecond); len(raised) != 1 || !strings.Contains(raised[0].message, "active set") {
		t.Fatalf("validator outside the active set did not alert at startup: %+v", raised)
	}
	chain.validatorMux.Lock()
	chain.valInfo.Jailed = true
	chain.validatorMux.Unlock()
	if raised := watchFor(t, c, chain, 300*time.Millisecond); len(raised) != 2 || !raised[0].quiet || !strings.Contains(raised[1].message, "jailed") {
		t.Fatalf("jailed validator did not alert: %+v", raised)
	}
}

func TestOldConfigsStillLoad(t *testing.T) {
	oldAlarms := alarms
	defer func() { alarms = oldAlarms }()
	directory := t.TempDir()
	chains := filepath.Join(directory, "chains.d")
	if err := os.Mkdir(chains, 0700); err != nil {
		t.Fatal(err)
	}
	// unknown setting and no node_down_alert_minutes
	config := "enable_dashbord: yes\nchains:\n  Juno:\n    chain_id: juno-1\n    valoper_address: junovaloper1example\n    nodes:\n      - url: http://localhost:26657\n"
	files := map[string]string{
		filepath.Join(directory, "config.yml"):       config,
		filepath.Join(chains, "Osmosis.mainnet.yml"): "chain_id: osmosis-1\nvaloper_address: osmovaloper1example\nnodes:\n  - url: http://localhost:26657\n",
		filepath.Join(chains, "Juno.yml"):            "chain_id: juno-2\nvaloper_address: junovaloper1example\nnodes:\n  - url: http://localhost:26657\n",
	}
	for name, contents := range files {
		if err := os.WriteFile(name, []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
	}
	password := ""
	c, err := loadConfig(filepath.Join(directory, "config.yml"), filepath.Join(directory, "state.json"), chains, &password)
	if err != nil {
		t.Fatalf("old config was rejected: %v", err)
	}
	defer func() { c.cancel(); c.deliveryCancel() }()
	if fatal, problems := validateConfig(c); fatal {
		t.Fatalf("old config was rejected: %v", problems)
	}
	if c.NodeDownMin != 3 || c.Chains["Osmosis"] == nil || c.Chains["Juno"] == nil || c.Chains["Juno"].ChainId != "juno-2" {
		t.Fatalf("old config was not loaded the same way: %d %v", c.NodeDownMin, c.Chains)
	}
}

func TestRefreshReusesSlashingWindow(t *testing.T) {
	auditConfig(t)
	address, _ := bech32.ConvertAndEncode("cosmosvalcons", make([]byte, 20))
	signing, _ := (&slashing.QuerySigningInfoResponse{ValSigningInfo: slashing.ValidatorSigningInfo{Address: address}}).Marshal()
	params, _ := (&slashing.QueryParamsResponse{Params: slashing.Params{SignedBlocksWindow: 1000}}).Marshal()
	var mux sync.Mutex
	paramQueries := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			ID     interface{}       `json:"id"`
			Params map[string]string `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		value := signing
		if request.Params["path"] == "/cosmos.slashing.v1beta1.Query/Params" {
			mux.Lock()
			paramQueries++
			mux.Unlock()
			value = params
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"jsonrpc": "2.0", "id": request.ID, "result": map[string]interface{}{"response": map[string]interface{}{"code": 0, "value": value}}})
	}))
	defer server.Close()
	client, _ := rpchttp.New(server.URL, "/websocket")
	// Token display metadata is unrelated to slashing-window reuse.
	chain := &ChainConfig{ValAddress: address, client: client, bondTokenAt: time.Now()}
	for _, first := range []bool{true, false, false, true} {
		if err := chain.GetValInfo(first); err != nil {
			t.Fatal(err)
		}
	}
	info, _ := chain.validatorState()
	if paramQueries != 2 || info.Window != 1000 {
		t.Fatalf("slashing params were queried %d times, expected only when connecting", paramQueries)
	}
}

func TestQueuedNotificationIsOnlySavedOnce(t *testing.T) {
	c := notificationConfig(t, "http://127.0.0.1:1")
	c.alert("Chain", "missed blocks", "critical", false, nil)
	if !c.queueSaved(alarms.Outbox[0].Sequence) {
		t.Fatal("queued notification was not recorded as saved")
	}
	alarms.notifyMux.Lock()
	alarms.NextNotification++
	alarms.notifyMux.Unlock()
	if c.queueSaved(alarms.NextNotification) {
		t.Fatal("unsaved notification was reported as saved")
	}
}

func TestDashboardOnlyUpdatedOnChange(t *testing.T) {
	c := auditConfig(t)
	c.EnableDash = true
	c.updateChan = make(chan *dash.ChainStatus, 16)
	chain := &ChainConfig{name: "Juno", ChainId: "juno-1", ValAddress: "junovaloper1example", blocksResults: make([]int, showBLocks),
		valInfo: &ValInfo{Moniker: "validator", Bonded: true, Valcons: "junovalcons1example"}}
	watchFor(t, c, chain, 500*time.Millisecond)
	if len(c.updateChan) != 1 {
		t.Fatalf("dashboard was sent %d updates for an unchanged chain", len(c.updateChan))
	}
}

func TestValidatorNodesLoadFromConfig(t *testing.T) {
	oldAlarms := alarms
	defer func() { alarms = oldAlarms }()
	directory := t.TempDir()
	config := `node_down_alert_minutes: 3
chains:
  Realio:
    chain_id: realionetwork_3301-1
    validators:
      - name: RIO
        valoper_address: realiovaloper1rio
        nodes:
          - url: http://node-a:26657
            alert_if_down: yes
      - name: dstrx
        valoper_address: realiovaloper1dstrx
        nodes:
          - url: http://node-b:26657
            alert_if_down: yes
`
	if err := os.WriteFile(filepath.Join(directory, "config.yml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	password := ""
	c, err := loadConfig(filepath.Join(directory, "config.yml"), filepath.Join(directory, "state.json"), filepath.Join(directory, "chains.d"), &password)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { c.cancel(); c.deliveryCancel() }()
	if fatal, problems := validateConfig(c); fatal {
		t.Fatalf("validators with their own nodes were rejected: %v", problems)
	}
	dstrx := c.Chains["Realio / dstrx"]
	if dstrx == nil || len(dstrx.Nodes) != 1 || dstrx.Nodes[0].Url != "http://node-b:26657" {
		t.Fatalf("dstrx did not get node B: %+v", dstrx)
	}
}

func TestIncompleteAlertSettingsOnlyWarn(t *testing.T) {
	c := &Config{Listen: "8888", NodeDownMin: 3, Discord: DiscordConfig{Enabled: yes(true)}, Chains: map[string]*ChainConfig{
		"Juno": {ChainId: "juno-1", ValAddress: "junovaloper1example", Nodes: []*NodeConfig{{Url: "not a url"}}},
	}}
	alerts := &c.Chains["Juno"].Alerts
	alerts.Discord.Enabled = yes(true)
	alerts.StalledAlerts, alerts.MissedBlocksAlerts, alerts.ConsecutiveAlerts = true, true, true
	fatal, problems := validateConfig(c)
	if fatal {
		t.Fatalf("settings older versions accepted stopped startup: %v", problems)
	}
	if alerts.Stalled != 10 || alerts.ConsecutiveMissed != 5 || alerts.JailRiskPercent != 90 {
		t.Fatalf("zero thresholds were not replaced: %+v", alerts)
	}
	if !strings.Contains(strings.Join(problems, "\n"), "discord alerts need a valid webhook URL") {
		t.Fatalf("missing webhook was not reported: %v", problems)
	}
}

func TestReadyWhileSomeValidatorsAreMonitored(t *testing.T) {
	c := auditConfig(t)
	c.Chains["A"] = &ChainConfig{}
	c.Chains["B"] = &ChainConfig{}
	if c.healthStatus().Ready {
		t.Fatal("ready before anything is monitored")
	}
	c.Chains["A"].setMonitoring(true)
	if status := c.healthStatus(); !status.Ready || status.Monitoring != 1 || status.Validators != 2 {
		t.Fatalf("one validator's node being down marked the monitor as not ready: %+v", status)
	}
}

func TestRejectedWebsocketSaysWhy(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "Bad Request", http.StatusBadRequest)
	}))
	defer server.Close()
	_, err := newClientContext(context.Background(), server.URL, true)
	if err == nil || !strings.Contains(err.Error(), "refused the websocket connection (HTTP 400)") {
		t.Fatalf("got %v", err)
	}
}

const opPDKey = "0123456789abcdef0123456789abcdef"

var opDests = []string{"telegram", "pagerduty", "discord", "slack", "gotify"}

// opRestartRig restarts tenderduty from its config and state file, and counts what each destination got.
type opRestartRig struct {
	t          *testing.T
	h          *groupHarness
	gotify     *httptest.Server
	gmu        sync.Mutex
	gt         []GotifyMessage
	configFile string
	stateFile  string
	chains     string
	tgMin      string
	extra      string
	current    *Config
}

type opSent struct{ raised, recovered int }

func newOpRestartRig(t *testing.T) *opRestartRig {
	t.Helper()
	h := newGroupHarness(t, map[string]string{"a1": hostAIP, "a2": hostAIP, "b2": hostBIP})
	r := &opRestartRig{t: t, h: h}
	r.gotify = httptest.NewServer(http.HandlerFunc(r.serveGotify))
	t.Cleanup(r.gotify.Close)
	dir := t.TempDir()
	r.configFile = filepath.Join(dir, "config.yml")
	r.stateFile = filepath.Join(dir, "state.json")
	r.chains = filepath.Join(dir, "chains.d")
	if err := os.Mkdir(r.chains, 0700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if r.current != nil {
			r.stop(r.current)
		}
	})
	return r
}

func (r *opRestartRig) serveGotify(w http.ResponseWriter, req *http.Request) {
	if req.URL.Path != "/message" || req.Header.Get("X-Gotify-Key") != gotifyTestToken {
		http.NotFound(w, req)
		return
	}
	var m GotifyMessage
	_ = json.NewDecoder(req.Body).Decode(&m)
	r.gmu.Lock()
	r.gt = append(r.gt, m)
	r.gmu.Unlock()
	_, _ = w.Write([]byte(`{"id":1}`))
}

// yaml enables the given destinations for every chain; extra and tgMin are top-level settings.
func (r *opRestartRig) yaml(dests ...string) string {
	tg := "telegram:\n  enabled: yes\n  api_key: \"" + testToken + "\"\n  channel: \"@one\"\n"
	if r.tgMin != "" {
		tg += "  min_severity: " + r.tgMin + "\n"
	}
	frag := map[string]string{
		"telegram":  tg,
		"discord":   "discord:\n  enabled: yes\n  webhook: \"" + r.h.server.URL + "/discord\"\n",
		"slack":     "slack:\n  enabled: yes\n  webhook: \"" + r.h.server.URL + "/slack\"\n",
		"gotify":    "gotify:\n  enabled: yes\n  url: \"" + r.gotify.URL + "\"\n  token: \"" + gotifyTestToken + "\"\n",
		"pagerduty": "pagerduty:\n  enabled: yes\n  api_key: \"" + opPDKey + "\"\n",
	}
	var b strings.Builder
	b.WriteString("node_down_alert_minutes: 3\n" + r.extra)
	for _, d := range dests {
		b.WriteString(frag[d])
	}
	b.WriteString("chains:\n" +
		"  AlphaOne:\n    chain_id: alphaone-1\n    valoper_address: alphaonevaloper\n    nodes:\n      - url: http://a1:26657\n        alert_if_down: yes\n" +
		"  AlphaTwo:\n    chain_id: alphatwo-1\n    valoper_address: alphatwovaloper\n    nodes:\n      - url: http://a2:26657\n        alert_if_down: yes\n")
	return b.String()
}

func (r *opRestartRig) write(content string) {
	r.t.Helper()
	if err := os.WriteFile(r.configFile, []byte(content), 0600); err != nil {
		r.t.Fatal(err)
	}
}

func (r *opRestartRig) writeChain(name, content string) {
	r.t.Helper()
	if err := os.WriteFile(filepath.Join(r.chains, name+".yml"), []byte(content), 0600); err != nil {
		r.t.Fatal(err)
	}
}

// start loads config and state like startup does; nothing is delivered until boot.
func (r *opRestartRig) start(yaml string) *Config {
	r.t.Helper()
	r.write(yaml)
	password := ""
	c, err := loadConfig(r.configFile, r.stateFile, r.chains, &password)
	if err != nil {
		r.t.Fatal(err)
	}
	if fatal, problems := validateConfig(c); fatal {
		r.t.Fatal(problems)
	}
	for _, cc := range c.Chains {
		cc.restoredChecked = true
	}
	td, r.h.c, r.current = c, c, c
	return c
}

// boot does what run does after the monitors start: recovery queue first, then the workers.
func (r *opRestartRig) boot(c *Config) {
	c.restoreRecoveries()
	c.startWorker(c.runNotifications)
}

func (r *opRestartRig) stop(c *Config) {
	c.cancel()
	c.deliveryCancel()
	c.workers.Wait()
	r.current = nil
}

// restart saves the state as a clean shutdown would, then stops.
func (r *opRestartRig) restart(c *Config) {
	r.t.Helper()
	if err := c.persistState(); err != nil {
		r.t.Fatal(err)
	}
	r.stop(c)
}

func (r *opRestartRig) reload(c *Config) error {
	password := ""
	return c.reload(r.configFile, r.chains, &password)
}

// settle waits until the queue is empty and nothing more arrives shortly after.
func (r *opRestartRig) settle() {
	r.h.waitFor("the outbox to drain", r.h.drained)
	time.Sleep(4 * groupUnit)
	r.h.waitFor("the outbox to drain", r.h.drained)
}

// opRecovery spots a recovery in the text each destination receives.
func opRecovery(text string) bool {
	for _, mark := range []string{"Resolved", "Alert stopped", "No longer watched", "back up"} {
		if strings.Contains(text, mark) {
			return true
		}
	}
	return false
}

func (r *opRestartRig) sent() map[string]opSent {
	out := make(map[string]opSent)
	for _, d := range opDests {
		out[d] = opSent{}
	}
	add := func(dest string, recovered bool) {
		s := out[dest]
		if recovered {
			s.recovered++
		} else {
			s.raised++
		}
		out[dest] = s
	}
	r.h.mux.Lock()
	for _, m := range r.h.telegram {
		add("telegram", opRecovery(m.text))
	}
	for _, e := range r.h.pd {
		add("pagerduty", e.Action == "resolve")
	}
	for _, m := range r.h.discord {
		add("discord", opRecovery(m.Content))
	}
	for _, m := range r.h.slack {
		add("slack", opRecovery(m.Text))
	}
	r.h.mux.Unlock()
	r.gmu.Lock()
	for _, m := range r.gt {
		add("gotify", opRecovery(m.Title))
	}
	r.gmu.Unlock()
	return out
}

// expect compares what each destination got; destinations missing from want must get nothing.
func (r *opRestartRig) expect(what string, want map[string]opSent) {
	r.t.Helper()
	for _, d := range opDests {
		if _, ok := want[d]; !ok {
			want[d] = opSent{}
		}
	}
	if got := r.sent(); !reflect.DeepEqual(got, want) {
		r.t.Errorf("%s: sent %v, want %v", what, got, want)
	}
}

func opAll(raised, recovered int) map[string]opSent {
	out := make(map[string]opSent)
	for _, d := range opDests {
		out[d] = opSent{raised, recovered}
	}
	return out
}

func (r *opRestartRig) pdKeys() (triggers, resolves []string) {
	r.h.mux.Lock()
	defer r.h.mux.Unlock()
	for _, e := range r.h.pd {
		if e.Action == "resolve" {
			resolves = append(resolves, e.Key)
		} else {
			triggers = append(triggers, e.Key)
		}
	}
	return triggers, resolves
}

func opOutboxLen() int {
	alarms.notifyMux.RLock()
	defer alarms.notifyMux.RUnlock()
	return len(alarms.Outbox)
}

// opAgeSent moves every delivery record back, as if it had been sent that long ago.
func opAgeSent(age time.Duration) {
	alarms.notifyMux.Lock()
	defer alarms.notifyMux.Unlock()
	for _, sent := range []map[string]time.Time{alarms.SentPdAlarms, alarms.SentTgAlarms, alarms.SentDiAlarms, alarms.SentSlkAlarms, alarms.SentGtAlarms} {
		for k := range sent {
			sent[k] = time.Now().Add(-age)
		}
	}
}

// Every destination already got the alarm and tenderduty restarts: neither check resends it.
func TestOpRestartNoResendAnywhereAfterRestore(t *testing.T) {
	r := newOpRestartRig(t)
	c := r.start(r.yaml(opDests...))
	r.boot(c)
	r.h.down("AlphaOne")
	r.settle()
	r.expect("before restart", opAll(1, 0))
	r.restart(c)

	c = r.start(r.yaml(opDests...))
	r.boot(c)
	r.settle()
	r.h.down("AlphaOne")
	r.settle()
	r.expect("after restart", opAll(1, 0))
}

// Died after the raise was queued, before sending: each destination gets it once, then never again.
func TestOpRestartSendsRaiseLeftInOutboxOnce(t *testing.T) {
	r := newOpRestartRig(t)
	c := r.start(r.yaml(opDests...))
	r.h.down("AlphaOne")
	if n := opOutboxLen(); n != len(opDests) {
		t.Fatalf("%d queued, want %d", n, len(opDests))
	}
	r.restart(c)

	c = r.start(r.yaml(opDests...))
	r.boot(c)
	r.settle()
	r.expect("first start", opAll(1, 0))
	r.restart(c)

	c = r.start(r.yaml(opDests...))
	r.boot(c)
	r.settle()
	r.expect("second start", opAll(1, 0))
}

// Four destinations got the raise, slack was added while down: one recovery each, none for slack.
func TestOpRestartResolvedWhileDownOneRecoveryPerRaise(t *testing.T) {
	r := newOpRestartRig(t)
	c := r.start(r.yaml("telegram", "pagerduty", "discord", "gotify"))
	r.boot(c)
	r.h.down("AlphaOne")
	r.settle()
	r.restart(c)

	c = r.start(r.yaml(opDests...))
	r.boot(c)
	r.h.up("AlphaOne")
	r.h.up("AlphaOne")
	r.settle()
	want := opAll(1, 1)
	want["slack"] = opSent{}
	r.expect("after recovery", want)
}

// PagerDuty opened with the valcons key: after a restart the recovery must still close with that key.
func TestOpRestartPagerDutyResolveUsesTriggerKey(t *testing.T) {
	r := newOpRestartRig(t)
	msg := nodeDownMessage("http://a1:26657", "alphaone-1")
	trigger := "valcons-consensus-key"
	c := r.start(r.yaml("pagerduty"))
	r.boot(c)
	c.alert("AlphaOne", msg, "critical", false, &trigger)
	r.settle()
	r.restart(c)

	c = r.start(r.yaml("pagerduty"))
	r.boot(c)
	nodeKey := "http://a1:26657"
	c.alert("AlphaOne", msg, "info", true, &nodeKey)
	r.settle()
	triggers, resolves := r.pdKeys()
	if !reflect.DeepEqual(triggers, []string{trigger}) || !reflect.DeepEqual(resolves, []string{trigger}) {
		t.Fatalf("triggers %v, resolves %v, want both %q", triggers, resolves, trigger)
	}
}

// Node recovered before tenderduty sent the recovery: after restart each destination gets one, then none.
func TestOpRestartQueuedRecoveryDeliveredOnce(t *testing.T) {
	r := newOpRestartRig(t)
	c := r.start(r.yaml(opDests...))
	r.boot(c)
	r.h.down("AlphaOne")
	r.settle()
	r.restart(c)

	c = r.start(r.yaml(opDests...))
	r.h.up("AlphaOne")
	r.restart(c)

	c = r.start(r.yaml(opDests...))
	r.boot(c)
	r.settle()
	r.expect("first start", opAll(1, 1))
	r.restart(c)

	c = r.start(r.yaml(opDests...))
	r.boot(c)
	r.settle()
	r.expect("second start", opAll(1, 1))
}

// Grouped card still waiting at restart: one card with both nodes per destination, and no repeat.
func TestOpRestartGroupedCardAcrossRestart(t *testing.T) {
	r := newOpRestartRig(t)
	r.extra = "group_node_alerts: yes\ngroup_wait_seconds: 2\n"
	c := r.start(r.yaml("telegram", "discord"))
	r.h.down("AlphaOne")
	r.h.down("AlphaTwo")
	r.restart(c)

	c = r.start(r.yaml("telegram", "discord"))
	r.boot(c)
	r.settle()
	sent := r.h.telegramSent()
	if len(sent) != 1 || !strings.Contains(sent[0].text, "2 nodes down") {
		t.Fatalf("telegram got %+v, want one card with 2 nodes down", sent)
	}
	r.expect("first start", map[string]opSent{"telegram": {1, 0}, "discord": {1, 0}})
	r.restart(c)

	c = r.start(r.yaml("telegram", "discord"))
	r.boot(c)
	r.settle()
	r.expect("second start", map[string]opSent{"telegram": {1, 0}, "discord": {1, 0}})
}

// Alarm delivered a day ago: after restart it is still active but must not be announced again.
func TestOpRestartAlarmOlderThanADayIsNotRaisedAgain(t *testing.T) {
	r := newOpRestartRig(t)
	c := r.start(r.yaml(opDests...))
	r.boot(c)
	msg, id := nodeDownMessage("http://a1:26657", "alphaone-1"), "http://a1:26657"
	c.recordSince("AlphaOne", msg, "critical", false, false, &id, time.Now().Add(-25*time.Hour))
	r.settle()
	r.expect("raised a day ago", opAll(1, 0))
	opAgeSent(25 * time.Hour)
	r.restart(c)

	c = r.start(r.yaml(opDests...))
	r.boot(c)
	r.h.down("AlphaOne") // reconcile pass, the alarm is still active
	r.settle()
	r.expect("after restart", opAll(1, 0))
}

// Node came back while down, alarm raised a day ago: every destination that got it gets a recovery.
func TestOpRestartAlarmOlderThanADayRecoversEverywhere(t *testing.T) {
	r := newOpRestartRig(t)
	c := r.start(r.yaml(opDests...))
	r.boot(c)
	msg, id := nodeDownMessage("http://a1:26657", "alphaone-1"), "http://a1:26657"
	c.recordSince("AlphaOne", msg, "critical", false, false, &id, time.Now().Add(-25*time.Hour))
	r.settle()
	opAgeSent(25 * time.Hour)
	r.restart(c)

	c = r.start(r.yaml(opDests...))
	r.boot(c)
	r.h.up("AlphaOne")
	r.settle()
	r.expect("after recovery", opAll(1, 1))
}

// A chain file added while running: the new chain is watched, the active alarm stays.
func TestOpReloadAddedChainLeavesRunningAlarms(t *testing.T) {
	r := newOpRestartRig(t)
	c := r.start(r.yaml("telegram"))
	r.boot(c)
	r.h.down("AlphaOne")
	r.settle()
	r.expect("before reload", map[string]opSent{"telegram": {1, 0}})

	r.writeChain("betatwo", "chain_id: betatwo-1\nvaloper_address: betatwovaloper\nnodes:\n  - url: http://127.0.0.1:1\n    alert_if_down: yes\n")
	if err := r.reload(c); err != nil {
		t.Fatal(err)
	}
	c.chainsMux.RLock()
	added := c.Chains["betatwo"]
	c.chainsMux.RUnlock()
	if added == nil || added.cancel == nil {
		t.Fatal("added chain is not monitored")
	}
	if !alarmIsActive("AlphaOne", nodeDownMessage("http://a1:26657", "alphaone-1")) {
		t.Fatal("reload changed the active alarm")
	}
	r.settle()
	r.expect("after reload", map[string]opSent{"telegram": {1, 0}})
	r.stop(c)
}

// A chain file removed while running: its alarm clears once, and a second reload sends nothing.
func TestOpReloadRemovedChainClearsOnce(t *testing.T) {
	r := newOpRestartRig(t)
	r.writeChain("betatwo", "chain_id: betatwo-1\nvaloper_address: betatwovaloper\nnodes:\n  - url: http://127.0.0.1:1\n    alert_if_down: yes\n")
	c := r.start(r.yaml("telegram"))
	r.boot(c)
	r.h.down("AlphaOne")
	r.h.down("betatwo")
	r.settle()
	r.expect("before removal", map[string]opSent{"telegram": {2, 0}})

	if err := os.Remove(filepath.Join(r.chains, "betatwo.yml")); err != nil {
		t.Fatal(err)
	}
	if err := r.reload(c); err != nil {
		t.Fatal(err)
	}
	r.settle()
	r.expect("after removal", map[string]opSent{"telegram": {2, 1}})
	if err := r.reload(c); err != nil {
		t.Fatal(err)
	}
	r.settle()
	r.expect("after a second reload", map[string]opSent{"telegram": {2, 1}})
	if alarmIsActive("betatwo", nodeDownMessage("http://127.0.0.1:1", "betatwo-1")) {
		t.Fatal("removed chain's alarm is still active")
	}
	r.stop(c)
}

// A broken config: the old one keeps running, the error is reported once, new alarms still go out.
func TestOpReloadBrokenYAMLKeepsMonitoring(t *testing.T) {
	r := newOpRestartRig(t)
	c := r.start(r.yaml("telegram"))
	r.boot(c)
	r.h.down("AlphaOne")
	r.settle()
	before := c.settings()

	r.write("telegram: [\n")
	if err := r.reload(c); err == nil {
		t.Fatal("broken YAML was accepted")
	}
	if c.settings() != before {
		t.Fatal("the failed reload replaced the running settings")
	}
	id := "warn-id"
	c.alert("AlphaOne", "validator has missed 5 blocks on alphaone-1", "critical", false, &id)
	r.settle()
	sent := r.h.telegramSent()
	notice := false
	for _, m := range sent {
		notice = notice || strings.Contains(m.text, "Config reload failed")
	}
	if !notice {
		t.Fatalf("no reload error reached telegram: %+v", sent)
	}
	r.expect("after broken reload", map[string]opSent{"telegram": {3, 0}})
	r.stop(c)
}

// telegram min_severity raised to critical, then lowered to warning: only later warnings go out.
func TestOpReloadMinSeverityChange(t *testing.T) {
	r := newOpRestartRig(t)
	r.tgMin = "critical"
	c := r.start(r.yaml("telegram"))
	r.boot(c)
	warn, warnID := "validator has missed 5 blocks on alphaone-1", "warn-id"
	c.alert("AlphaOne", warn, "warning", false, &warnID)
	r.settle()
	r.expect("warning at critical", map[string]opSent{})

	r.tgMin = "warning"
	r.write(r.yaml("telegram"))
	if err := r.reload(c); err != nil {
		t.Fatal(err)
	}
	next, nextID := "validator has missed 7 blocks on alphaone-1", "next-id"
	c.alert("AlphaOne", next, "warning", false, &nextID)
	r.settle()
	r.expect("new warning after lowering", map[string]opSent{"telegram": {1, 0}})

	c.alert("AlphaOne", warn, "info", true, &warnID)
	r.settle()
	r.expect("recovery of the warning never sent", map[string]opSent{"telegram": {1, 0}})
	r.stop(c)
}

// Two quick reloads add the same chain once and keep its state; removing it clears the alarm once.
func TestOpReloadTwiceQuickly(t *testing.T) {
	r := newOpRestartRig(t)
	c := r.start(r.yaml("telegram"))
	r.boot(c)
	r.writeChain("betatwo", "chain_id: betatwo-1\nvaloper_address: betatwovaloper\nnodes:\n  - url: http://127.0.0.1:1\n    alert_if_down: yes\n")
	if err := r.reload(c); err != nil {
		t.Fatal(err)
	}
	c.chainsMux.RLock()
	first := c.Chains["betatwo"]
	c.chainsMux.RUnlock()
	if err := r.reload(c); err != nil {
		t.Fatal(err)
	}
	c.chainsMux.RLock()
	second := c.Chains["betatwo"]
	c.chainsMux.RUnlock()
	if first == nil || first != second {
		t.Fatal("second reload restarted the chain it just added")
	}
	r.h.down("betatwo")
	r.settle()

	if err := os.Remove(filepath.Join(r.chains, "betatwo.yml")); err != nil {
		t.Fatal(err)
	}
	if err := r.reload(c); err != nil {
		t.Fatal(err)
	}
	if err := r.reload(c); err != nil {
		t.Fatal(err)
	}
	r.settle()
	r.expect("after removal", map[string]opSent{"telegram": {1, 1}})
	r.stop(c)
}

// A PID file left by an exited process: reload is refused and nothing is signalled.
func TestOpReloadDeadPIDIsRefused(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	if err := cmd.Run(); err != nil {
		t.Skipf("could not run a child process: %v", err)
	}
	state := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(state+".pid", []byte(strconv.Itoa(cmd.Process.Pid)), 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	reloadAfterEdit(state, &out, nil)
	if !strings.Contains(out.String(), "Saved, but reload failed") || strings.Contains(out.String(), "Applied") {
		t.Fatalf("dead PID reported as: %q", out.String())
	}
}
