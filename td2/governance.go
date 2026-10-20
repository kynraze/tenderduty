package tenderduty

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/cosmos/cosmos-sdk/types/bech32"
	dash "github.com/kynraze/tenderduty/v2/td2/dashboard"
	rpchttp "github.com/tendermint/tendermint/rpc/client/http"
	"google.golang.org/protobuf/encoding/protowire"
)

type govProposal struct {
	ID          uint64
	Title       string
	Summary     string `json:"-"`
	VotingStart time.Time
	VotingEnd   time.Time
	Voted       bool
}

// Try v1 before v1beta1, whose title is in content.
var govModules = []string{"cosmos.gov.v1", "cosmos.gov.v1beta1"}

const proposalStatusVoting = 2

// voterAddress is the validator's account: the valoper's bytes with the account prefix, empty if unknown.
func voterAddress(valoper string) string {
	prefix, bz, err := bech32.DecodeAndConvert(valoper)
	if err != nil || !strings.HasSuffix(prefix, "valoper") {
		return ""
	}
	voter, err := bech32.ConvertAndEncode(strings.TrimSuffix(prefix, "valoper"), bz)
	if err != nil {
		return ""
	}
	return voter
}

func getProposals(ctx context.Context, client *rpchttp.HTTP, voter string) ([]govProposal, error) {
	// QueryProposalsRequest{proposal_status: voting, pagination: {limit: 100}}
	request := protowire.AppendTag(nil, 1, protowire.VarintType)
	request = protowire.AppendVarint(request, proposalStatusVoting)
	request = protowire.AppendTag(request, 4, protowire.BytesType)
	request = protowire.AppendBytes(request, protowire.AppendVarint(protowire.AppendTag(nil, 3, protowire.VarintType), 100))
	var lastErr error
	for _, module := range govModules {
		resp, err := client.ABCIQuery(ctx, "/"+module+".Query/Proposals", request)
		if err != nil {
			return nil, err
		}
		if resp.Response.Code != 0 {
			lastErr = fmt.Errorf("%s proposals: %s", module, resp.Response.Log)
			continue
		}
		proposals, err := parseProposals(resp.Response.Value, module == "cosmos.gov.v1beta1")
		if err != nil {
			return nil, err
		}
		return proposals, markVoted(ctx, client, proposals, voter, module)
	}
	// e.g. Shentu: an old proposal holds a message type the node dropped, so every Proposals query fails
	raw, err := rawVotingProposals(ctx, client)
	if err != nil {
		return nil, fmt.Errorf("%w; gov store: %v", lastErr, err)
	}
	proposals, err := parseProposals(raw, false)
	if err != nil {
		return nil, err
	}
	// the store doesn't say which gov answers votes; an SDK 0.45 chain has only v1beta1
	return proposals, markVoted(ctx, client, proposals, voter, govModules...)
}

func markVoted(ctx context.Context, client *rpchttp.HTTP, proposals []govProposal, voter string, modules ...string) error {
	if voter == "" {
		return nil
	}
	for i := range proposals {
		for _, module := range modules {
			voted, err := hasVoted(ctx, client, module, proposals[i].ID, voter)
			if err != nil {
				return err
			}
			if voted {
				proposals[i].Voted = true
				break
			}
		}
	}
	return nil
}

// rawVotingProposals reads the active queue (prefix 1, key ends in the id) and each proposal (prefix 0) from the gov store,
// wrapped like a v1 Proposals response.
func rawVotingProposals(ctx context.Context, client *rpchttp.HTTP) ([]byte, error) {
	resp, err := client.ABCIQuery(ctx, "/store/gov/subspace", []byte{1})
	if err != nil {
		return nil, err
	}
	if resp.Response.Code != 0 {
		return nil, errors.New(resp.Response.Log)
	}
	var out []byte
	err = eachField(resp.Response.Value, func(num protowire.Number, typ protowire.Type, pair []byte, _ uint64) error {
		var key []byte
		if num == 1 && typ == protowire.BytesType {
			_ = eachField(pair, func(num protowire.Number, typ protowire.Type, value []byte, _ uint64) error {
				if num == 1 && typ == protowire.BytesType {
					key = value
				}
				return nil
			})
		}
		if len(key) < 9 {
			return nil
		}
		r, err := client.ABCIQuery(ctx, "/store/gov/key", append([]byte{0}, key[len(key)-8:]...))
		if err != nil {
			return err
		}
		if r.Response.Code == 0 && len(r.Response.Value) > 0 {
			out = protowire.AppendBytes(protowire.AppendTag(out, 1, protowire.BytesType), r.Response.Value)
		}
		return nil
	})
	return out, err
}

// hasVoted: a chain answers with an error when there is no vote.
func hasVoted(ctx context.Context, client *rpchttp.HTTP, module string, id uint64, voter string) (bool, error) {
	request := protowire.AppendVarint(protowire.AppendTag(nil, 1, protowire.VarintType), id)
	request = protowire.AppendString(protowire.AppendTag(request, 2, protowire.BytesType), voter)
	resp, err := client.ABCIQuery(ctx, "/"+module+".Query/Vote", request)
	if err != nil {
		return false, err
	}
	return resp.Response.Code == 0 && len(resp.Response.Value) > 0, nil
}

// parseProposals reads fields by number to support custom proposal messages.
func parseProposals(b []byte, legacy bool) ([]govProposal, error) {
	proposals := make([]govProposal, 0)
	err := eachField(b, func(num protowire.Number, typ protowire.Type, value []byte, _ uint64) error {
		if num != 1 || typ != protowire.BytesType {
			return nil
		}
		p := govProposal{}
		err := eachField(value, func(num protowire.Number, typ protowire.Type, value []byte, varint uint64) error {
			switch {
			case num == 1 && typ == protowire.VarintType:
				p.ID = varint
			case num == 2 && typ == protowire.BytesType && legacy:
				// content Any, legacy proposal with the title first
				return eachField(value, func(num protowire.Number, typ protowire.Type, value []byte, _ uint64) error {
					if num == 2 && typ == protowire.BytesType {
						p.Title = firstString(value)
						return eachField(value, func(num protowire.Number, typ protowire.Type, value []byte, _ uint64) error {
							if num == 2 && typ == protowire.BytesType {
								p.Summary = clipRunes(string(value), 10000)
							}
							return nil
						})
					}
					return nil
				})
			case num == 8 && typ == protowire.BytesType:
				p.VotingStart = parseTimestamp(value)
			case num == 9 && typ == protowire.BytesType:
				p.VotingEnd = parseTimestamp(value)
			case num == 11 && typ == protowire.BytesType && !legacy:
				p.Title = string(value)
			case num == 12 && typ == protowire.BytesType && !legacy:
				p.Summary = clipRunes(string(value), 10000)
			}
			return nil
		})
		if err != nil {
			return err
		}
		proposals = append(proposals, p)
		return nil
	})
	return proposals, err
}

// eachField calls fn per field with its bytes value or varint.
func eachField(b []byte, fn func(num protowire.Number, typ protowire.Type, value []byte, varint uint64) error) error {
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return protowire.ParseError(n)
		}
		b = b[n:]
		var value []byte
		var varint uint64
		switch typ {
		case protowire.VarintType:
			varint, n = protowire.ConsumeVarint(b)
		case protowire.BytesType:
			value, n = protowire.ConsumeBytes(b)
		default:
			n = protowire.ConsumeFieldValue(num, typ, b)
		}
		if n < 0 {
			return protowire.ParseError(n)
		}
		b = b[n:]
		if err := fn(num, typ, value, varint); err != nil {
			return err
		}
	}
	return nil
}

func firstString(b []byte) string {
	title := ""
	_ = eachField(b, func(num protowire.Number, typ protowire.Type, value []byte, _ uint64) error {
		if num == 1 && typ == protowire.BytesType && title == "" {
			title = string(value)
		}
		return nil
	})
	return title
}

func parseTimestamp(b []byte) time.Time {
	var seconds, nanos uint64
	_ = eachField(b, func(num protowire.Number, typ protowire.Type, _ []byte, varint uint64) error {
		if num == 1 {
			seconds = varint
		} else if num == 2 {
			nanos = varint
		}
		return nil
	})
	return time.Unix(int64(seconds), int64(nanos)).UTC()
}

// govEndFormat is how the end of voting is written, the time left is parsed from it when sent.
const govEndFormat = "2 Jan 2006 15:04 UTC"

// govTitle replaces controls with spaces and caps titles at 200 runes.
func govTitle(title string) string {
	title = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, title)
	return clipRunes(strings.TrimSpace(title), 200)
}

// proposal alarm keys contain only stable fields
func (cc *ChainConfig) govOpenMessage(p govProposal) string {
	title := ""
	if t := govTitle(p.Title); t != "" {
		title = " " + t
	}
	return fmt.Sprintf("Proposal #%d%s\nEnds %s", p.ID, title, p.VotingEnd.UTC().Format(govEndFormat))
}

func (cc *ChainConfig) govRemindMessage(p govProposal, moniker string) string {
	return cc.govOpenMessage(p) + "\n" + moniker + " has not voted"
}

func (cc *ChainConfig) govEarlyRemindMessage(p govProposal, moniker string) string {
	return cc.govRemindMessage(p, moniker) + " yet"
}

// Short votes keep only the final reminder, with two hours between stages.
func govEarlyReminderDue(p govProposal, now time.Time, percent int, final time.Duration) bool {
	if p.Voted || p.VotingStart.IsZero() || percent <= 0 || percent >= 100 {
		return false
	}
	period := p.VotingEnd.Sub(p.VotingStart)
	remaining := period/100*time.Duration(percent) + period%100*time.Duration(percent)/100
	return remaining-final >= 2*time.Hour && period-remaining >= 2*time.Hour &&
		!now.Before(p.VotingEnd.Add(-remaining)) && now.Before(p.VotingEnd.Add(-final))
}

// Final reminders outrank early reminders when several restored alarms clear together.
func govReminderRank(message string) int {
	if strings.HasSuffix(message, " has not voted yet") {
		return 1
	}
	if strings.HasSuffix(message, " has not voted") {
		return 2
	}
	return 0
}

var govMessageId = regexp.MustCompile(`^Proposal #(\d+)`)

// govTimeLeft adds the time left to the Ends or Can unjail after line while it is ahead.
func govTimeLeft(message string) string {
	lines := strings.Split(message, "\n")
	for i, line := range lines {
		prefix := "Ends "
		if strings.HasPrefix(line, "Can unjail after ") {
			prefix = "Can unjail after "
		}
		end, err := time.Parse(govEndFormat, strings.TrimPrefix(line, prefix))
		if !strings.HasPrefix(line, prefix) || err != nil {
			continue
		}
		if left := time.Until(end); left > 0 {
			lines[i] = line + " · " + shortDuration(left) + " left"
		}
	}
	return strings.Join(lines, "\n")
}

// shortDuration writes the two largest units.
func shortDuration(d time.Duration) string {
	minutes := int(d.Minutes())
	days, hours, minutes := minutes/(24*60), minutes/60%24, minutes%60
	switch {
	case days > 0:
		return fmt.Sprintf("%dd %dh", days, hours)
	case hours > 0:
		return fmt.Sprintf("%dh %dm", hours, minutes)
	case minutes > 0:
		return fmt.Sprintf("%dm", minutes)
	default:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
}

// govOutcomes is keyed chain#id, read when the alarm clears.
var govOutcomes sync.Map

// govResolvedLabel is the proposal's result once voting ended, else the vote.
func govResolvedLabel(chain, message string) (icon, label string) {
	if match := govMessageId.FindStringSubmatch(message); match != nil {
		if outcome, ok := govOutcomes.Load(chain + "#" + match[1]); ok {
			if outcome == "Passed" {
				return "✅", "Passed"
			}
			return "❌", outcome.(string)
		}
	}
	for _, line := range strings.Split(message, "\n") {
		if end, err := time.Parse(govEndFormat, strings.TrimPrefix(line, "Ends ")); err == nil && time.Now().After(end) {
			return "🏁", "Voting ended"
		}
	}
	return "✅", "Voted"
}

// getOutcome returns passed, rejected or failed.
func getOutcome(ctx context.Context, client *rpchttp.HTTP, id uint64) string {
	request := protowire.AppendVarint(protowire.AppendTag(nil, 1, protowire.VarintType), id)
	for _, module := range govModules {
		resp, err := client.ABCIQuery(ctx, "/"+module+".Query/Proposal", request)
		if err != nil || resp.Response.Code != 0 {
			continue
		}
		status := uint64(0)
		_ = eachField(resp.Response.Value, func(num protowire.Number, typ protowire.Type, value []byte, _ uint64) error {
			if num == 1 && typ == protowire.BytesType {
				return eachField(value, func(num protowire.Number, typ protowire.Type, _ []byte, varint uint64) error {
					if num == 3 && typ == protowire.VarintType {
						status = varint
					}
					return nil
				})
			}
			return nil
		})
		switch status {
		case 3:
			return "Passed"
		case 4:
			return "Rejected"
		case 5:
			return "Failed"
		}
	}
	return ""
}

// govAlarms keeps proposal notices and the currently due vote reminder.
func (cc *ChainConfig) govAlarms(valInfo *ValInfo, reconcile bool) {
	cc.govAlarmsAt(valInfo, reconcile, time.Now())
}

func (cc *ChainConfig) govAlarmsAt(valInfo *ValInfo, reconcile bool, now time.Time) {
	proposals, loaded := cc.govSnapshot()
	if !loaded {
		return
	}
	first := td.firstGovSight(cc.name)
	voter := voterAddress(cc.ValAddress)
	settings := cc.alertSettings()
	final := time.Duration(settings.GovRemindMinutes) * time.Minute
	wanted := make(map[string]string) // message to alert id
	priority := make(map[string]string)
	for _, p := range proposals {
		// a closed vote can stay listed until the next refresh
		if p.Voted || !p.VotingEnd.IsZero() && !p.VotingEnd.After(now) {
			continue
		}
		message := cc.govOpenMessage(p)
		if first {
			id := fmt.Sprintf("%sgov%d", valInfo.Valcons, p.ID)
			td.record(cc.name, message, settings.GovSeverity, false, false, &id, true)
			td.collectGovBacklog(cc.name, p, now)
		}
		wanted[message], priority[message] = fmt.Sprintf("%sgov%d", valInfo.Valcons, p.ID), settings.GovSeverity
		if voter != "" && p.VotingEnd.Sub(now) <= final {
			message = cc.govRemindMessage(p, valInfo.Moniker)
			wanted[message], priority[message] = fmt.Sprintf("%sgovremind%d", valInfo.Valcons, p.ID), "critical"
		} else if voter != "" && govEarlyReminderDue(p, now, settings.GovEarlyRemindPercent, final) {
			message = cc.govEarlyRemindMessage(p, valInfo.Moniker)
			wanted[message], priority[message] = fmt.Sprintf("%sgovearly%d", valInfo.Valcons, p.ID), "warning"
		}
	}
	active := activeMessages(cc.name, "Proposal #")
	for message, id := range wanted {
		if !active[message] || reconcile {
			id := id
			td.alert(cc.name, message, priority[message], false, &id)
		}
	}
	if first {
		if err := td.persistState(); err != nil {
			l("could not persist governance first sight:", err)
		}
	}
	// A new stage clears older stages quietly; otherwise the highest stage announces.
	stillOpen := make(map[string]bool)
	closing := make(map[string]string)
	for message := range wanted {
		if match := govMessageId.FindStringSubmatch(message); match != nil {
			stillOpen[match[1]] = true
		}
	}
	for message := range active {
		if match := govMessageId.FindStringSubmatch(message); match != nil {
			previous, ok := closing[match[1]]
			if !ok || govReminderRank(message) > govReminderRank(previous) ||
				(govReminderRank(message) == govReminderRank(previous) && message < previous) {
				closing[match[1]] = message
			}
		}
	}
	for message := range active {
		if _, ok := wanted[message]; !ok {
			id := govAlarmId(valInfo, message)
			if match := govMessageId.FindStringSubmatch(message); match != nil &&
				(stillOpen[match[1]] || closing[match[1]] != message) {
				td.resolveQuietly(cc.name, message, &id)
				continue
			}
			td.alert(cc.name, message, "info", true, &id)
		}
	}
}

// govAlarmId rebuilds the id from the message, for alarms no longer wanted.
func govAlarmId(valInfo *ValInfo, message string) string {
	id := uint64(0)
	if match := govMessageId.FindStringSubmatch(message); match != nil {
		id, _ = strconv.ParseUint(match[1], 10, 64)
	}
	if govReminderRank(message) == 1 {
		return fmt.Sprintf("%sgovearly%d", valInfo.Valcons, id)
	}
	if govReminderRank(message) == 2 {
		return fmt.Sprintf("%sgovremind%d", valInfo.Valcons, id)
	}
	return fmt.Sprintf("%sgov%d", valInfo.Valcons, id)
}

func (cc *ChainConfig) refreshProposals(ctx context.Context, client *rpchttp.HTTP) {
	if !cc.alertSettings().GovAlerts && !td.reportsEnabled() {
		return
	}
	proposals, err := getProposals(ctx, client, voterAddress(cc.ValAddress))
	if err != nil {
		l(cc.ChainId, "could not read governance proposals:", err)
		return
	}
	// keep the result of proposals that left voting, for the clearing message
	previous, _ := cc.govSnapshot()
	td.reportsMux.Lock()
	if remembered := td.reportData.OpenVotes[cc.name]; len(remembered) > 0 {
		previous = append([]govProposal(nil), remembered...)
	}
	td.reportsMux.Unlock()
	observed := append([]govProposal(nil), proposals...)
	open := make(map[uint64]bool)
	for _, p := range proposals {
		open[p.ID] = true
	}
	for _, p := range previous {
		if open[p.ID] {
			continue
		}
		if !p.Voted && voterAddress(cc.ValAddress) != "" {
			for _, module := range govModules {
				if voted, err := hasVoted(ctx, client, module, p.ID, voterAddress(cc.ValAddress)); err == nil && voted {
					p.Voted = true
					observed = append(observed, p)
					break
				}
			}
		}
		if outcome := getOutcome(ctx, client, p.ID); outcome != "" {
			govOutcomes.Store(fmt.Sprintf("%s#%d", cc.name, p.ID), outcome)
		}
	}
	cc.stateMux.Lock()
	td.recordReportVotes(cc.name, previous, observed, time.Now())
	cc.proposals, cc.proposalsLoaded = proposals, true
	cc.stateMux.Unlock()
}

func (cc *ChainConfig) govSnapshot() ([]govProposal, bool) {
	cc.stateMux.RLock()
	defer cc.stateMux.RUnlock()
	return append([]govProposal(nil), cc.proposals...), cc.proposalsLoaded
}

func activeMessages(chain, prefix string) map[string]bool {
	alarms.notifyMux.RLock()
	defer alarms.notifyMux.RUnlock()
	found := make(map[string]bool)
	for message := range alarms.AllAlarms[chain] {
		if strings.HasPrefix(message, prefix) {
			found[message] = true
		}
	}
	return found
}

// firstGovSight reports whether this chain's proposals load for the first time ever.
func (c *Config) firstGovSight(name string) bool {
	alarms.notifyMux.Lock()
	defer alarms.notifyMux.Unlock()
	seen := alarms.GovSeen[name]
	for message := range alarms.AllAlarms[name] {
		if strings.HasPrefix(message, "Proposal #") {
			seen = true
			break
		}
	}
	if alarms.GovSeen == nil {
		alarms.GovSeen = make(map[string]bool)
	}
	alarms.GovSeen[name] = true
	return !seen
}

type govBacklogEntry struct {
	chain    string
	proposal govProposal
}

func (c *Config) collectGovBacklog(name string, p govProposal, now time.Time) {
	c.govSummaryMux.Lock()
	defer c.govSummaryMux.Unlock()
	if len(c.govSummary) == 0 {
		c.govSummaryAt = now.Add(2 * time.Minute)
	}
	c.govSummary = append(c.govSummary, govBacklogEntry{chain: name, proposal: p})
}

func (c *Config) flushGovBacklog(now time.Time) string {
	c.govSummaryMux.Lock()
	if len(c.govSummary) == 0 || now.Before(c.govSummaryAt) {
		c.govSummaryMux.Unlock()
		return ""
	}
	entries := c.govSummary
	c.govSummary, c.govSummaryAt = nil, time.Time{}
	c.govSummaryMux.Unlock()
	if c.context().Err() != nil {
		return ""
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].chain != entries[j].chain {
			return entries[i].chain < entries[j].chain
		}
		return entries[i].proposal.ID < entries[j].proposal.ID
	})
	names := make([]string, 0, len(entries))
	lines := make([]string, 0, len(entries))
	chainLines := make(map[string][]string)
	for _, entry := range entries {
		p := entry.proposal
		line := fmt.Sprintf("%s #%d %s · %s left", entry.chain, p.ID, clipRunes(govTitle(p.Title), 60), shortDuration(p.VotingEnd.Sub(now)))
		lines = append(lines, line)
		if len(chainLines[entry.chain]) == 0 {
			names = append(names, entry.chain)
		}
		chainLines[entry.chain] = append(chainLines[entry.chain], line)
	}
	body := "Proposals already in voting\n" + clipRunes(strings.Join(lines, "\n"), 900)
	c.noticeByDestination(names, body, func(msg *alertMsg, names []string) {
		var lines []string
		for _, name := range names {
			lines = append(lines, chainLines[name]...)
		}
		msg.message = "Proposals already in voting\n" + clipRunes(strings.Join(lines, "\n"), 900)
		if len(names) > 1 {
			msg.chain, msg.extra = "", ""
		}
	})
	return body
}

// parseGovTally keeps token counts exact until converting the final percentages.
func parseGovTally(raw []byte, bonded string) (*dash.ProposalTally, error) {
	counts := [4]*big.Int{new(big.Int), new(big.Int), new(big.Int), new(big.Int)}
	found := false
	err := eachField(raw, func(num protowire.Number, typ protowire.Type, value []byte, _ uint64) error {
		if num != 1 || typ != protowire.BytesType {
			return nil
		}
		found = true
		return eachField(value, func(num protowire.Number, typ protowire.Type, value []byte, _ uint64) error {
			if num >= 1 && num <= 4 && typ == protowire.BytesType {
				n, ok := new(big.Int).SetString(string(value), 10)
				if !ok || n.Sign() < 0 {
					return errors.New("invalid tally count")
				}
				counts[num-1] = n
			}
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errors.New("missing tally result")
	}
	total := new(big.Int)
	for _, count := range counts {
		total.Add(total, count)
	}
	percent := func(n, d *big.Int) float64 {
		if d.Sign() == 0 {
			return 0
		}
		value, _ := new(big.Rat).SetFrac(new(big.Int).Mul(n, big.NewInt(100)), d).Float64()
		return value
	}
	result := &dash.ProposalTally{Yes: percent(counts[0], total), Abstain: percent(counts[1], total), No: percent(counts[2], total), Veto: percent(counts[3], total)}
	if n, ok := new(big.Int).SetString(bonded, 10); ok && n.Sign() > 0 {
		turnout := percent(total, n)
		result.Turnout = &turnout
	}
	return result, nil
}

func proposalTally(ctx context.Context, client *rpchttp.HTTP, id uint64) (*dash.ProposalTally, error) {
	if client == nil {
		return nil, errors.New("no current client")
	}
	request := protowire.AppendVarint(protowire.AppendTag(nil, 1, protowire.VarintType), id)
	var lastErr error
	for _, module := range govModules {
		resp, err := client.ABCIQuery(ctx, "/"+module+".Query/TallyResult", request)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.Response.Code != 0 {
			lastErr = errors.New(resp.Response.Log)
			continue
		}
		tally, err := parseGovTally(resp.Response.Value, "")
		if err != nil {
			lastErr = err
			continue
		}
		pool, err := client.ABCIQuery(ctx, "/cosmos.staking.v1beta1.Query/Pool", nil)
		if err == nil && pool.Response.Code == 0 {
			bonded := ""
			err = eachField(pool.Response.Value, func(num protowire.Number, typ protowire.Type, value []byte, _ uint64) error {
				if num == 1 && typ == protowire.BytesType {
					return eachField(value, func(num protowire.Number, typ protowire.Type, value []byte, _ uint64) error {
						if num == 2 && typ == protowire.BytesType {
							bonded = string(value)
						}
						return nil
					})
				}
				return nil
			})
			if err == nil {
				tally, _ = parseGovTally(resp.Response.Value, bonded)
			}
		}
		return tally, nil
	}
	return nil, lastErr
}

type proposalTallyEntry struct {
	done    chan struct{}
	expires time.Time
	tally   *dash.ProposalTally
	err     error
}

type proposalTallyCache struct {
	sync.Mutex
	entries map[uint64]*proposalTallyEntry
	logged  map[uint64]bool
}

func (cc *ChainConfig) cachedProposalTally(ctx context.Context, id uint64) (*dash.ProposalTally, error) {
	cache := &cc.proposalTallies
	cache.Lock()
	if cache.entries == nil {
		cache.entries = make(map[uint64]*proposalTallyEntry)
		cache.logged = make(map[uint64]bool)
	}
	entry := cache.entries[id]
	if entry == nil || (!entry.expires.IsZero() && time.Now().After(entry.expires)) {
		entry = &proposalTallyEntry{done: make(chan struct{})}
		cache.entries[id] = entry
		for key, old := range cache.entries {
			if key != id && !old.expires.IsZero() && time.Now().After(old.expires) {
				delete(cache.entries, key)
			}
		}
		go func() {
			query, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			tally, err := proposalTally(query, cc.clientSnapshot(), id)
			cache.Lock()
			defer cache.Unlock()
			entry.tally, entry.err = tally, err
			ttl := 5 * time.Minute
			if err != nil {
				ttl = time.Minute
				if !cache.logged[id] {
					l(cc.ChainId, "proposal", id, "tally unavailable:", err)
					cache.logged[id] = true
				}
			}
			entry.expires = time.Now().Add(ttl)
			close(entry.done)
		}()
	}
	cache.Unlock()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-entry.done:
		return entry.tally, entry.err
	}
}

func (c *Config) proposalDetails(ctx context.Context, chain string, id uint64) (dash.ProposalDetail, error) {
	c.chainsMux.RLock()
	cc := c.Chains[chain]
	c.chainsMux.RUnlock()
	if cc == nil {
		return dash.ProposalDetail{}, errors.New("unknown chain")
	}
	proposals, _ := cc.govSnapshot()
	for _, p := range proposals {
		if p.ID != id {
			continue
		}
		info, _ := cc.validatorState()
		result := dash.ProposalDetail{Chain: chain, ChainID: cc.ChainId, Moniker: info.Moniker, ID: id, Title: p.Title, Summary: p.Summary, End: p.VotingEnd.Unix(), Voted: p.Voted}
		if !p.VotingStart.IsZero() {
			result.Start = p.VotingStart.Unix()
		}
		tally, err := cc.cachedProposalTally(ctx, id)
		result.Tally = tally
		if err != nil {
			result.TallyError = "tally unavailable from this node"
		}
		return result, nil
	}
	return dash.ProposalDetail{}, errors.New("proposal is not open")
}
