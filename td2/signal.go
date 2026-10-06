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
	BinaryVersion  string
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

// errNoSignal means the query found no x/signal module.
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
	state := &signalState{Version: info.Response.AppVersion + 1, BinaryVersion: info.Response.Version}

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

// refreshSignal logs missing x/signal once and skips later queries.
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
		cc.stateMux.Lock()
		failed := cc.signalReadFailed
		cc.signalReadFailed = true
		cc.stateMux.Unlock()
		if !failed {
			l(cc.ChainId, "could not read the version signalling:", err)
		}
		// Query failures can be unrelated to the node stopping.
		info, rpcErr := client.ABCIInfo(ctx)
		cc.stateMux.Lock()
		cc.signalRPCFailed = rpcErr != nil
		if rpcErr == nil && cc.signal.UpgradeHeight > 0 {
			cc.signal.BinaryVersion = info.Response.Version
		}
		cc.stateMux.Unlock()
		return
	}
	cc.stateMux.Lock()
	previous, height, blockTime := cc.signal, cc.lastBlockNum, cc.blockTime
	failed := cc.signalReadFailed
	cc.signalReadFailed, cc.signalRPCFailed = false, false
	cc.signal, cc.signalLoaded = *state, true
	cc.stateMux.Unlock()
	if failed {
		l(cc.ChainId, "version signalling readable again")
	}
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

func signalRemindMessage(version uint64) string {
	return signalMissingMessage(version) + ", upgrade soon"
}

func isSignalReminder(message string) bool {
	return strings.HasPrefix(message, signalMissingPrefix) && strings.HasSuffix(message, ", upgrade soon")
}

func signalDetail(chain, message string) string {
	if !strings.HasPrefix(message, signalMissingPrefix) {
		return ""
	}
	const action = "upgrade celestia-appd, then send the signal tx"
	td.chainsMux.RLock()
	cc := td.Chains[chain]
	td.chainsMux.RUnlock()
	if cc == nil {
		return action
	}
	cc.stateMux.RLock()
	state := cc.signal
	cc.stateMux.RUnlock()
	if state.Total == 0 {
		return action
	}
	percent := func(power uint64) string {
		return strconv.FormatFloat(math.Round(float64(power)/float64(state.Total)*10000)/100, 'f', -1, 64)
	}
	return fmt.Sprintf("%s%% signalled, the upgrade unlocks at %s%%: %s", percent(state.Power), percent(state.Threshold), action)
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
	return strings.HasPrefix(message, "Node stopped at the version ") || isSignalUpgrade(message) || strings.HasPrefix(message, signalMissingPrefix) ||
		strings.HasPrefix(message, "Version ") && (strings.HasSuffix(message, " signalling has started") || strings.HasSuffix(message, " reached the upgrade threshold"))
}

var signalVersion = regexp.MustCompile(`ersion (\d+)`)

// signalAlarmId rebuilds IDs for restored alarms.
func signalAlarmId(valInfo *ValInfo, message string) string {
	version := ""
	if match := signalVersion.FindStringSubmatch(message); match != nil {
		version = match[1]
	}
	switch {
	case strings.HasPrefix(message, "Node stopped at the version "):
		return valInfo.Valcons + "signalstopped" + version
	case isSignalReminder(message):
		return valInfo.Valcons + "signalremind" + version
	case strings.HasPrefix(message, signalMissingPrefix):
		return valInfo.Valcons + "signalmissing" + version
	case strings.HasSuffix(message, " reached the upgrade threshold"):
		return valInfo.Valcons + "signalthreshold" + version
	// include height so rescheduled upgrades have separate PagerDuty incidents
	case strings.HasSuffix(message, " is close"):
		return valInfo.Valcons + "signalupgradesoon" + messageHeight(message)
	case isSignalUpgrade(message):
		return valInfo.Valcons + "signalupgrade" + messageHeight(message)
	}
	return valInfo.Valcons + "signalstart" + version
}

func signalLabel(msg *alertMsg) (icon, state string) {
	switch {
	case strings.HasPrefix(msg.message, "Node stopped at the version "):
		if msg.resolved {
			return "✅", "Blocks advancing"
		}
		return "🚨", "Node stopped"
	case isSignalUpgrade(msg.message):
		if msg.resolved {
			if r, ok := storedResolution(msg.chain, msg.message); ok {
				return r.icon, r.label
			}
			if outcome, ok := upgradeOutcomes.Load(msg.chain + "@" + messageHeight(msg.message)); ok && outcome == "Cancelled" {
				return "🚫", "Cancelled"
			}
			return "✅", "Height reached"
		}
		if strings.HasSuffix(msg.message, " is close") {
			return "⏰", "Upgrade soon"
		}
		return "⬆️", "Upgrade scheduled"
	case msg.resolved && strings.HasPrefix(msg.message, signalMissingPrefix):
		return "✅", "Signalled"
	case msg.resolved:
		return "✅", "Resolved"
	case isSignalReminder(msg.message):
		return "🔔", "Signal reminder"
	case strings.HasPrefix(msg.message, signalMissingPrefix):
		return "⚠️", "Not signalled"
	case strings.HasSuffix(msg.message, " reached the upgrade threshold"):
		return "🚦", "Threshold reached"
	}
	return "📣", "Signalling"
}

func (cc *ChainConfig) signalAlarms(valInfo *ValInfo, reconcile bool) {
	cc.stateMux.RLock()
	state, loaded, height := cc.signal, cc.signalLoaded, cc.lastBlockNum
	cc.stateMux.RUnlock()
	if !loaded || !cc.alertSettings().UpgradeAlerts {
		return
	}
	wanted, priority := make(map[string]string), make(map[string]string)
	want := func(message, severity string) {
		wanted[message], priority[message] = signalAlarmId(valInfo, message), severity
	}
	settings := cc.alertSettings()
	scheduled := state.UpgradeHeight > height && state.UpgradeHeight > 0
	thresholdReached := state.Threshold > 0 && state.Power >= state.Threshold
	if state.UpgradeHeight == 0 && !state.Signalled && state.Total > 0 {
		percent := float64(state.Power) / float64(state.Total) * 100
		if percent >= float64(settings.SignalPercent) {
			want(signalMissingMessage(state.Version), "warning")
		}
		if settings.SignalRemindPercent > settings.SignalPercent && percent >= float64(settings.SignalRemindPercent) && !thresholdReached {
			want(signalRemindMessage(state.Version), "critical")
		}
	}
	version := state.UpgradeVersion
	if version == 0 {
		version = state.Version
	}
	if scheduled {
		want(signalUpgradeMessage(version, state.UpgradeHeight), "critical")
		soon := signalUpgradeSoonMessage(version, state.UpgradeHeight)
		if !cc.signalNodesReady(state.BinaryVersion, version) {
			// retain an active reminder despite ETA changes until nodes are ready
			eta, ok := upgradeETAs.Load(fmt.Sprintf("%s@%d", cc.name, state.UpgradeHeight))
			if ok && time.Until(eta.(time.Time)) <= time.Duration(settings.SignalRemindMinutes)*time.Minute || alarmIsActive(cc.name, soon) {
				want(soon, "critical")
			}
		}
	}
	cc.signalStoppedAlarm(valInfo, state, reconcile)
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
	announce := make(map[string]string)
	quiet := func(message string) bool {
		return strings.HasSuffix(message, " signalling has started") || strings.HasSuffix(message, " reached the upgrade threshold") ||
			strings.HasPrefix(message, signalMissingPrefix) && state.UpgradeHeight > 0 ||
			isSignalReminder(message) && thresholdReached && !state.Signalled
	}
	for message := range active {
		if _, ok := wanted[message]; ok || !isSignalMessage(message) || quiet(message) {
			continue
		}
		if match := signalVersion.FindStringSubmatch(message); match != nil {
			if best, ok := announce[match[1]]; !ok || signalRank(message) < signalRank(best) {
				announce[match[1]] = message
			}
		}
	}
	for message := range active {
		if _, ok := wanted[message]; ok || !isSignalMessage(message) {
			continue
		}
		id := signalAlarmId(valInfo, message)
		match := signalVersion.FindStringSubmatch(message)
		r, ok := cc.signalResolution(message, state, valInfo.Moniker)
		if !ok && isSignalUpgrade(message) {
			continue
		}
		if ok {
			resolve(cc.name, message, r)
		}
		if quiet(message) || match != nil && announce[match[1]] != message {
			td.resolveQuietly(cc.name, message, &id)
			continue
		}
		td.alert(cc.name, message, "info", true, &id)
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
	case isSignalReminder(message):
		return 3
	case strings.HasPrefix(message, signalMissingPrefix):
		return 4
	}
	return 5
}

// signalResolution says why the alarm cleared.
func (cc *ChainConfig) signalResolution(message string, state signalState, moniker string) (resolution, bool) {
	if isSignalUpgrade(message) {
		height, _ := strconv.ParseInt(messageHeight(message), 10, 64)
		cc.stateMux.RLock()
		current := cc.lastBlockNum
		cc.stateMux.RUnlock()
		if current >= height || state.Version > signalMessageVersion(message) {
			return resolution{"✅", "Height reached", "upgrade height reached" + cc.notUpgradedNodes(signalMessageVersion(message))}, true
		}
		if strings.HasSuffix(message, " is close") && cc.signalNodesReady(state.BinaryVersion, signalMessageVersion(message)) {
			if cc.hasOwnNodes() {
				return resolution{"✅", "All nodes ready", "all nodes ready"}, true
			}
			return resolution{"✅", "Node ready", signalBinaryDetail(state.BinaryVersion, signalMessageVersion(message))}, true
		}
		// cancelled only when a successful read shows the upgrade gone or moved
		cc.stateMux.RLock()
		unconfirmed := cc.signalReadFailed
		cc.stateMux.RUnlock()
		if unconfirmed || state.UpgradeHeight == height {
			return resolution{}, false
		}
		return resolution{"🚫", "Cancelled", "upgrade cancelled or rescheduled"}, true
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
	case isSignalReminder(message) && state.UpgradeHeight > 0:
		return resolution{"⬆️", "Upgrade scheduled", fmt.Sprintf("Version %d upgrade is scheduled at height %s", version, thousands(state.UpgradeHeight))}, true
	case isSignalReminder(message) && state.Threshold > 0 && state.Power >= state.Threshold:
		return resolution{"🚦", "Threshold reached", signalThresholdMessage(version)}, true
	case isSignalReminder(message):
		return resolution{"✅", "Resolved", fmt.Sprintf("Version %d signalling dropped below %d%%", version, cc.alertSettings().SignalRemindPercent)}, true
	case strings.HasSuffix(message, " reached the upgrade threshold") && state.UpgradeHeight > 0:
		return resolution{"⬆️", "Upgrade scheduled", fmt.Sprintf("Version %d upgrade is scheduled at height %s", version, thousands(state.UpgradeHeight))}, true
	case strings.HasSuffix(message, " reached the upgrade threshold"):
		return resolution{"✅", "Resolved", fmt.Sprintf("Version %d fell back under the upgrade threshold", version)}, true
	}
	return resolution{"✅", "Resolved", fmt.Sprintf("Version %d signalling dropped below %d%%", version, cc.alertSettings().SignalPercent)}, true
}

var binaryMajor = regexp.MustCompile(`^v?(\d+)(?:\.|-|$)`)

func parseBinaryMajor(version string) (uint64, bool) {
	match := binaryMajor.FindStringSubmatch(strings.TrimSpace(version))
	if match == nil {
		return 0, false
	}
	major, err := strconv.ParseUint(match[1], 10, 64)
	return major, err == nil
}

func signalMessageVersion(message string) uint64 {
	match := signalVersion.FindStringSubmatch(message)
	if match == nil {
		return 0
	}
	version, _ := strconv.ParseUint(match[1], 10, 64)
	return version
}

func signalBinaryReady(binary string, version uint64) bool {
	major, known := parseBinaryMajor(binary)
	return known && major >= version
}

func signalBinaryDetail(binary string, version uint64) string {
	if _, known := parseBinaryMajor(binary); !known {
		return ""
	}
	binary = strings.TrimPrefix(strings.TrimSpace(binary), "v")
	if signalBinaryReady(binary, version) {
		return "node ready: celestia-app " + binary
	}
	return fmt.Sprintf("node runs celestia-app %s, version %d needs v%d or later", binary, version, version)
}

func signalUpgradeDetail(chain, message string) string {
	td.chainsMux.RLock()
	cc := td.Chains[chain]
	td.chainsMux.RUnlock()
	if cc == nil {
		return ""
	}
	cc.stateMux.RLock()
	binary := cc.signal.BinaryVersion
	cc.stateMux.RUnlock()
	if cc.hasOwnNodes() {
		return cc.ownNodeDetails(signalMessageVersion(message), strings.HasSuffix(message, " is close"))
	}
	return signalBinaryDetail(binary, signalMessageVersion(message))
}

func signalStoppedMessage(version uint64) string {
	return fmt.Sprintf("Node stopped at the version %d upgrade height", version)
}

func (cc *ChainConfig) signalStoppedAlarm(val *ValInfo, state signalState, reconcile bool) {
	own := cc.hasOwnNodes()
	now := time.Now()
	cc.stateMux.Lock()
	height := cc.lastBlockNum
	switch {
	case state.UpgradeHeight > 0:
		cc.signalLast = signalUpgrade{Version: signalScheduledVersion(state), Height: state.UpgradeHeight}
	case cc.signalLast.Height > 0 && !cc.signalReadFailed && height < cc.signalLast.Height-signalStopMargin, cc.signalLast.Height > 0 && height >= cc.signalLast.Height+signalLastKeep:
		cc.signalLast = signalUpgrade{}
	}
	if state.UpgradeHeight > 0 && height <= state.UpgradeHeight && (cc.signalWatch.Height != state.UpgradeHeight || cc.signalWatch.Version != signalScheduledVersion(state)) {
		version := state.UpgradeVersion
		if version == 0 {
			version = state.Version
		}
		cc.signalWatch = signalUpgrade{Version: version, Height: state.UpgradeHeight}
		cc.signalWatchHeight, cc.signalWatchSince = height, now
		cc.signalWatchReady = signalBinaryReady(state.BinaryVersion, version)
	} else if cc.signalWatch.Height > height && state.UpgradeHeight == 0 && !cc.signalReadFailed {
		cc.signalWatch = signalUpgrade{}
	}
	watch := cc.signalWatch
	if watch.Height > 0 && height != cc.signalWatchHeight {
		cc.signalWatchHeight, cc.signalWatchSince = height, now
	}
	if watch.Height > height {
		cc.signalWatchReady = signalBinaryReady(state.BinaryVersion, watch.Version)
	}
	if !own && cc.signalStoppedHeight > 0 && height > cc.signalStoppedHeight {
		cc.signalWatch = signalUpgrade{}
		cc.signalStoppedHeight = 0
		watch = cc.signalWatch
	}
	stopped := !own && watch.Height > 0 && height >= watch.Height && state.Version <= watch.Version && !cc.signalWatchReady &&
		(cc.signalStoppedHeight == height || cc.signalRPCFailed || cc.noNodes || !cc.lastBlockTime.IsZero() && now.Sub(cc.lastBlockTime) >= 2*time.Minute && now.Sub(cc.signalWatchSince) >= 2*time.Minute)
	if stopped {
		cc.signalStoppedHeight = height
	}
	cc.stateMux.Unlock()
	wanted := ""
	if stopped {
		wanted = signalStoppedMessage(watch.Version)
	}
	for message := range activeMessages(cc.name, "Node stopped at the version ") {
		if message == wanted {
			continue
		}
		id := signalAlarmId(val, message)
		if own {
			td.resolveQuietly(cc.name, message, &id)
			continue
		}
		resolve(cc.name, message, resolution{"✅", "Blocks advancing", "blocks advancing again"})
		td.alert(cc.name, message, "info", true, &id)
	}
	if wanted != "" && (!alarmIsActive(cc.name, wanted) || reconcile) {
		id := signalAlarmId(val, wanted)
		td.alert(cc.name, wanted, "critical", false, &id)
	}
}

const (
	signalStopMargin = 120  // blocks before the upgrade height still counted as stopped by it
	signalLastKeep   = 1000 // blocks past the upgrade height the upgrade is remembered
)

func signalScheduledVersion(state signalState) uint64 {
	if state.UpgradeVersion > 0 {
		return state.UpgradeVersion
	}
	return state.Version
}
