package tenderduty

import (
	dash "github.com/blockpane/tenderduty/v2/td2/dashboard"
	rpchttp "github.com/tendermint/tendermint/rpc/client/http"
	"time"
)

type nodeStatus struct {
	down, wasDown, syncing bool
	checked                bool // false until the node is checked during this run, a restored down state may be stale
	lastMsg                string
	downSince              time.Time
}

func (cc *ChainConfig) dashboardStatus() *dash.ChainStatus {
	info, _ := cc.validatorState()
	cc.stateMux.RLock()
	height, observed, unavailable := cc.lastBlockNum, cc.lastBlockTime, cc.noNodes
	blocks := append([]int(nil), cc.blocksResults...)
	cc.stateMux.RUnlock()
	lastBlockAt := int64(0)
	if !observed.IsZero() {
		lastBlockAt = observed.Unix()
	}
	healthy := 0
	message := getAlarms(cc.name)
	for _, node := range cc.Nodes {
		status := node.snapshot()
		if !status.down {
			healthy++
		} else if !td.HideLogs && status.lastMsg != "" {
			message += "\n - " + status.lastMsg
		}
	}
	return &dash.ChainStatus{
		MsgType: "status", Name: cc.name, ChainId: cc.ChainId, Moniker: info.Moniker,
		Bonded: info.Bonded, Jailed: info.Jailed, Tombstoned: info.Tombstoned,
		Missed: info.Missed, Window: info.Window, Nodes: len(cc.Nodes), HealthyNodes: healthy,
		SigningStale: info.SigningStale, ValidatorStale: info.ValidatorStale, Monitoring: cc.isMonitoring(),
		NoNodes: unavailable, ActiveAlerts: alarms.getCount(cc.name), Height: height,
		LastBlockAt: lastBlockAt, LastError: message, Blocks: blocks,
	}
}

func (node *NodeConfig) snapshot() nodeStatus {
	node.stateMux.RLock()
	defer node.stateMux.RUnlock()
	return nodeStatus{node.down, node.wasDown, node.syncing, node.checked, node.lastMsg, node.downSince}
}

func (node *NodeConfig) markDown(message string, syncing bool) nodeStatus {
	node.stateMux.Lock()
	if !node.down {
		node.downSince = time.Now()
	}
	node.down = true
	node.syncing = syncing
	node.checked = true
	node.lastMsg = message
	status := nodeStatus{node.down, node.wasDown, node.syncing, node.checked, node.lastMsg, node.downSince}
	node.stateMux.Unlock()
	return status
}

func (node *NodeConfig) markHealthy() {
	node.stateMux.Lock()
	defer node.stateMux.Unlock()
	if node.down {
		node.wasDown = true
	}
	node.down = false
	node.syncing = false
	node.checked = true
	node.lastMsg = ""
	node.downSince = time.Time{}
}

func (node *NodeConfig) clearRecovery() {
	node.stateMux.Lock()
	node.wasDown = false
	node.stateMux.Unlock()
}

func (cc *ChainConfig) blockState() (time.Time, bool, float64) {
	cc.stateMux.RLock()
	defer cc.stateMux.RUnlock()
	return cc.lastBlockTime, cc.observedBlock, cc.statConsecutiveMiss
}

func (cc *ChainConfig) blocksSnapshot() []int {
	cc.stateMux.RLock()
	defer cc.stateMux.RUnlock()
	return append([]int(nil), cc.blocksResults...)
}

func (cc *ChainConfig) hasNoNodes() bool {
	cc.stateMux.RLock()
	defer cc.stateMux.RUnlock()
	return cc.noNodes
}

func (cc *ChainConfig) setNoNodes(value bool) {
	cc.stateMux.Lock()
	cc.noNodes = value
	cc.stateMux.Unlock()
}

func (cc *ChainConfig) clientSnapshot() *rpchttp.HTTP {
	cc.stateMux.RLock()
	defer cc.stateMux.RUnlock()
	return cc.client
}

func (cc *ChainConfig) setClient(client *rpchttp.HTTP) {
	cc.stateMux.Lock()
	cc.client = client
	cc.stateMux.Unlock()
}

func (cc *ChainConfig) validatorState() (*ValInfo, *ValInfo) {
	cc.validatorMux.RLock()
	defer cc.validatorMux.RUnlock()
	current := &ValInfo{Moniker: "not connected"}
	if cc.valInfo != nil {
		copy := *cc.valInfo
		current = &copy
	}
	var previous *ValInfo
	if cc.lastValInfo != nil {
		copy := *cc.lastValInfo
		previous = &copy
	}
	return current, previous
}
