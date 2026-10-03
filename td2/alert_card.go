package tenderduty

import (
	"html"
	"regexp"
	"strings"
	"time"
)

// An alert is shown as a card: a header, an optional title, and a labelled line for each fact. The card is worked out
// from the message that is the alarm's key and its live detail lines, so nothing here changes what is sent to
// pagerduty or kept as state. A message this file doesn't know is shown as it is, as the card's note.

const (
	dotGreen  = "🟢"
	dotYellow = "🟡"
	dotRed    = "🔴"
	dotWhite  = "⚪"
)

// alertField is one labelled line of a card.
type alertField struct {
	Icon, Label, Value string
	Code               bool // the value is shown as code: hosts, urls and chain ids
}

type alertCard struct {
	Icon, State string
	Title       string // bold line above the fields: a proposal's number and name, an upgrade's name
	Fields      []alertField
	Note        string // the body text of a message that is not known, with no fields
	Extra       string // the chain's extra_info
}

func (c alertCard) known() bool { return c.Note == "" }

// cardValueLimit keeps one odd value (an error from a host) from taking over a card.
const cardValueLimit = 300

func row(icon, label, value string) alertField {
	return alertField{Icon: icon, Label: label, Value: value}
}

func codeField(icon, label, value string) alertField {
	return alertField{Icon: icon, Label: label, Value: value, Code: true}
}

func validatorField(moniker string) alertField { return row("👤", "Validator", moniker) }

func chainField(chainId string) alertField { return codeField("⛓", "Chain", chainId) }

func who(moniker, chainId string) []alertField {
	return []alertField{validatorField(moniker), chainField(chainId)}
}

// cardParts are the facts of one message. The ident ones say who and where and stay true when the alarm clears, the
// state ones are about the problem or what replaced it.
type cardParts struct {
	title        string
	ident, state []alertField
	used         bool // the rule already used the detail lines
}

// cardRule reads one form of message: m is the regex match of the first line, rest the lines after it.
type cardRule struct {
	re    *regexp.Regexp
	build func(msg *alertMsg, m, rest []string) cardParts
}

func matchRule(msg *alertMsg, rules []cardRule, first string, rest []string) (cardParts, bool) {
	for _, r := range rules {
		m := r.re.FindStringSubmatch(first)
		if m == nil {
			continue
		}
		p := r.build(msg, m, rest)
		if !p.used {
			p.state = append(p.state, noteFields(rest)...)
		}
		return p, true
	}
	return cardParts{}, false
}

func noteFields(lines []string) []alertField {
	var notes []alertField
	for _, line := range lines {
		if line != "" {
			notes = append(notes, row("ℹ️", "Note", line))
		}
	}
	return notes
}

var (
	cardMissed          = regexp.MustCompile(`^(.+) has missed (\d+) blocks on (\S+)$`)
	cardStalled         = regexp.MustCompile(`^stalled: have not seen a new block on (\S+) in (\d+) minutes$`)
	cardNodeDown        = regexp.MustCompile(`^RPC node (\S+) has been down for > (.+) on (\S+)$`)
	cardTombstoned      = regexp.MustCompile(`^(.+) is tombstoned on (\S+)$`)
	cardJailed          = regexp.MustCompile(`^(.+) is jailed on (\S+)$`)
	cardInactive        = regexp.MustCompile(`^(.+) is no longer in the active set on (\S+)$`)
	cardLegacyInactive  = regexp.MustCompile(`^(.+) is no longer active: validator is (.+)$`)
	cardJailSoon        = regexp.MustCompile(`^(.+) will be jailed in under (.+) on (\S+) if it keeps missing blocks$`)
	cardSignalStart     = regexp.MustCompile(`^Version (\d+) signalling has started$`)
	cardSignalMissing   = regexp.MustCompile(`^Validator has not signalled version (\d+)$`)
	cardSignalThreshold = regexp.MustCompile(`^Version (\d+) reached the upgrade threshold$`)
	cardNotRegistered   = regexp.MustCompile(`^Validator is not a registered Fibre provider on (\S+)$`)
	cardFibreDown       = regexp.MustCompile(`^Fibre at (\S+) is unreachable on (\S+)$`)
	cardFibreIdentity   = regexp.MustCompile(`^Fibre at (\S+) does not prove it belongs to this validator on (\S+)$`)
	cardSilent          = regexp.MustCompile(`^Fibre has sent no metrics for over (.+) on (\S+)$`)
	cardStorageFull     = regexp.MustCompile(`^Fibre storage is nearly full on (\S+?)(?:,.*)?$`)
	cardSignFailed      = regexp.MustCompile(`^Fibre could not sign payment promises on (\S+)$`)
	cardSigningSlow     = regexp.MustCompile(`^Fibre signing is slower than 10ms on (\S+)$`)
	cardStorageErrors   = regexp.MustCompile(`^Fibre storage operations are failing on (\S+)$`)
	cardObjectErrors    = regexp.MustCompile(`^Fibre object storage requests are failing on (\S+)$`)
	cardIdle            = regexp.MustCompile(`^Fibre has had no successful upload for over (.+) on (\S+)$`)

	cardUpgrade       = regexp.MustCompile(`^Upgrade (.+) at height ([\d,]+)(?: is close)?$`)
	cardSignalUpgrade = regexp.MustCompile(`^Version (\d+) upgrade at height ([\d,]+)(?: is close)?$`)

	// what is true once an alarm clears, the texts of resolved.go and the resolutions
	backSigning       = regexp.MustCompile(`^(.+) is signing blocks again on (\S+)$`)
	backWindow        = regexp.MustCompile(`^(.+) is back under (\d+)% of the slashing window missed on (\S+)$`)
	backProducing     = regexp.MustCompile(`^(\S+) is producing blocks again$`)
	backRpc           = regexp.MustCompile(`^RPC endpoints are working again for (\S+)$`)
	backNode          = regexp.MustCompile(`^RPC node (\S+) is back up on (\S+)$`)
	backNotTombstoned = regexp.MustCompile(`^(.+) is no longer tombstoned on (\S+)$`)
	backActive        = regexp.MustCompile(`^(.+) is back in the active set on (\S+)$`)
	backUnjailed      = regexp.MustCompile(`^(.+) was unjailed on (\S+)$`)
	backProposals     = regexp.MustCompile(`^(.+) missed fewer than (\d+) block proposals in the last (\d+ minutes?) on (\S+)$`)
	backJailRisk      = regexp.MustCompile(`^(.+) is back under (\d+)% of the misses allowed before jailing on (\S+)$`)
	backJailSoon      = regexp.MustCompile(`^(.+) is no longer close to being jailed on (\S+)$`)

	backRegistered   = regexp.MustCompile(`^(.+) is a registered Fibre provider on (\S+) at (\S+)$`)
	backUnregistered = regexp.MustCompile(`^Fibre at (\S+) is no longer registered on (\S+)$`)
	backMoved        = regexp.MustCompile(`^Fibre moved from (\S+) to (\S+) on (\S+)$`)
	backFibreUp      = regexp.MustCompile(`^Fibre at (\S+) answers again on (\S+)$`)
	backVerified     = regexp.MustCompile(`^Fibre at (\S+) proves it belongs to (.+) on (\S+)$`)
	backUnchecked    = regexp.MustCompile(`^Fibre at (\S+) is no longer checked, the validator's consensus key isn't known$`)

	backMetrics       = regexp.MustCompile(`^Fibre metrics are arriving again on (\S+)$`)
	backStorage       = regexp.MustCompile(`^Fibre storage is back under (\d+)% of its budget on (\S+), no uploads turned away for (\d+ minutes)$`)
	backSigned        = regexp.MustCompile(`^Fibre is signing payment promises again on (\S+)$`)
	backSigningFast   = regexp.MustCompile(`^Fibre signing is back under 10ms on (\S+)$`)
	backStorageOps    = regexp.MustCompile(`^Fibre storage operations are succeeding again on (\S+)$`)
	backObjectOps     = regexp.MustCompile(`^Fibre object storage requests are succeeding again on (\S+)$`)
	backUploads       = regexp.MustCompile(`^Fibre stored an upload again on (\S+)$`)
	backUpgraded      = regexp.MustCompile(`^(\S+) runs version (\d+) now$`)
	backSignalled     = regexp.MustCompile(`^(.+) signalled version (\d+) on (\S+)$`)
	backScheduled     = regexp.MustCompile(`^Version (\d+) upgrade is scheduled at height ([\d,]+)$`)
	backFellBack      = regexp.MustCompile(`^Version (\d+) fell back under the upgrade threshold$`)
	backSignalDropped = regexp.MustCompile(`^Version (\d+) signalling dropped below (\d+)%$`)

	cardVoted          = regexp.MustCompile(`^(.+) voted$`)
	cardVotedEnds      = regexp.MustCompile(`^Voted, voting ends (.+)$`)
	cardNoVote         = regexp.MustCompile(`^Voting ended without a vote from (.+)$`)
	cardVotingEnded    = regexp.MustCompile(`^Voting ended (.+)$`)
	cardMetricsLast    = regexp.MustCompile(`^last metrics (.+)$`)
	cardMetricsNone    = regexp.MustCompile(`^no metrics since (.+)$`)
	cardStorageDetail  = regexp.MustCompile(`^(?:(.+?), )?(\d+ uploads?) turned away (in the last 10 minutes)\. (.+)$`)
	cardSignDetail     = regexp.MustCompile(`^(\d+ of \d+ signatures) failed (.+)$`)
	cardSlowDetail     = regexp.MustCompile(`^(\d+ of \d+ signatures) took over 10ms (.+)$`)
	cardStoreDetail    = regexp.MustCompile(`^(put \d+, get \d+, prune \d+) failed (in the last .+)$`)
	cardObjectDetail   = regexp.MustCompile(`^(timeout \d+, throttled \d+, error \d+ of \d+ requests) (.+)$`)
	cardLastUploadLine = regexp.MustCompile(`^last successful upload (.+)$`)
)

// raisedRules read the alarms' messages, with the live detail lines under them.
var raisedRules = []cardRule{
	{cardMissed, func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: who(m[1], m[3]), state: []alertField{row("🎯", "Missed", m[2]+" blocks in a row")}}
	}},
	{percentageMissed, func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: who(m[1], m[3]), state: []alertField{row("📉", "Window", "> "+m[2]+"% missed")}}
	}},
	{cardStalled, func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: []alertField{chainField(m[1])}, state: []alertField{row("🧱", "Blocks", dotRed+" no new block in "+m[2]+" minutes")}}
	}},
	{noRpcNodes, func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: []alertField{chainField(m[1])}, state: []alertField{row("🔌", "RPC", dotRed+" no endpoint working")}}
	}},
	{cardNodeDown, func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: []alertField{codeField("🔌", "Node", m[1]), chainField(m[3])}, state: []alertField{row("📡", "Status", dotRed+" down for > "+m[2])}}
	}},
	{cardTombstoned, func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: who(m[1], m[2]), state: []alertField{row("☠️", "Status", dotRed+" tombstoned")}}
	}},
	{cardJailed, func(msg *alertMsg, m, rest []string) cardParts {
		p := cardParts{ident: who(m[1], m[2]), state: []alertField{row("🔒", "Status", dotRed+" jailed")}, used: true}
		for _, line := range rest {
			if after, ok := strings.CutPrefix(line, "Can unjail after "); ok {
				p.state = append(p.state, row("🔓", "Unjail after", after))
			} else {
				p.state = append(p.state, noteFields([]string{line})...)
			}
		}
		return p
	}},
	// the old message, still in state files saved by earlier versions, has no chain id
	{cardLegacyInactive, func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: []alertField{validatorField(m[1])}, state: []alertField{row("🔒", "Status", dotRed+" "+m[2])}}
	}},
	{cardInactive, func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: who(m[1], m[2]), state: []alertField{row("🔒", "Status", dotRed+" not in the active set")}}
	}},
	{canUnjail, func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: who(m[1], m[2]), state: []alertField{row("🔓", "Status", dotYellow+" jail time is over, an unjail transaction is needed")}}
	}},
	{proposalsMissed, func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: who(m[1], m[4]), state: []alertField{row("📦", "Proposals", "missed "+m[2]+" in "+m[3])}}
	}},
	{jailRisk, func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: who(m[1], m[3]), state: []alertField{row("🚧", "Misses used", m[2]+"% of those allowed before jailing")}}
	}},
	{cardJailSoon, func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: who(m[1], m[3]), state: []alertField{row("⏳", "Jailed in", "under "+m[2]+" if it keeps missing blocks")}}
	}},

	{cardSignalStart, func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: []alertField{row("🔖", "Version", m[1])}, state: []alertField{row("📣", "Signalling", dotYellow+" started")}}
	}},
	{cardSignalMissing, func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: []alertField{row("🔖", "Version", m[1])}, state: []alertField{row("🗳", "Signal", dotRed+" not signalled by this validator")}}
	}},
	{cardSignalThreshold, func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: []alertField{row("🔖", "Version", m[1])}, state: []alertField{row("🚦", "Threshold", dotYellow+" reached, no upgrade scheduled yet")}}
	}},

	{cardNotRegistered, func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: []alertField{chainField(m[1])}, state: []alertField{row("🧵", "Registered", dotRed+" no")}}
	}},
	{cardFibreDown, func(msg *alertMsg, m, rest []string) cardParts {
		reach := dotRed + " no"
		if d := fibreDownFor(msg.chain, m[1]); d > 0 {
			reach += ", for " + shortDuration(d)
		}
		return cardParts{ident: []alertField{codeField("🌐", "Host", m[1]), chainField(m[2])}, state: append([]alertField{row("📡", "Reachable", reach)}, whyFields(rest)...), used: true}
	}},
	{cardFibreIdentity, func(msg *alertMsg, m, rest []string) cardParts {
		return cardParts{ident: []alertField{codeField("🌐", "Host", m[1]), chainField(m[2])},
			state: append([]alertField{row("🔐", "Identity", dotRed+" NOT verified")}, whyFields(rest)...), used: true}
	}},

	{cardSilent, func(msg *alertMsg, m, rest []string) cardParts {
		return metricsParts(m[2], row("📨", "Metrics", dotRed+" none for over "+m[1]), kindSilent, rest)
	}},
	{cardStorageFull, func(msg *alertMsg, m, rest []string) cardParts {
		return metricsParts(m[1], row("🗄", "Storage", dotRed+" nearly full"), kindStorage, rest)
	}},
	{cardSignFailed, func(msg *alertMsg, m, rest []string) cardParts {
		return metricsParts(m[1], row("✍️", "Signing", dotRed+" failing"), kindSignFailed, rest)
	}},
	{cardSigningSlow, func(msg *alertMsg, m, rest []string) cardParts {
		return metricsParts(m[1], row("🐢", "Signing", dotYellow+" slower than 10 ms"), kindSigning, rest)
	}},
	{cardStorageErrors, func(msg *alertMsg, m, rest []string) cardParts {
		return metricsParts(m[1], row("🗄", "Storage", dotRed+" operations failing"), kindStorageErrors, rest)
	}},
	{cardObjectErrors, func(msg *alertMsg, m, rest []string) cardParts {
		return metricsParts(m[1], row("🪣", "Object storage", dotRed+" requests failing"), kindObject, rest)
	}},
	{cardIdle, func(msg *alertMsg, m, rest []string) cardParts {
		return metricsParts(m[2], row("💤", "Uploads", dotRed+" none for over "+m[1]), kindIdle, rest)
	}},
}

// whyFields is the detail line of a Fibre host that is down or failed its identity check.
func whyFields(rest []string) []alertField {
	if len(rest) == 0 || rest[0] == "" {
		return nil
	}
	return []alertField{row("❓", "Why", rest[0])}
}

// metricsParts is a Fibre metrics alarm: the chain, what is wrong and the figures behind it, from the detail line.
func metricsParts(chainId string, state alertField, kind string, rest []string) cardParts {
	p := cardParts{ident: []alertField{chainField(chainId)}, state: []alertField{state}, used: true}
	if len(rest) == 0 {
		return p
	}
	line := rest[0]
	var found []alertField
	switch kind {
	case kindSilent:
		if m := cardMetricsLast.FindStringSubmatch(line); m != nil {
			found = []alertField{row("🕒", "Last metrics", m[1])}
		} else if m := cardMetricsNone.FindStringSubmatch(line); m != nil {
			found = []alertField{row("🕒", "Last metrics", "none since "+m[1])}
		}
	case kindStorage:
		if m := cardStorageDetail.FindStringSubmatch(line); m != nil {
			if m[1] != "" {
				found = append(found, row("💾", "Used", strings.TrimSuffix(m[1], " used")))
			}
			found = append(found, row("🚫", "Turned away", m[2]+" "+m[3]), row("ℹ️", "Note", m[4]))
		}
	case kindSignFailed:
		if m := cardSignDetail.FindStringSubmatch(line); m != nil {
			found = []alertField{row("🎯", "Failed", m[1]+" "+m[2])}
		}
	case kindSigning:
		if m := cardSlowDetail.FindStringSubmatch(line); m != nil {
			found = []alertField{row("🎯", "Slow", m[1]+" over 10 ms "+m[2])}
		}
	case kindStorageErrors:
		if m := cardStoreDetail.FindStringSubmatch(line); m != nil {
			found = []alertField{row("🎯", "Failed", m[1]+" "+m[2])}
		}
	case kindObject:
		if m := cardObjectDetail.FindStringSubmatch(line); m != nil {
			found = []alertField{row("🎯", "Failed", m[1]+" "+m[2])}
		}
	case kindIdle:
		if m := cardLastUploadLine.FindStringSubmatch(line); m != nil {
			found = []alertField{row("🕒", "Last upload", m[1])}
		}
	}
	if found == nil {
		found = []alertField{row("📝", "Detail", line)}
	}
	p.state = append(p.state, found...)
	return p
}

// resolvedRules read what is true once an alarm clears, as resolved.go and the resolutions word it.
var resolvedRules = []cardRule{
	{backSigning, func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: who(m[1], m[2]), state: []alertField{row("✍️", "Signing", dotGreen+" signing blocks again")}}
	}},
	{backWindow, func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: who(m[1], m[3]), state: []alertField{row("📉", "Window", dotGreen+" back under "+m[2]+"% missed")}}
	}},
	{backProducing, func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: []alertField{chainField(m[1])}, state: []alertField{row("🧱", "Blocks", dotGreen+" producing again")}}
	}},
	{backRpc, func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: []alertField{chainField(m[1])}, state: []alertField{row("🔌", "RPC", dotGreen+" endpoints working again")}}
	}},
	{backNode, func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: []alertField{codeField("🔌", "Node", m[1]), chainField(m[2])}, state: []alertField{row("📡", "Status", dotGreen+" back up")}}
	}},
	{backNotTombstoned, func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: who(m[1], m[2]), state: []alertField{row("☠️", "Status", dotGreen+" no longer tombstoned")}}
	}},
	{backActive, func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: who(m[1], m[2]), state: []alertField{row("🔒", "Status", dotGreen+" back in the active set")}}
	}},
	{backUnjailed, func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: who(m[1], m[2]), state: []alertField{row("🔓", "Status", dotGreen+" unjailed")}}
	}},
	{backProposals, func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: who(m[1], m[4]), state: []alertField{row("📦", "Proposals", dotGreen+" fewer than "+m[2]+" missed in the last "+m[3])}}
	}},
	{backJailRisk, func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: who(m[1], m[3]), state: []alertField{row("🚧", "Misses used", dotGreen+" back under "+m[2]+"%")}}
	}},
	{backJailSoon, func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: who(m[1], m[2]), state: []alertField{row("⏳", "Jailing", dotGreen+" no longer close")}}
	}},

	{backRegistered, func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: []alertField{codeField("🌐", "Host", m[3]), validatorField(m[1]), chainField(m[2])}, state: []alertField{row("🧵", "Registered", dotGreen+" yes")}}
	}},
	{backUnregistered, func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: []alertField{codeField("🌐", "Host", m[1]), chainField(m[2])}, state: []alertField{row("🧵", "Registered", dotWhite+" removed from the registry")}}
	}},
	{backMoved, func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: []alertField{codeField("🌐", "Host", m[2]), chainField(m[3])}, state: []alertField{codeField("↩️", "Was", m[1])}}
	}},
	{backFibreUp, func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: []alertField{codeField("🌐", "Host", m[1]), chainField(m[2])}, state: []alertField{row("📡", "Reachable", dotGreen+" yes")}}
	}},
	{backVerified, func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: []alertField{codeField("🌐", "Host", m[1]), validatorField(m[2]), chainField(m[3])}, state: []alertField{row("🔐", "Identity", dotGreen+" verified")}}
	}},
	{backUnchecked, func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: []alertField{codeField("🌐", "Host", m[1])},
			state: []alertField{row("🔐", "Identity", dotWhite+" not checked"), row("❓", "Why", "the validator's consensus key isn't known")}}
	}},

	{backMetrics, func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: []alertField{chainField(m[1])}, state: []alertField{row("📨", "Metrics", dotGreen+" arriving again")}}
	}},
	{backStorage, func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: []alertField{chainField(m[2])},
			state: []alertField{row("🗄", "Storage", dotGreen+" under "+m[1]+"% of its budget"), row("🚫", "Turned away", "no uploads for "+m[3])}}
	}},
	{backSigned, func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: []alertField{chainField(m[1])}, state: []alertField{row("✍️", "Signing", dotGreen+" signing again")}}
	}},
	{backSigningFast, func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: []alertField{chainField(m[1])}, state: []alertField{row("🐢", "Signing", dotGreen+" back under 10 ms")}}
	}},
	{backStorageOps, func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: []alertField{chainField(m[1])}, state: []alertField{row("🗄", "Storage", dotGreen+" operations succeeding")}}
	}},
	{backObjectOps, func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: []alertField{chainField(m[1])}, state: []alertField{row("🪣", "Object storage", dotGreen+" requests succeeding")}}
	}},
	{backUploads, func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: []alertField{chainField(m[1])}, state: []alertField{row("💤", "Uploads", dotGreen+" arriving again")}}
	}},

	{backUpgraded, func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: []alertField{chainField(m[1])}, state: []alertField{row("🔖", "Version", dotGreen+" version "+m[2]+" is running")}}
	}},
	{backSignalled, func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: who(m[1], m[3]), state: []alertField{row("🗳", "Signal", dotGreen+" version "+m[2]+" signalled")}}
	}},
	{backScheduled, func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: []alertField{row("🔖", "Version", m[1]), row("📏", "Height", m[2])}, state: []alertField{row("⬆️", "Upgrade", dotYellow+" scheduled")}}
	}},
	{backFellBack, func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: []alertField{row("🔖", "Version", m[1])}, state: []alertField{row("🚦", "Threshold", dotWhite+" back under")}}
	}},
	{backSignalDropped, func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: []alertField{row("🔖", "Version", m[1])}, state: []alertField{row("📣", "Signalling", dotWhite+" dropped below "+m[2]+"%")}}
	}},
}

// fibreDownFor is how long the chain's Fibre host has been unreachable, 0 when that isn't known for this host.
func fibreDownFor(chain, host string) time.Duration {
	if td == nil {
		return 0
	}
	td.chainsMux.RLock()
	cc := td.Chains[chain]
	td.chainsMux.RUnlock()
	if cc == nil {
		return 0
	}
	cc.stateMux.RLock()
	defer cc.stateMux.RUnlock()
	if cc.fibre.Host != host || cc.fibre.Reachable || cc.fibre.DownSince.IsZero() {
		return 0
	}
	return max(time.Since(cc.fibre.DownSince), 0)
}

// proposalParts is a governance card: the proposal is its title, the rest are its end, and the vote or the result.
func (msg *alertMsg) proposalParts(first string, rest []string, state string) cardParts {
	p := cardParts{title: strings.TrimPrefix(first, "Proposal ")}
	if msg.resolved {
		switch state {
		case "Passed":
			p.state = append(p.state, row("📜", "Result", dotGreen+" passed"))
		case "Rejected", "Failed":
			p.state = append(p.state, row("📜", "Result", dotRed+" "+strings.ToLower(state)))
		}
	}
	for _, line := range rest {
		var m []string
		switch {
		case strings.HasPrefix(line, "Ends "):
			p.ident = append(p.ident, row("🗓", "Ends", strings.TrimPrefix(line, "Ends ")))
		case strings.HasSuffix(line, " has not voted"):
			p.state = append(p.state, validatorField(strings.TrimSuffix(line, " has not voted")), row("🗳", "Vote", dotRed+" not voted yet"))
		case msg.resolved && line == "Voting closed":
			p.state = append(p.state, row("🏁", "Voting", "closed"))
		default:
			if !msg.resolved {
				p.state = append(p.state, noteFields([]string{line})...)
			} else if m = cardVotedEnds.FindStringSubmatch(line); m != nil {
				p.state = append(p.state, row("🗳", "Vote", dotGreen+" voted"), row("🗓", "Voting ends", m[1]))
			} else if m = cardNoVote.FindStringSubmatch(line); m != nil {
				p.state = append(p.state, validatorField(m[1]), row("🗳", "Vote", dotRed+" no vote cast"))
			} else if m = cardVotingEnded.FindStringSubmatch(line); m != nil {
				p.state = append(p.state, row("🏁", "Voting", "ended "+m[1]))
			} else if m = cardVoted.FindStringSubmatch(line); m != nil {
				p.state = append(p.state, validatorField(m[1]), row("🗳", "Vote", dotGreen+" voted"))
			} else {
				p.state = append(p.state, noteFields([]string{line})...)
			}
		}
	}
	p.used = true
	return p
}

// upgradeParts is an upgrade card, from x/upgrade or x/signal: its name, height and estimated time, or how it ended.
func (msg *alertMsg) upgradeParts(title, height string, rest []string, state string) cardParts {
	p := cardParts{title: title, ident: []alertField{row("📏", "Height", height)}, used: true}
	if msg.resolved {
		status := dotGreen + " upgraded"
		if state == "Cancelled" {
			status = dotRed + " cancelled"
		}
		p.state = append(p.state, row("🏁", "Status", status))
	}
	for _, line := range rest {
		if after, ok := strings.CutPrefix(line, "Around "); ok && !msg.resolved {
			p.state = append(p.state, row("🕒", "Expected", after))
		} else {
			p.state = append(p.state, noteFields([]string{line})...)
		}
	}
	return p
}

// cardParts works out a message's facts, false for a message it doesn't know.
func (msg *alertMsg) parts(first string, rest []string, state string) (cardParts, bool) {
	orphan := msg.resolved && len(rest) > 0 && rest[len(rest)-1] == orphanNote
	if orphan {
		// nothing watches it any more, so it says who and where and that it stopped
		p, ok := raisedParts(msg, first, rest[:len(rest)-1], state)
		if !ok {
			return cardParts{}, false
		}
		p.state = []alertField{row("👁", "Watching", dotWhite+" stopped, the alert was turned off or its settings changed")}
		return p, true
	}
	if msg.resolved {
		if !strings.HasPrefix(first, "Proposal #") && !cardUpgrade.MatchString(first) && !cardSignalUpgrade.MatchString(first) {
			if p, ok := matchRule(msg, resolvedRules, first, rest); ok {
				return p, true
			}
			// no resolution is known (the process restarted since it cleared), the message still says who and where
			p, ok := matchRule(msg, raisedRules, first, nil)
			p.state = []alertField{row("📌", "Status", dotGreen+" cleared")}
			return p, ok
		}
	}
	return raisedParts(msg, first, rest, state)
}

func raisedParts(msg *alertMsg, first string, rest []string, state string) (cardParts, bool) {
	if strings.HasPrefix(first, "Proposal #") {
		return msg.proposalParts(first, rest, state), true
	}
	if m := cardUpgrade.FindStringSubmatch(first); m != nil {
		return msg.upgradeParts(m[1], m[2], rest, state), true
	}
	if m := cardSignalUpgrade.FindStringSubmatch(first); m != nil {
		return msg.upgradeParts("Version "+m[1], m[2], rest, state), true
	}
	return matchRule(msg, raisedRules, first, rest)
}

// card is the notification as a card, worked out from the alert's summary.
func (msg *alertMsg) card() alertCard {
	return msg.cardFrom(msg.summary())
}

// cardFrom is card for a given summary, the message and its detail lines.
func (msg *alertMsg) cardFrom(body string) alertCard {
	icon, state := msg.header()
	c := alertCard{Icon: icon, State: state, Extra: msg.extra}
	lines := strings.Split(body, "\n")
	p, ok := msg.parts(lines[0], lines[1:], state)
	if !ok || len(p.ident)+len(p.state) == 0 && p.title == "" {
		c.Note = body
		return c
	}
	c.Title = p.title
	fields := append(append([]alertField(nil), p.ident...), p.state...)
	orphan := msg.resolved && strings.HasSuffix(body, "\n"+orphanNote)
	if !msg.resolved && msg.severity != "" && msg.severity != "info" && state != strings.ToUpper(msg.severity[:1])+msg.severity[1:] {
		fields = append(fields, row("⚠️", "Severity", msg.severity))
	}
	if msg.resolved && !orphan && msg.lasted > 0 && msg.wasAProblem() {
		fields = append(fields, row("⏱", "Lasted", shortDuration(msg.lasted)))
	}
	for _, f := range fields {
		if f.Value = clipRunes(strings.TrimSpace(f.Value), cardValueLimit); f.Value != "" {
			c.Fields = append(c.Fields, f)
		}
	}
	if len(c.Fields) == 0 && c.Title == "" {
		c.Note = body
	}
	return c
}

// wasAProblem is whether how long the alarm lasted means anything. A proposal, an upgrade or signalling is news, not an
// outage, and a tombstone is permanent.
func (msg *alertMsg) wasAProblem() bool {
	m := msg.message
	return !strings.HasPrefix(m, "Proposal #") && !strings.HasPrefix(m, "Upgrade ") && !isSignalMessage(m) && !strings.Contains(m, " is tombstoned on ")
}

// clipRunes cuts text to n characters, ending in … when it was longer.
func clipRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

func (c alertCard) telegramLines() []string {
	var lines []string
	if c.Title != "" {
		lines = append(lines, "<b>"+html.EscapeString(c.Title)+"</b>")
	}
	for _, f := range c.Fields {
		value := html.EscapeString(f.Value)
		if f.Code {
			value = "<code>" + value + "</code>"
		} else {
			value = timeLeft.ReplaceAllString(value, "· <b>$1</b>")
		}
		lines = append(lines, statusField(f.Icon, html.EscapeString(f.Label), value))
	}
	if c.Note != "" {
		lines = append(lines, timeLeft.ReplaceAllString(html.EscapeString(c.Note), "· <b>$1</b>"))
	}
	return lines
}

// markdownLines are the card for discord's plain style.
func (c alertCard) markdownLines() []string {
	var lines []string
	if c.Title != "" {
		lines = append(lines, "**"+c.Title+"**")
	}
	for _, f := range c.Fields {
		lines = append(lines, f.Icon+" **"+f.Label+":** "+discordValue(f))
	}
	if c.Note != "" {
		lines = append(lines, timeLeft.ReplaceAllString(c.Note, "· **$1**"))
	}
	return lines
}

func discordValue(f alertField) string {
	if f.Code {
		return "`" + strings.ReplaceAll(f.Value, "`", "'") + "`"
	}
	return timeLeft.ReplaceAllString(f.Value, "· **$1**")
}

// discord's limits for an embed, in characters
const (
	discordContentLimit     = 2000
	discordDescriptionLimit = 4096
	discordFieldsLimit      = 25
	discordNameLimit        = 256
	discordValueLimit       = 1024
	discordFooterLimit      = 2048
	discordEmbedLimit       = 6000
)

// inlineLimit is how long a value can be for a field to sit beside others.
const inlineLimit = 24

type DiscordField struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline"`
}

type DiscordFooter struct {
	Text string `json:"text"`
}

// discordEmbed is the card as an embed, cut to discord's limits: it refuses a message that is over any of them.
func (c alertCard) discordEmbed(color uint) DiscordEmbed {
	description := ""
	if c.Title != "" {
		description = "**" + strings.ReplaceAll(c.Title, "**", "") + "**"
	}
	if c.Note != "" {
		if description != "" {
			description += "\n"
		}
		description += timeLeft.ReplaceAllString(c.Note, "· **$1**")
	}
	embed := DiscordEmbed{Description: clipRunes(description, discordDescriptionLimit), Color: color}
	if c.Extra != "" {
		embed.Footer = &DiscordFooter{Text: clipRunes(c.Extra, discordFooterLimit)}
	}
	total := len([]rune(embed.Description))
	if embed.Footer != nil {
		total += len([]rune(embed.Footer.Text))
	}
	for _, f := range c.Fields {
		if len(embed.Fields) == discordFieldsLimit {
			break
		}
		df := DiscordField{
			Name:   clipRunes(f.Icon+" "+f.Label, discordNameLimit),
			Value:  clipRunes(discordValue(f), discordValueLimit),
			Inline: len([]rune(f.Value)) < inlineLimit,
		}
		if total += len([]rune(df.Name)) + len([]rune(df.Value)); total > discordEmbedLimit {
			break
		}
		embed.Fields = append(embed.Fields, df)
	}
	return embed
}

// SlackField is a field of a legacy attachment, short ones sit two to a row.
type SlackField struct {
	Title string `json:"title"`
	Value string `json:"value"`
	Short bool   `json:"short"`
}

// slackText escapes what slack reads as markup in a message.
func slackText(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

func (c alertCard) slackFields() []SlackField {
	fields := make([]SlackField, 0, len(c.Fields))
	for _, f := range c.Fields {
		value := slackText(f.Value)
		if f.Code {
			value = "`" + strings.ReplaceAll(value, "`", "'") + "`"
		}
		fields = append(fields, SlackField{Title: f.Icon + " " + f.Label, Value: value, Short: len([]rune(f.Value)) < inlineLimit})
	}
	return fields
}

// slackTitle is the card's title in bold, for the attachment's text.
func slackTitle(c alertCard) string {
	if c.Title == "" {
		return ""
	}
	return "*" + strings.ReplaceAll(slackText(c.Title), "*", "") + "*"
}
