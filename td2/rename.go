package tenderduty

import (
	"strings"
	"sync"
)

func moveChainKey[V any](values map[string]V, old, next string) {
	if value, ok := values[old]; ok {
		values[next] = value
		delete(values, old)
	}
}

func renamePrefix(key, old, next, separator string) string {
	if strings.HasPrefix(key, old+separator) {
		return next + strings.TrimPrefix(key, old)
	}
	return key
}

func moveChainPrefix[V any](values map[string]V, old, next string) {
	for key, value := range values {
		renamed := renamePrefix(key, old, next, "\x00")
		if renamed != key {
			values[renamed] = value
			delete(values, key)
		}
	}
}

func moveChainSync(values *sync.Map, old, next, separator string) {
	values.Range(func(key, value interface{}) bool {
		text, ok := key.(string)
		if !ok {
			return true
		}
		renamed := renamePrefix(text, old, next, separator)
		if renamed != text {
			values.Store(renamed, value)
			values.Delete(key)
		}
		return true
	})
}

// Move name-keyed history after the old chain's workers have stopped.
func (c *Config) renameChainState(old, next string) {
	alarms.notifyMux.Lock()
	moveChainKey(alarms.AllAlarms, old, next)
	moveChainKey(alarms.flappingAlarms, old, next)
	moveChainKey(alarms.GovSeen, old, next)
	moveChainPrefix(alarms.SilentAlarms, old, next)
	moveChainPrefix(alarms.AlarmSeverities, old, next)
	moveChainPrefix(alarms.AlarmIDs, old, next)
	moveChainPrefix(alarms.PendingRecoveries, old, next)
	moveChainPrefix(alarms.SentPdAlarms, old, next)
	moveChainPrefix(alarms.SentTgAlarms, old, next)
	moveChainPrefix(alarms.SentDiAlarms, old, next)
	moveChainPrefix(alarms.SentSlkAlarms, old, next)
	moveChainPrefix(alarms.SentGtAlarms, old, next)
	moveChainPrefix(alarms.Escalated, old, next)
	moveChainPrefix(alarms.EscalationsSent, old, next)
	for _, recovery := range alarms.PendingRecoveries {
		if recovery.Chain == old {
			recovery.Chain = next
		}
	}
	for _, group := range alarms.Groups {
		for i := range group.Members {
			group.Members[i].Key = renamePrefix(group.Members[i].Key, old, next, "\x00")
		}
	}
	for i := range alarms.Outbox {
		item := &alarms.Outbox[i]
		if item.Chain == old {
			item.Chain = next
			if item.snapshot != nil {
				copy := *item.snapshot
				copy.chain = next
				item.snapshot = &copy
			}
		}
	}
	alarms.notifyMux.Unlock()
	c.reportsMux.Lock()
	moveChainKey(c.reportData.Hours, old, next)
	moveChainKey(c.reportData.Snapshots, old, next)
	moveChainKey(c.reportData.Last, old, next)
	moveChainKey(c.reportData.OpenVotes, old, next)
	for i := range c.reportData.Incidents {
		incident := &c.reportData.Incidents[i]
		if incident.Chain == old {
			incident.Chain = next
			incident.Key = renamePrefix(incident.Key, old, next, "\x00")
		}
	}
	c.reportsMux.Unlock()
	st := c.fibreData()
	st.mu.Lock()
	moveChainKey(st.backendTotals, old, next)
	moveChainKey(st.endorsing, old, next)
	moveChainKey(st.latestMissed, old, next)
	moveChainKey(st.histograms, old, next)
	moveChainKey(st.raw, old, next)
	moveChainKey(st.rawNotes, "limits:"+old, "limits:"+next)
	moveChainKey(st.rawNotes, "full:"+old, "full:"+next)
	moveChainKey(st.hours, old, next)
	moveChainKey(st.totals, old, next)
	moveChainKey(st.outcomes, old, next)
	moveChainKey(st.reach, old, next)
	for _, span := range st.short {
		moveChainKey(span, old, next)
	}
	for _, sender := range st.senders {
		if sender.chain == old {
			sender.chain = next
		}
	}
	st.mu.Unlock()
	bridge := c.bridgeData()
	bridge.mux.Lock()
	moveChainKey(bridge.tiers, old, next)
	bridge.mux.Unlock()
	c.govSummaryMux.Lock()
	for i := range c.govSummary {
		if c.govSummary[i].chain == old {
			c.govSummary[i].chain = next
		}
	}
	c.govSummaryMux.Unlock()
	moveChainSync(&resolutions, old, next, "\x00")
	moveChainSync(&govOutcomes, old, next, "#")
	moveChainSync(&upgradeETAs, old, next, "@")
	moveChainSync(&upgradeOutcomes, old, next, "@")
}

// Only unambiguous validator identities can carry history across names.
func (diff *chainDiff) pairRenames(old, next map[string]*ChainConfig) {
	diff.Renamed = make(map[string]string)
	for _, removed := range diff.Removed {
		before := old[removed]
		var match string
		count := 0
		for _, added := range diff.Added {
			after := next[added]
			if before.ChainId == after.ChainId && before.ValAddress == after.ValAddress {
				match = added
				count++
			}
		}
		if count != 1 {
			continue
		}
		count = 0
		for _, other := range diff.Removed {
			if old[other].ChainId == before.ChainId && old[other].ValAddress == before.ValAddress {
				count++
			}
		}
		if count == 1 {
			diff.Renamed[removed] = match
		}
	}
	removed, added := diff.Removed[:0], diff.Added[:0]
	for _, name := range diff.Removed {
		if _, paired := diff.Renamed[name]; !paired {
			removed = append(removed, name)
		}
	}
	targets := make(map[string]bool)
	for _, name := range diff.Renamed {
		targets[name] = true
	}
	for _, name := range diff.Added {
		if !targets[name] {
			added = append(added, name)
		}
	}
	diff.Removed, diff.Added = removed, added
}
