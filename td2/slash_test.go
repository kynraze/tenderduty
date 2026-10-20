package tenderduty

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	rpchttp "github.com/tendermint/tendermint/rpc/client/http"
)

const atomoneValcons = "atonevalcons1z8uz2ec5gmxln0wsh2yd65f07pg8t82dx4uumc"

func atomoneSlashFixture(t *testing.T) (slashBlock, *ValInfo) {
	t.Helper()
	data, err := os.ReadFile("testdata/slash_atomone.json")
	if err != nil {
		t.Fatal(err)
	}
	var block slashBlock
	if err := json.Unmarshal(data, &block); err != nil {
		t.Fatal(err)
	}
	return block, &ValInfo{Valcons: atomoneValcons, Conspub: ToBytes("11F825671446CDF9BDD0BA88DD512FF050759D4D"), SlashDoubleSign: "0.05", SlashDowntime: "0.0001", Tombstoned: true, Jailed: true}
}

func TestSlashEventLayouts(t *testing.T) {
	fixture, val := atomoneSlashFixture(t)
	for _, layout := range []string{"begin_block_events", "finalize_block_events", "result_begin_block", "result_finalize_block"} {
		for _, encoded := range []bool{false, true} {
			t.Run(layout+map[bool]string{false: "/plain", true: "/base64"}[encoded], func(t *testing.T) {
				data, _ := json.Marshal(fixture.Finalize)
				var events []slashEvent
				if err := json.Unmarshal(data, &events); err != nil {
					t.Fatal(err)
				}
				if encoded {
					for i := range events {
						for j := range events[i].Attributes {
							a := &events[i].Attributes[j]
							a.Key = base64.StdEncoding.EncodeToString([]byte(a.Key))
							a.Value = base64.StdEncoding.EncodeToString([]byte(a.Value))
						}
					}
				}
				data, _ = json.Marshal(events)
				if strings.HasPrefix(layout, "result_") {
					data = append(append([]byte(`{"events":`), data...), '}')
				}
				var block slashBlock
				if err := json.Unmarshal([]byte(`{"`+layout+`":`+string(data)+`}`), &block); err != nil {
					t.Fatal(err)
				}
				var d slashDetails
				if !d.read(block, val, 10584954) || d.Reason != "double_sign" || d.Burned != "89868300000" || d.Height != 10584954 {
					t.Fatalf("slash: %+v", d)
				}
				other := *val
				other.Valcons = "othervalcons1"
				var unrelated slashDetails
				if unrelated.read(block, &other, 10584954) {
					t.Fatal("accepted another validator's slash")
				}
			})
		}
	}
}

func TestAtomoneDoubleSignEvidence(t *testing.T) {
	block, val := atomoneSlashFixture(t)
	var d slashDetails
	if !d.read(block, val, 10584954) {
		t.Fatal("no evidence")
	}
	at, _ := time.Parse(time.RFC3339Nano, "2026-09-30T00:53:19.082857529Z")
	if d.VoteHeight != 10584953 || !d.Time.Equal(at) || d.Round != 0 || d.VoteType != 1 {
		t.Fatalf("evidence: %+v", d)
	}
	d.Denom, d.Symbol, d.Exponent, d.DisplayKnown, d.Fraction = "uatone", "ATONE", 6, true, "0.05"
	want := "Double-signed 10,584,953 \u00b7 round 0 \u00b7 prevote \u00b7 30 Sep 00:53 UTC\nSlashed 89,868 ATONE (5%)"
	if got := d.detail(true, time.UTC); got != want {
		t.Fatalf("detail %q, want %q", got, want)
	}
	if got := d.detail(true, time.FixedZone("WIB", 7*3600)); !strings.Contains(got, "30 Sep 07:53 WIB") {
		t.Fatalf("timezone: %s", got)
	}
	block.Block.Evidence.Evidence[0].Value.VoteB.ValidatorAddress = "OTHER"
	block.Finalize = nil
	if (&slashDetails{}).read(block, val, 10584954) {
		t.Fatal("accepted mismatched validators")
	}
}

func TestSlashedAmount(t *testing.T) {
	for _, tc := range []struct {
		name, burned, fraction, want string
		display                      bool
	}{
		{"AtomOne double sign", "89868300000", "0.05", "Slashed 89,868 ATONE (5%)", true},
		{"downtime", "12300000", "0.0001", "Slashed 12.3 ATONE (0.01%)", true},
		{"Celestia downtime", "0", "0", "", true},
		{"Celestia double sign", "12300000", "0.02", "Slashed 12.3 ATONE (2%)", true},
		{"base denom", "12300000", "0.0001", "Slashed 12,300,000 uatone (0.01%)", false},
		{"small slash", "1", "0.0001", "Slashed 0.000001 ATONE (0.01%)", true},
		{"unknown params", "1000000", "", "Slashed 1 ATONE", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := slashDetails{Burned: tc.burned, Fraction: tc.fraction, Denom: "uatone", Symbol: "ATONE", Exponent: 6, DisplayKnown: tc.display}
			if got := d.slashed(); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
			if tc.burned == "0" && len(noteFields(strings.Split(d.detail(false, time.UTC), "\n"))) != 0 {
				t.Fatal("zero slash made a row")
			}
		})
	}
}

func TestSlashVoteTypes(t *testing.T) {
	for _, tc := range []struct {
		kind jsonInt
		want string
	}{{1, "prevote"}, {2, "precommit"}, {32, "proposal"}, {0, ""}, {99, ""}} {
		block, val := atomoneSlashFixture(t)
		block.Finalize = nil
		votes := &block.Block.Evidence.Evidence[0].Value
		votes.VoteA.Type, votes.VoteB.Type = tc.kind, tc.kind
		votes.VoteA.Round, votes.VoteB.Round = 3, 3
		var d slashDetails
		if !d.read(block, val, 10584954) {
			t.Fatal("no evidence")
		}
		want := "Double-signed 10,584,953 \u00b7 round 3"
		if tc.want != "" {
			want += " \u00b7 " + tc.want
		}
		want += " \u00b7 30 Sep 00:53 UTC"
		if got := d.detail(true, time.UTC); got != want {
			t.Fatalf("type %d: %q", tc.kind, got)
		}
	}
}

func TestSlashNilVoteEvidence(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*duplicateVotes)
		want bool
	}{
		{"nil vote a", func(v *duplicateVotes) { v.VoteA.BlockID.Hash = "" }, true},
		{"nil vote b", func(v *duplicateVotes) { v.VoteB.BlockID.Hash = "" }, true},
		{"both nil", func(v *duplicateVotes) { v.VoteA.BlockID.Hash, v.VoteB.BlockID.Hash = "", "" }, false},
		{"same block", func(v *duplicateVotes) { v.VoteB.BlockID = v.VoteA.BlockID }, false},
		{"same hash case", func(v *duplicateVotes) { v.VoteB.BlockID.Hash = strings.ToLower(v.VoteA.BlockID.Hash) }, false},
		{"different parts", func(v *duplicateVotes) {
			v.VoteB.BlockID = v.VoteA.BlockID
			v.VoteB.BlockID.Parts.Total = 1
		}, true},
		{"different height", func(v *duplicateVotes) { v.VoteB.Height++ }, false},
		{"different round", func(v *duplicateVotes) { v.VoteB.Round++ }, false},
		{"different type", func(v *duplicateVotes) { v.VoteB.Type = 2 }, false},
		{"different validator", func(v *duplicateVotes) { v.VoteB.ValidatorAddress = "OTHER" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			block, val := atomoneSlashFixture(t)
			block.Finalize = nil
			tc.edit(&block.Block.Evidence.Evidence[0].Value)
			var d slashDetails
			if got := d.read(block, val, 10584954); got != tc.want {
				t.Fatalf("accepted %v, want %v: %+v", got, tc.want, d)
			}
			if tc.want && (d.VoteHeight != 10584953 || d.Round != 0 || d.VoteType != 1) {
				t.Fatalf("lost evidence details: %+v", d)
			}
		})
	}
}

func TestSlashAlertCards(t *testing.T) {
	c := auditConfig(t)
	block, val := atomoneSlashFixture(t)
	var d slashDetails
	d.read(block, val, 10584954)
	d.Denom, d.Symbol, d.Exponent, d.DisplayKnown, d.Fraction = "uatone", "ATONE", 6, true, "0.05"
	cc := &ChainConfig{name: "AtomOne", ChainId: "atomone-1", valInfo: val, slash: d}
	c.Chains[cc.name] = cc
	for _, message := range []string{"POSTHUMAN is no longer in the active set on atomone-1", "POSTHUMAN is tombstoned on atomone-1"} {
		fields := noteFields(strings.Split(jailDetail(cc.name, message), "\n"))
		labels := map[string]string{}
		for _, field := range fields {
			labels[field.Label] = field.Value
		}
		if labels["Double-signed"] == "" || labels["Two blocks"] != "" || labels["Slashed"] != "89,868 ATONE (5%)" || labels["Proposer"] != "" {
			t.Fatalf("fields: %+v", fields)
		}
	}
	message := &alertMsg{chain: cc.name, severity: "critical", message: "POSTHUMAN is no longer in the active set on atomone-1"}
	if got := buildTelegramMessage(message); !strings.Contains(got, "<b>Slashed:</b> 89,868 ATONE (5%)") || !strings.Contains(got, "<b>Double-signed:</b>") || strings.Contains(got, "<b>Two blocks:</b>") {
		t.Fatalf("card: %s", got)
	}
	val.Tombstoned = false
	cc.slash.Fraction = "0.0001"
	cc.slash.Burned = "12300000"
	line := jailDetail(cc.name, "POSTHUMAN is jailed on atomone-1")
	if !strings.Contains(line, "Slashed 12.3 ATONE (0.01%)") || strings.Contains(line, "Double-signed") {
		t.Fatalf("jail detail: %s", line)
	}
	cc.slash.Burned = "0"
	if strings.Contains(jailDetail(cc.name, "POSTHUMAN is jailed on atomone-1"), "Slashed") {
		t.Fatal("zero slash in jail alert")
	}
}

func TestSlashHistoryAndRestart(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(map[bool]string{false: "transition", true: "restart"}[restart], func(t *testing.T) {
			c := auditConfig(t)
			var calls, searches atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				switch r.URL.Path {
				case "/block_results":
					h := r.URL.Query().Get("height")
					if h == "100" {
						_, _ = w.Write([]byte(`{"result":{"begin_block_events":[{"type":"slash","attributes":[{"key":"address","value":"` + atomoneValcons + `"},{"key":"reason","value":"missing_signature"},{"key":"burned_coins","value":"0"}]}]}}`))
					} else {
						_, _ = w.Write([]byte(`{"result":{}}`))
					}
				case "/block":
					_, _ = w.Write([]byte(`{"result":{}}`))
				case "/block_search":
					searches.Add(1)
					if r.URL.Query().Get("order_by") != `"desc"` || r.URL.Query().Get("per_page") != "5" {
						t.Error("search not bounded and newest first")
					}
					if !strings.Contains(r.URL.Query().Get("query"), "slash.address='") {
						t.Error("wrong search query")
					}
					_, _ = w.Write([]byte(`{"result":{"blocks":[{"block":{"header":{"height":"100"}}}]}}`))
				default:
					t.Errorf("unexpected RPC: %s", r.URL.Path)
				}
			}))
			defer server.Close()
			client, err := rpchttp.New(server.URL, "/websocket")
			if err != nil {
				t.Fatal(err)
			}
			cc := &ChainConfig{name: "AtomOne", client: client}
			c.Chains[cc.name] = cc
			val := &ValInfo{Valcons: atomoneValcons, Jailed: true, SlashDowntime: "0"}
			height := int64(102)
			if restart {
				height = 200
			}
			cc.collectSlash(val, &ValInfo{}, restart, height)
			d := cc.slashSnapshot()
			if d.Height != 100 || d.Reason != "missing_signature" || !d.Searched || d.slashed() != "" {
				t.Fatalf("history: %+v", d)
			}
			if restart && searches.Load() != 1 || !restart && searches.Load() != 0 {
				t.Fatalf("searches: %d", searches.Load())
			}
			before := calls.Load()
			cc.collectSlash(val, val, true, height+1)
			if calls.Load() != before {
				t.Fatal("fetched a known incident again")
			}
			cc.collectSlash(&ValInfo{Valcons: val.Valcons}, val, false, height+1)
			if cc.slashSnapshot().Valcons != "" {
				t.Fatal("kept details after recovery")
			}
		})
	}
}

func TestSlashLiveAndPersisted(t *testing.T) {
	c := auditConfig(t)
	block, val := atomoneSlashFixture(t)
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		t.Errorf("unexpected RPC: %s", r.URL)
	}))
	defer server.Close()
	cc := &ChainConfig{name: "AtomOne", ChainId: "atomone-1", ValAddress: "valoper", valInfo: val}
	c.Chains[cc.name] = cc
	data, _ := json.Marshal(block)
	cc.observeSlash(context.Background(), server.URL, data, val)
	cc.observeSlash(context.Background(), server.URL, data, val)
	if calls.Load() != 0 {
		t.Fatalf("unexpected fetches: %d", calls.Load())
	}
	stateFile := filepath.Join(t.TempDir(), "state.json")
	if err := saveState(stateFile); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(stateFile)
	if err != nil {
		t.Fatal(err)
	}
	var saved savedState
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(saved.Slashes[cc.name], cc.slashSnapshot()) {
		t.Fatalf("lost persisted details: %+v", saved.Slashes)
	}
	var legacy map[string]interface{}
	if err := json.Unmarshal(data, &legacy); err != nil {
		t.Fatal(err)
	}
	slash := legacy["slashes"].(map[string]interface{})[cc.name].(map[string]interface{})
	for _, key := range []string{"hash_a", "hash_b", "kept", "dropped"} {
		slash[key] = "OLD-HASH"
	}
	slash["canonical_checked"] = true
	data, err = json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stateFile, data, 0600); err != nil {
		t.Fatal(err)
	}
	configFile := filepath.Join(filepath.Dir(stateFile), "config.yml")
	if err := os.WriteFile(configFile, []byte("chains:\n  AtomOne:\n    chain_id: atomone-1\n    valoper_address: valoper\n"), 0600); err != nil {
		t.Fatal(err)
	}
	password := ""
	restored, err := loadConfig(configFile, stateFile, filepath.Join(filepath.Dir(stateFile), "chains.d"), &password)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.cancel()
	defer restored.deliveryCancel()
	if !reflect.DeepEqual(restored.Chains[cc.name].slashSnapshot(), cc.slashSnapshot()) {
		t.Fatal("restart lost slash details")
	}
	next := &ChainConfig{name: cc.name, ChainId: cc.ChainId, ValAddress: cc.ValAddress}
	carryChainHistory(cc, next)
	if !reflect.DeepEqual(next.slashSnapshot(), cc.slashSnapshot()) {
		t.Fatal("reload lost slash details")
	}
}

func TestSlashRestartWithoutDetails(t *testing.T) {
	for _, indexed := range []bool{false, true} {
		t.Run(map[bool]string{false: "no indexer", true: "jailed index"}[indexed], func(t *testing.T) {
			c := auditConfig(t)
			var searches atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/block_search":
					searches.Add(1)
					if !indexed {
						_, _ = w.Write([]byte(`{"error":{"code":-32603,"message":"indexing disabled"}}`))
						return
					}
					if strings.Contains(r.URL.Query().Get("query"), "slash.jailed=") {
						_, _ = w.Write([]byte(`{"result":{"blocks":[{"block":{"header":{"height":"100"}}}]}}`))
					} else {
						_, _ = w.Write([]byte(`{"result":{"blocks":[]}}`))
					}
				case "/block_results":
					_, _ = w.Write([]byte(`{"result":{"finalize_block_events":[{"type":"slash","attributes":[{"key":"address","value":"` + atomoneValcons + `"},{"key":"reason","value":"missing_signature"},{"key":"burned_coins","value":"0"}]}]}}`))
				default:
					t.Errorf("unexpected RPC: %s", r.URL.Path)
				}
			}))
			defer server.Close()
			client, err := rpchttp.New(server.URL, "/websocket")
			if err != nil {
				t.Fatal(err)
			}
			cc := &ChainConfig{name: "AtomOne", client: client, slash: slashDetails{Valcons: atomoneValcons, Searched: true}}
			c.Chains[cc.name] = cc
			val := &ValInfo{Valcons: atomoneValcons, Jailed: true, SlashDowntime: "0"}
			cc.collectSlash(val, &ValInfo{}, true, 200)
			if searches.Load() != 2 {
				t.Fatalf("restart searches: %d", searches.Load())
			}
			if indexed && cc.slashSnapshot().Height != 100 || !indexed && cc.slashSnapshot().Height != 0 {
				t.Fatalf("details: %+v", cc.slashSnapshot())
			}
			cc.collectSlash(val, val, true, 201)
			if searches.Load() != 2 {
				t.Fatal("repeated unknown restart search")
			}
		})
	}
}
