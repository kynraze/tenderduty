package tenderduty

import (
	"context"
	"fmt"
	"net"
	"reflect"
	"sort"
	"strings"
	"time"

	dash "github.com/kynraze/tenderduty/v2/td2/dashboard"
)

// settings returns an immutable snapshot; runtime fields stay on c.
func (c *Config) settings() *Config {
	c.settingsMux.RLock()
	defer c.settingsMux.RUnlock()
	if c.liveSettings != nil {
		return c.liveSettings
	}
	return c
}

func (cc *ChainConfig) alertSettings() AlertConfig {
	cc.stateMux.RLock()
	defer cc.stateMux.RUnlock()
	return cc.Alerts
}

func (cc *ChainConfig) extraInfo() string {
	cc.stateMux.RLock()
	defer cc.stateMux.RUnlock()
	return cc.ExtraInfo
}

func (cc *ChainConfig) publicFallbackEnabled() bool {
	cc.stateMux.RLock()
	defer cc.stateMux.RUnlock()
	return cc.PublicFallback
}

func (cc *ChainConfig) settingsRevision() uint64 {
	cc.stateMux.RLock()
	revision := cc.settingsVersion
	cc.stateMux.RUnlock()
	td.settingsMux.RLock()
	revision += td.settingsVersion
	td.settingsMux.RUnlock()
	return revision
}

func (cc *ChainConfig) context() context.Context {
	cc.stateMux.RLock()
	ctx := cc.ctx
	cc.stateMux.RUnlock()
	if ctx != nil {
		return ctx
	}
	return td.context()
}

type chainDiff struct {
	Added, Removed, Updated, Unchanged []string
	Restarted                          map[string]string
	Renamed                            map[string]string
}

// Compare node settings, not health state or mutexes.
func restartReason(old, next *ChainConfig) string {
	var reasons []string
	if old.Bridge != next.Bridge || old.alertSettings().BridgeAlerts != next.alertSettings().BridgeAlerts {
		reasons = append(reasons, "bridge changed")
	}
	if old.ChainId != next.ChainId {
		reasons = append(reasons, "chain_id changed")
	}
	if old.ValAddress != next.ValAddress || old.ValconsOverride != next.ValconsOverride {
		reasons = append(reasons, "validator changed")
	}
	nodesChanged := len(old.Nodes) != len(next.Nodes)
	if !nodesChanged {
		for i, node := range old.Nodes {
			if node.Url != next.Nodes[i].Url || node.AlertIfDown != next.Nodes[i].AlertIfDown {
				nodesChanged = true
				break
			}
		}
	}
	if nodesChanged {
		reasons = append(reasons, "nodes changed")
	}
	return strings.Join(reasons, ", ")
}

func diffChains(old, next map[string]*ChainConfig) chainDiff {
	diff := chainDiff{Restarted: make(map[string]string)}
	for name, chain := range old {
		other := next[name]
		switch {
		case other == nil:
			diff.Removed = append(diff.Removed, name)
		case restartReason(chain, other) != "":
			diff.Restarted[name] = restartReason(chain, other)
		case !reflect.DeepEqual(chain.alertSettings(), other.alertSettings()) || chain.extraInfo() != other.extraInfo() || chain.publicFallbackEnabled() != other.publicFallbackEnabled() || !reflect.DeepEqual(chain.reportTestnet(), other.reportTestnet()):
			diff.Updated = append(diff.Updated, name)
		default:
			diff.Unchanged = append(diff.Unchanged, name)
		}
	}
	for name := range next {
		if old[name] == nil {
			diff.Added = append(diff.Added, name)
		}
	}
	for _, names := range [][]string{diff.Added, diff.Removed, diff.Updated, diff.Unchanged} {
		sort.Strings(names)
	}
	return diff
}

func keepListeners(old, next *Config) []string {
	var changed []string
	for _, key := range restartSettings {
		before, after := configField(reflect.ValueOf(old).Elem(), key), configField(reflect.ValueOf(next).Elem(), key)
		if !reflect.DeepEqual(before.Interface(), after.Interface()) {
			changed = append(changed, key)
		}
		after.Set(before)
	}
	return changed
}

func (c *Config) startChain(name string, cc *ChainConfig) {
	cc.stateMux.Lock()
	cc.ctx, cc.cancel = context.WithCancel(c.context())
	cc.stateMux.Unlock()
	start := func(work func()) {
		cc.workers.Add(1)
		c.startWorker(func() { defer cc.workers.Done(); work() })
	}
	start(cc.watch)
	start(func() { cc.lookupDashboardNetwork(cc.context()) })
	start(func() { cc.lookupDashboardLogo(cc.context()) })
	if cc.Bridge.URL != "" {
		start(func() { cc.watchBridge(cc.context()) })
	}
	start(func() { cc.monitorHealth(cc.context(), name) })
	start(func() {
		for cc.context().Err() == nil {
			if err := cc.newRpc(); err != nil {
				l(cc.ChainId, err)
			} else {
				if err := cc.GetValInfo(true); err != nil {
					l(cc.ChainId, err)
				}
				info, _ := cc.validatorState()
				if len(info.Conspub) == 20 && info.Valcons != "" {
					cc.WsRun()
				}
			}
			if !waitContext(cc.context(), 5*time.Second) {
				return
			}
		}
	})
}

func stopChain(cc *ChainConfig) {
	cc.stateMux.Lock()
	cc.retiring = true
	cancel := cc.cancel
	cc.stateMux.Unlock()
	if cancel != nil {
		cancel()
		cc.workers.Wait()
	}
}

// Resolve before the chain is removed.
func (c *Config) removeChainAlarms(name string) {
	alarms.notifyMux.RLock()
	messages := make([]string, 0, len(alarms.AllAlarms[name]))
	for message := range alarms.AllAlarms[name] {
		messages = append(messages, message)
	}
	alarms.notifyMux.RUnlock()
	for _, message := range messages {
		id := c.alarmID(name, message)
		resolve(name, message, removedResolution(message))
		c.alert(name, message, "info", true, &id)
	}
}

func (c *Config) rememberChainAlarmIDs(name string) {
	alarms.notifyMux.RLock()
	messages := make([]string, 0, len(alarms.AllAlarms[name]))
	for message := range alarms.AllAlarms[name] {
		messages = append(messages, message)
	}
	alarms.notifyMux.RUnlock()
	for _, message := range messages {
		id := c.alarmID(name, message)
		alarms.notifyMux.Lock()
		if alarms.AlarmIDs == nil {
			alarms.AlarmIDs = make(map[string]string)
		}
		if !alarms.AllAlarms[name][message].IsZero() {
			alarms.AlarmIDs[name+"\x00"+message] = id
		}
		alarms.notifyMux.Unlock()
	}
}

func carryChainHistory(old, next *ChainConfig) {
	if old.ChainId != next.ChainId || old.ValAddress != next.ValAddress {
		return
	}
	old.stateMux.RLock()
	defer old.stateMux.RUnlock()
	next.blocksResults = append([]int(nil), old.blocksResults...)
	next.lastBlockTime, next.lastBlockNum, next.observedBlock = old.lastBlockTime, old.lastBlockNum, old.observedBlock
	next.statTotalSigns, next.statTotalProps, next.statTotalMiss = old.statTotalSigns, old.statTotalProps, old.statTotalMiss
	next.statPrevoteMiss, next.statPrecommitMiss = old.statPrevoteMiss, old.statPrecommitMiss
	next.statConsecutiveMiss, next.statProposalMiss = old.statConsecutiveMiss, old.statProposalMiss
	next.proposalMisses = append([]time.Time(nil), old.proposalMisses...)
	next.signal, next.signalLoaded, next.signalUnsupported = old.signal, old.signalLoaded, old.signalUnsupported
	next.signalReadFailed, next.upgradeReadFailed, next.signalRPCFailed = old.signalReadFailed, old.upgradeReadFailed, old.signalRPCFailed
	next.signalWatch, next.signalWatchReady = old.signalWatch, old.signalWatchReady
	next.signalWatchHeight, next.signalWatchSince = old.signalWatchHeight, old.signalWatchSince
	next.signalStoppedHeight, next.signalLast = old.signalStoppedHeight, old.signalLast
	next.blockTime = old.blockTime
	next.jailedSince = old.jailedSince
	next.bondToken, next.bondTokenAt = old.bondToken, old.bondTokenAt
	next.slash = old.slash
	next.slashSearchDone = old.slashSearchDone
}

// The outbox keeps the exact PagerDuty key even if the validator changed.
func (c *Config) alarmID(name, message string) string {
	alarms.notifyMux.RLock()
	if id := alarms.AlarmIDs[name+"\x00"+message]; id != "" {
		alarms.notifyMux.RUnlock()
		return id
	}
	for _, item := range alarms.Outbox {
		if item.Chain == name && item.Message == message {
			alarms.notifyMux.RUnlock()
			return item.ID
		}
	}
	if recovery := alarms.PendingRecoveries[name+"\x00"+message]; recovery != nil {
		alarms.notifyMux.RUnlock()
		return recovery.ID
	}
	alarms.notifyMux.RUnlock()
	c.chainsMux.RLock()
	cc := c.Chains[name]
	c.chainsMux.RUnlock()
	if cc == nil {
		return ""
	}
	info, _ := cc.validatorState()
	// Older state files have no dedup keys; these match the keys watch uses.
	switch {
	case strings.HasPrefix(message, "no RPC endpoints are working"):
		return cc.ValAddress + "norpc"
	case strings.Contains(message, "RPC node ") && strings.Contains(message, " has been down for "):
		return strings.SplitN(strings.SplitN(message, "RPC node ", 2)[1], " has been down for ", 2)[0]
	case strings.Contains(message, " of the slashing window's blocks on "):
		return info.Valcons + "percent"
	case strings.Contains(message, " has missed ") && strings.Contains(message, " blocks on "):
		return info.Valcons + "consecutive"
	case strings.Contains(message, " block proposal"):
		return info.Valcons + "proposal"
	case strings.Contains(message, " is close to jail on "):
		return info.Valcons + "jail"
	case strings.Contains(message, " of the misses allowed before jailing on "):
		return info.Valcons + "jail"
	case strings.Contains(message, " if it keeps missing blocks"):
		return info.Valcons + "jailsoon"
	case strings.HasPrefix(message, "Proposal #"):
		return govAlarmId(info, message)
	case strings.HasPrefix(message, "Upgrade "):
		return upgradeAlarmId(info, message)
	case isSignalMessage(message):
		return signalAlarmId(info, message)
	case strings.HasPrefix(message, fibreEndorsementPrefix):
		return info.Valcons + "fibreendorsement"
	case isBridgeMessage(message):
		return cc.bridgeAlarmID(message)
	case isFibreMessage(message):
		return fibreAlarmId(info, message)
	case isFibreMetricsMessage(message):
		return fibreMetricsAlarmId(info, message)
	case strings.Contains(message, " is tombstoned on "):
		return info.Valcons + "tombstoned"
	case strings.Contains(message, " can unjail on "):
		return info.Valcons + "unjail"
	case strings.Contains(message, " is jailed on "), strings.Contains(message, " is no longer in the active set on "):
		return info.Valcons + "jailed"
	default:
		return info.Valcons
	}
}

func (c *Config) reload(configFile, chainDirectory string, password *string) error {
	next, err := loadConfigMode(configFile, "", chainDirectory, password, false)
	if err != nil {
		c.reloadFailed([]string{err.Error()})
		return err
	}
	defer next.cancel()
	defer next.deliveryCancel()
	if c.settings().chainDirectoryPresent && !next.chainDirectoryPresent {
		err := fmt.Errorf("chains directory %s is missing", chainDirectory)
		c.reloadFailed([]string{err.Error()})
		return err
	}
	fatal, problems := validateConfig(next)
	if fatal {
		c.reloadFailed(problems)
		return fmt.Errorf("configuration is invalid")
	}
	if err := fibreTokenKept(c.settings(), next); err != nil {
		c.reloadFailed([]string{err.Error()})
		return err
	}
	for _, problem := range problems {
		l(problem)
	}
	c.applyReload(next)
	return nil
}

// fibreTokenKept rejects token removal from a running non-loopback listener.
func fibreTokenKept(old, next *Config) error {
	if old.FibreMetrics.Listen == "" || next.FibreMetrics.Token != "" || next.FibreMetrics.AllowWithoutToken {
		return nil
	}
	host, _, _ := net.SplitHostPort(old.FibreMetrics.Listen)
	if fibreMetricsLoopback(host) {
		return nil
	}
	return fmt.Errorf("Not reloaded: otel_metrics.listen changes only on restart, and dropping the token now would leave %s open without one. Restart tenderduty to apply.", old.FibreMetrics.Listen)
}

func (c *Config) applyReload(next *Config) chainDiff {
	oldSettings := c.settings()
	restarts := keepListeners(oldSettings, next)
	applied := changedSharedSettings(oldSettings, next)
	c.chainsMux.RLock()
	old := make(map[string]*ChainConfig, len(c.Chains))
	for name, chain := range c.Chains {
		old[name] = chain
	}
	c.chainsMux.RUnlock()
	diff := diffChains(old, next.Chains)
	diff.pairRenames(old, next.Chains)
	for name, renamed := range diff.Renamed {
		stopChain(old[name])
		c.rememberChainAlarmIDs(name)
		carryChainHistory(old[name], next.Chains[renamed])
		c.renameChainState(name, renamed)
	}
	for _, name := range diff.Removed {
		stopChain(old[name])
		c.removeChainAlarms(name)
	}
	for name := range diff.Restarted {
		stopChain(old[name])
		c.rememberChainAlarmIDs(name)
		carryChainHistory(old[name], next.Chains[name])
	}
	// No worker reads a replaced chain after this point.
	c.chainsMux.Lock()
	for name, renamed := range diff.Renamed {
		delete(c.Chains, name)
		c.Chains[renamed] = next.Chains[renamed]
	}
	for _, name := range diff.Removed {
		delete(c.Chains, name)
	}
	for name := range diff.Restarted {
		c.Chains[name] = next.Chains[name]
	}
	for _, name := range diff.Added {
		c.Chains[name] = next.Chains[name]
	}
	for _, name := range diff.Updated {
		chain, other := c.Chains[name], next.Chains[name]
		chain.stateMux.Lock()
		chain.Alerts, chain.ExtraInfo, chain.PublicFallback = other.Alerts, other.ExtraInfo, other.PublicFallback
		chain.Testnet = other.Testnet
		chain.settingsVersion++
		chain.stateMux.Unlock()
	}
	// server labels do not need a restart
	for _, name := range append(append([]string(nil), diff.Updated...), diff.Unchanged...) {
		c.Chains[name].adoptServers(next.Chains[name])
	}
	c.settingsMux.Lock()
	c.liveSettings = next
	if oldSettings.NodeDownMin != next.NodeDownMin || oldSettings.NodeDownSeverity != next.NodeDownSeverity ||
		!reflect.DeepEqual(oldSettings.Pagerduty, next.Pagerduty) || !reflect.DeepEqual(oldSettings.Telegram, next.Telegram) ||
		!reflect.DeepEqual(oldSettings.Discord, next.Discord) || !reflect.DeepEqual(oldSettings.Slack, next.Slack) || !reflect.DeepEqual(oldSettings.Gotify, next.Gotify) {
		c.settingsVersion++
	}
	c.settingsMux.Unlock()
	c.chainsMux.Unlock()
	for _, name := range diff.Updated {
		cc := next.Chains[name]
		c.startWorker(func() { cc.lookupDashboardNetwork(c.context()) })
		c.startWorker(func() { cc.lookupDashboardLogo(c.context()) })
	}
	for _, name := range diff.Removed {
		c.sendUpdate(&dash.ChainStatus{MsgType: "remove", Name: name})
		c.sendStat(&promUpdate{name: name, remove: true})
		l("reloaded config: removed", name)
	}
	for name, renamed := range diff.Renamed {
		c.sendUpdate(&dash.ChainStatus{MsgType: "remove", Name: name})
		c.sendStat(&promUpdate{name: name, remove: true})
		c.sendUpdate(next.Chains[renamed].dashboardStatus())
		c.startChain(renamed, next.Chains[renamed])
		l(fmt.Sprintf("reloaded config: renamed %s to %s", name, renamed))
	}
	for name, reason := range diff.Restarted {
		info, _ := old[name].validatorState()
		next.Chains[name].clearOrphanedAlarms(info, next.Chains[name].ValAddress+"norpc")
		c.sendStat(&promUpdate{name: name, remove: true})
		c.sendUpdate(next.Chains[name].dashboardStatus())
		c.startChain(name, next.Chains[name])
		l(fmt.Sprintf("reloaded config: restarted %s (%s)", name, reason))
	}
	for _, name := range diff.Added {
		c.sendUpdate(next.Chains[name].dashboardStatus())
		c.startChain(name, next.Chains[name])
		l("reloaded config: added", name)
	}
	for _, name := range diff.Updated {
		l("reloaded config: updated settings for", name)
	}
	for _, setting := range restarts {
		l("restart tenderduty to apply", setting)
	}
	if !reflect.DeepEqual(oldSettings.Telegram, next.Telegram) {
		c.restartCommands()
	}
	c.releaseHeld()
	l(reloadSummary(diff, applied, restarts))
	return diff
}

func (c *Config) reloadFailed(problems []string) {
	for _, problem := range problems {
		l("config reload failed:", problem)
	}
	body := "Config reload failed\n" + clipRunes(strings.Join(problems, "\n"), 900)
	c.chainsMux.RLock()
	names := make([]string, 0, len(c.Chains))
	for name := range c.Chains {
		names = append(names, name)
	}
	c.chainsMux.RUnlock()
	// the whole config failed, no single chain heads it
	c.noticeByDestination(names, body, func(msg *alertMsg, _ []string) { msg.chain, msg.extra = "", "" })
}

// noticeByDestination sends an informational message once to each enabled chat destination.
// prepare can tailor each notice to the chains sharing its destination.
func (c *Config) noticeByDestination(names []string, body string, prepare func(*alertMsg, []string)) {
	if c.context().Err() != nil {
		return
	}
	type noticeDestination struct {
		msg   alertMsg
		names []string
		send  func(*alertMsg) error
	}
	seen := make(map[string]*noticeDestination)
	var destinations []*noticeDestination
	for _, name := range names {
		msg := c.makeAlert(name, body, "info", false, nil)
		if msg == nil {
			continue
		}
		// Notices go to enabled chats even if their incident severity filter is higher.
		c.chainsMux.RLock()
		chain := c.Chains[name]
		c.chainsMux.RUnlock()
		if chain == nil {
			continue
		}
		alerts, settings := chain.alertSettings(), c.settings()
		msg.tg = enabledFor(settings.Telegram.Enabled, alerts.Telegram.Enabled)
		msg.disc = enabledFor(settings.Discord.Enabled, alerts.Discord.Enabled)
		msg.slk = enabledFor(settings.Slack.Enabled, alerts.Slack.Enabled)
		msg.gotify = enabledFor(settings.Gotify.Enabled, alerts.Gotify.Enabled)
		msg.pd, msg.transient = false, true
		msg.ctx = c.deliveryCtx
		for _, destination := range []struct {
			key     string
			enabled bool
			send    func(*alertMsg) error
		}{
			{"tg:" + msg.tgKey + ":" + msg.tgChannel, msg.tg, notifyTg},
			{"di:" + msg.discHook, msg.disc, notifyDiscord},
			{"slk:" + msg.slkHook, msg.slk, notifySlack},
			{"gt:" + msg.gotifyURL, msg.gotify, notifyGotify},
		} {
			if !destination.enabled {
				continue
			}
			if previous := seen[destination.key]; previous != nil {
				previous.names = append(previous.names, name)
				continue
			}
			next := &noticeDestination{msg: *msg, names: []string{name}, send: destination.send}
			seen[destination.key] = next
			destinations = append(destinations, next)
		}
	}
	for _, destination := range destinations {
		copy, send := destination.msg, destination.send
		if prepare != nil {
			prepare(&copy, destination.names)
		}
		c.startWorker(func() {
			if err := send(&copy); err != nil {
				l("could not send notice:", err)
			}
		})
	}
}

func (c *Config) restartCommands() {
	if c.commandsCancel != nil {
		c.commandsCancel()
		<-c.commandsDone
	}
	settings := c.settings().Telegram
	if !on(settings.Enabled) || !settings.Commands {
		c.commandsCancel = nil
		return
	}
	ctx, cancel := context.WithCancel(c.context())
	c.commandsCancel, c.commandsDone = cancel, make(chan struct{})
	done := c.commandsDone
	c.startWorker(func() { defer close(done); c.telegramCommandsContext(ctx) })
}

func changedSharedSettings(old, next *Config) []string {
	var changed []string
	var walk func(reflect.Value, reflect.Value, string)
	walk = func(a, b reflect.Value, prefix string) {
		t := a.Type()
		for i := 0; i < t.NumField(); i++ {
			field := t.Field(i)
			key := strings.Split(field.Tag.Get("yaml"), ",")[0]
			if field.PkgPath != "" || key == "" || key == "-" || key == "chains" {
				continue
			}
			if field.Type.Kind() == reflect.Struct {
				walk(a.Field(i), b.Field(i), prefix+key+".")
			} else if !reflect.DeepEqual(a.Field(i).Interface(), b.Field(i).Interface()) {
				changed = append(changed, prefix+key)
			}
		}
	}
	walk(reflect.ValueOf(old).Elem(), reflect.ValueOf(next).Elem(), "")
	sort.Strings(changed)
	return changed
}

func reloadSummary(diff chainDiff, applied, restarts []string) string {
	if len(diff.Renamed)+len(applied)+len(restarts)+len(diff.Added)+len(diff.Removed)+len(diff.Restarted)+len(diff.Updated) == 0 {
		return "reloaded config: no changes"
	}
	prefix := "reloaded config: "
	if len(applied) > 0 {
		prefix += "settings applied: " + strings.Join(applied, ", ") + "; "
	}
	if len(diff.Renamed) > 0 {
		prefix += fmt.Sprintf("%d renamed, ", len(diff.Renamed))
	}
	return prefix + fmt.Sprintf("%d added, %d removed, %d restarted, %d updated, %d settings need a restart", len(diff.Added), len(diff.Removed), len(diff.Restarted), len(diff.Updated), len(restarts))
}
