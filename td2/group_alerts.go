package tenderduty

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// group_node_alerts combines chat messages; alarms, outbox and PagerDuty remain per node.

const (
	defaultGroupWaitSeconds = 60
	minGroupWaitSeconds     = 10
	groupLookupTimeout      = 2 * time.Second
	serverCacheTime         = 10 * time.Minute
	serverFailedTime        = time.Minute
	groupListLimit          = 20 // discord allows 25 fields
)

var groupWaitUnit = time.Second

var lookupIPs = func(ctx context.Context, host string) ([]net.IPAddr, error) {
	return net.DefaultResolver.LookupIPAddr(ctx, host)
}

type serverEntry struct {
	server  string
	expires time.Time
}

var (
	serverCacheMux sync.Mutex
	serverCache    = make(map[string]serverEntry)
)

func (c *Config) groupSettings() (on bool, wait time.Duration) {
	settings := c.settings()
	seconds := settings.GroupWaitSeconds
	if seconds <= 0 {
		seconds = defaultGroupWaitSeconds
	}
	return settings.GroupNodeAlerts, time.Duration(seconds) * groupWaitUnit
}

func (node *NodeConfig) serverLabel() string {
	node.stateMux.RLock()
	defer node.stateMux.RUnlock()
	return node.Server
}

func (cc *ChainConfig) adoptServers(next *ChainConfig) {
	for i, node := range cc.Nodes {
		if i >= len(next.Nodes) || next.Nodes[i] == nil || next.Nodes[i].Url != node.Url {
			continue
		}
		server := next.Nodes[i].serverLabel()
		node.stateMux.Lock()
		node.Server = server
		node.stateMux.Unlock()
	}
}

func (c *Config) nodeServer(chain, nodeURL string) string {
	c.chainsMux.RLock()
	cc := c.Chains[chain]
	c.chainsMux.RUnlock()
	if cc != nil {
		for _, node := range cc.Nodes {
			if node != nil && node.Url == nodeURL {
				if label := node.serverLabel(); label != "" {
					return label
				}
				break
			}
		}
	}
	if name := c.serverName(nodeURL); name != "" {
		return name
	}
	server := resolveServer(c.context(), nodeURL)
	if name := c.namedServer(server); name != "" {
		return name
	}
	return server
}

func (c *Config) namedServer(host string) string {
	return c.settings().Servers[strings.ToLower(host)]
}

// serverName uses only the URL host; displaying a node never needs DNS.
func (c *Config) serverName(nodeURL string) string {
	parsed, err := url.Parse(nodeURL)
	if err != nil || parsed.Hostname() == "" {
		return ""
	}
	return c.namedServer(parsed.Hostname())
}

func resolveServer(ctx context.Context, nodeURL string) string {
	parsed, err := url.Parse(nodeURL)
	if err != nil || parsed.Hostname() == "" {
		return nodeURL
	}
	host := parsed.Hostname()
	serverCacheMux.Lock()
	cached, ok := serverCache[host]
	serverCacheMux.Unlock()
	if ok && time.Now().Before(cached.expires) {
		return cached.server
	}
	lookup, cancel := context.WithTimeout(ctx, groupLookupTimeout)
	addrs, err := lookupIPs(lookup, host)
	cancel()
	if ctx.Err() != nil {
		return host
	}
	server, ttl := host, serverFailedTime
	var lowest net.IP
	for _, addr := range addrs {
		if ip := addr.IP.To16(); ip != nil && (lowest == nil || bytes.Compare(ip, lowest) < 0) {
			lowest, server, ttl = ip, addr.IP.String(), serverCacheTime
		}
	}
	if err != nil {
		server, ttl = host, serverFailedTime
	}
	serverCacheMux.Lock()
	serverCache[host] = serverEntry{server: server, expires: time.Now().Add(ttl)}
	serverCacheMux.Unlock()
	return server
}

func nodeDownParts(message string) (nodeURL, down string, ok bool) {
	first, _, _ := strings.Cut(withoutSeverity(message), "\n")
	m := cardNodeDown.FindStringSubmatch(first)
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}

func chatDest(dest notifyDest) bool { return dest == tg || dest == di || dest == slk || dest == gt }

func destOn(msg *alertMsg, dest notifyDest) bool {
	switch dest {
	case tg:
		return msg.tg
	case di:
		return msg.disc
	case slk:
		return msg.slk
	case gt:
		return msg.gotify
	}
	return false
}

func destIdentity(msg *alertMsg, dest notifyDest) string {
	switch dest {
	case tg:
		return msg.tgKey + "\x00" + msg.tgChannel
	case di:
		return msg.discHook
	case slk:
		return msg.slkHook
	case gt:
		return msg.gotifyURL + "\x00" + msg.gotifyToken
	}
	return ""
}

func postGrouped(msg *alertMsg, dest notifyDest) error {
	switch dest {
	case tg:
		return postTg(msg)
	case di:
		return postDiscord(msg)
	case slk:
		return postSlack(msg)
	case gt:
		return postGotify(msg)
	}
	return nil
}

func (item queuedNotification) key() string { return item.Chain + "\x00" + item.Message }

// notifyMux held.
func (a *alarmCache) sent(item queuedNotification) bool {
	if item.Escalated && !item.Resolved {
		return !a.EscalationsSent[item.key()].IsZero()
	}
	return a.sentTo(item.Destination, item.key(), item.Message)
}

// claim marks an idle notification in flight. notifyMux held.
func (a *alarmCache) claim(dest notifyDest, key string) bool {
	if a.inFlight == nil {
		a.inFlight = make(map[notifyDest]map[string]bool)
	}
	if a.inFlight[dest] == nil {
		a.inFlight[dest] = make(map[string]bool)
	}
	if a.inFlight[dest][key] {
		return false
	}
	a.inFlight[dest][key] = true
	return true
}

func (c *Config) groupDelay(item queuedNotification) time.Duration {
	if item.Resolved || !chatDest(item.Destination) || item.Queued.IsZero() {
		return 0
	}
	if _, _, ok := nodeDownParts(item.Message); !ok {
		return 0
	}
	on, wait := c.groupSettings()
	if !on {
		return 0
	}
	return time.Until(item.Queued.Add(wait))
}

// notifyGroup stores sent messages for recovery, keyed by chain and message.
type notifyGroup struct {
	Destination notifyDest    `json:"destination"`
	Server      string        `json:"server"`
	Members     []groupMember `json:"members"`
}

type groupMember struct {
	Key    string    `json:"key"`
	Raised time.Time `json:"raised,omitempty"`
}

// notifyMux held.
func (a *alarmCache) groupOf(dest notifyDest, key string) (string, *notifyGroup) {
	for id, group := range a.Groups {
		if group.Destination != dest {
			continue
		}
		for _, member := range group.Members {
			if member.Key == key {
				return id, group
			}
		}
	}
	return "", nil
}

// notifyMux held.
func (a *alarmCache) forgetMember(dest notifyDest, key string) {
	for id, group := range a.Groups {
		if group.Destination != dest {
			continue
		}
		kept := make([]groupMember, 0, len(group.Members))
		for _, member := range group.Members {
			if member.Key != key {
				kept = append(kept, member)
			}
		}
		switch {
		case len(kept) == len(group.Members):
		case len(kept) == 0:
			delete(a.Groups, id)
		default:
			group.Members = kept
		}
	}
}

func (a *alarmCache) pruneGroups() {
	for id, group := range a.Groups {
		live := false
		for _, member := range group.Members {
			_, message, _ := strings.Cut(member.Key, "\x00")
			live = live || a.sentTo(group.Destination, member.Key, message)
		}
		if !live {
			delete(a.Groups, id)
		}
	}
}

type groupCard struct {
	Server  string
	Members []groupLine
	Down    string
	Lasted  time.Duration
}

type groupLine struct{ Chain, URL string }

func (g *groupCard) heading() string {
	if address := td.settings().AlertNodeAddress; address != nil && !*address {
		if len(g.Members) > 0 {
			member := g.Members[0]
			return "Server " + td.nodeDisplay(member.Chain, member.URL)
		}
		return "Server"
	}
	return "Server " + g.Server
}

func (g *groupCard) summary(msg *alertMsg) string {
	if msg.resolved {
		return g.heading() + ": all nodes back up"
	}
	body := fmt.Sprintf("%s: %s down", g.heading(), plural(len(g.Members), "node"))
	for _, member := range g.Members {
		if detail := ownNodeDownDetail(member.Chain, member.URL); detail != "" {
			body += "\n" + td.nodeDisplay(member.Chain, member.URL) + " · " + detail
		}
	}
	return body
}

func (g *groupCard) card(msg *alertMsg) alertCard {
	c := alertCard{Extra: msg.extra}
	if msg.resolved {
		c.Icon, c.State = "✅", "Back up"
	} else {
		c.Icon, _ = msg.header()
		c.State = plural(len(g.Members), "node") + " down"
	}
	var fields []alertField
	for i, member := range g.Members {
		if i == groupListLimit {
			fields = append(fields, row("🔌", "More", plural(len(g.Members)-groupListLimit, "node")))
			break
		}
		value := td.nodeDisplay(member.Chain, member.URL)
		if !msg.resolved {
			if detail := ownNodeDownDetail(member.Chain, member.URL); detail != "" {
				value += " · " + detail
			}
		}
		fields = append(fields, codeField("🔌", member.Chain, value))
	}
	if msg.resolved {
		fields = append(fields, row("📡", "Status", dotGreen+" all nodes back up"))
		if g.Lasted > 0 {
			fields = append(fields, row("⏱", "Lasted", shortDuration(g.Lasted)))
		}
	} else {
		fields = append(fields, row("📡", "Status", dotRed+" down for > "+g.Down))
	}
	for _, f := range fields {
		if f.Value = clipRunes(strings.TrimSpace(f.Value), cardValueLimit); f.Value != "" {
			c.Fields = append(c.Fields, f)
		}
	}
	return c
}

func sortLines(lines []groupLine) {
	sort.Slice(lines, func(i, j int) bool {
		if lines[i].Chain != lines[j].Chain {
			return lines[i].Chain < lines[j].Chain
		}
		return lines[i].URL < lines[j].URL
	})
}

func sharedExtra(extras []string) string {
	for _, extra := range extras {
		if extra != extras[0] {
			return ""
		}
	}
	if len(extras) == 0 {
		return ""
	}
	return extras[0]
}

// deliverGrouped reports false when the notification goes out as usual.
func (c *Config) deliverGrouped(item queuedNotification, msg *alertMsg) (bool, error) {
	if !chatDest(item.Destination) || !destOn(msg, item.Destination) {
		return false, nil
	}
	if item.Resolved {
		return c.groupRecovery(item, msg)
	}
	return c.groupRaise(item, msg)
}

type groupSibling struct {
	item queuedNotification
	msg  *alertMsg
	url  string
}

func (c *Config) groupRaise(item queuedNotification, msg *alertMsg) (bool, error) {
	on, _ := c.groupSettings()
	nodeURL, down, ok := nodeDownParts(item.Message)
	if !on || !ok {
		return false, nil
	}
	dest, ownKey := item.Destination, item.key()
	identity := destIdentity(msg, dest)
	// resolve DNS outside notifyMux and recheck candidates under the lock
	server := c.nodeServer(item.Chain, nodeURL)

	alarms.notifyMux.RLock()
	var waiting []queuedNotification
	seen := map[string]bool{item.workerKey(): true}
	for _, other := range alarms.Outbox {
		if other.Destination != dest || other.Resolved || seen[other.workerKey()] {
			continue
		}
		if _, _, ok := nodeDownParts(other.Message); !ok {
			continue
		}
		seen[other.workerKey()] = true
		if head, exists, skip := alarms.headOf(other.workerKey()); exists && !skip && !head.Resolved && !alarms.inFlight[dest][head.key()] && !alarms.sent(head) {
			waiting = append(waiting, head)
		}
	}
	alarms.notifyMux.RUnlock()

	var candidates []groupSibling
	for _, other := range waiting {
		otherURL, _, _ := nodeDownParts(other.Message)
		m := c.queuedAlert(other)
		if m == nil || !destOn(m, dest) || destIdentity(m, dest) != identity || c.nodeServer(other.Chain, otherURL) != server {
			continue
		}
		candidates = append(candidates, groupSibling{item: other, msg: m, url: otherURL})
	}
	if len(candidates) == 0 {
		return false, nil
	}

	free := func(it queuedNotification) bool {
		head, exists, skip := alarms.headOf(it.workerKey())
		return exists && !skip && head.Sequence == it.Sequence && !alarms.inFlight[dest][it.key()] && !alarms.sent(it)
	}
	alarms.notifyMux.Lock()
	if alarms.inFlight[dest][ownKey] {
		alarms.notifyMux.Unlock()
		return true, errNotificationBusy
	}
	if !free(item) {
		alarms.notifyMux.Unlock()
		return false, nil
	}
	var siblings []groupSibling
	for _, candidate := range candidates {
		if free(candidate.item) {
			siblings = append(siblings, candidate)
		}
	}
	if len(siblings) == 0 {
		alarms.notifyMux.Unlock()
		return false, nil
	}
	raisedAt := func(it queuedNotification) groupMember {
		raised := alarms.AllAlarms[it.Chain][it.Message]
		if raised.IsZero() {
			raised = it.Queued
		}
		return groupMember{Key: it.key(), Raised: raised}
	}
	members := []groupMember{raisedAt(item)}
	alarms.claim(dest, ownKey)
	for _, sibling := range siblings {
		alarms.claim(dest, sibling.item.key())
		members = append(members, raisedAt(sibling.item))
	}
	alarms.notifyMux.Unlock()

	for _, queued := range append([]uint64{item.Sequence}, siblingSequences(siblings)...) {
		if !c.queueSaved(queued) {
			if err := c.persistState(); err != nil {
				l("⚠️ could not save notification queue, sending anyway", err)
			}
			break
		}
	}

	lines := []groupLine{{Chain: item.Chain, URL: nodeURL}}
	all := []*alertMsg{msg}
	extras := []string{msg.extra}
	for _, sibling := range siblings {
		lines = append(lines, groupLine{Chain: sibling.item.Chain, URL: sibling.url})
		all = append(all, sibling.msg)
		extras = append(extras, sibling.msg.extra)
	}
	sortLines(lines)
	grouped := *msg
	grouped.group = &groupCard{Server: server, Members: lines, Down: down}
	grouped.extra, grouped.quiet = sharedExtra(extras), false
	err := postGrouped(&grouped, dest)
	for _, sent := range all {
		completeNotify(sent, dest, err)
	}
	if err != nil {
		l(item.Chain, "grouped notification delivery failed:", err)
		return true, err
	}
	alarms.notifyMux.Lock()
	if alarms.Groups == nil {
		alarms.Groups = make(map[string]*notifyGroup)
	}
	alarms.Groups[fmt.Sprint(item.Sequence)] = &notifyGroup{Destination: dest, Server: server, Members: members}
	alarms.notifyMux.Unlock()
	for _, sibling := range siblings {
		if err := c.acknowledge(sibling.item.Sequence); err != nil {
			l("could not save notification acknowledgement:", err)
		}
	}
	return true, nil
}

func siblingSequences(siblings []groupSibling) []uint64 {
	sequences := make([]uint64, 0, len(siblings))
	for _, sibling := range siblings {
		sequences = append(sequences, sibling.item.Sequence)
	}
	return sequences
}

// groupRecovery sends when the last node recovers.
func (c *Config) groupRecovery(item queuedNotification, msg *alertMsg) (bool, error) {
	dest, ownKey := item.Destination, item.key()
	alarms.notifyMux.Lock()
	id, group := alarms.groupOf(dest, ownKey)
	if group == nil || !alarms.sent(item) {
		alarms.notifyMux.Unlock()
		return false, nil
	}
	if len(group.Members) < 2 {
		delete(alarms.Groups, id)
		alarms.notifyMux.Unlock()
		return false, nil
	}
	if alarms.inFlight[dest][ownKey] {
		alarms.notifyMux.Unlock()
		return true, errNotificationBusy
	}
	remaining := false
	var others []queuedNotification
	for _, member := range group.Members {
		if member.Key == ownKey {
			continue
		}
		var recovery *queuedNotification
		for i := range alarms.Outbox {
			if candidate := alarms.Outbox[i]; candidate.Destination == dest && candidate.Resolved && candidate.key() == member.Key {
				recovery = &candidate
				break
			}
		}
		if recovery == nil {
			_, message, _ := strings.Cut(member.Key, "\x00")
			remaining = remaining || alarms.sentTo(dest, member.Key, message)
			continue
		}
		if head, exists, skip := alarms.headOf(recovery.workerKey()); exists && !skip && head.Sequence == recovery.Sequence && !alarms.inFlight[dest][member.Key] {
			others = append(others, head)
		}
	}
	if remaining {
		alarms.notifyMux.Unlock()
		msg.quiet = true
		return false, nil
	}
	alarms.claim(dest, ownKey)
	for _, other := range others {
		alarms.claim(dest, other.key())
	}
	server := group.Server
	members := append([]groupMember(nil), group.Members...)
	alarms.notifyMux.Unlock()

	var earliest time.Time
	lines := make([]groupLine, 0, len(members))
	extras := make([]string, 0, len(members))
	for _, member := range members {
		chain, message, _ := strings.Cut(member.Key, "\x00")
		nodeURL, _, _ := nodeDownParts(message)
		lines = append(lines, groupLine{Chain: chain, URL: nodeURL})
		if !member.Raised.IsZero() && (earliest.IsZero() || member.Raised.Before(earliest)) {
			earliest = member.Raised
		}
		c.chainsMux.RLock()
		cc := c.Chains[chain]
		c.chainsMux.RUnlock()
		if cc != nil {
			extras = append(extras, cc.extraInfo())
		} else {
			extras = append(extras, "")
		}
	}
	sortLines(lines)
	lasted := item.Lasted
	if !earliest.IsZero() {
		lasted = max(time.Since(earliest), 0)
	}
	grouped := *msg
	grouped.group = &groupCard{Server: server, Members: lines, Lasted: lasted}
	grouped.extra, grouped.quiet, grouped.lasted = sharedExtra(extras), false, lasted
	err := postGrouped(&grouped, dest)
	completeNotify(msg, dest, err)
	for _, other := range others {
		completeNotify(&alertMsg{chain: other.Chain, message: other.Message, resolved: true}, dest, err)
	}
	if err != nil {
		l(item.Chain, "grouped notification delivery failed:", err)
		return true, err
	}
	alarms.notifyMux.Lock()
	delete(alarms.Groups, id)
	alarms.notifyMux.Unlock()
	for _, other := range others {
		if err := c.acknowledge(other.Sequence); err != nil {
			l("could not save notification acknowledgement:", err)
		}
	}
	return true, nil
}
