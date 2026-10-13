package tenderduty

import (
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	dash "github.com/kynraze/tenderduty/v2/td2/dashboard"
	rpchttp "github.com/tendermint/tendermint/rpc/client/http"
)

type nodeStatus struct {
	down, wasDown, syncing bool
	checked                bool // false until the node is checked during this run, a restored down state may be stale
	lastMsg                string
	downSince              time.Time
	downHeight             int64
	binaryVersion          string
	binaryReadable         bool
}

func (cc *ChainConfig) dashboardStatus() *dash.ChainStatus {
	info, _ := cc.validatorState()
	cc.stateMux.RLock()
	height, observed, unavailable := cc.lastBlockNum, cc.lastBlockTime, cc.noNodes
	blocks := append([]int(nil), cc.blocksResults...)
	signed, proposed, missedProps := int64(cc.statTotalSigns), int64(cc.statTotalProps), int64(cc.statProposalMiss)
	cc.stateMux.RUnlock()
	lastBlockAt := int64(0)
	if !observed.IsZero() {
		lastBlockAt = observed.Unix()
	}
	var rpcNodes []dash.RPCNodeStatus
	healthy := 0
	message := getAlarms(cc.name)
	for _, node := range cc.Nodes {
		status := node.snapshot()
		if !td.HideLogs {
			version := ""
			if node.AlertIfDown {
				version = nodeBinaryLabel(status)
			}
			rpcNodes = append(rpcNodes, dash.RPCNodeStatus{Endpoint: nodeEndpoint(node.Url), Down: status.down, Version: version})
		}
		if !status.down {
			healthy++
		} else if !td.HideLogs && status.lastMsg != "" {
			message += "\n - " + status.lastMsg
		}
	}
	active, notices, noticeLabel := cc.dashboardAlerts()
	status := &dash.ChainStatus{
		MsgType: "status", Name: cc.name, ChainId: cc.ChainId, Moniker: info.Moniker,
		Bonded: info.Bonded, Jailed: info.Jailed, Tombstoned: info.Tombstoned,
		Missed: info.Missed, Window: info.Window, Nodes: len(cc.Nodes), HealthyNodes: healthy, RPCNodes: rpcNodes,
		SigningStale: info.SigningStale, ValidatorStale: info.ValidatorStale, Monitoring: cc.isMonitoring(),
		NoNodes: unavailable, ActiveAlerts: active, NoticeAlerts: notices, NoticeLabel: noticeLabel, Height: height,
		LastBlockAt: lastBlockAt, LastError: message, Alerts: cc.dashboardAlertLines(), Blocks: blocks,
		Signed: signed, Proposed: proposed, MissedProps: missedProps,
		VotingPower: info.VotingPower, TotalPower: info.TotalPower, Rank: info.Rank, ActiveSet: info.ActiveSet,
		MaxValidators: info.MaxValidators, MaxMissed: info.MaxMissed,
	}
	if cc.alertSettings().GovAlerts {
		if proposals, loaded := cc.govSnapshot(); loaded {
			for _, p := range proposals {
				title := p.Title
				if len(title) > 200 {
					end := 200
					for !utf8.RuneStart(title[end]) {
						end--
					}
					title = title[:end]
				}
				status.Proposals = append(status.Proposals, dash.Proposal{ID: p.ID, Title: title, End: p.VotingEnd.Unix(), Voted: p.Voted})
			}
		}
	}
	if fibre := cc.fibreReport(time.Now()); fibre != nil {
		status.Fibre = fibre.dashboard()
	}
	text, tone := cc.bridgeStatus()
	if text != "" {
		title, facts := cc.bridgeFacts(time.Now())
		status.Bridge = &dash.BridgeStatus{Text: text, Tone: tone, Title: title}
		for _, fact := range facts {
			status.Bridge.Facts = append(status.Bridge.Facts, dash.BridgeFact{Label: fact.Label, Value: fact.Value, Tone: fact.Tone})
		}
	}
	return status
}

func (cc *ChainConfig) dashboardAlertLines() []dash.AlertLine {
	if td.HideLogs {
		return nil
	}
	alarms.notifyMux.RLock()
	messages := make(map[string]string, len(alarms.AllAlarms[cc.name]))
	for message := range alarms.AllAlarms[cc.name] {
		messages[message] = alarms.AlarmSeverities[cc.name+"\x00"+message]
	}
	alarms.notifyMux.RUnlock()
	type line struct {
		dash.AlertLine
		severity string
	}
	var lines []line
	for message, severity := range messages {
		if strings.HasPrefix(message, "Proposal #") {
			continue
		}
		if _, close := messages[message+" is close"]; close {
			continue
		}
		body := (&alertMsg{chain: cc.name, message: strings.TrimPrefix(message, "🚨 "), severity: severity}).summary()
		text, rest, _ := strings.Cut(body, "\n")
		text = strings.TrimPrefix(text, "🚨 ")
		if strings.HasPrefix(text, "Severity: ") {
			_, text, _ = strings.Cut(strings.TrimPrefix(text, "Severity: "), " ")
		}
		detail := ""
		for _, extra := range strings.Split(rest, "\n") {
			if extra != "" && !strings.HasPrefix(extra, "≈ ") && !strings.HasPrefix(extra, "Around ") {
				detail = extra
				break
			}
		}
		notice, _ := cc.noticeAlarm(message, severity)
		lines = append(lines, line{dash.AlertLine{Text: text, Detail: detail, Notice: notice}, severity})
	}
	sort.Slice(lines, func(i, j int) bool {
		if lines[i].Notice != lines[j].Notice {
			return !lines[i].Notice
		}
		if left, right := severityRank(lines[i].severity), severityRank(lines[j].severity); left != right {
			return left > right
		}
		return lines[i].Text < lines[j].Text
	})
	var result []dash.AlertLine
	for _, line := range lines {
		result = append(result, line.AlertLine)
	}
	return result
}

// noticeAlarm keeps reminders actionable even when the scheduled upgrade is a notice.
func (cc *ChainConfig) noticeAlarm(message, severity string) (notice, upgrade bool) {
	if strings.HasPrefix(message, "Proposal #") {
		return true, false
	}
	if strings.HasSuffix(message, " is close") {
		return false, false
	}
	if strings.HasPrefix(message, "Upgrade ") && strings.Contains(message, " at height ") {
		return true, true
	}
	if isSignalUpgrade(message) {
		cc.stateMux.RLock()
		binary := cc.signal.BinaryVersion
		cc.stateMux.RUnlock()
		return cc.signalNodesReady(binary, signalMessageVersion(message)), true
	}
	return severity == "info", false
}

func (cc *ChainConfig) dashboardAlerts() (active, notices int, label string) {
	alarms.notifyMux.RLock()
	messages := make(map[string]string, len(alarms.AllAlarms[cc.name]))
	for message := range alarms.AllAlarms[cc.name] {
		messages[message] = alarms.AlarmSeverities[cc.name+"\x00"+message]
	}
	alarms.notifyMux.RUnlock()
	proposals, votePending := 0, false
	for message, severity := range messages {
		if strings.HasPrefix(message, "Proposal #") {
			proposals++
			votePending = votePending || strings.HasSuffix(message, " has not voted") || strings.HasSuffix(message, " has not voted yet")
		}
		if notice, upgrade := cc.noticeAlarm(message, severity); notice {
			notices++
			if upgrade {
				label = "Upgrade scheduled"
			}
		}
	}
	if notices > 0 && label == "" {
		label = "Notice"
		if proposals == len(messages) {
			label = "Proposal open"
			if votePending {
				label = "Vote pending"
			}
		}
	}
	return len(messages), notices, label
}

func (node *NodeConfig) snapshot() nodeStatus {
	node.stateMux.RLock()
	defer node.stateMux.RUnlock()
	return nodeStatus{
		down: node.down, wasDown: node.wasDown, syncing: node.syncing, checked: node.checked,
		lastMsg: node.lastMsg, downSince: node.downSince, downHeight: node.downHeight,
		binaryVersion: node.binaryVersion, binaryReadable: node.binaryReadable,
	}
}

func (node *NodeConfig) markDown(message string, syncing bool) nodeStatus {
	return node.markDownAt(message, syncing, 0)
}

func (node *NodeConfig) markDownAt(message string, syncing bool, height int64) nodeStatus {
	node.stateMux.Lock()
	if !node.down {
		node.downSince = time.Now()
		node.downHeight = height
	}
	node.down = true
	node.binaryReadable = false
	node.syncing = syncing
	node.checked = true
	node.lastMsg = message
	status := nodeStatus{
		down: node.down, wasDown: node.wasDown, syncing: node.syncing, checked: node.checked,
		lastMsg: node.lastMsg, downSince: node.downSince, downHeight: node.downHeight,
		binaryVersion: node.binaryVersion, binaryReadable: node.binaryReadable,
	}
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
	node.downHeight = 0
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

func (cc *ChainConfig) averageBlockTime() time.Duration {
	cc.stateMux.RLock()
	defer cc.stateMux.RUnlock()
	return cc.blockTime
}

// recentProposalMisses counts the missed proposals within the window and forgets older ones.
func (cc *ChainConfig) recentProposalMisses(window time.Duration) int {
	cc.stateMux.Lock()
	defer cc.stateMux.Unlock()
	since := time.Now().Add(-window)
	recent := cc.proposalMisses[:0]
	for _, missed := range cc.proposalMisses {
		if missed.After(since) {
			recent = append(recent, missed)
		}
	}
	cc.proposalMisses = recent
	return len(recent)
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
