package tenderduty

import (
	"fmt"
	"strings"
)

type stalledNode struct {
	name, version, baseline string
	readable                bool
}

func atHaltPlan(plan *upgradePlan, height int64) bool {
	return plan != nil && height >= plan.Height-1 && height <= plan.Height
}

func stalledMessage(chain string, minutes, sources int, fallback bool, source string, plan *upgradePlan, height int64, nodes []stalledNode) (string, string) {
	if atHaltPlan(plan, height) {
		var old, upgraded []string
		unknown := false
		for _, node := range nodes {
			version, baseline := strings.TrimSpace(node.version), strings.TrimSpace(node.baseline)
			if !node.readable || version == "" || baseline == "" {
				unknown = true
				continue
			}
			if version == baseline {
				old = append(old, node.name+" still runs "+version)
			} else {
				upgraded = append(upgraded, node.name+" runs "+version)
			}
		}
		prefix := fmt.Sprintf("%s stopped at upgrade %s height %s: ", chain, plan.Name, thousands(plan.Height))
		if len(old) > 0 {
			return prefix + strings.Join(old, ", ") + ", switch the binary", "critical"
		}
		if !unknown && len(upgraded) > 0 {
			return prefix + strings.Join(upgraded, ", ") + ", waiting for the network", "warning"
		}
	}
	message := fmt.Sprintf("stalled: have not seen a new block on %s in %d minutes", chain, minutes)
	if sources == 1 && !fallback {
		message += "\n" + source + " is the only source, so either it or the chain stopped"
	}
	return message, "critical"
}

func (cc *ChainConfig) stalledMessage() (string, string) {
	cc.stateMux.Lock()
	if cc.haltPlan != nil && cc.lastBlockNum > cc.haltPlan.Height {
		cc.haltPlan = nil
	}
	plan, height := cc.upgrade, cc.lastBlockNum
	if atHaltPlan(cc.haltPlan, height) {
		plan = cc.haltPlan
	}
	if plan != nil && cc.signalLoaded && cc.signal.coversPlan(plan.Name) {
		plan = nil
	}
	cc.stateMux.Unlock()
	var nodes []stalledNode
	source := ""
	for _, node := range cc.Nodes {
		name := td.nodeDisplay(cc.name, node.Url)
		source = name
		if node.AlertIfDown {
			node.stateMux.RLock()
			nodes = append(nodes, stalledNode{name, node.binaryVersion, node.preUpgradeVersion, node.binaryReadable})
			node.stateMux.RUnlock()
		}
	}
	return stalledMessage(cc.ChainId, cc.alertSettings().Stalled, len(cc.Nodes), cc.publicFallbackEnabled(), source, plan, height, nodes)
}

func (cc *ChainConfig) isStalledMessage(message string) bool {
	return strings.HasPrefix(message, "stalled: have not seen a new block on "+cc.ChainId+" in ") || strings.HasPrefix(message, cc.ChainId+" stopped at upgrade ")
}

// Alarm keys already persist the sent text.
func (cc *ChainConfig) restoreStalledAlarm() {
	cc.lastBlockAlarm, cc.lastBlockAlarmMsg = false, ""
	for message := range activeMessages(cc.name, "") {
		if cc.isStalledMessage(message) {
			cc.lastBlockAlarm, cc.lastBlockAlarmMsg = true, message
			return
		}
	}
}

func (cc *ChainConfig) stalledAlarm(val *ValInfo, resolved bool) {
	message, priority := cc.lastBlockAlarmMsg, "critical"
	if message == "" {
		if resolved {
			message = fmt.Sprintf("stalled: have not seen a new block on %s in %d minutes", cc.ChainId, cc.alertSettings().Stalled)
		} else {
			message, priority = cc.stalledMessage()
		}
	} else if strings.HasSuffix(message, ", waiting for the network") {
		priority = "warning"
	}
	cc.lastBlockAlarm = !resolved
	cc.lastBlockAlarmMsg = message
	if resolved {
		priority = "info"
		cc.lastBlockAlarmMsg = ""
	}
	td.alert(cc.name, message, priority, resolved, &val.Valcons)
}
