package tenderduty

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/cosmos/cosmos-sdk/crypto/keys/ed25519"
	"github.com/cosmos/cosmos-sdk/crypto/keys/secp256k1"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/types/bech32"
	slashing "github.com/cosmos/cosmos-sdk/x/slashing/types"
	staking "github.com/cosmos/cosmos-sdk/x/staking/types"
	rpchttp "github.com/tendermint/tendermint/rpc/client/http"
)

// ValInfo holds most of the stats/info used for secondary alarms. It is refreshed roughly every minute.
type ValInfo struct {
	Moniker          string    `json:"moniker"`
	Bonded           bool      `json:"bonded"`
	Jailed           bool      `json:"jailed"`
	Tombstoned       bool      `json:"tombstoned"`
	JailedUntil      time.Time `json:"jailed_until"` // when it can unjail
	Missed           int64     `json:"missed"`
	Window           int64     `json:"window"`
	Conspub          []byte    `json:"conspub"`           // 20-byte consensus address, not the key
	ConsKey          []byte    `json:"conskey,omitempty"` // raw ed25519 public key, nil for other key types
	Valcons          string    `json:"valcons"`
	SigningUpdatedAt time.Time `json:"signing_updated_at"`
	SigningStale     bool      `json:"signing_stale"`
	ValidatorStale   bool      `json:"validator_stale"`
	VotingPower      int64     `json:"voting_power"`
	TotalPower       int64     `json:"total_power"`
	Rank             int       `json:"rank"` // 0 when not in the active set
	ActiveSet        int       `json:"active_set"`
	MaxValidators    int       `json:"max_validators"` // 0 when unknown
	MaxMissed        int64     `json:"max_missed"`     // 0 when unknown
}

// GetValInfo the first bool is used to determine if extra information about the validator should be printed.
func (cc *ChainConfig) GetValInfo(first bool) (err error) {
	cc.refreshMux.Lock()
	defer cc.refreshMux.Unlock()
	client := cc.clientSnapshot()
	info := &ValInfo{}
	signingRead := false
	previous, _ := cc.validatorState()
	defer func() {
		// Some consumer chains cannot answer slashing queries, but blocks can still be monitored.
		if err != nil {
			if info.Valcons == "" || len(info.Conspub) < 20 {
				info = previous
				info.ValidatorStale = true
			} else if info.Valcons == previous.Valcons {
				if !signingRead {
					info.Tombstoned, info.Missed, info.JailedUntil = previous.Tombstoned, previous.Missed, previous.JailedUntil
				}
				info.Window, info.MaxMissed, info.MaxValidators = previous.Window, previous.MaxMissed, previous.MaxValidators
				info.SigningUpdatedAt = previous.SigningUpdatedAt
			}
			info.SigningStale = true
			cc.validatorMux.Lock()
			cc.lastValInfo = cc.valInfo
			cc.valInfo = info
			cc.validatorMux.Unlock()
		}
	}()
	if client == nil {
		return errors.New("nil rpc client")
	}
	ctx, cancel := context.WithTimeout(cc.context(), 10*time.Second)
	defer cancel()

	// Fetch info from /cosmos.staking.v1beta1.Query/Validator
	// it's easier to ask people to provide valoper since it's readily available on
	// explorers, so make it easy and lookup the consensus key for them.
	info.Conspub, info.ConsKey, info.Moniker, info.Jailed, info.Bonded, err = getVal(ctx, client, cc.ValAddress)
	if err != nil {
		return
	}
	if len(info.Conspub) < 20 {
		return errors.New("invalid validator consensus key")
	}
	if first && info.Bonded {
		l(fmt.Sprintf("⚙️ found %s (%s) in validator set", cc.ValAddress, info.Moniker))
	} else if first && !info.Bonded {
		l(fmt.Sprintf("❌ %s (%s) is INACTIVE", cc.ValAddress, info.Moniker))
	}

	// voting power is display only; keep the last reading if the node can't list the set
	power, powerErr := getVotingPower(ctx, client.Remote(), info.Conspub)
	if powerErr != nil {
		power = votingPower{power: previous.VotingPower, total: previous.TotalPower, rank: previous.Rank, size: previous.ActiveSet}
	}
	info.VotingPower, info.TotalPower, info.Rank, info.ActiveSet = power.power, power.total, power.rank, power.size

	if strings.Contains(cc.ValAddress, "valcons") {
		// no need to change prefix for signing info query
		info.Valcons = cc.ValAddress
	} else {
		// need to know the prefix for when we serialize the slashing info query, this is too fragile.
		// for now, we perform specific chain overrides based on known values because the valoper is used
		// in so many places.
		var prefix string
		split := strings.Split(cc.ValAddress, "valoper")
		if len(split) != 2 {
			if pre, ok := altValopers.getAltPrefix(cc.ValAddress); ok {
				info.Valcons, err = bech32.ConvertAndEncode(pre, info.Conspub[:20])
				if err != nil {
					return
				}
			} else {
				err = errors.New("❓ could not determine bech32 prefix from valoper address: " + cc.ValAddress)
				return
			}
		} else {
			prefix = split[0] + "valcons"
			info.Valcons, err = bech32.ConvertAndEncode(prefix, info.Conspub[:20])
			if err != nil {
				return
			}
		}
		if first {
			l("⚙️", cc.ValAddress, "is using consensus key:", info.Valcons)
		}

	}

	// get current signing information (tombstoned, missed block count)
	qSigning := slashing.QuerySigningInfoRequest{ConsAddress: info.Valcons}
	b, err := qSigning.Marshal()
	if err != nil {
		return
	}
	resp, err := client.ABCIQuery(ctx, "/cosmos.slashing.v1beta1.Query/SigningInfo", b)
	if err != nil {
		return
	}
	if resp == nil || resp.Response.Code != 0 || len(resp.Response.Value) == 0 {
		err = errors.New("could not query validator slashing status, got empty response")
		return
	}
	slash := &slashing.QuerySigningInfoResponse{}
	err = slash.Unmarshal(resp.Response.Value)
	if err != nil {
		return
	}
	info.Tombstoned = slash.ValSigningInfo.Tombstoned
	if info.Tombstoned {
		l(fmt.Sprintf("❗️☠️ %s (%s) is tombstoned 🪦❗️", cc.ValAddress, info.Moniker))
	}
	info.Missed = slash.ValSigningInfo.MissedBlocksCounter
	info.JailedUntil = slash.ValSigningInfo.JailedUntil.UTC()
	signingRead = true

	// finally get the signed blocks window, it rarely changes so only on (re)connect
	if !first && previous.Valcons == info.Valcons {
		info.Window, info.MaxMissed, info.MaxValidators = previous.Window, previous.MaxMissed, previous.MaxValidators
	}
	if info.Window == 0 {
		qParams := &slashing.QueryParamsRequest{}
		b, err = qParams.Marshal()
		if err != nil {
			return
		}
		resp, err = client.ABCIQuery(ctx, "/cosmos.slashing.v1beta1.Query/Params", b)
		if err != nil {
			return
		}
		if resp == nil || resp.Response.Code != 0 || len(resp.Response.Value) == 0 {
			err = errors.New("🛑 could not query slashing params, got empty response")
			return
		}
		params := &slashing.QueryParamsResponse{}
		err = params.Unmarshal(resp.Response.Value)
		if err != nil {
			return
		}
		info.Window = params.Params.SignedBlocksWindow
		info.MaxMissed = maxMissed(info.Window, params.Params.MinSignedPerWindow)
		info.MaxValidators = getMaxValidators(ctx, client)
	}
	if info.Window <= 0 {
		return errors.New("invalid signed blocks window")
	}
	info.SigningUpdatedAt = time.Now()
	cc.validatorMux.Lock()
	cc.lastValInfo = cc.valInfo
	cc.valInfo = info
	cc.validatorMux.Unlock()
	if td.Prom {
		td.sendStat(cc.mkUpdate(metricWindowMissed, float64(info.Missed), ""))
		td.sendStat(cc.mkUpdate(metricWindowSize, float64(info.Window), ""))
		td.sendStat(cc.mkUpdate(metricVotingPower, float64(info.VotingPower), ""))
		td.sendStat(cc.mkUpdate(metricVotingPowerRank, float64(info.Rank), ""))
		if info.MaxMissed > 0 {
			td.sendStat(cc.mkUpdate(metricMissesBeforeJail, float64(info.missesBeforeJail()), ""))
		}
		if first {
			td.sendStat(cc.mkUpdate(metricTotalNodes, float64(len(cc.Nodes)), ""))
		}
	}
	return
}

// validatorNotFoundError: the chain has no such validator (or, with activeOnly, none in the active set).
type validatorNotFoundError struct {
	address    string
	activeOnly bool
}

func (e *validatorNotFoundError) Error() string {
	if e.activeOnly {
		return "consensus validator " + e.address + " was not found in the active set"
	}
	return "could not find validator " + e.address
}

func getVal(ctx context.Context, client *rpchttp.HTTP, valoper string) (pub, consKey []byte, moniker string, jailed, bonded bool, err error) {
	if strings.Contains(valoper, "valcons") {
		_, bz, err := bech32.DecodeAndConvert(valoper)
		if err != nil {
			return nil, nil, "", false, false, errors.New("could not decode and convert your address" + valoper)
		}

		hexAddress := fmt.Sprintf("%X", bz)
		return ToBytes(hexAddress), nil, valoper, false, true, nil
	}

	q := staking.QueryValidatorRequest{
		ValidatorAddr: valoper,
	}
	b, err := q.Marshal()
	if err != nil {
		return
	}
	resp, err := client.ABCIQuery(ctx, "/cosmos.staking.v1beta1.Query/Validator", b)
	if err != nil {
		return
	}
	if resp == nil || resp.Response.Code != 0 || len(resp.Response.Value) == 0 {
		return nil, nil, "", false, false, &validatorNotFoundError{address: valoper}
	}
	val := &staking.QueryValidatorResponse{}
	err = val.Unmarshal(resp.Response.Value)
	if err != nil {
		return
	}
	if val.Validator.ConsensusPubkey == nil {
		return nil, nil, "", false, false, errors.New("got invalid consensus pubkey for " + valoper)
	}

	pubBytes := make([]byte, 0)
	switch val.Validator.ConsensusPubkey.TypeUrl {
	case "/cosmos.crypto.ed25519.PubKey":
		pk := ed25519.PubKey{}
		err = pk.Unmarshal(val.Validator.ConsensusPubkey.Value)
		if err != nil {
			return
		}
		pubBytes, consKey = pk.Address().Bytes(), pk.Bytes()
	case "/cosmos.crypto.secp256k1.PubKey":
		pk := secp256k1.PubKey{}
		err = pk.Unmarshal(val.Validator.ConsensusPubkey.Value)
		if err != nil {
			return
		}
		pubBytes = pk.Address().Bytes()
	}
	if len(pubBytes) == 0 {
		return nil, nil, "", false, false, errors.New("could not get pubkey for" + valoper)
	}

	return pubBytes, consKey, val.Validator.GetMoniker(), val.Validator.Jailed, val.Validator.Status == 3, nil
}

// Same rounding as the slashing module.
func maxMissed(window int64, minSigned sdk.Dec) int64 {
	return window - minSigned.MulInt64(window).RoundInt64()
}

func (info *ValInfo) missesBeforeJail() int64 {
	if left := info.MaxMissed - info.Missed; left > 0 {
		return left
	}
	return 0
}

// Returns 0 if the chain doesn't answer.
func getMaxValidators(ctx context.Context, client *rpchttp.HTTP) int {
	b, err := (&staking.QueryParamsRequest{}).Marshal()
	if err != nil {
		return 0
	}
	resp, err := client.ABCIQuery(ctx, "/cosmos.staking.v1beta1.Query/Params", b)
	if err != nil || resp == nil || resp.Response.Code != 0 {
		return 0
	}
	params := &staking.QueryParamsResponse{}
	if params.Unmarshal(resp.Response.Value) != nil {
		return 0
	}
	return int(params.Params.MaxValidators)
}

type votingPower struct {
	power, total int64
	rank, size   int
}

// jsonInt accepts a number or a string.
type jsonInt int64

func (i *jsonInt) UnmarshalJSON(b []byte) error {
	v, err := strconv.ParseInt(strings.Trim(string(b), `"`), 10, 64)
	*i = jsonInt(v)
	return err
}

// Only address and power are decoded, so unknown key types are fine; the set is sorted by power for the rank.
func getVotingPower(ctx context.Context, remote string, conspub []byte) (votingPower, error) {
	result := votingPower{}
	address := strings.ToUpper(hex.EncodeToString(conspub))
	client := &http.Client{Timeout: 10 * time.Second}
	for page := 1; page <= 20; page++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/validators?page=%d&per_page=100", strings.TrimRight(remote, "/"), page), nil)
		if err != nil {
			return result, err
		}
		resp, err := client.Do(req)
		if err != nil {
			return result, err
		}
		set := struct {
			Result struct {
				Validators []struct {
					Address     string  `json:"address"`
					VotingPower jsonInt `json:"voting_power"`
				} `json:"validators"`
				Total jsonInt `json:"total"`
			} `json:"result"`
		}{}
		err = json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&set)
		_ = resp.Body.Close()
		if err != nil {
			return result, err
		}
		for _, v := range set.Result.Validators {
			result.size++
			result.total += int64(v.VotingPower)
			if v.Address == address {
				result.power, result.rank = int64(v.VotingPower), result.size
			}
		}
		if len(set.Result.Validators) == 0 || result.size >= int(set.Result.Total) {
			return result, nil
		}
	}
	return result, nil
}

func ToBytes(address string) []byte {
	bz, _ := hex.DecodeString(strings.ToLower(address))
	return bz
}
