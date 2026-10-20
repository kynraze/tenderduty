package tenderduty

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	bank "github.com/cosmos/cosmos-sdk/x/bank/types"
	staking "github.com/cosmos/cosmos-sdk/x/staking/types"
)

// One incident, copied under stateMux and persisted with the alarms.
type slashDetails struct {
	Valcons         string    `json:"valcons"`
	Height          int64     `json:"height,omitempty"`
	SeenHeight      int64     `json:"seen_height,omitempty"`
	Searched        bool      `json:"searched,omitempty"`
	Reason          string    `json:"reason,omitempty"`
	Burned          string    `json:"burned,omitempty"`
	Denom           string    `json:"denom,omitempty"`
	Symbol          string    `json:"symbol,omitempty"`
	Exponent        uint32    `json:"exponent,omitempty"`
	DisplayKnown    bool      `json:"display_known,omitempty"`
	TokenChecked    bool      `json:"token_checked,omitempty"`
	EvidenceChecked bool      `json:"evidence_checked,omitempty"`
	ResultsChecked  bool      `json:"results_checked,omitempty"`
	Fraction        string    `json:"fraction,omitempty"`
	VoteHeight      int64     `json:"vote_height,omitempty"`
	Round           int64     `json:"round,omitempty"`
	VoteType        int64     `json:"vote_type,omitempty"`
	Time            time.Time `json:"time,omitempty"`
}

type slashEvent struct {
	Type       string `json:"type"`
	Attributes []struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	} `json:"attributes"`
}

type slashVote struct {
	Height           jsonInt `json:"height"`
	Round            jsonInt `json:"round"`
	Type             jsonInt `json:"type"`
	ValidatorAddress string  `json:"validator_address"`
	BlockID          struct {
		Hash  string `json:"hash"`
		Parts struct {
			Total jsonInt `json:"total"`
			Hash  string  `json:"hash"`
		} `json:"parts"`
	} `json:"block_id"`
}

type duplicateVotes struct {
	VoteA     slashVote `json:"vote_a"`
	VoteB     slashVote `json:"vote_b"`
	Timestamp time.Time `json:"timestamp"`
}

type slashBlock struct {
	Block struct {
		Header struct {
			Height jsonInt `json:"height"`
		} `json:"header"`
		Evidence struct {
			Evidence []struct {
				Type  string         `json:"type"`
				Value duplicateVotes `json:"value"`
			} `json:"evidence"`
		} `json:"evidence"`
	} `json:"block"`
	Begin       []slashEvent `json:"begin_block_events"`
	Finalize    []slashEvent `json:"finalize_block_events"`
	ResultBegin struct {
		Events []slashEvent `json:"events"`
	} `json:"result_begin_block"`
	ResultFinalize struct {
		Events []slashEvent `json:"events"`
	} `json:"result_finalize_block"`
}

func slashAttribute(value string) string {
	if decoded, err := base64.StdEncoding.DecodeString(value); err == nil {
		text := string(decoded)
		if text == "address" || text == "jailed" || text == "reason" || text == "burned_coins" || text == "power" || text == "double_sign" || text == "missing_signature" || strings.Contains(text, "valcons1") {
			return text
		}
		if _, ok := new(big.Int).SetString(text, 10); ok {
			return text
		}
	}
	return value
}

func (d *slashDetails) read(block slashBlock, val *ValInfo, height int64) bool {
	if val == nil || val.Valcons == "" || height <= 0 {
		return false
	}
	found := false
	events := append(append(append(append([]slashEvent(nil), block.Begin...), block.Finalize...), block.ResultBegin.Events...), block.ResultFinalize.Events...)
	for _, event := range events {
		if event.Type != "slash" {
			continue
		}
		attrs := map[string]string{}
		for _, a := range event.Attributes {
			value := slashAttribute(a.Value)
			if decoded, err := base64.StdEncoding.DecodeString(a.Value); err == nil && string(decoded) == val.Valcons {
				value = val.Valcons
			}
			attrs[slashAttribute(a.Key)] = value
		}
		if attrs["address"] != val.Valcons && attrs["jailed"] != val.Valcons {
			continue
		}
		if height < d.Height {
			continue
		}
		if height > d.Height {
			*d = slashDetails{Valcons: val.Valcons, Height: height}
		}
		found = true
		if attrs["reason"] != "" {
			d.Reason = attrs["reason"]
		}
		if attrs["burned_coins"] != "" {
			d.Burned = attrs["burned_coins"]
		}
	}
	address := hex.EncodeToString(val.Conspub)
	for _, evidence := range block.Block.Evidence.Evidence {
		v := evidence.Value
		if !strings.HasSuffix(evidence.Type, "/DuplicateVoteEvidence") || address == "" || !strings.EqualFold(v.VoteA.ValidatorAddress, address) || !strings.EqualFold(v.VoteB.ValidatorAddress, address) {
			continue
		}
		if v.VoteA.Height <= 0 || v.VoteA.Height != v.VoteB.Height || v.VoteA.Round != v.VoteB.Round || v.VoteA.Type != v.VoteB.Type || (strings.EqualFold(v.VoteA.BlockID.Hash, v.VoteB.BlockID.Hash) && v.VoteA.BlockID.Parts.Total == v.VoteB.BlockID.Parts.Total && strings.EqualFold(v.VoteA.BlockID.Parts.Hash, v.VoteB.BlockID.Parts.Hash)) {
			continue
		}
		if d.Height > height {
			continue
		}
		if d.Height < height {
			*d = slashDetails{Valcons: val.Valcons, Height: height}
		}
		d.Reason, d.VoteHeight, d.Time = "double_sign", int64(v.VoteA.Height), v.Timestamp
		d.Round, d.VoteType = int64(v.VoteA.Round), int64(v.VoteA.Type)
		found = true
	}
	return found
}

func slashRPC(ctx context.Context, remote, path string, query url.Values, result interface{}) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(remote, "/")+"/"+path+"?"+query.Encode(), nil)
	if err != nil {
		return err
	}
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("slash RPC status %d", resp.StatusCode)
	}
	var envelope struct {
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&envelope); err != nil {
		return err
	}
	if len(envelope.Result) == 0 || string(envelope.Result) == "null" {
		return fmt.Errorf("slash RPC unavailable")
	}
	return json.Unmarshal(envelope.Result, result)
}

func (cc *ChainConfig) slashSnapshot() slashDetails {
	cc.stateMux.RLock()
	defer cc.stateMux.RUnlock()
	return cc.slash
}

func (cc *ChainConfig) saveSlash(d slashDetails) {
	cc.stateMux.Lock()
	cc.slash = d
	cc.stateMux.Unlock()
}

func (cc *ChainConfig) observeSlash(ctx context.Context, remote string, data []byte, val *ValInfo) {
	var block slashBlock
	if json.Unmarshal(data, &block) != nil {
		return
	}
	cc.slashMux.Lock()
	defer cc.slashMux.Unlock()
	d := cc.slashSnapshot()
	if d.Valcons != val.Valcons {
		d = slashDetails{Valcons: val.Valcons}
	}
	if !d.read(block, val, int64(block.Block.Header.Height)) {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cc.enrichSlash(ctx, remote, &d, val)
	cc.saveSlash(d)
}

// Fetch history for fresh jail or tombstone incidents, or unknown incidents after restart.
func (cc *ChainConfig) collectSlash(val, previous *ValInfo, first bool, height int64) {
	if val.Valcons == "" {
		return
	}
	cc.slashMux.Lock()
	defer cc.slashMux.Unlock()
	d := cc.slashSnapshot()
	if !val.Jailed && !val.Tombstoned {
		if previous.Jailed || previous.Tombstoned {
			cc.saveSlash(slashDetails{})
		}
		return
	}
	if d.Valcons != val.Valcons || val.Tombstoned && !previous.Tombstoned && d.Reason != "double_sign" {
		d = slashDetails{Valcons: val.Valcons}
	}
	client := cc.clientSnapshot()
	if client == nil {
		return
	}
	ctx, cancel := context.WithTimeout(cc.context(), 20*time.Second)
	defer cancel()
	remote := client.Remote()
	if height <= 0 {
		cc.stateMux.RLock()
		height = cc.lastBlockNum
		cc.stateMux.RUnlock()
	}
	if d.SeenHeight == 0 {
		d.SeenHeight = height
	}
	if d.Height == 0 && !d.Searched {
		d.SeenHeight, d.Searched = height, true
		for h := height; h > 0 && h > height-12; h-- {
			q := url.Values{"height": {strconv.FormatInt(h, 10)}}
			var results, block slashBlock
			_ = slashRPC(ctx, remote, "block_results", q, &results)
			found := d.read(results, val, h)
			if val.Tombstoned || found {
				_ = slashRPC(ctx, remote, "block", q, &block)
				found = d.read(block, val, h) || found
				if found {
					d.EvidenceChecked = true
				}
			}
			if found {
				break
			}
		}

		d.SeenHeight, d.Searched = height, true
	}
	cc.stateMux.Lock()
	startup := first && !cc.slashSearchDone
	if startup {
		cc.slashSearchDone = true
	}
	cc.stateMux.Unlock()
	if startup {
		// Retry incomplete lookups once after restart; known facts need no RPC.
		if val.Tombstoned && d.VoteHeight == 0 {
			d.EvidenceChecked = false
		}
		if d.Burned == "" {
			d.ResultsChecked = false
		}
		if d.Denom == "" {
			d.TokenChecked = false
		}
	}
	if startup && d.Height == 0 {
		searchCtx, searchCancel := context.WithTimeout(cc.context(), 10*time.Second)
		cc.searchSlash(searchCtx, remote, &d, val)
		searchCancel()
		d.SeenHeight, d.Searched = height, true
	}
	if d.Height > 0 && d.Burned == "" && !d.ResultsChecked {
		d.ResultsChecked = true
		var results slashBlock
		if slashRPC(ctx, remote, "block_results", url.Values{"height": {strconv.FormatInt(d.Height, 10)}}, &results) == nil {
			d.read(results, val, d.Height)
		}
	}
	if val.Tombstoned && d.Height > 0 && d.VoteHeight == 0 && !d.EvidenceChecked {
		d.EvidenceChecked = true
		var block slashBlock
		if slashRPC(ctx, remote, "block", url.Values{"height": {strconv.FormatInt(d.Height, 10)}}, &block) == nil {
			d.read(block, val, d.Height)
		}
	}
	enrichCtx, enrichCancel := context.WithTimeout(cc.context(), 10*time.Second)
	defer enrichCancel()
	cc.enrichSlash(enrichCtx, remote, &d, val)
	cc.saveSlash(d)
}

// Old incidents may exceed the scan limit; unindexed nodes reject history queries.
func (cc *ChainConfig) searchSlash(ctx context.Context, remote string, d *slashDetails, val *ValInfo) {
	for _, key := range []string{"slash.address", "slash.jailed"} {
		var search struct {
			Blocks []slashBlock `json:"blocks"`
		}
		q := url.Values{"query": {strconv.Quote(fmt.Sprintf("%s='%s'", key, val.Valcons))}, "order_by": {strconv.Quote("desc")}, "per_page": {"5"}}
		if slashRPC(ctx, remote, "block_search", q, &search) != nil {
			continue
		}
		for _, block := range search.Blocks {
			h := int64(block.Block.Header.Height)
			if h <= 0 {
				continue
			}
			var results slashBlock
			_ = slashRPC(ctx, remote, "block_results", url.Values{"height": {strconv.FormatInt(h, 10)}}, &results)
			found := d.read(results, val, h)
			found = d.read(block, val, h) || found
			if found {
				break
			}
		}
		if d.Height != 0 {
			break
		}
	}
}

func (cc *ChainConfig) enrichSlash(ctx context.Context, remote string, d *slashDetails, val *ValInfo) {
	if d.Reason == "double_sign" && val.SlashDoubleSign != "" {
		d.Fraction = val.SlashDoubleSign
	} else if d.Reason == "missing_signature" && val.SlashDowntime != "" {
		d.Fraction = val.SlashDowntime
	}
	if amount, ok := new(big.Int).SetString(d.Burned, 10); ok && amount.Sign() > 0 && !d.TokenChecked {
		d.TokenChecked = true
		cc.slashToken(ctx, d)
	}
}

type bondToken struct {
	Denom        string
	Symbol       string
	Exponent     uint32
	DisplayKnown bool
	Multistaking bool
}

func (t bondToken) stakeKnown() bool {
	return t.DisplayKnown && t.Symbol != "" && t.Exponent <= 30 && !t.Multistaking
}

func (cc *ChainConfig) cachedBondToken(ctx context.Context) bondToken {
	cc.stateMux.Lock()
	token := cc.bondToken
	if token.DisplayKnown || time.Since(cc.bondTokenAt) < 10*time.Minute {
		cc.stateMux.Unlock()
		return token
	}
	cc.bondTokenAt = time.Now()
	cc.stateMux.Unlock()
	token = cc.resolveBondToken(ctx)
	cc.stateMux.Lock()
	cc.bondToken = token
	cc.stateMux.Unlock()
	return token
}

func (cc *ChainConfig) slashToken(ctx context.Context, d *slashDetails) {
	token := cc.cachedBondToken(ctx)
	d.Denom, d.Symbol, d.Exponent, d.DisplayKnown = token.Denom, token.Symbol, token.Exponent, token.DisplayKnown
}

func (cc *ChainConfig) resolveBondToken(ctx context.Context) (token bondToken) {
	d := &token
	client := cc.clientSnapshot()
	if client == nil {
		return
	}
	request := append(binary.AppendUvarint([]byte{0x0a}, uint64(len(cc.ValAddress))), cc.ValAddress...)
	if resp, err := client.ABCIQuery(ctx, "/multistaking.v1.Query/ValidatorMultiStakingCoin", request); err == nil && resp != nil && resp.Response.Code == 0 {
		d.Denom, _ = firstStringField(resp.Response.Value)
		d.Multistaking = d.Denom != ""
	}
	if d.Denom == "" {
		b, _ := (&staking.QueryParamsRequest{}).Marshal()
		resp, err := client.ABCIQuery(ctx, "/cosmos.staking.v1beta1.Query/Params", b)
		var params staking.QueryParamsResponse
		if err == nil && resp != nil && resp.Response.Code == 0 && params.Unmarshal(resp.Response.Value) == nil {
			d.Denom = params.Params.BondDenom
		}
	}
	if d.Denom == "" {
		return
	}
	b, _ := (&bank.QueryDenomMetadataRequest{Denom: d.Denom}).Marshal()
	resp, err := client.ABCIQuery(ctx, "/cosmos.bank.v1beta1.Query/DenomMetadata", b)
	var metadata bank.QueryDenomMetadataResponse
	if err == nil && resp != nil && resp.Response.Code == 0 && metadata.Unmarshal(resp.Response.Value) == nil && metadata.Metadata.Symbol != "" {
		for _, unit := range metadata.Metadata.DenomUnits {
			if unit.Denom == metadata.Metadata.Display {
				d.Symbol, d.Exponent, d.DisplayKnown = metadata.Metadata.Symbol, unit.Exponent, true
				return
			}
		}
	}
	chain, err := fetchRegistryChain(ctx, cc.ChainId)
	if err != nil {
		return
	}
	for _, asset := range chain.Assets {
		if string(asset.Base) != d.Denom || asset.Symbol == "" {
			continue
		}
		for _, unit := range asset.DenomUnits {
			if unit.Denom == string(asset.Display) {
				d.Symbol, d.Exponent, d.DisplayKnown = asset.Symbol, unit.Exponent, true
				return
			}
		}
	}
	return
}

func groupedAmount(value string) string {
	whole, fraction, _ := strings.Cut(value, ".")
	for i := len(whole) - 3; i > 0; i -= 3 {
		whole = whole[:i] + "," + whole[i:]
	}
	if fraction != "" {
		return whole + "." + fraction
	}
	return whole
}

func (d slashDetails) slashed() string {
	amount, ok := new(big.Int).SetString(d.Burned, 10)
	if !ok || amount.Sign() <= 0 {
		return ""
	}
	value, denom := amount.String(), d.Denom
	if d.DisplayKnown && d.Symbol != "" && d.Exponent <= 30 {
		scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(d.Exponent)), nil)
		ratio := new(big.Rat).SetFrac(amount, scale)
		precision := 1
		if ratio.Cmp(big.NewRat(1000, 1)) >= 0 {
			precision = 0
		}
		value = ratio.FloatString(precision)
		if precision > 0 {
			value = strings.TrimSuffix(value, ".0")
		}
		if value == "0" {
			value = ratio.FloatString(int(d.Exponent))
			value = strings.TrimRight(strings.TrimRight(value, "0"), ".")
		}
		denom = d.Symbol
	}
	line := strings.TrimSpace(groupedAmount(value) + " " + denom)
	if fraction, ok := new(big.Rat).SetString(d.Fraction); ok {
		percent := fraction.Mul(fraction, big.NewRat(100, 1)).FloatString(8)
		percent = strings.TrimRight(strings.TrimRight(percent, "0"), ".")
		line += " (" + percent + "%)"
	}
	return "Slashed " + line
}

func (d slashDetails) detail(tombstoned bool, loc *time.Location) string {
	var lines []string
	if tombstoned && d.VoteHeight > 0 {
		line := "Double-signed " + thousands(d.VoteHeight) + " · round " + strconv.FormatInt(d.Round, 10)
		switch d.VoteType {
		case 1:
			line += " · prevote"
		case 2:
			line += " · precommit"
		case 32:
			line += " · proposal"
		}
		if !d.Time.IsZero() {
			line += " · " + d.Time.In(loc).Format("2 Jan 15:04 MST")
		}
		lines = append(lines, line)
	}
	if line := d.slashed(); line != "" {
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}
