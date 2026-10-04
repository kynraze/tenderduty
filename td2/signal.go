package tenderduty

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/cosmos/cosmos-sdk/types/bech32"
	rpchttp "github.com/tendermint/tendermint/rpc/client/http"
	"google.golang.org/protobuf/encoding/protowire"
)

// signalUpgrade is the upgrade x/signal scheduled once enough power signalled.
type signalUpgrade struct {
	Version uint64
	Height  int64
}

// signalState is the tally for the version after the chain's, and whether our validator signalled it.
type signalState struct {
	Version        uint64
	Power          uint64
	Threshold      uint64
	Total          uint64
	Signalled      bool
	UpgradeVersion uint64
	UpgradeHeight  int64 // 0 when no upgrade is scheduled
}

// parseTally reads QueryVersionTallyResponse{voting_power: 1, threshold_power: 2, total_voting_power: 3}.
// Protobuf omits zero values, so voting_power is absent when nobody signalled.
func parseTally(b []byte) (power, threshold, total uint64, err error) {
	err = eachField(b, func(num protowire.Number, typ protowire.Type, _ []byte, varint uint64) error {
		if typ != protowire.VarintType {
			return nil
		}
		switch num {
		case 1:
			power = varint
		case 2:
			threshold = varint
		case 3:
			total = varint
		}
		return nil
	})
	return
}

// parseSignalUpgrade reads QueryGetUpgradeResponse{upgrade: {app_version: 1, upgrade_height: 2}}, nil when none is scheduled.
func parseSignalUpgrade(b []byte) (*signalUpgrade, error) {
	var upgrade *signalUpgrade
	err := eachField(b, func(num protowire.Number, typ protowire.Type, value []byte, _ uint64) error {
		if num != 1 || typ != protowire.BytesType {
			return nil
		}
		upgrade = &signalUpgrade{}
		return eachField(value, func(num protowire.Number, typ protowire.Type, _ []byte, varint uint64) error {
			switch {
			case num == 1 && typ == protowire.VarintType:
				upgrade.Version = varint
			case num == 2 && typ == protowire.VarintType:
				// a bogus height would wrap negative
				if varint > math.MaxInt64 {
					return fmt.Errorf("upgrade height %d is out of range", varint)
				}
				upgrade.Height = int64(varint)
			}
			return nil
		})
	})
	if upgrade != nil && upgrade.Height <= 0 {
		return nil, err
	}
	return upgrade, err
}

// parseSignalledVersion reads a validator's signalled version from the store: 8 bytes big-endian, none when empty.
func parseSignalledVersion(b []byte) (version uint64, signalled bool, err error) {
	if len(b) == 0 {
		return 0, false, nil
	}
	if len(b) != 8 {
		return 0, false, fmt.Errorf("signalled version is %d bytes, expected 8", len(b))
	}
	return binary.BigEndian.Uint64(b), true, nil
}

// abciError is a query answered with an error code.
type abciError struct {
	path, codespace, log string
	code                 uint32
}

func (e *abciError) Error() string {
	return e.path + ": " + e.log
}

// errNoSignal means the chain has no x/signal, which never changes.
var errNoSignal = errors.New("no x/signal on this chain")

func abciQuery(ctx context.Context, client *rpchttp.HTTP, path string, data []byte) ([]byte, error) {
	resp, err := client.ABCIQuery(ctx, path, data)
	if err != nil {
		return nil, err
	}
	if resp.Response.Code != 0 {
		return nil, &abciError{path: path, codespace: resp.Response.Codespace, log: resp.Response.Log, code: resp.Response.Code}
	}
	return resp.Response.Value, nil
}

// moduleMissing: the sdk answers code 6, unknown request, when the chain lacks the module.
func moduleMissing(err error) bool {
	var answer *abciError
	return errors.As(err, &answer) && answer.codespace == "sdk" && answer.code == 6
}

func getSignalState(ctx context.Context, client *rpchttp.HTTP, valoper string) (*signalState, error) {
	info, err := client.ABCIInfo(ctx)
	if err != nil {
		return nil, err
	}
	state := &signalState{Version: info.Response.AppVersion + 1}

	value, err := abciQuery(ctx, client, "/celestia.signal.v1.Query/VersionTally", protowire.AppendVarint(protowire.AppendTag(nil, 1, protowire.VarintType), state.Version))
	if moduleMissing(err) {
		return nil, errNoSignal
	}
	if err != nil {
		return nil, err
	}
	if state.Power, state.Threshold, state.Total, err = parseTally(value); err != nil {
		return nil, err
	}

	if value, err = abciQuery(ctx, client, "/celestia.signal.v1.Query/GetUpgrade", nil); err != nil {
		return nil, err
	}
	upgrade, err := parseSignalUpgrade(value)
	if err != nil {
		return nil, err
	}
	if upgrade != nil {
		state.UpgradeVersion, state.UpgradeHeight = upgrade.Version, upgrade.Height
	}

	_, address, err := bech32.DecodeAndConvert(valoper)
	if err != nil {
		return nil, err
	}
	if value, err = abciQuery(ctx, client, "/store/signal/key", address); err != nil {
		return nil, err
	}
	signalled, ok, err := parseSignalledVersion(value)
	if err != nil {
		return nil, err
	}
	state.Signalled = ok && signalled == state.Version
	return state, nil
}

// refreshSignal skips chains without x/signal after logging once, so signal_enabled can sit in alert_defaults.
func (cc *ChainConfig) refreshSignal(ctx context.Context, client *rpchttp.HTTP) {
	cc.stateMux.RLock()
	unsupported := cc.signalUnsupported
	cc.stateMux.RUnlock()
	if unsupported {
		return
	}
	state, err := getSignalState(ctx, client, cc.ValAddress)
	if errors.Is(err, errNoSignal) {
		cc.stateMux.Lock()
		cc.signalUnsupported = true
		cc.stateMux.Unlock()
		l(cc.ChainId, "has no x/signal, not watching version signalling")
		return
	}
	if err != nil {
		l(cc.ChainId, "could not read the version signalling:", err)
		return
	}
	cc.stateMux.Lock()
	previous, height, blockTime := cc.signal, cc.lastBlockNum, cc.blockTime
	cc.signal, cc.signalLoaded = *state, true
	cc.stateMux.Unlock()
	if previous.UpgradeHeight > 0 && state.UpgradeHeight != previous.UpgradeHeight {
		outcome := "Cancelled"
		if height >= previous.UpgradeHeight {
			outcome = "Upgraded"
		}
		upgradeOutcomes.Store(fmt.Sprintf("%s@%d", cc.name, previous.UpgradeHeight), outcome)
	}
	if state.UpgradeHeight > height && blockTime > 0 && height > 0 {
		upgradeETAs.Store(fmt.Sprintf("%s@%d", cc.name, state.UpgradeHeight), time.Now().Add(time.Duration(state.UpgradeHeight-height)*blockTime).UTC())
	}
}

// the messages are the alarms' keys, so no percentages or times in them
func signalStartMessage(version uint64) string {
	return fmt.Sprintf("Version %d signalling has started", version)
}

func signalMissingMessage(version uint64) string {
	return fmt.Sprintf("Validator has not signalled version %d", version)
}

func signalThresholdMessage(version uint64) string {
	return fmt.Sprintf("Version %d reached the upgrade threshold", version)
}

func signalUpgradeMessage(version uint64, height int64) string {
	return fmt.Sprintf("Version %d upgrade at height %s", version, thousands(height))
}

func signalUpgradeSoonMessage(version uint64, height int64) string {
	return signalUpgradeMessage(version, height) + " is close"
}

const signalMissingPrefix = "Validator has not signalled version "

func isSignalUpgrade(message string) bool {
	return strings.HasPrefix(message, "Version ") && strings.Contains(message, " upgrade at height ")
}

func isSignalMessage(message string) bool {
	return isSignalUpgrade(message) || strings.HasPrefix(message, signalMissingPrefix) ||
		strings.HasPrefix(message, "Version ") && (strings.HasSuffix(message, " signalling has started") || strings.HasSuffix(message, " reached the upgrade threshold"))
}

var signalVersion = regexp.MustCompile(`ersion (\d+)`)

// signalAlarmId derives the id from the message, so alarms restored from state resolve with the same one.
func signalAlarmId(valInfo *ValInfo, message string) string {
	version := ""
	if match := signalVersion.FindStringSubmatch(message); match != nil {
		version = match[1]
	}
	switch {
	case strings.HasPrefix(message, signalMissingPrefix):
		return valInfo.Valcons + "signalmissing" + version
	case strings.HasSuffix(message, " reached the upgrade threshold"):
		return valInfo.Valcons + "signalthreshold" + version
	// by height like upgradeAlarmId: a rescheduled upgrade must not share the pagerduty dedup key of the one it replaces
	case strings.HasSuffix(message, " is close"):
		return valInfo.Valcons + "signalupgradesoon" + messageHeight(message)
	case isSignalUpgrade(message):
		return valInfo.Valcons + "signalupgrade" + messageHeight(message)
	}
	return valInfo.Valcons + "signalstart" + version
}

func signalLabel(msg *alertMsg) (icon, state string) {
	switch {
	case isSignalUpgrade(msg.message):
		if msg.resolved {
			return upgradeResolvedLabel(msg.chain, msg.message)
		}
		if strings.HasSuffix(msg.message, " is close") {
			return "⏰", "Upgrade soon"
		}
		return "⬆️", "Upgrade scheduled"
	case msg.resolved && strings.HasPrefix(msg.message, signalMissingPrefix):
		return "✅", "Signalled"
	case msg.resolved:
		return "✅", "Resolved"
	case strings.HasPrefix(msg.message, signalMissingPrefix):
		return "⚠️", "Not signalled"
	case strings.HasSuffix(msg.message, " reached the upgrade threshold"):
		return "🚦", "Threshold reached"
	}
	return "📣", "Signalling"
}

// signalAlarms works like upgradeAlarms.
func (cc *ChainConfig) signalAlarms(valInfo *ValInfo, reconcile bool) {
	cc.stateMux.RLock()
	state, loaded, height := cc.signal, cc.signalLoaded, cc.lastBlockNum
	cc.stateMux.RUnlock()
	if !loaded {
		return
	}
	wanted := make(map[string]string)
	priority := make(map[string]string)
	want := func(message, priorityName string) {
		wanted[message], priority[message] = signalAlarmId(valInfo, message), priorityName
	}
	scheduled := state.UpgradeHeight > 0 && height < state.UpgradeHeight
	started := state.Total > 0 && float64(state.Power)/float64(state.Total)*100 >= float64(cc.alertSettings().SignalPercent)
	if started {
		want(signalStartMessage(state.Version), cc.alertSettings().SignalPriority)
		if !state.Signalled {
			want(signalMissingMessage(state.Version), cc.alertSettings().SignalPriority)
		}
	}
	if state.Threshold > 0 && state.Power >= state.Threshold && state.UpgradeHeight == 0 {
		want(signalThresholdMessage(state.Version), "critical")
	}
	if scheduled {
		version := state.UpgradeVersion
		if version == 0 {
			version = state.Version
		}
		want(signalUpgradeMessage(version, state.UpgradeHeight), "critical")
		if eta, ok := upgradeETAs.Load(fmt.Sprintf("%s@%d", cc.name, state.UpgradeHeight)); ok && time.Until(eta.(time.Time)) <= time.Duration(cc.alertSettings().SignalRemindMinutes)*time.Minute {
			want(signalUpgradeSoonMessage(version, state.UpgradeHeight), "critical")
		}
	}
	active := activeMessages(cc.name, "Version ")
	for message := range activeMessages(cc.name, signalMissingPrefix) {
		active[message] = true
	}
	for message, id := range wanted {
		if !active[message] || reconcile {
			id := id
			td.alert(cc.name, message, priority[message], false, &id)
		}
	}
	// alarms of one version clear together, only the one that says the most is announced
	// a threshold cleared by a scheduled upgrade is announced by the upgrade alarm
	announce := make(map[string]string) // version to the message that announces it
	for message := range active {
		if _, ok := wanted[message]; ok || !isSignalMessage(message) {
			continue
		}
		version := signalVersion.FindStringSubmatch(message)
		if version == nil {
			continue
		}
		if best, ok := announce[version[1]]; !ok || signalRank(message) < signalRank(best) {
			announce[version[1]] = message
		}
	}
	for message := range active {
		if _, ok := wanted[message]; !ok && isSignalMessage(message) {
			if r, ok := cc.signalResolution(message, state, valInfo.Moniker); ok {
				resolve(cc.name, message, r)
			}
			id := signalAlarmId(valInfo, message)
			version := signalVersion.FindStringSubmatch(message)
			scheduledNow := false
			for raised := range wanted {
				scheduledNow = scheduledNow || isSignalUpgrade(raised) && !active[raised] && version != nil && signalVersion.FindStringSubmatch(raised)[1] == version[1]
			}
			if version != nil && announce[version[1]] != message || strings.HasSuffix(message, " reached the upgrade threshold") && scheduledNow {
				td.resolveQuietly(cc.name, message, &id)
				continue
			}
			td.alert(cc.name, message, "info", true, &id)
		}
	}
}

// signalRank orders a version's alarms by what their recovery says, reminder first.
func signalRank(message string) int {
	switch {
	case isSignalUpgrade(message) && strings.HasSuffix(message, " is close"):
		return 0
	case isSignalUpgrade(message):
		return 1
	case strings.HasSuffix(message, " signalling has started"):
		return 2
	case strings.HasPrefix(message, signalMissingPrefix):
		return 3
	}
	return 4
}

// signalResolution says why a signalling alarm cleared, a scheduled upgrade's own message needs none.
func (cc *ChainConfig) signalResolution(message string, state signalState, moniker string) (resolution, bool) {
	if isSignalUpgrade(message) {
		return resolution{}, false
	}
	match := signalVersion.FindStringSubmatch(message)
	if match == nil {
		return resolution{}, false
	}
	version, err := strconv.ParseUint(match[1], 10, 64)
	if err != nil {
		return resolution{}, false
	}
	// state.Version is the one after the version the chain runs
	if state.Version > version {
		return resolution{"✅", "Upgraded", fmt.Sprintf("%s runs version %d now", cc.ChainId, state.Version-1)}, true
	}
	switch {
	case strings.HasPrefix(message, signalMissingPrefix) && state.Signalled:
		return resolution{"✅", "Signalled", fmt.Sprintf("%s signalled version %d on %s", moniker, version, cc.ChainId)}, true
	case strings.HasSuffix(message, " reached the upgrade threshold") && state.UpgradeHeight > 0:
		return resolution{"⬆️", "Upgrade scheduled", fmt.Sprintf("Version %d upgrade is scheduled at height %s", version, thousands(state.UpgradeHeight))}, true
	case strings.HasSuffix(message, " reached the upgrade threshold"):
		return resolution{"✅", "Resolved", fmt.Sprintf("Version %d fell back under the upgrade threshold", version)}, true
	}
	return resolution{"✅", "Resolved", fmt.Sprintf("Version %d signalling dropped below %d%%", version, cc.alertSettings().SignalPercent)}, true
}
