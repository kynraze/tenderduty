package tenderduty

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	_ "time/tzdata"

	query "github.com/cosmos/cosmos-sdk/types/query"
	staking "github.com/cosmos/cosmos-sdk/x/staking/types"
)

const (
	reportKeep             = 30 * 24 * time.Hour
	reportLateLimit        = 6 * time.Hour
	reportYellowSigned     = 99.5
	reportDefaultRedSigned = 95.0
	reportIncidentLimit    = 10000
)

type ReportsConfig struct {
	Daily           string `yaml:"daily"`
	Weekly          string `yaml:"weekly"`
	Timezone        string `yaml:"timezone"`
	IncludeTestnets *bool  `yaml:"include_testnets"`
}

func (r ReportsConfig) location(fallback string) (*time.Location, error) {
	zone := r.Timezone
	if zone == "" {
		zone = fallback
	}
	if zone == "" {
		zone = "UTC"
	}
	return loadTimezone(zone)
}

func reportClock(value string, weekly bool) (time.Weekday, int, int, error) {
	day := time.Sunday
	if weekly {
		parts := strings.Fields(strings.ToLower(value))
		if len(parts) != 2 {
			return day, 0, 0, fmt.Errorf("reports.weekly must be weekday HH:MM")
		}
		found := false
		for d := time.Sunday; d <= time.Saturday; d++ {
			if parts[0] == strings.ToLower(d.String()) {
				day, found = d, true
			}
		}
		if !found {
			return day, 0, 0, fmt.Errorf("reports.weekly has an unknown weekday")
		}
		value = parts[1]
	}
	t, err := time.Parse("15:04", value)
	if err != nil {
		return day, 0, 0, fmt.Errorf("reports time %q must be HH:MM", value)
	}
	return day, t.Hour(), t.Minute(), nil
}

func (r ReportsConfig) validate(fallback string) error {
	if _, err := r.location(fallback); err != nil {
		return fmt.Errorf("reports: %w", err)
	}
	for _, schedule := range []struct {
		value  string
		weekly bool
	}{{r.Daily, false}, {r.Weekly, true}} {
		if schedule.value != "" {
			if _, _, _, err := reportClock(schedule.value, schedule.weekly); err != nil {
				return err
			}
		}
	}
	return nil
}

// Latest local occurrence, yesterday's included across midnight.
func reportDue(now, last time.Time, schedule string, weekly bool, loc *time.Location) (time.Time, bool, bool) {
	if schedule == "" {
		return time.Time{}, false, false
	}
	day, hour, minute, err := reportClock(schedule, weekly)
	if err != nil {
		return time.Time{}, false, false
	}
	local := now.In(loc)
	date := time.Date(local.Year(), local.Month(), local.Day(), 12, 0, 0, 0, loc)
	for i := 0; i < 8; i++ {
		candidate := time.Date(date.Year(), date.Month(), date.Day(), hour, minute, 0, 0, loc)
		if (!weekly || date.Weekday() == day) && !candidate.After(now) {
			if !candidate.After(last) {
				return candidate, false, false
			}
			age := now.Sub(candidate)
			return candidate, age <= reportLateLimit, age >= time.Minute
		}
		date = date.AddDate(0, 0, -1)
	}
	return time.Time{}, false, false
}

type reportHour struct {
	At          time.Time `json:"at"`
	Signed      int64     `json:"signed,omitempty"`
	Missed      int64     `json:"missed,omitempty"`
	Proposed    int64     `json:"proposed,omitempty"`
	MissedProps int64     `json:"missed_props,omitempty"`
	Watched     float64   `json:"watched,omitempty"`
	Votes       int       `json:"votes,omitempty"`
}

type reportSnapshot struct {
	At         time.Time `json:"at"`
	Power      *int64    `json:"power,omitempty"`
	Total      *int64    `json:"total,omitempty"`
	Rank       *int      `json:"rank,omitempty"`
	ActiveSet  *int      `json:"active_set,omitempty"`
	Self       *float64  `json:"self,omitempty"`
	Delegated  *float64  `json:"delegated,omitempty"`
	Delegators *uint64   `json:"delegators,omitempty"`
}

type reportIncident struct {
	Chain string    `json:"chain"`
	Key   string    `json:"key"`
	Kind  string    `json:"kind"`
	Start time.Time `json:"start"`
	End   time.Time `json:"end,omitempty"`
}

type reportHistory struct {
	Hours     map[string][]reportHour     `json:"hours,omitempty"`
	Snapshots map[string][]reportSnapshot `json:"snapshots,omitempty"`
	Incidents []reportIncident            `json:"incidents,omitempty"`
	Last      map[string]time.Time        `json:"last,omitempty"`
	OpenVotes map[string][]govProposal    `json:"open_votes,omitempty"`
	Pending   []reportJob                 `json:"pending,omitempty"`
}

var reportNetworks sync.Map

func (cc *ChainConfig) reportTestnet() *bool {
	cc.stateMux.RLock()
	defer cc.stateMux.RUnlock()
	return cc.Testnet
}

func (cc *ChainConfig) reportIsTestnet() bool {
	if override := cc.reportTestnet(); override != nil {
		return *override
	}
	known, _ := reportNetworks.Load(cc.ChainId)
	return known == true
}

func (c *Config) reportHourLocked(chain string, now time.Time) *reportHour {
	if c.reportData.Hours == nil {
		c.reportData.Hours = make(map[string][]reportHour)
	}
	at := now.UTC().Truncate(time.Hour)
	hours := c.reportData.Hours[chain]
	i := sort.Search(len(hours), func(i int) bool { return !hours[i].At.Before(at) })
	if i == len(hours) || !hours[i].At.Equal(at) {
		hours = append(hours, reportHour{})
		copy(hours[i+1:], hours[i:])
		hours[i] = reportHour{At: at}
		c.reportData.Hours[chain] = hours
	}
	return &hours[i]
}

func (c *Config) recordReportBlock(chain string, now time.Time, status StatusType, missedProposal bool) {
	c.reportsMux.Lock()
	defer c.reportsMux.Unlock()
	h := c.reportHourLocked(chain, now)
	switch status {
	case Statusmissed, StatusPrevote, StatusPrecommit:
		h.Missed++
	case StatusSigned, StatusProposed, StatusMissedProposal:
		h.Signed++
	}
	if status == StatusProposed {
		h.Proposed++
	}
	if missedProposal {
		h.MissedProps++
	}
}

// stateMux held. Silence beyond a minute and downtime are not counted.
func (cc *ChainConfig) reportWatchingLocked(now time.Time) {
	from := cc.reportWatchAt
	if now.Before(from) {
		return
	}
	cc.reportWatchAt = now
	if from.IsZero() || !cc.monitoring || !now.After(from) {
		return
	}
	end := now
	if stale := cc.monitoringSince.Add(time.Minute); end.After(stale) {
		end = stale
	}
	if !end.After(from) {
		return
	}
	td.reportsMux.Lock()
	defer td.reportsMux.Unlock()
	for from.Before(end) {
		next := from.Truncate(time.Hour).Add(time.Hour)
		if next.After(end) {
			next = end
		}
		td.reportHourLocked(cc.name, from).Watched += next.Sub(from).Seconds()
		from = next
	}
}

func reportAlarmKind(chain, message string) string {
	first, _, _ := strings.Cut(message, "\n")
	switch {
	case cardMissed.MatchString(first), percentageMissed.MatchString(first):
		return "Missed blocks"
	case cardNodeDown.MatchString(first):
		return "RPC node down"
	case cardStalled.MatchString(first):
		return "Chain stalled"
	case noRpcNodes.MatchString(first):
		return "RPC endpoints down"
	case proposalsMissed.MatchString(first):
		return "Missed proposals"
	case jailRisk.MatchString(first), cardJailSoon.MatchString(first):
		return "Jail risk"
	}
	_, kind := (&alertMsg{chain: chain, message: message, severity: "warning"}).header()
	return kind
}

func (c *Config) recordReportAlarm(chain, key string, start, end time.Time) {
	c.reportsMux.Lock()
	defer c.reportsMux.Unlock()
	for i := len(c.reportData.Incidents) - 1; i >= 0; i-- {
		event := &c.reportData.Incidents[i]
		if event.Chain == chain && event.Key == key && event.End.IsZero() {
			event.End = end
			return
		}
	}
	c.reportData.Incidents = append(c.reportData.Incidents, reportIncident{Chain: chain, Key: key, Kind: reportAlarmKind(chain, key), Start: start, End: end})
	c.trimReportsLocked(time.Now())
}

func (c *Config) recordReportVotes(chain string, previous, current []govProposal, now time.Time) {
	c.reportsMux.Lock()
	defer c.reportsMux.Unlock()
	if remembered := c.reportData.OpenVotes[chain]; len(remembered) > 0 {
		previous = remembered
	}
	unvoted := make(map[uint64]bool)
	for _, p := range previous {
		unvoted[p.ID] = !p.Voted
	}
	if c.reportData.OpenVotes == nil {
		c.reportData.OpenVotes = make(map[string][]govProposal)
	}
	c.reportData.OpenVotes[chain] = append([]govProposal(nil), current...)
	for _, p := range current {
		if p.Voted && unvoted[p.ID] {
			c.reportHourLocked(chain, now).Votes++
		}
	}
}

func (c *Config) trimReportsLocked(now time.Time) {
	cutoff := now.Add(-reportKeep)
	for chain, hours := range c.reportData.Hours {
		kept := hours[:0]
		for _, h := range hours {
			if !h.At.Before(cutoff) && !h.At.After(now) && h.Watched >= 0 && h.Watched <= 3600 && h.Signed >= 0 && h.Missed >= 0 {
				kept = append(kept, h)
			}
		}
		c.reportData.Hours[chain] = kept
	}
	for chain, snapshots := range c.reportData.Snapshots {
		kept := snapshots[:0]
		for _, s := range snapshots {
			if !s.At.Before(cutoff) && !s.At.After(now) {
				kept = append(kept, s)
			}
		}
		if len(kept) > 1500 {
			kept = kept[len(kept)-1500:]
		}
		c.reportData.Snapshots[chain] = kept
	}
	kept := c.reportData.Incidents[:0]
	for _, incident := range c.reportData.Incidents {
		if !incident.Start.After(now) && (incident.End.IsZero() || !incident.End.Before(cutoff)) {
			kept = append(kept, incident)
		}
	}
	if len(kept) > reportIncidentLimit {
		kept = kept[len(kept)-reportIncidentLimit:]
	}
	c.reportData.Incidents = kept
	for chain, proposals := range c.reportData.OpenVotes {
		kept := proposals[:0]
		for _, p := range proposals {
			if p.VotingEnd.After(cutoff) {
				kept = append(kept, p)
			}
		}
		c.reportData.OpenVotes[chain] = kept
	}
	pending := c.reportData.Pending[:0]
	for _, job := range c.reportData.Pending {
		if !job.At.Before(cutoff) {
			pending = append(pending, job)
		}
	}
	c.reportData.Pending = pending
}

func (c *Config) savedReports(now time.Time) *reportHistory {
	c.reportsMux.Lock()
	defer c.reportsMux.Unlock()
	c.trimReportsLocked(now)
	// Copy before saveState releases the report lock.
	data, _ := json.Marshal(c.reportData)
	copy := &reportHistory{}
	_ = json.Unmarshal(data, copy)
	return copy
}

func (c *Config) restoreReports(history *reportHistory, now time.Time) {
	if history == nil {
		return
	}
	c.reportsMux.Lock()
	defer c.reportsMux.Unlock()
	c.reportData = *history
	c.trimReportsLocked(now)
}

func reportTotals(hours []reportHour, start, end time.Time) reportHour {
	total := reportHour{}
	for _, h := range hours {
		if h.At.Before(start.Truncate(time.Hour)) || !h.At.Before(end) {
			continue
		}
		total.Signed += h.Signed
		total.Missed += h.Missed
		total.Proposed += h.Proposed
		total.MissedProps += h.MissedProps
		total.Watched += h.Watched
		total.Votes += h.Votes
	}
	total.Watched = math.Min(total.Watched, end.Sub(start).Seconds())
	return total
}

func reportSnapshotAt(snapshots []reportSnapshot, at time.Time) *reportSnapshot {
	var best *reportSnapshot
	for i := range snapshots {
		s := &snapshots[i]
		if !s.At.After(at) && (best == nil || s.At.After(best.At)) {
			best = s
		}
	}
	if best != nil && at.Sub(best.At) > 2*time.Hour {
		return nil
	}
	return best
}

func reportSigned(h reportHour) (float64, bool) {
	if h.Signed+h.Missed == 0 {
		return 0, false
	}
	return 100 * float64(h.Signed) / float64(h.Signed+h.Missed), true
}

func (cc *ChainConfig) reportStake(now time.Time) reportSnapshot {
	s := reportSnapshot{At: now}
	info, _ := cc.validatorState()
	client := cc.clientSnapshot()
	if client == nil {
		return s
	}
	ctx, cancel := context.WithTimeout(cc.context(), 15*time.Second)
	defer cancel()
	if power, err := getVotingPower(ctx, client.Remote(), info.Conspub); err == nil && len(info.Conspub) > 0 {
		s.Power, s.Total, s.Rank, s.ActiveSet = &power.power, &power.total, &power.rank, &power.size
	}
	// Count only: one delegation plus the total count.
	request := staking.QueryValidatorDelegationsRequest{ValidatorAddr: cc.ValAddress, Pagination: &query.PageRequest{Limit: 1, CountTotal: true}}
	if b, err := request.Marshal(); err == nil {
		if response, err := client.ABCIQuery(ctx, "/cosmos.staking.v1beta1.Query/ValidatorDelegations", b); err == nil && response != nil && response.Response.Code == 0 {
			s.Delegators, _ = parseReportDelegators(response.Response.Value)
		}
	}
	account := voterAddress(cc.ValAddress)
	if account != "" {
		request := staking.QueryDelegationRequest{DelegatorAddr: account, ValidatorAddr: cc.ValAddress}
		if b, err := request.Marshal(); err == nil {
			if response, err := client.ABCIQuery(ctx, "/cosmos.staking.v1beta1.Query/Delegation", b); err == nil && response != nil && response.Response.Code == 0 {
				s.Self, _ = parseReportSelf(response.Response.Value)
			}
		}
	}
	requestVal := staking.QueryValidatorRequest{ValidatorAddr: cc.ValAddress}
	if b, err := requestVal.Marshal(); err == nil {
		if response, err := client.ABCIQuery(ctx, "/cosmos.staking.v1beta1.Query/Validator", b); err == nil && response != nil && response.Response.Code == 0 {
			val := staking.QueryValidatorResponse{}
			if val.Unmarshal(response.Response.Value) == nil && !val.Validator.Tokens.IsNil() && s.Self != nil {
				tokens, err := strconv.ParseFloat(val.Validator.Tokens.String(), 64)
				// delegation is in base units, power in consensus power; scale by tokens per power
				if err == nil && tokens > 0 && !math.IsInf(tokens, 0) && s.Power != nil && *s.Power > 0 {
					scale := float64(*s.Power) / tokens
					self, delegated := *s.Self*scale, (tokens-*s.Self)*scale
					s.Self = &self
					if delegated >= 0 {
						s.Delegated = &delegated
					}
				} else {
					s.Self = nil
				}
			}
		}
	}
	return s
}

func (c *Config) collectReportSnapshots(now time.Time, force bool) {
	c.chainsMux.RLock()
	chains := make(map[string]*ChainConfig, len(c.Chains))
	for name, cc := range c.Chains {
		chains[name] = cc
	}
	c.chainsMux.RUnlock()
	for name, cc := range chains {
		cc.stateMux.Lock()
		cc.reportWatchingLocked(now)
		cc.stateMux.Unlock()
		c.reportsMux.Lock()
		snapshots := c.reportData.Snapshots[name]
		due := force || len(snapshots) == 0 || now.Sub(snapshots[len(snapshots)-1].At) >= time.Hour
		c.reportsMux.Unlock()
		if !due {
			continue
		}
		if cc.reportTestnet() == nil {
			if _, known := reportNetworks.Load(cc.ChainId); !known {
				ctx, cancel := context.WithTimeout(c.context(), 10*time.Second)
				_, _ = fetchRegistryChain(ctx, cc.ChainId)
				cancel()
			}
		}
		snapshot := cc.reportStake(now)
		c.reportsMux.Lock()
		if c.reportData.Snapshots == nil {
			c.reportData.Snapshots = make(map[string][]reportSnapshot)
		}
		c.reportData.Snapshots[name] = append(c.reportData.Snapshots[name], snapshot)
		c.trimReportsLocked(now)
		c.reportsMux.Unlock()
	}
}

func sortedReportNames(chains map[string]*ChainConfig) []string {
	names := make([]string, 0, len(chains))
	for name := range chains {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (c *Config) reportsEnabled() bool {
	s := c.settings()
	return s.Reports.Daily != "" || s.Reports.Weekly != "" || s.Telegram.Commands
}
