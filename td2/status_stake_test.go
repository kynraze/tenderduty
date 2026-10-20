package tenderduty

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cosmos/cosmos-sdk/types/bech32"
	bank "github.com/cosmos/cosmos-sdk/x/bank/types"
	staking "github.com/cosmos/cosmos-sdk/x/staking/types"
	rpchttp "github.com/tendermint/tendermint/rpc/client/http"
	"google.golang.org/protobuf/encoding/protowire"
)

func TestStakeDisplayUnits(t *testing.T) {
	for _, tc := range []struct {
		tokens       string
		exponent     uint32
		symbol, want string
	}{
		{"305100000000", 6, "ATONE", "305.1K ATONE"},
		{"19800000000000", 9, "CHEQ", "19.8K CHEQ"},
		{"19800000000000000000000", 18, "COIN", "19.8K COIN"},
	} {
		t.Run(tc.symbol, func(t *testing.T) {
			info := &ValInfo{Tokens: tc.tokens, VotingPower: 19800000, Token: bondToken{Symbol: tc.symbol, Exponent: tc.exponent, DisplayKnown: true}}
			if got := info.stakeDisplay(); got != tc.want {
				t.Fatalf("stake %q, want %q", got, tc.want)
			}
			info.Token.DisplayKnown = false
			if info.stakeDisplay() != "" {
				t.Fatal("unknown token displayed")
			}
			info.Token.DisplayKnown, info.Token.Multistaking = true, true
			if info.stakeDisplay() != "" {
				t.Fatal("assumed multistaking units")
			}
		})
	}
}

func TestReportStakeTokenScaling(t *testing.T) {
	auditConfig(t)
	field := func(num protowire.Number, value []byte) []byte {
		return protowire.AppendBytes(protowire.AppendTag(nil, num, protowire.BytesType), value)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/validators" {
			_, _ = w.Write([]byte(`{"result":{"validators":[{"address":"01","voting_power":"19800000"}],"total":"1"}}`))
			return
		}
		var request struct {
			ID     interface{} `json:"id"`
			Params struct {
				Path string `json:"path"`
			} `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		var value []byte
		switch request.Params.Path {
		case "/cosmos.staking.v1beta1.Query/Validator":
			value = field(1, field(5, []byte("19800000000000")))
		case "/cosmos.staking.v1beta1.Query/Delegation":
			value = field(1, field(2, field(2, []byte("1200000000000"))))
		case "/cosmos.staking.v1beta1.Query/ValidatorDelegations":
			page := protowire.AppendVarint(protowire.AppendTag(nil, 2, protowire.VarintType), 6)
			value = field(2, page)
		default:
			t.Errorf("unexpected query %s", request.Params.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"jsonrpc": "2.0", "id": request.ID, "result": map[string]interface{}{"response": map[string]interface{}{"code": 0, "value": value}}})
	}))
	defer server.Close()
	client, err := rpchttp.New(server.URL, "/websocket")
	if err != nil {
		t.Fatal(err)
	}
	address, err := bech32.ConvertAndEncode("cosmosvaloper", make([]byte, 20))
	if err != nil {
		t.Fatal(err)
	}
	cc := &ChainConfig{client: client, ValAddress: address, valInfo: &ValInfo{Conspub: []byte{1}}, bondToken: bondToken{DisplayKnown: true, Symbol: "CHEQ", Exponent: 9}}
	s := cc.reportStake(time.Now())
	if s.Stake == nil || math.Abs(*s.Stake-19800) > 1e-8 || s.Symbol != "CHEQ" || s.Self == nil || math.Abs(*s.Self-1200) > 1e-8 || s.Delegated == nil || math.Abs(*s.Delegated-18600) > 1e-8 || s.Power == nil || *s.Power != 19800000 {
		t.Fatalf("token snapshot: %+v", s)
	}
	for _, multi := range []bool{false, true} {
		cc.bondToken.DisplayKnown, cc.bondToken.Multistaking = multi, multi
		cc.bondTokenAt = time.Now()
		s = cc.reportStake(time.Now())
		if s.Stake != nil || s.Symbol != "" || s.Self == nil || *s.Self != 1200000 || s.Delegated == nil || *s.Delegated != 18600000 {
			t.Fatalf("fallback snapshot: %+v", s)
		}
	}
}

// cheqd: ncheq with 9 decimals, from the chain's denom metadata or else the registry; multistaking keeps power
func TestResolveBondToken(t *testing.T) {
	auditConfig(t)
	useRegistries(t, registryServer(t, map[string]string{
		"/":      `{"chains":[{"chain_id":"cheqd-mainnet-1","path":"cheqd"}]}`,
		"/cheqd": `{"chain":{"chain_id":"cheqd-mainnet-1","network_type":"mainnet","assets":[{"base":"ncheq","display":"cheq","symbol":"CHEQ","denom_units":[{"denom":"ncheq","exponent":0},{"denom":"cheq","exponent":9}]}]}}`,
	}), registryServer(t, map[string]string{"/": `{"chains":[]}`}))
	field := func(num protowire.Number, value []byte) []byte {
		return protowire.AppendBytes(protowire.AppendTag(nil, num, protowire.BytesType), value)
	}
	metadata, _ := (&bank.QueryDenomMetadataResponse{Metadata: bank.Metadata{Base: "ncheq", Display: "cheq", Symbol: "CHEQ",
		DenomUnits: []*bank.DenomUnit{{Denom: "ncheq"}, {Denom: "cheq", Exponent: 9}}}}).Marshal()
	params, _ := (&staking.QueryParamsResponse{Params: staking.Params{BondDenom: "ncheq"}}).Marshal()
	for _, tc := range []struct {
		name  string
		multi bool
		meta  bool
		want  bondToken
	}{
		{"metadata", false, true, bondToken{Denom: "ncheq", Symbol: "CHEQ", Exponent: 9, DisplayKnown: true}},
		{"registry", false, false, bondToken{Denom: "ncheq", Symbol: "CHEQ", Exponent: 9, DisplayKnown: true}},
		{"multistaking", true, false, bondToken{Denom: "ario", Multistaking: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					ID     interface{} `json:"id"`
					Params struct {
						Path string `json:"path"`
					} `json:"params"`
				}
				_ = json.NewDecoder(r.Body).Decode(&request)
				code, value := 0, []byte(nil)
				switch request.Params.Path {
				case "/multistaking.v1.Query/ValidatorMultiStakingCoin":
					if code = 1; tc.multi {
						code, value = 0, field(1, []byte("ario"))
					}
				case "/cosmos.staking.v1beta1.Query/Params":
					value = params
				case "/cosmos.bank.v1beta1.Query/DenomMetadata":
					if code = 1; tc.meta {
						code, value = 0, metadata
					}
				}
				_ = json.NewEncoder(w).Encode(map[string]interface{}{"jsonrpc": "2.0", "id": request.ID, "result": map[string]interface{}{"response": map[string]interface{}{"code": code, "value": value}}})
			}))
			defer server.Close()
			client, err := rpchttp.New(server.URL, "/websocket")
			if err != nil {
				t.Fatal(err)
			}
			cc := &ChainConfig{client: client, ChainId: "cheqd-mainnet-1", ValAddress: "cheqdvaloper1test"}
			if got := cc.resolveBondToken(context.Background()); got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestBondTokenFailedLookupIsThrottled(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var request struct {
			ID interface{} `json:"id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"jsonrpc": "2.0", "id": request.ID, "error": map[string]interface{}{"code": -32601, "message": "unsupported"}})
	}))
	defer server.Close()
	client, err := rpchttp.New(server.URL, "/websocket")
	if err != nil {
		t.Fatal(err)
	}
	cc := &ChainConfig{client: client}
	cc.cachedBondToken(context.Background())
	before := calls.Load()
	cc.cachedBondToken(context.Background())
	if before == 0 || calls.Load() != before {
		t.Fatal("failed lookup was not throttled")
	}
	cc.bondTokenAt = time.Now().Add(-11 * time.Minute)
	cc.cachedBondToken(context.Background())
	if calls.Load() <= before {
		t.Fatal("failed lookup was not retried")
	}
	cc.bondToken = bondToken{DisplayKnown: true, Symbol: "CHEQ", Exponent: 9}
	before = calls.Load()
	var slash slashDetails
	cc.slashToken(context.Background(), &slash)
	if calls.Load() != before || !slash.DisplayKnown || slash.Symbol != "CHEQ" {
		t.Fatal("slash did not reuse cached token")
	}
}

func TestStatusSnapshotFactsAndPrivateNodes(t *testing.T) {
	c := auditConfig(t)
	c.HideLogs = true
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	count, self := uint64(6), 12000.0
	c.reportData.Snapshots = map[string][]reportSnapshot{"Cheqd": {{At: now.Add(-3 * time.Hour), Delegators: &count, Self: &self, Symbol: "CHEQ"}}}
	c.reportData.Hours = map[string][]reportHour{"Cheqd": {
		{At: now.Add(-time.Hour), Signed: 9972, Missed: 28, Proposed: 3, MissedProps: 3},
		{At: now.Add(-25 * time.Hour), Signed: 1, Missed: 99},
	}}
	cc := &ChainConfig{ChainId: "cheqd-1", valInfo: &ValInfo{Bonded: true, VotingPower: 19800000, TotalPower: 198000000, Tokens: "19800000000000", Token: bondToken{DisplayKnown: true, Symbol: "CHEQ", Exponent: 9}},
		Nodes: []*NodeConfig{{Url: "http://secret-a", AlertIfDown: true, binaryVersion: "1.2.3"}, {Url: "http://secret-b", down: true, downSince: now.Add(-5 * time.Minute)}}, proposalsLoaded: true}
	cc.Alerts.GovAlerts = true
	cc.proposals = []govProposal{{ID: 342, VotingEnd: now.Add(28 * time.Hour)}, {ID: 343, Voted: true, VotingEnd: now.Add(48 * time.Hour)}}
	c.Chains["Cheqd"] = cc
	text := cc.statusDetail("Cheqd", now)
	for _, want := range []string{"Voting power:</b> 19.8K CHEQ (10%)", "Delegators:</b> 6 · self 12K CHEQ", "Signing 24h:</b> 99.72% · proposed 3/6", "Governance:</b> 2 open · 1 not voted (#342, ends 1d 4h)", "Node 1:</b> healthy", "Node 2:</b> down 5m"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "secret-") {
		t.Fatal("private node host exposed")
	}
	telegramHTML(t, text)
	cc.proposals = append(cc.proposals, govProposal{ID: 344, VotingEnd: now.Add(29 * time.Hour)}, govProposal{ID: 345, VotingEnd: now.Add(30 * time.Hour)})
	text = cc.statusDetail("Cheqd", now)
	if !strings.Contains(text, "4 open · 3 not voted") || !strings.Contains(text, "#344, ends") || strings.Contains(text, "#345, ends") || !strings.Contains(text, "+1 more") {
		t.Fatal(text)
	}
	c.reportData.Snapshots["Cheqd"][0].Self = nil
	text = cc.statusDetail("Cheqd", now)
	if !strings.Contains(text, "Delegators:</b> 6") || strings.Contains(text, "self ") {
		t.Fatal(text)
	}
	cc.Alerts.GovAlerts = false
	cc.valInfo.Token.DisplayKnown = false
	text = cc.statusDetail("Cheqd", now)
	if strings.Contains(text, "Governance:") || !strings.Contains(text, "Voting power:</b> 19,800,000") {
		t.Fatal(text)
	}
	cc.valInfo.Token = bondToken{DisplayKnown: true, Symbol: "C<&", Exponent: 9}
	if text = cc.statusDetail("Cheqd", now); !strings.Contains(text, "19.8K C&lt;&amp;") {
		t.Fatal(text)
	}
}

func TestStatusChainLookup(t *testing.T) {
	c := auditConfig(t)
	for _, name := range []string{"Cheqd", "Realio-Network / RIO", "Realio-Network / RST", "Realio-Network / DSTRX"} {
		c.Chains[name] = &ChainConfig{}
	}
	for _, name := range []string{"Cheqd", "cHEQD", "che"} {
		if got, cc := c.chainNamed(name); got != "Cheqd" || cc == nil {
			t.Fatalf("%s: %q", name, got)
		}
	}
	got, cc := c.chainNamed("realio")
	if cc != nil || !strings.Contains(got, `"realio" matches Realio-Network / DSTRX, Realio-Network / RIO, Realio-Network / RST; be more specific`) {
		t.Fatal(got)
	}
	c.Chains["che"] = &ChainConfig{}
	if got, cc := c.chainNamed("che"); got != "che" || cc == nil {
		t.Fatal("prefix beat exact name")
	}
}

func TestReportTokenRowsAndCoverage(t *testing.T) {
	for _, weekly := range []bool{false, true} {
		c := auditConfig(t)
		c.Reports.Timezone = "UTC"
		now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
		span, label := 24*time.Hour, "24h"
		if weekly {
			span, label = 7*24*time.Hour, "7d"
		}
		c.reportData.Hours = make(map[string][]reportHour)
		c.reportData.Snapshots = make(map[string][]reportSnapshot)
		stake, power := 19800.0, int64(19800000)
		for _, name := range []string{"A", "B"} {
			c.Chains[name] = &ChainConfig{valInfo: &ValInfo{Bonded: true}}
			c.reportData.Snapshots[name] = []reportSnapshot{{At: now, Power: &power, Stake: &stake, Symbol: "CHEQ"}}
			for i := 1; i <= int(span/time.Hour); i++ {
				c.reportData.Hours[name] = append(c.reportData.Hours[name], reportHour{At: now.Add(-time.Duration(i) * time.Hour), Watched: 3600, Signed: 100})
			}
		}
		r := c.buildReport(now, weekly, false, nil)
		if r.Footer != "Watched "+label+" of "+label+" · UTC" {
			t.Fatal(r.Footer)
		}
		if !strings.Contains(r.telegram(), "19.8K CHEQ") {
			t.Fatal(r.telegram())
		}
		c.reportData.Hours["A"] = []reportHour{{At: now.Add(-time.Hour), Watched: 3600}}
		r = c.buildReport(now, weekly, false, nil)
		if r.Footer != "Watched "+label+" of "+label+" on 1 of 2 chains, 1 partly (1h) · UTC" {
			t.Fatal(r.Footer)
		}
		c.reportData.Hours["B"] = []reportHour{{At: now.Add(-time.Hour), Watched: 1800}}
		r = c.buildReport(now, weekly, false, nil)
		if r.Footer != "Watched 30m–1h of "+label+" on 2 chains · UTC" {
			t.Fatal(r.Footer)
		}
		c.reportData.Hours["B"] = c.reportData.Hours["A"]
		r = c.buildReport(now, weekly, false, nil)
		if r.Footer != "Watched 1h of "+label+" on 2 chains · UTC" {
			t.Fatal(r.Footer)
		}
	}
}
