package tenderduty

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	rpchttp "github.com/tendermint/tendermint/rpc/client/http"
	"google.golang.org/protobuf/encoding/protowire"
)

type upgradePlan struct {
	Name   string
	Height int64
}

// getUpgradePlan returns nil when none is scheduled.
func getUpgradePlan(ctx context.Context, client *rpchttp.HTTP) (*upgradePlan, error) {
	resp, err := client.ABCIQuery(ctx, "/cosmos.upgrade.v1beta1.Query/CurrentPlan", nil)
	if err != nil {
		return nil, err
	}
	if resp.Response.Code != 0 {
		return nil, fmt.Errorf("current plan: %s", resp.Response.Log)
	}
	return parsePlan(resp.Response.Value)
}

func getAppliedPlanHeight(ctx context.Context, client *rpchttp.HTTP, name string) (int64, error) {
	request := protowire.AppendString(protowire.AppendTag(nil, 1, protowire.BytesType), name)
	resp, err := client.ABCIQuery(ctx, "/cosmos.upgrade.v1beta1.Query/AppliedPlan", request)
	if err != nil {
		return 0, err
	}
	if resp.Response.Code != 0 {
		return 0, fmt.Errorf("applied plan: %s", resp.Response.Log)
	}
	var height int64
	err = eachField(resp.Response.Value, func(num protowire.Number, typ protowire.Type, _ []byte, varint uint64) error {
		if num == 1 && typ == protowire.VarintType {
			if varint > math.MaxInt64 {
				return fmt.Errorf("applied upgrade height %d is out of range", varint)
			}
			height = int64(varint)
		}
		return nil
	})
	return height, err
}

// parsePlan reads QueryCurrentPlanResponse{plan: {name: 1, height: 3}} by field number.
func parsePlan(b []byte) (*upgradePlan, error) {
	var plan *upgradePlan
	err := eachField(b, func(num protowire.Number, typ protowire.Type, value []byte, _ uint64) error {
		if num != 1 || typ != protowire.BytesType {
			return nil
		}
		plan = &upgradePlan{}
		return eachField(value, func(num protowire.Number, typ protowire.Type, value []byte, varint uint64) error {
			switch {
			case num == 1 && typ == protowire.BytesType:
				plan.Name = string(value)
			case num == 3 && typ == protowire.VarintType:
				// a bogus height would wrap negative
				if varint > math.MaxInt64 {
					return fmt.Errorf("upgrade height %d is out of range", varint)
				}
				plan.Height = int64(varint)
			}
			return nil
		})
	})
	if plan != nil && plan.Height <= 0 {
		return nil, err
	}
	return plan, err
}

// thousands writes a height as 12,345,678.
func thousands(n int64) string {
	s := strconv.FormatInt(n, 10)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// upgrade alarm keys omit the changing ETA
func upgradeMessage(plan *upgradePlan) string {
	return fmt.Sprintf("Upgrade %s at height %s", plan.Name, thousands(plan.Height))
}

func upgradeSoonMessage(plan *upgradePlan) string {
	return upgradeMessage(plan) + " is close"
}

var upgradeHeight = regexp.MustCompile(` at height ([\d,]+)`)

func messageHeight(message string) string {
	if match := upgradeHeight.FindStringSubmatch(message); match != nil {
		return strings.ReplaceAll(match[1], ",", "")
	}
	return ""
}

// upgradeETAs and upgradeOutcomes are keyed chain@height.
var upgradeETAs, upgradeOutcomes sync.Map

// upgradeTimeLeft is the estimated time line, empty until the block time is known.
func upgradeTimeLeft(chain, message string) string {
	eta, ok := upgradeETAs.Load(chain + "@" + messageHeight(message))
	if !ok {
		return ""
	}
	if isSignalUpgrade(message) {
		return formatUpgradeETA(eta.(time.Time), time.Now(), alertLocation())
	}
	line := "Around " + eta.(time.Time).In(alertLocation()).Format("2 Jan 2006 15:04 MST")
	if left := time.Until(eta.(time.Time)); left > 0 {
		line += " · " + shortDuration(left) + " left"
	}
	return line
}

func upgradeResolvedLabel(chain, message string) (icon, label string) {
	if outcome, ok := upgradeOutcomes.Load(chain + "@" + messageHeight(message)); ok && outcome == "Cancelled" {
		return "🚫", "Cancelled"
	}
	return "✅", "Upgraded"
}

// a replayed upgrade block never reaches the websocket, so ask the chain whether the plan ran
func upgradeOutcome(previous *upgradePlan, height, applied int64) string {
	if height >= previous.Height || applied > 0 {
		return "Upgraded"
	}
	return "Cancelled"
}

func (cc *ChainConfig) refreshUpgrade(ctx context.Context, client *rpchttp.HTTP) {
	plan, err := getUpgradePlan(ctx, client)
	if err != nil {
		cc.stateMux.Lock()
		failed := cc.upgradeReadFailed
		cc.upgradeReadFailed = true
		cc.stateMux.Unlock()
		if !failed {
			l(cc.ChainId, "could not read the upgrade plan:", err)
		}
		return
	}
	cc.stateMux.Lock()
	previous, height, blockTime := cc.upgrade, cc.lastBlockNum, cc.blockTime
	failed := cc.upgradeReadFailed
	cc.upgradeReadFailed = false
	cc.upgrade, cc.upgradeLoaded = plan, true
	if previous != nil && (plan == nil || *plan != *previous) {
		cc.haltPlan = previous
	}
	if cc.haltPlan != nil && height > cc.haltPlan.Height {
		cc.haltPlan = nil
	}
	cc.stateMux.Unlock()
	if failed {
		l(cc.ChainId, "upgrade plan readable again")
	}
	if previous != nil && (plan == nil || plan.Height != previous.Height) {
		var applied int64
		if height < previous.Height {
			applied, err = getAppliedPlanHeight(ctx, client, previous.Name)
			if err != nil {
				l(cc.ChainId, "could not read the applied upgrade plan:", err)
				applied = 0
			}
		}
		upgradeOutcomes.Store(fmt.Sprintf("%s@%d", cc.name, previous.Height), upgradeOutcome(previous, height, applied))
	}
	if plan != nil && blockTime > 0 && height > 0 && plan.Height > height {
		upgradeETAs.Store(fmt.Sprintf("%s@%d", cc.name, plan.Height), time.Now().Add(time.Duration(plan.Height-height)*blockTime).UTC())
	}
}

// upgradeAlarms keeps scheduled upgrades and due reminders active.
func (cc *ChainConfig) upgradeAlarms(valInfo *ValInfo, reconcile bool) {
	cc.stateMux.RLock()
	plan, loaded, height := cc.upgrade, cc.upgradeLoaded, cc.lastBlockNum
	signal, signalLoaded := cc.signal, cc.signalLoaded
	cc.stateMux.RUnlock()
	if !loaded {
		return
	}
	// Celestia exposes x/signal upgrades as v<N> plans one block later; skip duplicate alarms
	if plan != nil && signalLoaded && signal.coversPlan(plan.Name) {
		plan = nil
	}
	wanted := make(map[string]string)
	priority := make(map[string]string)
	if plan != nil && height < plan.Height {
		message := upgradeMessage(plan)
		wanted[message], priority[message] = fmt.Sprintf("%supgrade%d", valInfo.Valcons, plan.Height), "critical"
		if eta, ok := upgradeETAs.Load(fmt.Sprintf("%s@%d", cc.name, plan.Height)); ok && time.Until(eta.(time.Time)) <= time.Duration(cc.alertSettings().UpgradeRemindMinutes)*time.Minute {
			message = upgradeSoonMessage(plan)
			wanted[message], priority[message] = fmt.Sprintf("%supgradesoon%d", valInfo.Valcons, plan.Height), "critical"
		}
	}
	active := activeMessages(cc.name, "Upgrade ")
	for message, id := range wanted {
		if !active[message] || reconcile {
			id := id
			td.alert(cc.name, message, priority[message], false, &id)
		}
	}
	// alarm and reminder clear together, the reminder announces it
	reminded := make(map[string]bool)
	for message := range active {
		if _, ok := wanted[message]; !ok && strings.HasSuffix(message, " is close") {
			reminded[messageHeight(message)] = true
		}
	}
	for message := range active {
		if _, ok := wanted[message]; !ok {
			id := upgradeAlarmId(valInfo, message)
			if reminded[messageHeight(message)] && !strings.HasSuffix(message, " is close") {
				td.resolveQuietly(cc.name, message, &id)
				continue
			}
			td.alert(cc.name, message, "info", true, &id)
		}
	}
}

// stalledUpgradeDetail describes stalls at an upgrade height.
func stalledUpgradeDetail(chain string) string {
	td.chainsMux.RLock()
	cc := td.Chains[chain]
	td.chainsMux.RUnlock()
	if cc == nil {
		return ""
	}
	cc.stateMux.RLock()
	plan, signal, height := cc.upgrade, cc.signal, cc.lastBlockNum
	// the signal query drops the upgrade once its height is reached
	if signal.UpgradeHeight == 0 && cc.signalLast.Height > 0 {
		signal.UpgradeVersion, signal.UpgradeHeight = cc.signalLast.Version, cc.signalLast.Height
	}
	cc.stateMux.RUnlock()
	var name string
	var at int64
	switch {
	// halts at plan height - 1; x/signal plans appear one block after their upgrade height
	case plan != nil && (height == plan.Height-1 || height == plan.Height):
		name, at = plan.Name, plan.Height
		if signal.coversPlan(plan.Name) {
			name = "version " + strings.TrimPrefix(plan.Name, "v")
			if signal.UpgradeHeight > 0 {
				at = signal.UpgradeHeight
			}
		}
	case signal.UpgradeHeight > 0 && height == signal.UpgradeHeight:
		name, at = fmt.Sprintf("version %d", signal.UpgradeVersion), signal.UpgradeHeight
	default:
		return ""
	}
	return fmt.Sprintf("halted at the %s upgrade height %s, resumes once 2/3 of voting power runs the new binary", clipRunes(name, 80), thousands(at))
}

func (s signalState) coversPlan(name string) bool {
	for _, v := range []uint64{s.UpgradeVersion, s.Version, s.Version - 1} {
		if v > 0 && name == fmt.Sprintf("v%d", v) {
			return true
		}
	}
	return false
}

func upgradeAlarmId(valInfo *ValInfo, message string) string {
	if strings.HasSuffix(message, " is close") {
		return valInfo.Valcons + "upgradesoon" + messageHeight(message)
	}
	return valInfo.Valcons + "upgrade" + messageHeight(message)
}
