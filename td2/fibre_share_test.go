package tenderduty

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"

	rpchttp "github.com/tendermint/tendermint/rpc/client/http"
	"google.golang.org/protobuf/encoding/protowire"
)

func TestFibreRows(t *testing.T) {
	// measured from real Fibre on a devnet: six validators with a full stake budget of 100,000,000
	for _, tc := range []struct {
		name    string
		powers  []int64
		rows    []int
		budgets []int64
	}{
		{"six validators", []int64{25, 20, 20, 15, 10, 10},
			[]int{3072, 2458, 2458, 1844, 1229, 1229},
			[]int64{75_000_000, 60_009_765, 60_009_765, 45_019_531, 30_004_882, 30_004_882}},
		// over a third of the stake is every row
		{"three validators", []int64{50, 30, 20}, []int{4096, 3687, 2458}, []int64{100_000_000, 90_014_648, 60_009_765}},
	} {
		var total int64
		for _, p := range tc.powers {
			total += p
		}
		for i, p := range tc.powers {
			rows, minimum := fibreRows(p, total)
			if rows != tc.rows[i] || minimum {
				t.Errorf("%s: power %d of %d got %d rows (minimum %v), want %d", tc.name, p, total, rows, minimum, tc.rows[i])
			}
			if budget := fibreBudget(100_000_000, rows); budget != tc.budgets[i] {
				t.Errorf("%s: %d rows got budget %d, want %d", tc.name, rows, budget, tc.budgets[i])
			}
		}
	}
}

func TestFibreRowsMinimumAndEdges(t *testing.T) {
	// 0.296% of the stake is 37 rows by the formula, the minimum of 148 applies
	rows, minimum := fibreRows(296, 100_000)
	if rows != 148 || !minimum {
		t.Fatalf("0.296%% got %d rows, minimum %v", rows, minimum)
	}
	// a validator whose share gives exactly 148 is not raised
	if rows, minimum := fibreRows(148, 12288); rows != 148 || minimum {
		t.Fatalf("148 of 12288 got %d, minimum %v", rows, minimum)
	}
	if rows, minimum := fibreRows(149, 12288); rows != 149 || minimum {
		t.Fatalf("149 of 12288 got %d, minimum %v", rows, minimum)
	}
	for _, tc := range []struct {
		name         string
		power, total int64
		rows         int
	}{
		{"no power is not in the set", 0, 100, 0},
		{"negative power", -5, 100, 0},
		{"no total", 5, 0, 0},
		{"everything", 100, 100, 4096},
		{"more than everything", 150, 100, 4096},
		{"a third", 33_334, 100_000, 4096},
		{"huge numbers don't overflow", math.MaxInt64 / 2, math.MaxInt64, 4096},
		{"both at the largest", math.MaxInt64, math.MaxInt64, 4096},
		{"tiny share of huge stake", 1, math.MaxInt64, 148},
	} {
		if rows, _ := fibreRows(tc.power, tc.total); rows != tc.rows {
			t.Errorf("%s: got %d rows, want %d", tc.name, rows, tc.rows)
		}
	}
}

func TestFibreBudget(t *testing.T) {
	// Mocha: 2 TiB and the minimum of 148 rows is 74 GiB
	if got := fibreBudget(2<<40, 148); got != 79_456_894_976 {
		t.Fatalf("2 TiB at 148 rows is %d", got)
	}
	if got := fibreBudget(2<<40, 4096); got != 2<<40 {
		t.Fatalf("every row is %d", got)
	}
	if fibreBudget(2<<40, 0) != 0 || fibreBudget(0, 148) != 0 || fibreBudget(-1, 148) != 0 {
		t.Fatal("no rows or no stake is no budget")
	}
	// the product is past 64 bits
	if got := fibreBudget(math.MaxInt64, 4096); got != math.MaxInt64 {
		t.Fatalf("the largest stake at every row is %d", got)
	}
	if got := fibreBudget(math.MaxInt64, math.MaxInt); got != math.MaxInt64 {
		t.Fatalf("excess rows should clamp to every row: %d", got)
	}
	if got := fibreBudget(math.MaxInt64, 2048); got != math.MaxInt64/2 {
		t.Fatalf("half of the largest stake is %d", got)
	}
}

// QueryParamsResponse{params: {..., full_stake_storage_budget: 5}} with fields this code doesn't know around it
func fibreParamsResponse(stake uint64, withStake bool) []byte {
	var params []byte
	params = protowire.AppendVarint(protowire.AppendTag(params, 1, protowire.VarintType), 7)
	params = protowire.AppendString(protowire.AppendTag(params, 3, protowire.BytesType), "unknown")
	if withStake {
		params = protowire.AppendVarint(protowire.AppendTag(params, 5, protowire.VarintType), stake)
	}
	params = protowire.AppendFixed32(protowire.AppendTag(params, 9, protowire.Fixed32Type), 1)
	b := protowire.AppendBytes(protowire.AppendTag(nil, 1, protowire.BytesType), params)
	b = protowire.AppendFixed64(protowire.AppendTag(b, 4, protowire.Fixed64Type), 1)
	return protowire.AppendVarint(protowire.AppendTag(b, 2, protowire.VarintType), 1)
}

func TestParseFibreFullStake(t *testing.T) {
	if got, err := parseFibreFullStake(fibreParamsResponse(2<<40, true)); err != nil || got != 2<<40 {
		t.Fatalf("got %d, %v", got, err)
	}
	// protobuf leaves out zero, and so does an empty response
	for name, b := range map[string][]byte{"no field": fibreParamsResponse(0, false), "empty": nil, "empty params": {0x0a, 0x00}} {
		if got, err := parseFibreFullStake(b); err != nil || got != 0 {
			t.Errorf("%s: got %d, %v", name, got, err)
		}
	}
	// field 5 only counts as a varint, field 1 only as the params
	wrong := protowire.AppendBytes(protowire.AppendTag(nil, 5, protowire.BytesType), []byte("x"))
	wrong = protowire.AppendBytes(protowire.AppendTag(nil, 1, protowire.BytesType), wrong)
	wrong = protowire.AppendVarint(protowire.AppendTag(wrong, 5, protowire.VarintType), 99)
	if got, err := parseFibreFullStake(wrong); err != nil || got != 0 {
		t.Errorf("wrong wire types: got %d, %v", got, err)
	}
	whole := fibreParamsResponse(2<<40, true)
	if _, err := parseFibreFullStake(whole[:len(whole)-4]); err == nil {
		t.Error("a truncated message was accepted")
	}
	if _, err := parseFibreFullStake(fibreParamsResponse(math.MaxUint64, true)); err == nil {
		t.Error("a budget past int64 was accepted")
	}
}

// answers each query path with its own value or error
func pathNode(t *testing.T, answer func(path string) (value []byte, codespace string, code int)) *rpchttp.HTTP {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			ID     interface{} `json:"id"`
			Params struct {
				Path string `json:"path"`
			} `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		value, codespace, code := answer(request.Params.Path)
		response := map[string]interface{}{"code": code, "codespace": codespace, "log": "failed"}
		if code == 0 {
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

// TestRefreshFibreReadsTheParams: a failure keeps the last value and says nothing about Fibre
func TestRefreshFibreReadsTheParams(t *testing.T) {
	paramsOK, paramsCode, paramsSpace := true, 0, ""
	paramsValue := fibreParamsResponse(2<<40, true)
	queries := map[string]int{}
	dead := closedPort(t)
	client := pathNode(t, func(path string) ([]byte, string, int) {
		queries[path]++
		switch path {
		case "/celestia.valaddr.v1.Query/FibreProviderInfo":
			return fibreRegistration(dead), "", 0
		case "/celestia.fibre.v1.Query/Params":
			if paramsOK {
				return paramsValue, "", 0
			}
			return nil, paramsSpace, paramsCode
		}
		return nil, "sdk", 6
	})
	chain := fibreChain()
	chain.refreshFibre(context.Background(), client)
	if chain.fibreFullStake != 2<<40 || chain.fibreParamsFailing || !chain.fibreLoaded || !chain.fibre.Registered {
		t.Fatalf("stake %d, failing %v, state %+v", chain.fibreFullStake, chain.fibreParamsFailing, chain.fibre)
	}

	// failed reads retain the budget and keep the chain eligible for retries
	for _, tc := range []struct {
		name  string
		space string
		code  int
	}{{"no x/fibre", "sdk", 6}, {"a failing node", "sdk", 1}} {
		paramsOK, paramsSpace, paramsCode = false, tc.space, tc.code
		before := chain.fibre
		chain.refreshFibre(context.Background(), client)
		if chain.fibreFullStake != 2<<40 || !chain.fibreParamsFailing || chain.fibreUnsupported || !chain.fibreLoaded {
			t.Fatalf("%s: stake %d, failing %v, unsupported %v", tc.name, chain.fibreFullStake, chain.fibreParamsFailing, chain.fibreUnsupported)
		}
		if chain.fibre.Registered != before.Registered || chain.fibre.Host != before.Host || chain.fibre.Reachable != before.Reachable {
			t.Fatalf("%s: the provider state changed from %+v to %+v", tc.name, before, chain.fibre)
		}
	}
	if queries["/celestia.fibre.v1.Query/Params"] != 3 {
		t.Fatalf("the params were asked %d times", queries["/celestia.fibre.v1.Query/Params"])
	}

	paramsOK, paramsValue = true, fibreParamsResponse(4<<40, true)
	chain.refreshFibre(context.Background(), client)
	if chain.fibreFullStake != 4<<40 || chain.fibreParamsFailing {
		t.Fatalf("stake %d, failing %v", chain.fibreFullStake, chain.fibreParamsFailing)
	}

	// missing or zero budgets keep the last value
	paramsValue = fibreParamsResponse(0, false)
	chain.refreshFibre(context.Background(), client)
	if chain.fibreFullStake != 4<<40 || !chain.fibreParamsFailing {
		t.Fatalf("an empty answer left stake %d, failing %v", chain.fibreFullStake, chain.fibreParamsFailing)
	}
}
