package tenderduty

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	dash "github.com/kynraze/tenderduty/v2/td2/dashboard"
	rpchttp "github.com/tendermint/tendermint/rpc/client/http"
	"google.golang.org/protobuf/encoding/protowire"
)

// A node whose Proposals queries fail like Shentu's, answering store reads from store (hex data to value).
func govStoreNode(t *testing.T, store map[string][]byte, votedOn string) *rpchttp.HTTP {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			ID     interface{} `json:"id"`
			Params struct {
				Path string `json:"path"`
				Data string `json:"data"`
			} `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		response := map[string]interface{}{"code": 6, "log": "no concrete type registered for type URL /cosmwasm.wasm.v1.MsgUpdateParams"}
		value, ok := store[request.Params.Path+" "+strings.ToLower(request.Params.Data)]
		if votedOn != "" && request.Params.Path == "/"+votedOn+".Query/Vote" {
			value, ok = []byte{1}, true
		}
		if ok {
			response = map[string]interface{}{"code": 0, "value": base64.StdEncoding.EncodeToString(value)}
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"jsonrpc": "2.0", "id": request.ID, "result": map[string]interface{}{"response": response}})
	}))
	t.Cleanup(server.Close)
	client, err := rpchttp.New(server.URL, "/websocket")
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestGetProposalsFallsBackToTheGovStore(t *testing.T) {
	end := time.Date(2026, 10, 21, 1, 12, 5, 0, time.UTC)
	timestamp := func(at time.Time) []byte {
		return protowire.AppendVarint(protowire.AppendTag(nil, 1, protowire.VarintType), uint64(at.Unix()))
	}
	id := []byte{0, 0, 0, 0, 0, 0, 0, 57}
	proposal := protowire.AppendVarint(protowire.AppendTag(nil, 1, protowire.VarintType), 57)
	proposal = protowire.AppendBytes(protowire.AppendTag(proposal, 2, protowire.BytesType), protowire.AppendString(protowire.AppendTag(nil, 1, protowire.BytesType), "/cosmwasm.wasm.v1.MsgUpdateParams"))
	proposal = protowire.AppendBytes(protowire.AppendTag(proposal, 8, protowire.BytesType), timestamp(end.Add(-72*time.Hour)))
	proposal = protowire.AppendBytes(protowire.AppendTag(proposal, 9, protowire.BytesType), timestamp(end))
	proposal = protowire.AppendString(protowire.AppendTag(proposal, 11, protowire.BytesType), "Software upgrade")
	// active queue entry: 0x01 | time | id, valued with the id
	pair := protowire.AppendBytes(protowire.AppendTag(nil, 1, protowire.BytesType), append(append([]byte{1}, "2026-10-21T01:12:05.000000000"...), id...))
	pair = protowire.AppendBytes(protowire.AppendTag(pair, 2, protowire.BytesType), id)
	queue := protowire.AppendBytes(protowire.AppendTag(nil, 1, protowire.BytesType), pair)
	key := "/store/gov/key " + hex.EncodeToString(append([]byte{0}, id...))

	for _, tc := range []struct {
		name    string
		store   map[string][]byte
		votedOn string
		want    int
		err     bool
	}{
		{"open proposal", map[string][]byte{"/store/gov/subspace 01": queue, key: proposal}, "", 1, false},
		{"voted", map[string][]byte{"/store/gov/subspace 01": queue, key: proposal}, "cosmos.gov.v1", 1, false},
		{"voted on a chain without gov v1", map[string][]byte{"/store/gov/subspace 01": queue, key: proposal}, "cosmos.gov.v1beta1", 1, false},
		{"nothing in voting", map[string][]byte{"/store/gov/subspace 01": {}}, "", 0, false},
		{"store unreadable too", nil, "", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			proposals, err := getProposals(context.Background(), govStoreNode(t, tc.store, tc.votedOn), "shentu1voter")
			if (err != nil) != tc.err || len(proposals) != tc.want {
				t.Fatalf("got %+v, %v", proposals, err)
			}
			if tc.want == 1 {
				p := proposals[0]
				if p.ID != 57 || p.Title != "Software upgrade" || !p.VotingEnd.Equal(end) || p.Voted != (tc.votedOn != "") {
					t.Fatalf("wrong proposal %+v", p)
				}
			}
		})
	}
}

func field(b []byte, num protowire.Number, value []byte) []byte {
	return protowire.AppendBytes(protowire.AppendTag(b, num, protowire.BytesType), value)
}

func varintField(b []byte, num protowire.Number, value uint64) []byte {
	return protowire.AppendVarint(protowire.AppendTag(b, num, protowire.VarintType), value)
}

func TestParseProposals(t *testing.T) {
	start := varintField(nil, 1, 1_789_395_200)
	end := varintField(nil, 1, 1_790_000_000)
	v1 := varintField(nil, 1, 12)
	v1 = varintField(v1, 3, proposalStatusVoting)
	v1 = field(v1, 8, start)
	v1 = field(v1, 9, end)
	v1 = field(v1, 11, []byte("Enable IBC transfers"))
	got, err := parseProposals(field(nil, 1, v1), false)
	if err != nil || len(got) != 1 || got[0].ID != 12 || got[0].Title != "Enable IBC transfers" || !got[0].VotingEnd.Equal(time.Unix(1_790_000_000, 0)) || !got[0].VotingStart.Equal(time.Unix(1_789_395_200, 0)) {
		t.Fatalf("v1 proposal: %+v, %v", got, err)
	}

	withoutStart := field(varintField(nil, 1, 13), 9, end)
	got, err = parseProposals(field(nil, 1, withoutStart), false)
	if err != nil || len(got) != 1 || !got[0].VotingStart.IsZero() {
		t.Fatalf("missing start time: %+v, %v", got, err)
	}

	// v1beta1 keeps the title inside the content Any
	content := field(nil, 1, []byte("/cosmos.gov.v1beta1.TextProposal"))
	content = field(content, 2, field(nil, 1, []byte("Old style")))
	legacy := field(varintField(nil, 1, 7), 2, content)
	legacy = field(legacy, 8, start)
	legacy = field(legacy, 9, end)
	got, err = parseProposals(field(nil, 1, legacy), true)
	if err != nil || len(got) != 1 || got[0].ID != 7 || got[0].Title != "Old style" || !got[0].VotingStart.Equal(time.Unix(1_789_395_200, 0)) || !got[0].VotingEnd.Equal(time.Unix(1_790_000_000, 0)) {
		t.Fatalf("v1beta1 proposal: %+v, %v", got, err)
	}
}

func TestVoterAddress(t *testing.T) {
	if got := voterAddress("atonevaloper1zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3dqqvw0"); got[:6] != "atone1" {
		t.Fatalf("voter is %q", got)
	}
	if got := voterAddress("atonevalcons1zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3xq0ue9"); got != "" {
		t.Fatalf("a consensus address gave a voter %q", got)
	}
}

func TestGovernanceMessages(t *testing.T) {
	cc := &ChainConfig{name: "MANTRA", ChainId: "mantra-1"}
	p := govProposal{ID: 39, Title: "Community Pool", VotingEnd: time.Now().Add(7*24*time.Hour - 30*time.Minute).UTC().Truncate(time.Minute)}
	open := &alertMsg{chain: "MANTRA", severity: "info", message: cc.govOpenMessage(p)}
	if icon, label := open.header(); icon != "🗳" || label != "New proposal" {
		t.Fatalf("open header %s %s", icon, label)
	}
	if got := buildTelegramMessage(open); !strings.Contains(got, "<b>#39 Community Pool</b>\n🗓 <b>Ends:</b> ") || !strings.Contains(got, " · <b>6d 23h left</b>") {
		t.Fatalf("open message %q", got)
	}
	remind := &alertMsg{chain: "MANTRA", severity: "critical", message: cc.govRemindMessage(p, "Validator")}
	if icon, label := remind.header(); icon != "⏰" || label != "Vote reminder" || !strings.HasSuffix(remind.summary(), "\nValidator has not voted") {
		t.Fatalf("reminder %s %s %q", icon, label, remind.summary())
	}

	// cleared while voting is open is a vote, after that the result
	open.resolved = true
	if _, label := open.header(); label != "Voted" {
		t.Fatalf("cleared while open is %q", label)
	}
	govOutcomes.Store("MANTRA#39", "Rejected")
	defer govOutcomes.Delete("MANTRA#39")
	if icon, label := open.header(); icon != "❌" || label != "Rejected" {
		t.Fatalf("result is %s %s", icon, label)
	}
	ended := &alertMsg{chain: "Other", resolved: true, message: cc.govOpenMessage(govProposal{ID: 2, VotingEnd: time.Now().Add(-time.Hour).UTC()})}
	if _, label := ended.header(); label != "Voting ended" {
		t.Fatalf("ended without a result is %q", label)
	}
}

func TestMinSeverityRouting(t *testing.T) {
	c := auditConfig(t)
	c.Telegram.Enabled, c.Pagerduty.Enabled = yes(true), yes(true)
	chain := &ChainConfig{}
	chain.Alerts.Telegram.Enabled, chain.Alerts.Pagerduty.Enabled = yes(true), yes(true)
	chain.Alerts.Pagerduty.MinSeverity = "warning"
	c.Chains["Chain"] = chain
	if a := c.makeAlert("Chain", "new proposal", "info", false, nil); !a.tg || a.pd {
		t.Fatalf("info went to telegram %v, pagerduty %v", a.tg, a.pd)
	}
	if a := c.makeAlert("Chain", "missed blocks", "critical", false, nil); !a.tg || !a.pd {
		t.Fatalf("critical went to telegram %v, pagerduty %v", a.tg, a.pd)
	}
	if a := c.makeAlert("Chain", "missed blocks", "info", true, nil); !a.pd {
		t.Fatal("a recovery was kept from pagerduty")
	}
}

func TestProposalSummaries(t *testing.T) {
	summary := strings.Repeat("\u4e16", 10001)
	for _, legacy := range []bool{false, true} {
		proposal := varintField(nil, 1, 7)
		if legacy {
			content := field(field(nil, 1, []byte("Title")), 2, []byte(summary))
			proposal = field(proposal, 2, field(nil, 2, content))
		} else {
			proposal = field(field(proposal, 11, []byte("Title")), 12, []byte(summary))
		}
		got, err := parseProposals(field(nil, 1, proposal), legacy)
		if err != nil || len(got) != 1 || got[0].Title != "Title" || got[0].Summary != clipRunes(summary, 10000) {
			t.Fatalf("legacy=%v: %+v, %v", legacy, got, err)
		}
	}
}

func tallyBytes(counts ...string) []byte {
	var raw []byte
	for i, count := range counts {
		raw = field(raw, protowire.Number(i+1), []byte(count))
	}
	return field(nil, 1, raw)
}

func TestParseProposalTally(t *testing.T) {
	for _, tc := range []struct {
		name, bonded           string
		counts                 []string
		yes, abstain, no, veto float64
		turnout                *float64
	}{
		{"large", "2000000000000000000000", []string{"700000000000000000000", "200000000000000000000", "90000000000000000000", "10000000000000000000"}, 70, 20, 9, 1, floatPointer(50)},
		{"nothing cast", "100", []string{"0", "0", "0", "0"}, 0, 0, 0, 0, floatPointer(0)},
		{"bonded missing", "", []string{"3", "1"}, 75, 25, 0, 0, nil},
		{"bonded zero", "0", []string{"3", "1"}, 75, 25, 0, 0, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseGovTally(tallyBytes(tc.counts...), tc.bonded)
			if err != nil {
				t.Fatal(err)
			}
			if got.Yes != tc.yes || got.Abstain != tc.abstain || got.No != tc.no || got.Veto != tc.veto || math.IsNaN(got.Yes) {
				t.Fatalf("wrong percentages %+v", got)
			}
			if (got.Turnout == nil) != (tc.turnout == nil) || (got.Turnout != nil && *got.Turnout != *tc.turnout) {
				t.Fatalf("wrong turnout %+v", got)
			}
		})
	}
	for _, raw := range [][]byte{nil, {255}, tallyBytes("-1"), tallyBytes("bad")} {
		if _, err := parseGovTally(raw, ""); err == nil {
			t.Fatalf("accepted invalid tally %x", raw)
		}
	}
}

func floatPointer(n float64) *float64 { return &n }

func tallyNode(t *testing.T, answer func(string) ([]byte, bool)) *rpchttp.HTTP {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			ID     interface{} `json:"id"`
			Params struct {
				Path string `json:"path"`
			} `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		value, ok := answer(request.Params.Path)
		response := map[string]interface{}{"code": 6, "log": "unavailable at http://private-node:26657"}
		if ok {
			response = map[string]interface{}{"code": 0, "value": base64.StdEncoding.EncodeToString(value)}
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"jsonrpc": "2.0", "id": request.ID, "result": map[string]interface{}{"response": response}})
	}))
	t.Cleanup(server.Close)
	client, err := rpchttp.New(server.URL, "/websocket")
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestProposalTallyFallbackAndMissingPool(t *testing.T) {
	var paths []string
	var pathsMux sync.Mutex
	client := tallyNode(t, func(path string) ([]byte, bool) {
		pathsMux.Lock()
		paths = append(paths, path)
		pathsMux.Unlock()
		return tallyBytes("3", "1"), path == "/cosmos.gov.v1beta1.Query/TallyResult"
	})
	got, err := proposalTally(context.Background(), client, 7)
	if err != nil || got.Yes != 75 || got.Turnout != nil {
		t.Fatalf("%+v, %v", got, err)
	}
	pathsMux.Lock()
	defer pathsMux.Unlock()
	if strings.Join(paths, ",") != "/cosmos.gov.v1.Query/TallyResult,/cosmos.gov.v1beta1.Query/TallyResult,/cosmos.staking.v1beta1.Query/Pool" {
		t.Fatal(paths)
	}
}

func TestProposalTallyCache(t *testing.T) {
	var queries atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	client := tallyNode(t, func(path string) ([]byte, bool) {
		if strings.HasSuffix(path, "/Pool") {
			return field(nil, 1, field(nil, 2, []byte("200"))), true
		}
		queries.Add(1)
		once.Do(func() { close(started); <-release })
		return tallyBytes("70", "20", "9", "1"), true
	})
	cc := &ChainConfig{client: client}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := cc.cachedProposalTally(context.Background(), 7)
			if err != nil || got.Yes != 70 || got.Turnout == nil || *got.Turnout != 50 {
				t.Errorf("%+v, %v", got, err)
			}
		}()
	}
	<-started
	close(release)
	wg.Wait()
	if _, err := cc.cachedProposalTally(context.Background(), 7); err != nil {
		t.Fatal(err)
	}
	if queries.Load() != 1 {
		t.Fatalf("%d tally queries", queries.Load())
	}
	cc.proposalTallies.Lock()
	expires := cc.proposalTallies.entries[7].expires
	cc.proposalTallies.entries[7].expires = time.Now().Add(-time.Second)
	cc.proposalTallies.Unlock()
	if remaining := time.Until(expires); remaining < 4*time.Minute || remaining > 5*time.Minute {
		t.Fatalf("success TTL %v", remaining)
	}
	// Expiry uses the chain's current client, including after an RPC switch.
	cc.setClient(tallyNode(t, func(path string) ([]byte, bool) {
		queries.Add(1)
		return nil, false
	}))
	for i := 0; i < 2; i++ {
		if _, err := cc.cachedProposalTally(context.Background(), 7); err == nil {
			t.Fatal("wanted tally failure")
		}
	}
	if queries.Load() != 3 {
		t.Fatalf("failure was not cached: %d queries", queries.Load())
	}
	cc.proposalTallies.Lock()
	remaining := time.Until(cc.proposalTallies.entries[7].expires)
	cc.proposalTallies.Unlock()
	if remaining < 50*time.Second || remaining > time.Minute {
		t.Fatalf("failure TTL %v", remaining)
	}
}

func TestProposalEndpointOnlyQueriesOpenProposals(t *testing.T) {
	var queries atomic.Int32
	cc := &ChainConfig{ChainId: "akashnet-2", proposals: []govProposal{{ID: 7, Title: "Title", Summary: "Summary", VotingEnd: time.Now().Add(time.Hour)}}, client: tallyNode(t, func(path string) ([]byte, bool) {
		queries.Add(1)
		return nil, false
	})}
	c := &Config{Chains: map[string]*ChainConfig{"Akash": cc}}
	handler := dash.ProposalHandler(c.proposalDetails)
	for _, tc := range []struct {
		query  string
		status int
	}{
		{"chain=Unknown&id=7", 404}, {"chain=Akash&id=8", 404}, {"chain=Akash&id=bad", 400}, {"chain=Akash&id=7", 200},
	} {
		recorder := httptest.NewRecorder()
		handler(recorder, httptest.NewRequest("GET", "/proposal?"+tc.query, nil))
		if recorder.Code != tc.status {
			t.Fatalf("%s: %d %s", tc.query, recorder.Code, recorder.Body.String())
		}
		if tc.status != 200 && queries.Load() != 0 {
			t.Fatal("queried a non-open proposal")
		}
		if tc.status == 200 {
			var got dash.ProposalDetail
			if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.Tally != nil || got.TallyError != "tally unavailable from this node" || got.Summary != "Summary" || got.Title != "Title" || got.Start != 0 {
				t.Fatalf("%+v", got)
			}
			if strings.Contains(recorder.Body.String(), "private-node") {
				t.Fatal("RPC host exposed")
			}
		}
	}
}
