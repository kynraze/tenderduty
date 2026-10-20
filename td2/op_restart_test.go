package tenderduty

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const opPDKey = "0123456789abcdef0123456789abcdef"

var opDests = []string{"telegram", "pagerduty", "discord", "slack", "gotify"}

// opRestartRig restarts tenderduty from its config and state file, and counts what each destination got.
type opRestartRig struct {
	t          *testing.T
	h          *groupHarness
	gotify     *httptest.Server
	gmu        sync.Mutex
	gt         []GotifyMessage
	configFile string
	stateFile  string
	chains     string
	tgMin      string
	extra      string
	current    *Config
}

type opSent struct{ raised, recovered int }

func newOpRestartRig(t *testing.T) *opRestartRig {
	t.Helper()
	h := newGroupHarness(t, map[string]string{"a1": hostAIP, "a2": hostAIP, "b2": hostBIP})
	r := &opRestartRig{t: t, h: h}
	r.gotify = httptest.NewServer(http.HandlerFunc(r.serveGotify))
	t.Cleanup(r.gotify.Close)
	dir := t.TempDir()
	r.configFile = filepath.Join(dir, "config.yml")
	r.stateFile = filepath.Join(dir, "state.json")
	r.chains = filepath.Join(dir, "chains.d")
	if err := os.Mkdir(r.chains, 0700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if r.current != nil {
			r.stop(r.current)
		}
	})
	return r
}

func (r *opRestartRig) serveGotify(w http.ResponseWriter, req *http.Request) {
	if req.URL.Path != "/message" || req.Header.Get("X-Gotify-Key") != gotifyTestToken {
		http.NotFound(w, req)
		return
	}
	var m GotifyMessage
	_ = json.NewDecoder(req.Body).Decode(&m)
	r.gmu.Lock()
	r.gt = append(r.gt, m)
	r.gmu.Unlock()
	_, _ = w.Write([]byte(`{"id":1}`))
}

// yaml enables the given destinations for every chain; extra and tgMin are top-level settings.
func (r *opRestartRig) yaml(dests ...string) string {
	tg := "telegram:\n  enabled: yes\n  api_key: \"" + testToken + "\"\n  channel: \"@one\"\n"
	if r.tgMin != "" {
		tg += "  min_severity: " + r.tgMin + "\n"
	}
	frag := map[string]string{
		"telegram":  tg,
		"discord":   "discord:\n  enabled: yes\n  webhook: \"" + r.h.server.URL + "/discord\"\n",
		"slack":     "slack:\n  enabled: yes\n  webhook: \"" + r.h.server.URL + "/slack\"\n",
		"gotify":    "gotify:\n  enabled: yes\n  url: \"" + r.gotify.URL + "\"\n  token: \"" + gotifyTestToken + "\"\n",
		"pagerduty": "pagerduty:\n  enabled: yes\n  api_key: \"" + opPDKey + "\"\n",
	}
	var b strings.Builder
	b.WriteString("node_down_alert_minutes: 3\n" + r.extra)
	for _, d := range dests {
		b.WriteString(frag[d])
	}
	b.WriteString("chains:\n" +
		"  AlphaOne:\n    chain_id: alphaone-1\n    valoper_address: alphaonevaloper\n    nodes:\n      - url: http://a1:26657\n        alert_if_down: yes\n" +
		"  AlphaTwo:\n    chain_id: alphatwo-1\n    valoper_address: alphatwovaloper\n    nodes:\n      - url: http://a2:26657\n        alert_if_down: yes\n")
	return b.String()
}

func (r *opRestartRig) write(content string) {
	r.t.Helper()
	if err := os.WriteFile(r.configFile, []byte(content), 0600); err != nil {
		r.t.Fatal(err)
	}
}

func (r *opRestartRig) writeChain(name, content string) {
	r.t.Helper()
	if err := os.WriteFile(filepath.Join(r.chains, name+".yml"), []byte(content), 0600); err != nil {
		r.t.Fatal(err)
	}
}

// start loads config and state like startup does; nothing is delivered until boot.
func (r *opRestartRig) start(yaml string) *Config {
	r.t.Helper()
	r.write(yaml)
	password := ""
	c, err := loadConfig(r.configFile, r.stateFile, r.chains, &password)
	if err != nil {
		r.t.Fatal(err)
	}
	if fatal, problems := validateConfig(c); fatal {
		r.t.Fatal(problems)
	}
	for _, cc := range c.Chains {
		cc.restoredChecked = true
	}
	td, r.h.c, r.current = c, c, c
	return c
}

// boot does what run does after the monitors start: recovery queue first, then the workers.
func (r *opRestartRig) boot(c *Config) {
	c.restoreRecoveries()
	c.startWorker(c.runNotifications)
}

func (r *opRestartRig) stop(c *Config) {
	c.cancel()
	c.deliveryCancel()
	c.workers.Wait()
	r.current = nil
}

// restart saves the state as a clean shutdown would, then stops.
func (r *opRestartRig) restart(c *Config) {
	r.t.Helper()
	if err := c.persistState(); err != nil {
		r.t.Fatal(err)
	}
	r.stop(c)
}

func (r *opRestartRig) reload(c *Config) error {
	password := ""
	return c.reload(r.configFile, r.chains, &password)
}

// settle waits until the queue is empty and nothing more arrives shortly after.
func (r *opRestartRig) settle() {
	r.h.waitFor("the outbox to drain", r.h.drained)
	time.Sleep(4 * groupUnit)
	r.h.waitFor("the outbox to drain", r.h.drained)
}

// opRecovery spots a recovery in the text each destination receives.
func opRecovery(text string) bool {
	for _, mark := range []string{"Resolved", "Alert stopped", "No longer watched", "back up"} {
		if strings.Contains(text, mark) {
			return true
		}
	}
	return false
}

func (r *opRestartRig) sent() map[string]opSent {
	out := make(map[string]opSent)
	for _, d := range opDests {
		out[d] = opSent{}
	}
	add := func(dest string, recovered bool) {
		s := out[dest]
		if recovered {
			s.recovered++
		} else {
			s.raised++
		}
		out[dest] = s
	}
	r.h.mux.Lock()
	for _, m := range r.h.telegram {
		add("telegram", opRecovery(m.text))
	}
	for _, e := range r.h.pd {
		add("pagerduty", e.Action == "resolve")
	}
	for _, m := range r.h.discord {
		add("discord", opRecovery(m.Content))
	}
	for _, m := range r.h.slack {
		add("slack", opRecovery(m.Text))
	}
	r.h.mux.Unlock()
	r.gmu.Lock()
	for _, m := range r.gt {
		add("gotify", opRecovery(m.Title))
	}
	r.gmu.Unlock()
	return out
}

// expect compares what each destination got; destinations missing from want must get nothing.
func (r *opRestartRig) expect(what string, want map[string]opSent) {
	r.t.Helper()
	for _, d := range opDests {
		if _, ok := want[d]; !ok {
			want[d] = opSent{}
		}
	}
	if got := r.sent(); !reflect.DeepEqual(got, want) {
		r.t.Errorf("%s: sent %v, want %v", what, got, want)
	}
}

func opAll(raised, recovered int) map[string]opSent {
	out := make(map[string]opSent)
	for _, d := range opDests {
		out[d] = opSent{raised, recovered}
	}
	return out
}

func (r *opRestartRig) pdKeys() (triggers, resolves []string) {
	r.h.mux.Lock()
	defer r.h.mux.Unlock()
	for _, e := range r.h.pd {
		if e.Action == "resolve" {
			resolves = append(resolves, e.Key)
		} else {
			triggers = append(triggers, e.Key)
		}
	}
	return triggers, resolves
}

func opOutboxLen() int {
	alarms.notifyMux.RLock()
	defer alarms.notifyMux.RUnlock()
	return len(alarms.Outbox)
}

// opAgeSent moves every delivery record back, as if it had been sent that long ago.
func opAgeSent(age time.Duration) {
	alarms.notifyMux.Lock()
	defer alarms.notifyMux.Unlock()
	for _, sent := range []map[string]time.Time{alarms.SentPdAlarms, alarms.SentTgAlarms, alarms.SentDiAlarms, alarms.SentSlkAlarms, alarms.SentGtAlarms} {
		for k := range sent {
			sent[k] = time.Now().Add(-age)
		}
	}
}

// Every destination already got the alarm and tenderduty restarts: neither check resends it.
func TestOpRestartNoResendAnywhereAfterRestore(t *testing.T) {
	r := newOpRestartRig(t)
	c := r.start(r.yaml(opDests...))
	r.boot(c)
	r.h.down("AlphaOne")
	r.settle()
	r.expect("before restart", opAll(1, 0))
	r.restart(c)

	c = r.start(r.yaml(opDests...))
	r.boot(c)
	r.settle()
	r.h.down("AlphaOne")
	r.settle()
	r.expect("after restart", opAll(1, 0))
}

// Died after the raise was queued, before sending: each destination gets it once, then never again.
func TestOpRestartSendsRaiseLeftInOutboxOnce(t *testing.T) {
	r := newOpRestartRig(t)
	c := r.start(r.yaml(opDests...))
	r.h.down("AlphaOne")
	if n := opOutboxLen(); n != len(opDests) {
		t.Fatalf("%d queued, want %d", n, len(opDests))
	}
	r.restart(c)

	c = r.start(r.yaml(opDests...))
	r.boot(c)
	r.settle()
	r.expect("first start", opAll(1, 0))
	r.restart(c)

	c = r.start(r.yaml(opDests...))
	r.boot(c)
	r.settle()
	r.expect("second start", opAll(1, 0))
}

// Four destinations got the raise, slack was added while down: one recovery each, none for slack.
func TestOpRestartResolvedWhileDownOneRecoveryPerRaise(t *testing.T) {
	r := newOpRestartRig(t)
	c := r.start(r.yaml("telegram", "pagerduty", "discord", "gotify"))
	r.boot(c)
	r.h.down("AlphaOne")
	r.settle()
	r.restart(c)

	c = r.start(r.yaml(opDests...))
	r.boot(c)
	r.h.up("AlphaOne")
	r.h.up("AlphaOne")
	r.settle()
	want := opAll(1, 1)
	want["slack"] = opSent{}
	r.expect("after recovery", want)
}

// PagerDuty opened with the valcons key: after a restart the recovery must still close with that key.
func TestOpRestartPagerDutyResolveUsesTriggerKey(t *testing.T) {
	r := newOpRestartRig(t)
	msg := nodeDownMessage("http://a1:26657", "alphaone-1")
	trigger := "valcons-consensus-key"
	c := r.start(r.yaml("pagerduty"))
	r.boot(c)
	c.alert("AlphaOne", msg, "critical", false, &trigger)
	r.settle()
	r.restart(c)

	c = r.start(r.yaml("pagerduty"))
	r.boot(c)
	nodeKey := "http://a1:26657"
	c.alert("AlphaOne", msg, "info", true, &nodeKey)
	r.settle()
	triggers, resolves := r.pdKeys()
	if !reflect.DeepEqual(triggers, []string{trigger}) || !reflect.DeepEqual(resolves, []string{trigger}) {
		t.Fatalf("triggers %v, resolves %v, want both %q", triggers, resolves, trigger)
	}
}

// Node recovered before tenderduty sent the recovery: after restart each destination gets one, then none.
func TestOpRestartQueuedRecoveryDeliveredOnce(t *testing.T) {
	r := newOpRestartRig(t)
	c := r.start(r.yaml(opDests...))
	r.boot(c)
	r.h.down("AlphaOne")
	r.settle()
	r.restart(c)

	c = r.start(r.yaml(opDests...))
	r.h.up("AlphaOne")
	r.restart(c)

	c = r.start(r.yaml(opDests...))
	r.boot(c)
	r.settle()
	r.expect("first start", opAll(1, 1))
	r.restart(c)

	c = r.start(r.yaml(opDests...))
	r.boot(c)
	r.settle()
	r.expect("second start", opAll(1, 1))
}

// Grouped card still waiting at restart: one card with both nodes per destination, and no repeat.
func TestOpRestartGroupedCardAcrossRestart(t *testing.T) {
	r := newOpRestartRig(t)
	r.extra = "group_node_alerts: yes\ngroup_wait_seconds: 2\n"
	c := r.start(r.yaml("telegram", "discord"))
	r.h.down("AlphaOne")
	r.h.down("AlphaTwo")
	r.restart(c)

	c = r.start(r.yaml("telegram", "discord"))
	r.boot(c)
	r.settle()
	sent := r.h.telegramSent()
	if len(sent) != 1 || !strings.Contains(sent[0].text, "2 nodes down") {
		t.Fatalf("telegram got %+v, want one card with 2 nodes down", sent)
	}
	r.expect("first start", map[string]opSent{"telegram": {1, 0}, "discord": {1, 0}})
	r.restart(c)

	c = r.start(r.yaml("telegram", "discord"))
	r.boot(c)
	r.settle()
	r.expect("second start", map[string]opSent{"telegram": {1, 0}, "discord": {1, 0}})
}

// Alarm delivered a day ago: after restart it is still active but must not be announced again.
func TestOpRestartAlarmOlderThanADayIsNotRaisedAgain(t *testing.T) {
	r := newOpRestartRig(t)
	c := r.start(r.yaml(opDests...))
	r.boot(c)
	msg, id := nodeDownMessage("http://a1:26657", "alphaone-1"), "http://a1:26657"
	c.recordSince("AlphaOne", msg, "critical", false, false, &id, time.Now().Add(-25*time.Hour))
	r.settle()
	r.expect("raised a day ago", opAll(1, 0))
	opAgeSent(25 * time.Hour)
	r.restart(c)

	c = r.start(r.yaml(opDests...))
	r.boot(c)
	r.h.down("AlphaOne") // reconcile pass, the alarm is still active
	r.settle()
	r.expect("after restart", opAll(1, 0))
}

// Node came back while down, alarm raised a day ago: every destination that got it gets a recovery.
func TestOpRestartAlarmOlderThanADayRecoversEverywhere(t *testing.T) {
	r := newOpRestartRig(t)
	c := r.start(r.yaml(opDests...))
	r.boot(c)
	msg, id := nodeDownMessage("http://a1:26657", "alphaone-1"), "http://a1:26657"
	c.recordSince("AlphaOne", msg, "critical", false, false, &id, time.Now().Add(-25*time.Hour))
	r.settle()
	opAgeSent(25 * time.Hour)
	r.restart(c)

	c = r.start(r.yaml(opDests...))
	r.boot(c)
	r.h.up("AlphaOne")
	r.settle()
	r.expect("after recovery", opAll(1, 1))
}

// A chain file added while running: the new chain is watched, the active alarm stays.
func TestOpReloadAddedChainLeavesRunningAlarms(t *testing.T) {
	r := newOpRestartRig(t)
	c := r.start(r.yaml("telegram"))
	r.boot(c)
	r.h.down("AlphaOne")
	r.settle()
	r.expect("before reload", map[string]opSent{"telegram": {1, 0}})

	r.writeChain("betatwo", "chain_id: betatwo-1\nvaloper_address: betatwovaloper\nnodes:\n  - url: http://127.0.0.1:1\n    alert_if_down: yes\n")
	if err := r.reload(c); err != nil {
		t.Fatal(err)
	}
	c.chainsMux.RLock()
	added := c.Chains["betatwo"]
	c.chainsMux.RUnlock()
	if added == nil || added.cancel == nil {
		t.Fatal("added chain is not monitored")
	}
	if !alarmIsActive("AlphaOne", nodeDownMessage("http://a1:26657", "alphaone-1")) {
		t.Fatal("reload changed the active alarm")
	}
	r.settle()
	r.expect("after reload", map[string]opSent{"telegram": {1, 0}})
	r.stop(c)
}

// A chain file removed while running: its alarm clears once, and a second reload sends nothing.
func TestOpReloadRemovedChainClearsOnce(t *testing.T) {
	r := newOpRestartRig(t)
	r.writeChain("betatwo", "chain_id: betatwo-1\nvaloper_address: betatwovaloper\nnodes:\n  - url: http://127.0.0.1:1\n    alert_if_down: yes\n")
	c := r.start(r.yaml("telegram"))
	r.boot(c)
	r.h.down("AlphaOne")
	r.h.down("betatwo")
	r.settle()
	r.expect("before removal", map[string]opSent{"telegram": {2, 0}})

	if err := os.Remove(filepath.Join(r.chains, "betatwo.yml")); err != nil {
		t.Fatal(err)
	}
	if err := r.reload(c); err != nil {
		t.Fatal(err)
	}
	r.settle()
	r.expect("after removal", map[string]opSent{"telegram": {2, 1}})
	if err := r.reload(c); err != nil {
		t.Fatal(err)
	}
	r.settle()
	r.expect("after a second reload", map[string]opSent{"telegram": {2, 1}})
	if alarmIsActive("betatwo", nodeDownMessage("http://127.0.0.1:1", "betatwo-1")) {
		t.Fatal("removed chain's alarm is still active")
	}
	r.stop(c)
}

// A broken config: the old one keeps running, the error is reported once, new alarms still go out.
func TestOpReloadBrokenYAMLKeepsMonitoring(t *testing.T) {
	r := newOpRestartRig(t)
	c := r.start(r.yaml("telegram"))
	r.boot(c)
	r.h.down("AlphaOne")
	r.settle()
	before := c.settings()

	r.write("telegram: [\n")
	if err := r.reload(c); err == nil {
		t.Fatal("broken YAML was accepted")
	}
	if c.settings() != before {
		t.Fatal("the failed reload replaced the running settings")
	}
	id := "warn-id"
	c.alert("AlphaOne", "validator has missed 5 blocks on alphaone-1", "critical", false, &id)
	r.settle()
	sent := r.h.telegramSent()
	notice := false
	for _, m := range sent {
		notice = notice || strings.Contains(m.text, "Config reload failed")
	}
	if !notice {
		t.Fatalf("no reload error reached telegram: %+v", sent)
	}
	r.expect("after broken reload", map[string]opSent{"telegram": {3, 0}})
	r.stop(c)
}

// telegram min_severity raised to critical, then lowered to warning: only later warnings go out.
func TestOpReloadMinSeverityChange(t *testing.T) {
	r := newOpRestartRig(t)
	r.tgMin = "critical"
	c := r.start(r.yaml("telegram"))
	r.boot(c)
	warn, warnID := "validator has missed 5 blocks on alphaone-1", "warn-id"
	c.alert("AlphaOne", warn, "warning", false, &warnID)
	r.settle()
	r.expect("warning at critical", map[string]opSent{})

	r.tgMin = "warning"
	r.write(r.yaml("telegram"))
	if err := r.reload(c); err != nil {
		t.Fatal(err)
	}
	next, nextID := "validator has missed 7 blocks on alphaone-1", "next-id"
	c.alert("AlphaOne", next, "warning", false, &nextID)
	r.settle()
	r.expect("new warning after lowering", map[string]opSent{"telegram": {1, 0}})

	c.alert("AlphaOne", warn, "info", true, &warnID)
	r.settle()
	r.expect("recovery of the warning never sent", map[string]opSent{"telegram": {1, 0}})
	r.stop(c)
}

// Two quick reloads add the same chain once and keep its state; removing it clears the alarm once.
func TestOpReloadTwiceQuickly(t *testing.T) {
	r := newOpRestartRig(t)
	c := r.start(r.yaml("telegram"))
	r.boot(c)
	r.writeChain("betatwo", "chain_id: betatwo-1\nvaloper_address: betatwovaloper\nnodes:\n  - url: http://127.0.0.1:1\n    alert_if_down: yes\n")
	if err := r.reload(c); err != nil {
		t.Fatal(err)
	}
	c.chainsMux.RLock()
	first := c.Chains["betatwo"]
	c.chainsMux.RUnlock()
	if err := r.reload(c); err != nil {
		t.Fatal(err)
	}
	c.chainsMux.RLock()
	second := c.Chains["betatwo"]
	c.chainsMux.RUnlock()
	if first == nil || first != second {
		t.Fatal("second reload restarted the chain it just added")
	}
	r.h.down("betatwo")
	r.settle()

	if err := os.Remove(filepath.Join(r.chains, "betatwo.yml")); err != nil {
		t.Fatal(err)
	}
	if err := r.reload(c); err != nil {
		t.Fatal(err)
	}
	if err := r.reload(c); err != nil {
		t.Fatal(err)
	}
	r.settle()
	r.expect("after removal", map[string]opSent{"telegram": {1, 1}})
	r.stop(c)
}

// A PID file left by an exited process: reload is refused and nothing is signalled.
func TestOpReloadDeadPIDIsRefused(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	if err := cmd.Run(); err != nil {
		t.Skipf("could not run a child process: %v", err)
	}
	state := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(state+".pid", []byte(strconv.Itoa(cmd.Process.Pid)), 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	reloadAfterEdit(state, &out, nil)
	if !strings.Contains(out.String(), "Saved, but reload failed") || strings.Contains(out.String(), "Applied") {
		t.Fatalf("dead PID reported as: %q", out.String())
	}
}
