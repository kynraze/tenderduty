package tenderduty

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cosmos/cosmos-sdk/crypto/keys/ed25519"
	"github.com/cosmos/cosmos-sdk/crypto/keys/secp256k1"
	"github.com/cosmos/cosmos-sdk/types/bech32"
	slashing "github.com/cosmos/cosmos-sdk/x/slashing/types"
	staking "github.com/cosmos/cosmos-sdk/x/staking/types"
	rpchttp "github.com/tendermint/tendermint/rpc/client/http"
)

// ValInfo holds most of the stats/info used for secondary alarms. It is refreshed roughly every minute.
type ValInfo struct {
	Moniker    string `json:"moniker"`
	Bonded     bool   `json:"bonded"`
	Jailed     bool   `json:"jailed"`
	Tombstoned bool   `json:"tombstoned"`
	Missed     int64  `json:"missed"`
	Window     int64  `json:"window"`
	Conspub    []byte `json:"conspub"`
	Valcons    string `json:"valcons"`
}

// GetValInfo the first bool is used to determine if extra information about the validator should be printed.
func (cc *ChainConfig) GetValInfo(first bool) (err error) {
	cc.refreshMux.Lock()
	defer cc.refreshMux.Unlock()
	client := cc.clientSnapshot()
	info := &ValInfo{}
	defer func() {
		// Some consumer chains cannot answer slashing queries, but blocks can still be monitored.
		if err != nil && info.Valcons != "" && len(info.Conspub) >= 20 {
			cc.validatorMux.Lock()
			cc.lastValInfo = cc.valInfo
			cc.valInfo = info
			cc.validatorMux.Unlock()
		}
	}()
	if client == nil {
		return errors.New("nil rpc client")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Fetch info from /cosmos.staking.v1beta1.Query/Validator
	// it's easier to ask people to provide valoper since it's readily available on
	// explorers, so make it easy and lookup the consensus key for them.
	info.Conspub, info.Moniker, info.Jailed, info.Bonded, err = getVal(ctx, client, cc.ValAddress)
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

	// finally get the signed blocks window
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
	}
	if info.Window <= 0 {
		return errors.New("invalid signed blocks window")
	}
	cc.validatorMux.Lock()
	cc.lastValInfo = cc.valInfo
	cc.valInfo = info
	cc.validatorMux.Unlock()
	if td.Prom {
		td.statsChan <- cc.mkUpdate(metricWindowMissed, float64(info.Missed), "")
		td.statsChan <- cc.mkUpdate(metricWindowSize, float64(info.Window), "")
		if first {
			td.statsChan <- cc.mkUpdate(metricTotalNodes, float64(len(cc.Nodes)), "")
		}
	}
	return
}

// getVal returns the public key, moniker, and if the validator is jailed.
func getVal(ctx context.Context, client *rpchttp.HTTP, valoper string) (pub []byte, moniker string, jailed, bonded bool, err error) {
	if strings.Contains(valoper, "valcons") {
		_, bz, err := bech32.DecodeAndConvert(valoper)
		if err != nil {
			return nil, "", false, false, errors.New("could not decode and convert your address" + valoper)
		}

		hexAddress := fmt.Sprintf("%X", bz)
		return ToBytes(hexAddress), valoper, false, true, nil
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
	if resp.Response.Value == nil {
		return nil, "", false, false, errors.New("could not find validator " + valoper)
	}
	val := &staking.QueryValidatorResponse{}
	err = val.Unmarshal(resp.Response.Value)
	if err != nil {
		return
	}
	if val.Validator.ConsensusPubkey == nil {
		return nil, "", false, false, errors.New("got invalid consensus pubkey for " + valoper)
	}

	pubBytes := make([]byte, 0)
	switch val.Validator.ConsensusPubkey.TypeUrl {
	case "/cosmos.crypto.ed25519.PubKey":
		pk := ed25519.PubKey{}
		err = pk.Unmarshal(val.Validator.ConsensusPubkey.Value)
		if err != nil {
			return
		}
		pubBytes = pk.Address().Bytes()
	case "/cosmos.crypto.secp256k1.PubKey":
		pk := secp256k1.PubKey{}
		err = pk.Unmarshal(val.Validator.ConsensusPubkey.Value)
		if err != nil {
			return
		}
		pubBytes = pk.Address().Bytes()
	}
	if len(pubBytes) == 0 {
		return nil, "", false, false, errors.New("could not get pubkey for" + valoper)
	}

	return pubBytes, val.Validator.GetMoniker(), val.Validator.Jailed, val.Validator.Status == 3, nil
}

func ToBytes(address string) []byte {
	bz, _ := hex.DecodeString(strings.ToLower(address))
	return bz
}
