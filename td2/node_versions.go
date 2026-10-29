package tenderduty

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	rpchttp "github.com/tendermint/tendermint/rpc/client/http"
)

func nodeEndpoint(raw string) string {
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	endpoint, err := url.Parse(raw)
	if err != nil || endpoint.Host == "" {
		return "unknown endpoint"
	}
	return endpoint.Host
}

func (cc *ChainConfig) hasOwnNodes() bool {
	for _, node := range cc.Nodes {
		if node.AlertIfDown {
			return true
		}
	}
	return false
}

func (cc *ChainConfig) markNodeDown(node *NodeConfig, message string, syncing bool, stuck ...string) nodeStatus {
	cc.stateMux.RLock()
	height := cc.lastBlockNum
	cc.stateMux.RUnlock()
	return node.markDownAt(message, syncing, height, stuck...)
}

func (node *NodeConfig) binaryPollDue(now time.Time, scheduled bool) bool {
	if !node.AlertIfDown {
		return false
	}
	interval := 5 * time.Minute
	if scheduled {
		interval = time.Minute
	}
	node.stateMux.Lock()
	defer node.stateMux.Unlock()
	if !node.binaryCheckedAt.IsZero() && now.Sub(node.binaryCheckedAt) < interval {
		return false
	}
	node.binaryCheckedAt = now
	return true
}

func (node *NodeConfig) recordBinary(binary string, readable bool) {
	node.stateMux.Lock()
	defer node.stateMux.Unlock()
	if readable {
		node.binaryVersion = binary
	}
	node.binaryReadable = readable
}

func (cc *ChainConfig) refreshNodeBinary(ctx context.Context, node *NodeConfig, client *rpchttp.HTTP, now time.Time) {
	cc.stateMux.RLock()
	plan, halt, height := cc.upgrade, cc.haltPlan, cc.lastBlockNum
	scheduled := cc.signal.UpgradeHeight > height && cc.signal.UpgradeHeight > 0 || plan != nil && plan.Height > height || atHaltPlan(plan, height) || atHaltPlan(halt, height)
	cc.stateMux.RUnlock()
	node.recordPreUpgrade("", false, plan, halt, height)
	if !node.binaryPollDue(now, scheduled) {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	info, err := client.ABCIInfo(ctx)
	if err != nil {
		node.recordBinary("", false)
		return
	}
	node.recordBinary(info.Response.Version, true)
	node.recordPreUpgrade(info.Response.Version, true, plan, halt, height)
}

func ownNodeReady(status nodeStatus, version uint64) bool {
	return !status.down && status.binaryReadable && signalBinaryReady(status.binaryVersion, version)
}

func (cc *ChainConfig) signalNodesReady(binary string, version uint64) bool {
	if !cc.hasOwnNodes() {
		return signalBinaryReady(binary, version)
	}
	for _, node := range cc.Nodes {
		if node.AlertIfDown && !ownNodeReady(node.snapshot(), version) {
			return false
		}
	}
	return true
}

func (cc *ChainConfig) ownNodeDetails(version uint64, unreadyOnly bool) string {
	var lines []string
	for _, node := range cc.Nodes {
		if !node.AlertIfDown {
			continue
		}
		status := node.snapshot()
		ready := ownNodeReady(status, version)
		if ready && unreadyOnly {
			continue
		}
		detail := "version unknown"
		if _, known := parseBinaryMajor(status.binaryVersion); !status.down && status.binaryReadable && known {
			detail = "celestia-app " + strings.TrimPrefix(strings.TrimSpace(status.binaryVersion), "v")
			if ready {
				detail += " · ready"
			} else {
				detail += fmt.Sprintf(" · needs v%d", version)
			}
		}
		lines = append(lines, td.nodeDisplay(cc.name, node.Url)+" · "+detail)
	}
	return strings.Join(lines, "\n")
}

func (cc *ChainConfig) notUpgradedNodes(version uint64) string {
	var nodes []string
	for _, node := range cc.Nodes {
		if !node.AlertIfDown {
			continue
		}
		status := node.snapshot()
		if ownNodeReady(status, version) {
			continue
		}
		binary := strings.TrimPrefix(strings.TrimSpace(status.binaryVersion), "v")
		if _, known := parseBinaryMajor(binary); !known {
			binary = "version unknown"
		}
		nodes = append(nodes, td.nodeDisplay(cc.name, node.Url)+" ("+binary+")")
	}
	if len(nodes) == 0 {
		return ""
	}
	return " · not upgraded: " + strings.Join(nodes, ", ")
}

func nodeDownDetail(chain, message string) string {
	_, body, ok := strings.Cut(message, "\n")
	if !ok {
		body = message
	}
	match := cardNodeDown.FindStringSubmatch(body)
	if match == nil {
		return ""
	}
	return ownNodeDownDetail(chain, match[1])
}

func ownNodeDownDetail(chain, nodeURL string) string {
	td.chainsMux.RLock()
	cc := td.Chains[chain]
	td.chainsMux.RUnlock()
	if cc == nil {
		return ""
	}
	cc.stateMux.RLock()
	watch, chainHeight := cc.signalWatch, cc.lastBlockNum
	if cc.signalLast.Height > 0 {
		watch = cc.signalLast
	}
	if cc.signal.UpgradeHeight > 0 {
		watch = signalUpgrade{Version: signalScheduledVersion(cc.signal), Height: cc.signal.UpgradeHeight}
	}
	cc.stateMux.RUnlock()
	for _, node := range cc.Nodes {
		if node.Url != nodeURL || !node.AlertIfDown {
			continue
		}
		status := node.snapshot()
		major, known := parseBinaryMajor(status.binaryVersion)
		// the observed chain height can lag the node when it stops
		downHeight := status.downHeight
		if downHeight == 0 {
			downHeight = chainHeight
		}
		if status.down && !status.syncing && watch.Height > 0 && downHeight >= watch.Height-signalStopMargin && known && major < watch.Version {
			return fmt.Sprintf("stopped at the version %d upgrade height %s: runs celestia-app %s, upgrade to v%d", watch.Version, thousands(watch.Height), strings.TrimPrefix(strings.TrimSpace(status.binaryVersion), "v"), watch.Version)
		}
		if status.down {
			return status.stuckDetail
		}
	}
	return ""
}

func nodeBinaryLabel(status nodeStatus) string {
	if _, known := parseBinaryMajor(status.binaryVersion); known {
		return "v" + strings.TrimPrefix(strings.TrimSpace(status.binaryVersion), "v")
	}
	return "version unknown"
}

func (node *NodeConfig) recordPreUpgrade(binary string, readable bool, plan, halt *upgradePlan, height int64) {
	node.stateMux.Lock()
	defer node.stateMux.Unlock()
	if plan == nil && halt == nil {
		node.preUpgradeVersion, node.preUpgradeHeight = "", 0
	} else if node.AlertIfDown && readable && plan != nil && height < plan.Height && node.preUpgradeHeight != plan.Height {
		// the first version seen for this plan, a binary swapped in early must not become the baseline
		node.preUpgradeVersion, node.preUpgradeHeight = strings.TrimSpace(binary), plan.Height
	}
}
