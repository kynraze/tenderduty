package tenderduty

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	dash "github.com/kynraze/tenderduty/v2/td2/dashboard"
	"google.golang.org/protobuf/encoding/protowire"
)

const fibrePaymentType = "/celestia.fibre.v1.MsgPayForFibre"

type fibrePayment struct {
	height     int64
	blob       dash.FibreMissedBlob
	signatures [][]byte
}

func decodeFibrePayment(b []byte) (fibrePayment, error) {
	p := fibrePayment{}
	err := eachField(b, func(n protowire.Number, t protowire.Type, v []byte, _ uint64) error {
		if t != protowire.BytesType {
			return nil
		}
		switch n {
		case 2:
			return eachField(v, func(n protowire.Number, t protowire.Type, v []byte, u uint64) error {
				switch {
				case n == 2 && t == protowire.VarintType:
					p.height = int64(u)
				case n == 3 && t == protowire.BytesType:
					p.blob.Namespace = hex.EncodeToString(v)
				case n == 4 && t == protowire.VarintType:
					p.blob.BlobSize = u
				case n == 6 && t == protowire.BytesType:
					p.blob.Commitment = hex.EncodeToString(v)
				}
				return nil
			})
		case 3:
			p.signatures = append(p.signatures, v)
		}
		return nil
	})
	if err == nil && (p.height <= 0 || p.blob.BlobSize > 1<<32-1 || len(p.blob.Commitment) != 64 || len(p.blob.Namespace) > 128) {
		err = errors.New("invalid Fibre payment promise")
	}
	return p, err
}

// TxRaw -> TxBody -> Any; wrappers carry the inner tx in field 1.
func fibrePayments(tx []byte) []fibrePayment {
	for depth := 0; depth < 4; depth++ {
		var body []byte
		var wrapper string
		err := eachField(tx, func(n protowire.Number, t protowire.Type, v []byte, _ uint64) error {
			if t == protowire.BytesType {
				if n == 1 {
					body = v
				} else if n == 3 {
					wrapper = string(v)
				}
			}
			return nil
		})
		if err != nil || len(body) == 0 {
			return nil
		}
		var payments []fibrePayment
		err = eachField(body, func(n protowire.Number, t protowire.Type, v []byte, _ uint64) error {
			if n != 1 || t != protowire.BytesType {
				return nil
			}
			var typeURL string
			var message []byte
			if err := eachField(v, func(n protowire.Number, t protowire.Type, v []byte, _ uint64) error {
				if t == protowire.BytesType {
					if n == 1 {
						typeURL = string(v)
					} else if n == 2 {
						message = v
					}
				}
				return nil
			}); err != nil {
				return err
			}
			if typeURL == fibrePaymentType {
				if p, err := decodeFibrePayment(message); err == nil {
					payments = append(payments, p)
				}
			}
			return nil
		})
		if err == nil && len(payments) > 0 {
			return payments
		}
		if wrapper != "BLOB" && wrapper != "INDX" {
			return nil
		}
		tx = body
	}
	return nil
}

type fibreValidatorSlot struct {
	height int64
	index  int
	size   int
	powers []int64
	err    error
}

type fibreEndorsementWatcher struct {
	slots   []fibreValidatorSlot
	lastLog time.Time
	address string
}

func fibreRPC(ctx context.Context, remote, path string, dst interface{}) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(remote, "/")+path, nil)
	if err != nil {
		return err
	}
	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("Fibre RPC HTTP %d", response.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(response.Body, 16<<20)).Decode(dst)
}

func fibreValidatorIndex(ctx context.Context, remote, address string, height int64) (int, int, []int64, error) {
	index, size := -1, 0
	var powers []int64
	for page := 1; page <= 100; page++ {
		var reply struct {
			Result struct {
				Height     jsonInt `json:"block_height"`
				Validators []struct {
					Address string  `json:"address"`
					Power   jsonInt `json:"voting_power"`
				} `json:"validators"`
				Total jsonInt `json:"total"`
			} `json:"result"`
		}
		if err := fibreRPC(ctx, remote, fmt.Sprintf("/validators?height=%d&per_page=100&page=%d", height, page), &reply); err != nil {
			return -1, 0, nil, err
		}
		if int64(reply.Result.Height) != height || reply.Result.Total <= 0 || len(reply.Result.Validators) == 0 {
			return -1, 0, nil, errors.New("validator height unavailable")
		}
		for _, validator := range reply.Result.Validators {
			if strings.EqualFold(validator.Address, address) {
				index = size
			}
			powers = append(powers, int64(validator.Power))
			size++
		}
		if size == int(reply.Result.Total) {
			return index, size, powers, nil
		}
		if size > int(reply.Result.Total) {
			break
		}
	}
	return -1, 0, nil, errors.New("incomplete validator set")
}

func (w *fibreEndorsementWatcher) slot(ctx context.Context, remote, address string, height int64) fibreValidatorSlot {
	if w.address != address {
		w.slots, w.address = nil, address
	}
	for i, slot := range w.slots {
		if slot.height == height {
			w.slots = append(append(w.slots[:i:i], w.slots[i+1:]...), slot)
			return slot
		}
	}
	index, size, powers, err := fibreValidatorIndex(ctx, remote, address, height)
	slot := fibreValidatorSlot{height: height, index: index, size: size, powers: powers, err: err}
	if len(w.slots) == 64 {
		w.slots = w.slots[1:]
	}
	w.slots = append(w.slots, slot)
	return slot
}

func (w *fibreEndorsementWatcher) observe(ctx context.Context, cc *ChainConfig, block *rawBlock) {
	if block == nil || !cc.alertSettings().FibreAlerts {
		return
	}
	client := cc.clientSnapshot()
	if client == nil {
		return
	}
	var encoded json.RawMessage
	if block.Block.Data != nil {
		encoded = block.Block.Data.Txs
	}
	if len(encoded) == 0 {
		var reply struct {
			Result rawBlock `json:"result"`
		}
		if err := fibreRPC(ctx, client.Remote(), fmt.Sprintf("/block?height=%d", block.Block.Header.Height.val()), &reply); err != nil || reply.Result.Block.Header.Height != block.Block.Header.Height || reply.Result.Block.Data == nil || len(reply.Result.Block.Data.Txs) == 0 {
			return
		}
		encoded = reply.Result.Block.Data.Txs
	}
	var txs [][]byte
	if json.Unmarshal(encoded, &txs) != nil {
		return
	}
	info, _ := cc.validatorState()
	address := hex.EncodeToString(info.Conspub)
	now := time.Now()
	seen := false
	for _, tx := range txs {
		for _, payment := range fibrePayments(tx) {
			seen = true
			lookup, cancel := context.WithTimeout(ctx, 10*time.Second)
			slot := w.slot(lookup, client.Remote(), address, payment.height)
			cancel()
			result := "unknown"
			if slot.err == nil && len(payment.signatures) == slot.size {
				var total, signed float64
				payment.blob.Validators = slot.size
				for i, power := range slot.powers {
					total += float64(power)
					if len(payment.signatures[i]) > 0 {
						payment.blob.Signed++
						signed += float64(power)
					}
				}
				if total > 0 {
					payment.blob.Power = 100 * signed / total
				}
				if slot.index >= 0 && len(info.Conspub) == 20 {
					result = "missed"
					if len(payment.signatures[slot.index]) > 0 {
						result = "endorsed"
					}
				}
			}
			if result == "unknown" && (w.lastLog.IsZero() || now.Sub(w.lastLog) >= time.Hour) {
				l(cc.ChainId, "Fibre endorsement unknown at promise height", payment.height, slot.err)
				w.lastLog = now
			}
			payment.blob.Height, payment.blob.Time = block.Block.Header.Height.val(), now.Unix()
			td.fibreData().recordEndorsement(cc.name, result, payment.blob)
			cc.observeEndorsementAlert(result, payment.blob)
		}
	}
	if seen && td.EnableDash {
		select {
		case td.updateChan <- &dash.ChainStatus{MsgType: "fibre", Name: cc.name, Fibre: &dash.FibreStatus{}}:
		default:
		}
	}
}

func (st *fibreStore) recordEndorsement(chain, result string, blob dash.FibreMissedBlob) {
	st.mu.Lock()
	defer st.mu.Unlock()
	now := st.now()
	key := "endorsements_" + result
	values := map[string]float64{"endorsements_endorsed": 0, "endorsements_missed": 0, "endorsements_unknown": 0}
	values[key] = 1
	addFibreValues(st.hour(chain, now).Counts, values)
	for _, span := range []string{"1h", "24h"} {
		_, step := fibreRange(span)
		at := now.Truncate(step).Unix()
		if st.short[span][chain] == nil {
			st.short[span][chain] = make(map[int64]*fibreHour)
		}
		if st.short[span][chain][at] == nil {
			st.short[span][chain][at] = &fibreHour{Time: at, Counts: make(map[string]float64)}
		}
		addFibreValues(st.short[span][chain][at].Counts, values)
		duration, _ := fibreRange(span)
		for key := range st.short[span][chain] {
			if key < at-int64((duration-step)/time.Second) || key > at {
				delete(st.short[span][chain], key)
			}
		}
	}
	if st.totals[chain] == nil {
		st.totals[chain] = make(map[string]float64)
	}
	st.totals[chain][key]++
	st.recordEndorsing(chain, result, blob, now)
	if result == "missed" {
		st.latestMissed[chain] = append([]dash.FibreMissedBlob{blob}, st.latestMissed[chain]...)
		if len(st.latestMissed[chain]) > 20 {
			st.latestMissed[chain] = st.latestMissed[chain][:20]
		}
	}
}

func (st *fibreStore) endorsementSummary(chain string) (dash.FibreWindow, []dash.FibreMissedBlob) {
	st.mu.Lock()
	defer st.mu.Unlock()
	var day dash.FibreWindow
	duration, step := fibreRange("24h")
	end := st.now().Truncate(step).Unix()
	start := end - int64((duration-step)/time.Second)
	for at, h := range st.short["24h"][chain] {
		if at < start || at > end {
			delete(st.short["24h"][chain], at)
			continue
		}
		day.Endorsed += int64(h.Counts["endorsements_endorsed"])
		day.Missed += int64(h.Counts["endorsements_missed"])
		day.Unknown += int64(h.Counts["endorsements_unknown"])
	}
	return day, append([]dash.FibreMissedBlob{}, st.latestMissed[chain]...)
}

func endorsementText(endorsed, missed int64) string {
	if endorsed+missed == 0 {
		return "none seen yet"
	}
	return fmt.Sprintf("%d of %d · %.1f%%", endorsed, endorsed+missed, 100*float64(endorsed)/float64(endorsed+missed))
}

func (st *fibreStore) savedMissed() json.RawMessage {
	st.mu.Lock()
	defer st.mu.Unlock()
	b, _ := json.Marshal(st.latestMissed)
	return b
}

func (st *fibreStore) restoreMissed(raw json.RawMessage) {
	var chains map[string][]dash.FibreMissedBlob
	if json.Unmarshal(raw, &chains) != nil {
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	for chain, blobs := range chains {
		if len(chain) > 256 {
			continue
		}
		for _, blob := range blobs {
			commitment, err := hex.DecodeString(blob.Commitment)
			_, nsErr := hex.DecodeString(blob.Namespace)
			if err != nil || nsErr != nil || len(commitment) != 32 || len(blob.Namespace) > 128 || blob.BlobSize > 1<<32-1 || blob.Height <= 0 || blob.Time <= 0 || blob.Time > st.now().Unix() {
				continue
			}
			if blob.Validators < 0 || blob.Validators > 10000 || blob.Signed < 0 || blob.Signed > blob.Validators || blob.Power < 0 || blob.Power > 100 {
				blob.Signed, blob.Validators, blob.Power = 0, 0, 0
			}
			if len(st.latestMissed[chain]) < 20 {
				st.latestMissed[chain] = append(st.latestMissed[chain], blob)
			}
		}
	}
}
