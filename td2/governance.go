package tenderduty

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cosmos/cosmos-sdk/types/bech32"
	rpchttp "github.com/tendermint/tendermint/rpc/client/http"
	"google.golang.org/protobuf/encoding/protowire"
)

// govProposal is a proposal in its voting period.
type govProposal struct {
	ID        uint64
	Title     string
	VotingEnd time.Time
	Voted     bool
}

// gov module versions to try, v1 first. Both share the field numbers read here, v1beta1 keeps the title in content.
var govModules = []string{"cosmos.gov.v1", "cosmos.gov.v1beta1"}

const proposalStatusVoting = 2

// voterAddress is the validator's account, the valoper's bytes with the account prefix. Empty when it can't be told.
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

// getProposals lists the proposals in voting and, when the voter is known, whether it has voted on each.
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
		if voter != "" {
			for i := range proposals {
				proposals[i].Voted, err = hasVoted(ctx, client, module, proposals[i].ID, voter)
				if err != nil {
					return nil, err
				}
			}
		}
		return proposals, nil
	}
	return nil, lastErr
}

// hasVoted asks for the voter's vote, a chain answers with an error when there is none.
func hasVoted(ctx context.Context, client *rpchttp.HTTP, module string, id uint64, voter string) (bool, error) {
	request := protowire.AppendVarint(protowire.AppendTag(nil, 1, protowire.VarintType), id)
	request = protowire.AppendString(protowire.AppendTag(request, 2, protowire.BytesType), voter)
	resp, err := client.ABCIQuery(ctx, "/"+module+".Query/Vote", request)
	if err != nil {
		return false, err
	}
	return resp.Response.Code == 0 && len(resp.Response.Value) > 0, nil
}

// parseProposals reads QueryProposalsResponse by field number, only what the alerts need, so chains with their own
// proposal messages decode the same.
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
				// content Any, its value is the legacy proposal with the title first
				return eachField(value, func(num protowire.Number, typ protowire.Type, value []byte, _ uint64) error {
					if num == 2 && typ == protowire.BytesType {
						p.Title = firstString(value)
					}
					return nil
				})
			case num == 9 && typ == protowire.BytesType:
				p.VotingEnd = parseTimestamp(value)
			case num == 11 && typ == protowire.BytesType && !legacy:
				p.Title = string(value)
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

// eachField walks a protobuf message, giving bytes fields their value and varints their number.
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

// govEndFormat is how the end of voting is written, the time left is worked out from it when a message is sent.
const govEndFormat = "2 Jan 2006 15:04 UTC"

// the messages are the alarms' keys, so they only hold what doesn't change while the proposal is in voting
func (cc *ChainConfig) govOpenMessage(p govProposal) string {
	title := ""
	if p.Title != "" {
		title = " " + p.Title
	}
	return fmt.Sprintf("Proposal #%d%s\nEnds %s", p.ID, title, p.VotingEnd.Format(govEndFormat))
}

func (cc *ChainConfig) govRemindMessage(p govProposal, moniker string) string {
	return cc.govOpenMessage(p) + "\n" + moniker + " has not voted"
}

var govMessageId = regexp.MustCompile(`^Proposal #(\d+)`)

// govTimeLeft adds the time left to a message's Ends or Can unjail after line, while that time is still ahead.
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

// shortDuration writes a duration as its two largest units, 6d 23h, 5h 12m or 59m.
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

// govOutcomes holds how proposals ended, by chain name and id, for the message that clears their alarm.
var govOutcomes sync.Map

// govResolvedLabel says why a governance alarm cleared: the proposal's result once voting ended, else the vote.
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

// getOutcome reads a proposal's status after voting: passed, rejected or failed.
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

// govAlarms keeps one alarm per proposal waiting on a vote, and a reminder when voting is about to end. The wanted
// alarms are worked out fresh each time and compared with the active ones, so a restart picks up where it was.
func (cc *ChainConfig) govAlarms(valInfo *ValInfo, reconcile bool) {
	proposals, loaded := cc.govSnapshot()
	if !loaded {
		return
	}
	voter := voterAddress(cc.ValAddress)
	wanted := make(map[string]string) // message to alert id
	priority := make(map[string]string)
	for _, p := range proposals {
		if p.Voted {
			continue
		}
		message := cc.govOpenMessage(p)
		wanted[message], priority[message] = fmt.Sprintf("%sgov%d", valInfo.Valcons, p.ID), cc.alertSettings().GovPriority
		if voter != "" && time.Until(p.VotingEnd) <= time.Duration(cc.alertSettings().GovRemindMinutes)*time.Minute {
			message = cc.govRemindMessage(p, valInfo.Moniker)
			wanted[message], priority[message] = fmt.Sprintf("%sgovremind%d", valInfo.Valcons, p.ID), "critical"
		}
	}
	active := activeMessages(cc.name, "Proposal #")
	for message, id := range wanted {
		if !active[message] || reconcile {
			id := id
			td.alert(cc.name, message, priority[message], false, &id)
		}
	}
	// a proposal's alarm and its reminder clear together when the vote is cast or voting ends, the reminder says it
	reminded := make(map[string]bool)
	for message := range active {
		if _, ok := wanted[message]; !ok && strings.Contains(message, " has not voted") {
			if match := govMessageId.FindStringSubmatch(message); match != nil {
				reminded[match[1]] = true
			}
		}
	}
	for message := range active {
		if _, ok := wanted[message]; !ok {
			id := govAlarmId(valInfo, message)
			if match := govMessageId.FindStringSubmatch(message); match != nil && reminded[match[1]] && !strings.Contains(message, " has not voted") {
				td.resolveQuietly(cc.name, message, &id)
				continue
			}
			td.alert(cc.name, message, "info", true, &id)
		}
	}
}

// govAlarmId rebuilds an alarm's id from its message, for alarms that are no longer wanted.
func govAlarmId(valInfo *ValInfo, message string) string {
	id := uint64(0)
	if match := govMessageId.FindStringSubmatch(message); match != nil {
		id, _ = strconv.ParseUint(match[1], 10, 64)
	}
	if strings.Contains(message, " has not voted") {
		return fmt.Sprintf("%sgovremind%d", valInfo.Valcons, id)
	}
	return fmt.Sprintf("%sgov%d", valInfo.Valcons, id)
}

// refreshProposals reads the proposals in voting, it runs with the validator refresh.
func (cc *ChainConfig) refreshProposals(ctx context.Context, client *rpchttp.HTTP) {
	if !cc.alertSettings().GovAlerts && !td.reportsEnabled() {
		return
	}
	proposals, err := getProposals(ctx, client, voterAddress(cc.ValAddress))
	if err != nil {
		l(cc.ChainId, "could not read governance proposals:", err)
		return
	}
	// the ones that left voting since the last read have a result now, keep it for the message clearing their alarm
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

// activeMessages lists a chain's active alarms whose message starts with prefix.
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
