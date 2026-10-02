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

// upgradePlan is the upgrade x/upgrade has scheduled.
type upgradePlan struct {
	Name   string
	Height int64
}

// getUpgradePlan reads the scheduled upgrade, nil when there is none.
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
				// the height comes from a node's answer, a bogus one would wrap around to a negative height
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

// thousands writes a height with commas, 12,345,678.
func thousands(n int64) string {
	s := strconv.FormatInt(n, 10)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// the messages are the alarms' keys, the estimated time is looked up when a message is sent since it moves
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

// upgradeETAs and upgradeOutcomes hold, by chain name and height, when an upgrade should happen and how it went.
var upgradeETAs, upgradeOutcomes sync.Map

// upgradeTimeLeft is the estimated time line for an upgrade message, empty when the block time isn't known yet.
func upgradeTimeLeft(chain, message string) string {
	eta, ok := upgradeETAs.Load(chain + "@" + messageHeight(message))
	if !ok {
		return ""
	}
	line := "Around " + eta.(time.Time).Format("2 Jan 2006 15:04 UTC")
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

// refreshUpgrade reads the scheduled upgrade and estimates when it happens from the chain's block time.
func (cc *ChainConfig) refreshUpgrade(ctx context.Context, client *rpchttp.HTTP) {
	plan, err := getUpgradePlan(ctx, client)
	if err != nil {
		l(cc.ChainId, "could not read the upgrade plan:", err)
		return
	}
	cc.stateMux.Lock()
	previous, height, blockTime := cc.upgrade, cc.lastBlockNum, cc.blockTime
	cc.upgrade, cc.upgradeLoaded = plan, true
	cc.stateMux.Unlock()
	if previous != nil && (plan == nil || plan.Height != previous.Height) {
		outcome := "Cancelled"
		if height >= previous.Height {
			outcome = "Upgraded"
		}
		upgradeOutcomes.Store(fmt.Sprintf("%s@%d", cc.name, previous.Height), outcome)
	}
	if plan != nil && blockTime > 0 && height > 0 && plan.Height > height {
		upgradeETAs.Store(fmt.Sprintf("%s@%d", cc.name, plan.Height), time.Now().Add(time.Duration(plan.Height-height)*blockTime).UTC())
	}
}

// upgradeAlarms keeps an alarm while an upgrade is scheduled, and a reminder when it is close. Like governance the
// wanted alarms are worked out each time and compared with the active ones.
func (cc *ChainConfig) upgradeAlarms(valInfo *ValInfo, reconcile bool) {
	cc.stateMux.RLock()
	plan, loaded, height := cc.upgrade, cc.upgradeLoaded, cc.lastBlockNum
	cc.stateMux.RUnlock()
	if !loaded {
		return
	}
	wanted := make(map[string]string)
	priority := make(map[string]string)
	if plan != nil && height < plan.Height {
		message := upgradeMessage(plan)
		wanted[message], priority[message] = fmt.Sprintf("%supgrade%d", valInfo.Valcons, plan.Height), cc.Alerts.UpgradePriority
		if eta, ok := upgradeETAs.Load(fmt.Sprintf("%s@%d", cc.name, plan.Height)); ok && time.Until(eta.(time.Time)) <= time.Duration(cc.Alerts.UpgradeRemindMinutes)*time.Minute {
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
	for message := range active {
		if _, ok := wanted[message]; !ok {
			id := upgradeAlarmId(valInfo, message)
			td.alert(cc.name, message, "info", true, &id)
		}
	}
}

func upgradeAlarmId(valInfo *ValInfo, message string) string {
	if strings.HasSuffix(message, " is close") {
		return valInfo.Valcons + "upgradesoon" + messageHeight(message)
	}
	return valInfo.Valcons + "upgrade" + messageHeight(message)
}
