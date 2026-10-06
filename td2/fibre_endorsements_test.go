package tenderduty

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	dash "github.com/kynraze/tenderduty/v2/td2/dashboard"
	"github.com/prometheus/client_golang/prometheus"
	rpchttp "github.com/tendermint/tendermint/rpc/client/http"
	"google.golang.org/protobuf/encoding/protowire"
)

func endorsementBytes(b []byte, field protowire.Number, value []byte) []byte {
	return protowire.AppendBytes(protowire.AppendTag(b, field, protowire.BytesType), value)
}

func endorsementVarint(b []byte, field protowire.Number, value uint64) []byte {
	return protowire.AppendVarint(protowire.AppendTag(b, field, protowire.VarintType), value)
}

func endorsementTx(height uint64, signatures [][]byte, wrapper string) []byte {
	promise := endorsementVarint(nil, 2, height)
	promise = endorsementBytes(promise, 3, bytes.Repeat([]byte{1}, 29))
	promise = endorsementVarint(promise, 4, 2048)
	promise = endorsementBytes(promise, 6, bytes.Repeat([]byte{2}, 32))
	message := endorsementBytes(nil, 1, []byte("payer"))
	message = endorsementBytes(message, 2, promise)
	for _, signature := range signatures {
		message = endorsementBytes(message, 3, signature)
	}
	any := endorsementBytes(nil, 1, []byte(fibrePaymentType))
	any = endorsementBytes(any, 2, message)
	body := endorsementBytes(nil, 1, any)
	tx := endorsementBytes(nil, 1, body)
	if wrapper != "" {
		tx = endorsementBytes(nil, 1, tx)
		tx = endorsementBytes(tx, 3, []byte(wrapper))
	}
	return tx
}

func endorsementBlock(t *testing.T, txs [][]byte, height int64) *rawBlock {
	t.Helper()
	var b rawBlock
	encoded, err := json.Marshal(map[string]interface{}{"block": map[string]interface{}{
		"header": map[string]string{"height": strconv.FormatInt(height, 10)},
		"data":   map[string]interface{}{"txs": txs},
	}})
	if err != nil || json.Unmarshal(encoded, &b) != nil {
		t.Fatal("could not build block", err)
	}
	return &b
}

func TestFibreEndorsementTxDecoding(t *testing.T) {
	signatures := [][]byte{nil, bytes.Repeat([]byte{3}, 64), nil}
	for _, wrapper := range []string{"", "BLOB", "INDX"} {
		payments := fibrePayments(endorsementTx(100, signatures, wrapper))
		if len(payments) != 1 || payments[0].height != 100 || payments[0].blob.BlobSize != 2048 || !reflect.DeepEqual(payments[0].signatures[1], signatures[1]) || len(payments[0].signatures[0]) != 0 {
			t.Fatalf("%s: decoded %+v", wrapper, payments)
		}
	}
	normal := endorsementBytes(nil, 1, endorsementBytes(nil, 1, endorsementBytes(nil, 1, []byte("/cosmos.bank.v1beta1.MsgSend"))))
	for _, tx := range [][]byte{nil, {0xff}, normal, endorsementTx(100, signatures, "other"), endorsementTx(0, signatures, "")} {
		if got := fibrePayments(tx); len(got) != 0 {
			t.Fatalf("unexpected Fibre payment: %+v", got)
		}
	}
	tx := endorsementTx(100, signatures, "")
	tx = append(tx, 0xff)
	if len(fibrePayments(tx)) != 0 {
		t.Fatal("malformed outer transaction counted")
	}
}

func TestFibreEndorsementLiveCollection(t *testing.T) {
	r := newFibreRig(t)
	r.vi.Conspub = bytes.Repeat([]byte{7}, 20)
	address := hex.EncodeToString(r.vi.Conspub)
	requests, blockRequests := map[string]int{}, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/block" {
			blockRequests++
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"result": endorsementBlock(t, [][]byte{endorsementTx(100, [][]byte{nil, {1}, nil}, "")}, 205)})
			return
		}
		height := request.URL.Query().Get("height")
		requests[height]++
		if height == "101" {
			_, _ = fmt.Fprint(w, `{"error":{"message":"height pruned"}}`)
			return
		}
		if height == "102" {
			_, _ = fmt.Fprintf(w, `{"result":{"block_height":"102","total":"3","validators":[{"address":"A","voting_power":"0"},{"address":"%s","voting_power":0},{"address":"C","voting_power":"0"}]}}`, address)
			return
		}
		_, _ = fmt.Fprintf(w, `{"result":{"block_height":"%s","total":"3","validators":[{"address":"A","voting_power":"25"},{"address":"%s","voting_power":40},{"address":"C","voting_power":"35"}]}}`, height, address)
	}))
	defer server.Close()
	client, err := rpchttp.New(server.URL, "/websocket")
	if err != nil {
		t.Fatal(err)
	}
	r.cc.setClient(client)
	watcher := &fibreEndorsementWatcher{}
	for i, tx := range [][]byte{
		endorsementTx(100, [][]byte{nil, {1}, nil}, ""),
		endorsementTx(100, [][]byte{{1}, nil, {1}}, "INDX"),
		endorsementTx(101, [][]byte{nil, {1}, nil}, "BLOB"),
		endorsementTx(101, [][]byte{nil, {1}, nil}, ""),
	} {
		watcher.observe(context.Background(), r.cc, endorsementBlock(t, [][]byte{tx}, int64(200+i)))
	}
	day, missed := r.st.endorsementSummary("Chain")
	if day.Endorsed != 1 || day.Missed != 1 || day.Unknown != 2 || len(missed) != 1 || missed[0].Height != 201 || requests["100"] != 1 || requests["101"] != 1 || blockRequests != 0 {
		t.Fatalf("day=%+v, missed=%+v, requests=%v, blocks=%d", day, missed, requests, blockRequests)
	}
	if missed[0].Signed != 2 || missed[0].Validators != 3 || missed[0].Power != 60 {
		t.Fatalf("signature power: %+v", missed[0])
	}
	watcher.observe(context.Background(), r.cc, endorsementBlock(t, [][]byte{[]byte("not protobuf")}, 204))
	if requests["100"] != 1 || requests["101"] != 1 {
		t.Fatal("non-Fibre tx queried validators")
	}
	watcher.observe(context.Background(), r.cc, endorsementBlock(t, nil, 205))
	if blockRequests != 0 {
		t.Fatal("empty block triggered fallback")
	}
	block := endorsementBlock(t, nil, 205)
	block.Block.Data = nil
	watcher.observe(context.Background(), r.cc, block)
	if blockRequests != 1 || requests["100"] != 1 {
		t.Fatal("fallback must fetch block once and reuse validator cache")
	}
	watcher.observe(context.Background(), r.cc, endorsementBlock(t, [][]byte{endorsementTx(102, [][]byte{{1}, nil, {1}}, "")}, 206))
	_, missed = r.st.endorsementSummary("Chain")
	if missed[0].Signed != 2 || missed[0].Validators != 3 || missed[0].Power != 0 {
		t.Fatalf("zero total power: %+v", missed[0])
	}
	r.cc.Alerts.FibreAlerts = false
	watcher.observe(context.Background(), r.cc, block)
	if blockRequests != 1 {
		t.Fatal("disabled Fibre fetched a block")
	}
	if len(r.c.alertChan) != 0 {
		t.Fatal("endorsements emitted an alert")
	}
}

func TestFibreEndorsementValidatorPaginationAndLRU(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		validators := make([]map[string]string, 100)
		for i := range validators {
			validators[i] = map[string]string{"address": fmt.Sprintf("%d", i), "voting_power": "2"}
		}
		if r.URL.Query().Get("page") == "2" {
			validators = []map[string]string{{"address": "ours", "voting_power": "3"}}
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"result": map[string]interface{}{
			"block_height": r.URL.Query().Get("height"), "total": "101", "validators": validators,
		}})
	}))
	defer server.Close()
	w := &fibreEndorsementWatcher{}
	slot := w.slot(context.Background(), server.URL, "OURS", 1)
	if slot.err != nil || slot.index != 100 || slot.size != 101 || len(slot.powers) != 101 || slot.powers[0] != 2 || slot.powers[100] != 3 || requests != 2 {
		t.Fatalf("slot=%+v, requests=%d", slot, requests)
	}
	for height := int64(2); height <= 64; height++ {
		w.slot(context.Background(), server.URL, "ours", height)
	}
	w.slot(context.Background(), server.URL, "ours", 1)
	before := requests
	w.slot(context.Background(), server.URL, "ours", 65)
	w.slot(context.Background(), server.URL, "ours", 1)
	if len(w.slots) != 64 || requests != before+2 {
		t.Fatal("LRU did not retain recently used height")
	}
	w.slot(context.Background(), server.URL, "ours", 2)
	if requests != before+4 {
		t.Fatal("LRU did not evict least recently used height")
	}
}

func TestFibreEndorsementPersistenceAndPresentation(t *testing.T) {
	r := newFibreRig(t)
	blob := dash.FibreMissedBlob{Height: 100, Time: r.clock.now.Unix(), Commitment: strings.Repeat("02", 32), Namespace: "0102", BlobSize: 2048, Signed: 40, Validators: 84, Power: 67.25}
	r.st.recordEndorsement("Chain", "endorsed", blob)
	for i := 0; i < 25; i++ {
		blob.Height++
		r.st.recordEndorsement("Chain", "missed", blob)
	}
	r.st.recordEndorsement("Chain", "unknown", blob)
	path := filepath.Join(t.TempDir(), "state.json")
	if err := saveState(path); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved savedState
	if err := json.Unmarshal(body, &saved); err != nil {
		t.Fatal(err)
	}
	restored := newFibreStore()
	restored.now = r.clock.Now
	restored.restoreHours(saved.FibreRollups)
	restored.restoreShort(saved.FibreMinutes, "1h")
	restored.restoreShort(saved.FibreTenmin, "24h")
	restored.restoreMissed(saved.FibreMissed)
	day, missed := restored.endorsementSummary("Chain")
	if day.Endorsed != 1 || day.Missed != 25 || day.Unknown != 1 || len(missed) != 20 || missed[0].Height != 125 || missed[19].Height != 106 {
		t.Fatalf("day=%+v, missed=%+v", day, missed)
	}
	if !reflect.DeepEqual(r.st.latestMissed["Chain"], missed) {
		t.Fatalf("missed metadata changed after restore: %+v", missed)
	}
	for _, span := range []string{"1h", "24h", "7d", "30d"} {
		if !reflect.DeepEqual(r.st.series("Chain", span), restored.series("Chain", span)) {
			t.Fatalf("%s history changed after restore", span)
		}
	}
	report := r.cc.fibreReportFrom(r.clock.now, restored)
	if report.dashboard().Last24h.Missed != 25 || len(report.dashboard().LatestMissed) != 20 || !strings.Contains(strings.Join(report.telegramLines(), "\n"), "1 of 26 · 3.8%") {
		t.Fatal("endorsement summary missing without OTLP exports")
	}
	lines := r.c.reportFibre("Chain", r.clock.now.Add(-time.Hour), r.clock.now.Add(time.Minute), false)
	if len(lines) == 0 || !strings.Contains(lines[0], "endorsed 1 of 26 (3.8%)") {
		t.Fatalf("report missing endorsements: %v", lines)
	}
	r.advance(25 * time.Hour)
	day, _ = restored.endorsementSummary("Chain")
	if day.Endorsed+day.Missed+day.Unknown != 0 {
		t.Fatal("expired endorsements remained in 24h summary")
	}
}

func TestFibreEndorsementRestoreRejectsInvalidMisses(t *testing.T) {
	st := newFibreStore()
	blob := dash.FibreMissedBlob{Height: 1, Time: time.Now().Unix(), Commitment: strings.Repeat("ab", 32), Namespace: "ff", BlobSize: 1}
	invalid := blob
	invalid.Commitment = "not hex"
	future := blob
	future.Time += 3600
	raw, _ := json.Marshal(map[string][]dash.FibreMissedBlob{"Chain": {invalid, future, blob}})
	st.restoreMissed(raw)
	_, missed := st.endorsementSummary("Chain")
	if len(missed) != 1 {
		t.Fatalf("invalid misses restored: %+v", missed)
	}
}

func TestFibreEndorsementRestoreRejectsInvalidCounts(t *testing.T) {
	for _, count := range []string{"-1", "0.5", "1e30"} {
		st := newFibreStore()
		at := st.now().Truncate(time.Hour).Unix()
		raw := json.RawMessage(fmt.Sprintf(`{"Chain":{"%d":{"time":%d,"counts":{"endorsements_missed":%s}}}}`, at, at, count))
		st.restoreHours(raw)
		if len(st.hours["Chain"]) != 0 {
			t.Fatalf("invalid endorsement count restored: %s", count)
		}
	}
}

func TestFibreNewBlockCarriesBase64Transactions(t *testing.T) {
	tx := endorsementTx(1, [][]byte{{1}}, "")
	var block rawBlock
	if err := json.Unmarshal([]byte(fmt.Sprintf(`{"block":{"header":{"height":"2"},"data":{"txs":["%s"]}}}`, base64.StdEncoding.EncodeToString(tx))), &block); err != nil || block.Block.Data == nil {
		t.Fatal("NewBlock transactions not decoded", err)
	}
	var txs [][]byte
	if err := json.Unmarshal(block.Block.Data.Txs, &txs); err != nil || len(txs) != 1 || !bytes.Equal(txs[0], tx) {
		t.Fatal("base64 transaction not decoded", err)
	}
}

func TestFibreEndorsementPrometheusWithoutMetrics(t *testing.T) {
	r := newFibreRig(t)
	for _, result := range []string{"endorsed", "missed", "unknown"} {
		r.st.recordEndorsement("Chain", result, dash.FibreMissedBlob{})
	}
	registry := prometheus.NewRegistry()
	registry.MustRegister(&fibreCollector{config: r.c})
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	results := make(map[string]float64)
	for _, family := range families {
		if family.GetName() != "tenderduty_fibre_endorsements_total" {
			continue
		}
		for _, metric := range family.Metric {
			labels := make(map[string]string)
			for _, label := range metric.Label {
				labels[label.GetName()] = label.GetValue()
			}
			if labels["name"] != "Chain" || labels["chain_id"] != "celestia" || labels["moniker"] != "Kynraze" {
				t.Fatalf("unexpected labels: %v", labels)
			}
			results[labels["result"]] = metric.GetCounter().GetValue()
		}
	}
	if !reflect.DeepEqual(results, map[string]float64{"endorsed": 1, "missed": 1, "unknown": 1}) {
		t.Fatalf("missing counters without OTLP: %v", results)
	}
}

func TestFibreEndorsementRestoreDropsInvalidMetadata(t *testing.T) {
	for _, fields := range []dash.FibreMissedBlob{
		{Signed: -1, Validators: 3, Power: 60},
		{Signed: 4, Validators: 3, Power: 60},
		{Validators: -1},
		{Validators: 10001},
		{Signed: 2, Validators: 3, Power: -1},
		{Signed: 2, Validators: 3, Power: 101},
	} {
		st := newFibreStore()
		fields.Height, fields.Time = 1, st.now().Unix()
		fields.Commitment = strings.Repeat("ab", 32)
		raw, err := json.Marshal(map[string][]dash.FibreMissedBlob{"Chain": {fields}})
		if err != nil {
			t.Fatal(err)
		}
		st.restoreMissed(raw)
		_, missed := st.endorsementSummary("Chain")
		if len(missed) != 1 || missed[0].Signed != 0 || missed[0].Validators != 0 || missed[0].Power != 0 || missed[0].Commitment != fields.Commitment {
			t.Fatalf("invalid metadata lost row or survived: %+v", missed)
		}
	}
}
