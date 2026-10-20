package tenderduty

import (
	"fmt"
	"math/big"
	"strings"
	"time"

	staking "github.com/cosmos/cosmos-sdk/x/staking/types"
)

func validateChanges(a *AlertConfig, chain string) []string {
	var problems []string
	for _, setting := range []struct {
		name  string
		value *int
	}{{"changes_drop_percent", &a.ChangesDropPercent}, {"changes_rise_percent", &a.ChangesRisePercent}} {
		if *setting.value < 0 || *setting.value > 100 {
			*setting.value = 10
			problems = append(problems, fmt.Sprintf("warning: %s %s must be between 0 and 100, using 10", chain, setting.name))
		}
	}
	problems = append(problems, defaultAlertSeverity(&a.ChangesSeverity, chain+" changes_severity", "info")...)
	return problems
}

func (s *reportSnapshot) captureValidatorChanges(v staking.Validator) {
	known := func(value string) *string { return &value }
	if !v.Commission.Rate.IsNil() {
		s.Commission = known(v.Commission.Rate.String())
	}
	if !v.MinSelfDelegation.IsNil() {
		s.MinSelfDelegation = known(v.MinSelfDelegation.String())
	}
	s.Moniker = known(clipRunes(v.Description.Moniker, 300))
	s.Identity = known(clipRunes(v.Description.Identity, 300))
	s.Website = known(clipRunes(v.Description.Website, 300))
	s.SecurityContact = known(clipRunes(v.Description.SecurityContact, 300))
	s.Details = known(clipRunes(v.Description.Details, 300))
}

func changeRate(value string) string {
	if rate, ok := new(big.Rat).SetString(value); ok {
		percent := new(big.Rat).Mul(rate, big.NewRat(100, 1)).FloatString(18)
		return strings.TrimRight(strings.TrimRight(percent, "0"), ".") + "%"
	}
	return value
}

// Mixed token and power snapshots have no comparable stake or self-bond.
func comparableChangeStake(old, current reportSnapshot) (before, after float64, unit string, ok bool) {
	oldToken := old.Stake != nil && old.Symbol != ""
	newToken := current.Stake != nil && current.Symbol != ""
	if oldToken && newToken && old.Symbol == current.Symbol {
		return *old.Stake, *current.Stake, current.Symbol, true
	}
	if !oldToken && !newToken && old.Power != nil && current.Power != nil {
		return float64(*old.Power), float64(*current.Power), "power", true
	}
	return 0, 0, "", false
}

func validatorChanges(old *reportSnapshot, current reportSnapshot, a AlertConfig) string {
	if old == nil || !current.At.After(old.At) {
		return ""
	}
	span := reportDuration(current.At.Sub(old.At))
	var lines []string
	before, after, unit, comparable := comparableChangeStake(*old, current)
	if comparable && before > 0 && after >= 0 {
		percent := 100 * (after - before) / before
		if a.ChangesDropPercent > 0 && percent < -float64(a.ChangesDropPercent) || a.ChangesRisePercent > 0 && percent > float64(a.ChangesRisePercent) {
			lines = append(lines, fmt.Sprintf("Stake %+.1f%% in %s · %s → %s %s", percent, span, compactReportNumber(before), compactReportNumber(after), unit))
		}
	}
	if comparable && old.Self != nil && current.Self != nil && *old.Self > 0 && *current.Self >= 0 && *current.Self <= *old.Self*0.99 {
		lines = append(lines, fmt.Sprintf("Self-bond %s → %s %s", compactReportNumber(*old.Self), compactReportNumber(*current.Self), unit))
	}
	text := func(s string) string { return strings.NewReplacer("\r", " ", "\n", " ").Replace(s) }
	changed := func(before, after *string) bool { return before != nil && after != nil && *before != *after }
	if changed(old.Commission, current.Commission) {
		lines = append(lines, "Commission "+changeRate(*old.Commission)+" → "+changeRate(*current.Commission))
	}
	if changed(old.MinSelfDelegation, current.MinSelfDelegation) {
		lines = append(lines, "Min self delegation "+*old.MinSelfDelegation+" → "+*current.MinSelfDelegation)
	}
	if changed(old.Moniker, current.Moniker) {
		lines = append(lines, "Moniker "+text(*old.Moniker)+" → "+text(*current.Moniker))
	}
	var description []string
	for _, field := range []struct {
		name          string
		before, after *string
	}{
		{"identity", old.Identity, current.Identity},
		{"website", old.Website, current.Website},
		{"security contact", old.SecurityContact, current.SecurityContact},
		{"details", old.Details, current.Details},
	} {
		if changed(field.before, field.after) {
			description = append(description, field.name)
		}
	}
	if len(description) > 0 {
		lines = append(lines, "Description: "+strings.Join(description, ", "))
	}
	if len(lines) == 0 {
		return ""
	}
	// delegations move the stake; the validator itself didn't change
	title := "Stake changed"
	for _, line := range lines {
		if !strings.HasPrefix(line, "Stake ") {
			title = "Validator changed"
		}
	}
	return title + "\n" + strings.Join(lines, "\n")
}

// validatorChangeTitle returns the title of a validator change notice, or "".
func validatorChangeTitle(body string) string {
	for _, title := range []string{"Validator changed", "Stake changed"} {
		if strings.HasPrefix(body, title+"\n") {
			return title
		}
	}
	return ""
}

// Queue notices without recording an incident or a recovery.
func (c *Config) validatorChangeNotice(chain, body, severity string, at time.Time) {
	id := fmt.Sprintf("%s-changes-%d", chain, at.UnixNano())
	msg := c.makeAlert(chain, body, severity, false, &id)
	if msg == nil || c.context().Err() != nil {
		return
	}
	msg.transient = true
	alarms.notifyMux.Lock()
	alarms.enqueue(msg)
	alarms.notifyMux.Unlock()
	if err := c.persistState(); err != nil {
		l("could not save validator change notice:", err)
	}
}
