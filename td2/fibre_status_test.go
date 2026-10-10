package tenderduty

import (
	"encoding/json"
	"html"
	"regexp"
	"strings"
	"testing"
	"time"

	dash "github.com/kynraze/tenderduty/v2/td2/dashboard"
)

const statusHost = "fibre.example.com:7980"

// A registered validator on Mocha with 0.296% of the stake (the 148 minimum applies). With metrics, exporting for 10
// minutes: 42 uploads, 49 of 50 signatures fast, 12 MiB of the 74 GiB budget, last upload 18s ago, last export 34s ago.
func statusRig(t *testing.T, metrics bool, tweak func(f *fibreFeed)) *fibreRig {
	t.Helper()
	r := newFibreRig(t)
	r.cc.Alerts.FibreInstance = "fibre-host-1"
	r.cc.valInfo.VotingPower, r.cc.valInfo.TotalPower = 296, 100_000
	r.cc.fibreFullStake = 2 << 40
	r.cc.fibre = fibreState{Registered: true, Host: statusHost, Reachable: true, KeyKnown: true, IdentityOK: true}
	if !metrics {
		return r
	}
	f := r.feed(48 * time.Hour)
	totals := statusTotals{uploadsA: 100, uploadSecA: 2}
	f.r.c.ingestFibreMetrics(f.statusData(totals)) // a baseline
	r.advance(10 * time.Minute)
	totals.uploadsA, totals.uploadSecA = 142, 2.84
	totals.signOK, totals.signFast, totals.signSec = 50, 49, 0.2
	f.occupancy, f.budget = i64(12<<20), i64(fibreBudget(2<<40, 148))
	f.lastSuccess = i64(r.clock.now.Add(34*time.Second - 18*time.Second).Unix())
	if tweak != nil {
		tweak(f)
	}
	f.r.c.ingestFibreMetrics(f.statusData(totals))
	r.advance(34 * time.Second)
	return r
}

func (r *fibreRig) status() string {
	r.t.Helper()
	report := r.cc.fibreReport(r.clock.now)
	if report == nil {
		r.t.Fatal("no Fibre report")
	}
	return strings.Join(report.telegramLines(), "\n")
}

func TestFibreStatusText(t *testing.T) {
	const head = "🧵 <b>Fibre</b> <code>v10.4.0-mocha</code>"
	const host = "🌐 <b>Host:</b> <code>" + statusHost + "</code>"
	const reachable = "📡 <b>Reachable:</b> 🟢 yes"
	const verified = "🔐 <b>Identity:</b> 🟢 verified"
	const share = "📊 <b>Share:</b> 148/4096 rows · 3.61% min"
	const used = "🗃 <b>Shards:</b> 12 MiB of 74 GiB budget (0.02%)"
	const uploads = "⬆️ <b>Uploads 10m:</b> ✅ 42 · ❌ 0 · 🚫 0"
	const last = "🕒 <b>Last successful upload:</b> 18s ago"
	const signing = "✍️ <b>Signing:</b> 🟢 98% under 10 ms"
	const storage = "🗄 <b>Storage:</b> 🟢 ok"
	const received = "📨 <b>Metrics:</b> 34s ago"
	const noMetrics = "📨 <b>Metrics:</b> ⚪ not received, see docs/fibre.md"
	metrics := func(lines ...string) []string { return append(lines, uploads, last, signing, storage, received) }
	with := func(lines ...string) []string {
		return metrics(append([]string{head, host, reachable, verified}, lines...)...)
	}
	for _, tc := range []struct {
		name    string
		metrics bool
		feed    func(f *fibreFeed)
		chain   func(cc *ChainConfig)
		want    []string
	}{
		{"all good", true, nil, nil, with(share, used)},
		{"unreachable", true, nil, func(cc *ChainConfig) {
			cc.fibre.Reachable, cc.fibre.DownSince, cc.fibre.DownReason = false, time.Date(2026, 10, 3, 12, 5, 34, 0, time.UTC), "connection refused"
		}, metrics(head, host, "📡 <b>Reachable:</b> 🔴 no, for 5m (connection refused)", verified, share, used)},
		{"unreachable with no reason", true, nil, func(cc *ChainConfig) { cc.fibre.Reachable, cc.fibre.DownReason = false, "" },
			metrics(head, host, "📡 <b>Reachable:</b> 🔴 no", verified, share, used)},
		{"not registered", true, nil, func(cc *ChainConfig) { cc.fibre = fibreState{} },
			metrics(head, "🌐 <b>Host:</b> 🔴 not registered on the chain", share, used)},
		{"identity failed", true, nil, func(cc *ChainConfig) { cc.fibre.IdentityOK, cc.fibre.IdentityError = false, "not signed" },
			metrics(head, host, reachable, "🔐 <b>Identity:</b> 🔴 NOT verified", share, used)},
		{"no consensus key", true, nil, func(cc *ChainConfig) { cc.fibre.KeyKnown = false },
			metrics(head, host, reachable, "🔐 <b>Identity:</b> ⚪ not checked", share, used)},
		{"no metrics", false, nil, nil, []string{"🧵 <b>Fibre</b>", host, reachable, verified, share, "🗃 <b>Shards:</b> — of 74 GiB budget", noMetrics}},
		{"no metrics and not registered", false, nil, func(cc *ChainConfig) { cc.fibre = fibreState{} },
			[]string{"🧵 <b>Fibre</b>", "🌐 <b>Host:</b> 🔴 not registered on the chain", share, "🗃 <b>Shards:</b> — of 74 GiB budget", noMetrics}},
		{"unlimited budget", true, func(f *fibreFeed) { f.budget = i64(0) }, nil,
			with(share, "🗃 <b>Shards:</b> 12 MiB · unlimited budget")},
		{"not in the active set", true, nil, func(cc *ChainConfig) { cc.valInfo.VotingPower = 0 },
			with("📊 <b>Share:</b> ⚪ not in the active set, no rows", "🗃 <b>Shards:</b> 12 MiB (0.02%)")},
		{"the chain's parameter not read", true, nil, func(cc *ChainConfig) { cc.fibreFullStake = 0 }, with(share, "🗃 <b>Shards:</b> 12 MiB (0.02%)")},
		{"the validator set not read", true, nil, func(cc *ChainConfig) { cc.valInfo.VotingPower, cc.valInfo.TotalPower = 0, 0 }, with("🗃 <b>Shards:</b> 12 MiB (0.02%)")},
		{"Fibre reports another budget", true, func(f *fibreFeed) { f.budget = i64(70 << 30) }, nil,
			with(share, "🗃 <b>Shards:</b> 12 MiB of 74 GiB budget (0.02%) (Fibre reports 70 GiB)")},
		{"a share above the minimum", true, nil, func(cc *ChainConfig) { cc.valInfo.VotingPower = 25_000 },
			with("📊 <b>Share:</b> 3072/4096 rows · 75%", "🗃 <b>Shards:</b> 12 MiB of 1.5 TiB budget (0.02%) (Fibre reports 74 GiB)")},
		{"every row", true, nil, func(cc *ChainConfig) { cc.valInfo.VotingPower = 50_000 },
			with("📊 <b>Share:</b> 4096/4096 rows · 100% max", "🗃 <b>Shards:</b> 12 MiB of 2 TiB budget (0.02%) (Fibre reports 74 GiB)")},
		{"a little used", true, func(f *fibreFeed) { f.occupancy = i64(1 << 20) }, nil, with(share, "🗃 <b>Shards:</b> 1 MiB of 74 GiB budget (&lt;0.01%)")},
		{"storage errors", true, func(f *fibreFeed) { f.put, f.prune, f.timeout = 2, 1, 1 }, nil,
			[]string{head, host, reachable, verified, share, used + " · object", uploads, last, signing, "🗄 <b>Storage:</b> 🔴 4 errors", received}},
		{"one storage error", true, func(f *fibreFeed) { f.err = 1 }, nil,
			[]string{head, host, reachable, verified, share, used + " · object", uploads, last, signing, "🗄 <b>Storage:</b> 🔴 1 error", received}},
		{"text from outside is escaped", true, nil, func(cc *ChainConfig) {
			cc.fibre.Host, cc.fibre.Reachable, cc.fibre.DownSince, cc.fibre.DownReason = "h<1>:1", false, time.Time{}, `a<b> & "c"`
		}, metrics(head, "🌐 <b>Host:</b> <code>h&lt;1&gt;:1</code>", "📡 <b>Reachable:</b> 🔴 no (a&lt;b&gt; &amp; &#34;c&#34;)", verified, share, used)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := statusRig(t, tc.metrics, tc.feed)
			if tc.chain != nil {
				tc.chain(r.cc)
			}
			want := append([]string{tc.want[0], "<b>Endorsed (24h):</b> none seen yet"}, tc.want[1:]...)
			if got := r.status(); got != strings.Join(want, "\n") {
				t.Fatalf("got\n%s\nwant\n%s", got, strings.Join(want, "\n"))
			}
			if strings.Contains(r.status(), "<b>Budget:</b>") || strings.Contains(r.status(), "<b>Storage backend:</b>") {
				t.Fatal("separate Budget or Storage backend line")
			}
			telegramHTML(t, r.status())
			// keep lines short for phones
			for _, line := range strings.Split(r.status(), "\n") {
				limit := 60
				if strings.Contains(line, "<b>Shards:</b>") {
					limit = 80
				}
				if n := len([]rune(regexp.MustCompile(`<[^>]*>`).ReplaceAllString(line, ""))); n > limit {
					t.Errorf("%d characters: %s", n, line)
				}
			}
		})
	}
}

func TestFibreStatusBackendsOnShards(t *testing.T) {
	for _, backends := range [][]string{{"local"}, {"object"}, {"object", "local"}, {"s3<&>"}} {
		for _, occupancy := range []bool{false, true} {
			r := &fibreReport{Metrics: fibreSnapshot{Found: true, HaveOccupancy: occupancy, Occupancy: 12 << 20, BackendNames: backends}}
			used := "—"
			if occupancy {
				used = "12 MiB"
			}
			want := "🗃 <b>Shards:</b> " + used + " · " + html.EscapeString(strings.Join(backends, ", "))
			got := strings.Join(r.telegramLines(), "\n")
			if !strings.Contains(got, want) || strings.Contains(got, "<b>Storage backend:</b>") {
				t.Fatalf("got %q, want %q without a separate Storage backend line", got, want)
			}
			telegramHTML(t, got)
		}
	}
}

// fails on a < or & that isn't a telegram tag or entity
func telegramHTML(t *testing.T, reply string) {
	t.Helper()
	text := regexp.MustCompile(`</?(?:b|i|code)>`).ReplaceAllString(reply, "")
	text = regexp.MustCompile(`&(?:lt|gt|amp|quot|#\d+);`).ReplaceAllString(text, "_")
	if i := strings.IndexAny(text, "<>&"); i >= 0 {
		t.Errorf("not telegram HTML at %q", text[i:])
	}
}

func TestFibreStatusTextEdges(t *testing.T) {
	t.Run("one upload", func(t *testing.T) {
		r := newFibreRig(t)
		r.cc.Alerts.FibreInstance = "fibre-host-1"
		r.cc.valInfo.VotingPower, r.cc.valInfo.TotalPower = 296, 100_000
		f := r.feed(time.Minute)
		f.r.c.ingestFibreMetrics(f.statusData(statusTotals{uploadsA: 1, uploadSecA: 0.02}))
		r.advance(3 * time.Minute)
		got := r.status()
		// a window this process hasn't watched in full says how long it did
		for _, want := range []string{"⬆️ <b>Uploads 3m:</b> ✅ 1 · ❌ 0 · 🚫 0", "🕒 <b>Last successful upload:</b> 3m ago",
			"✍️ <b>Signing:</b> ⚪ no signatures yet", "📨 <b>Metrics:</b> 3m ago"} {
			if !strings.Contains(got, want) {
				t.Errorf("no %q in\n%s", want, got)
			}
		}
	})
	t.Run("signing seen only before the last 10 minutes", func(t *testing.T) {
		r := statusRig(t, true, nil)
		r.advance(11 * time.Minute)
		f := r.feed(48 * time.Hour)
		r.c.ingestFibreMetrics(f.data())
		if got := r.status(); !strings.Contains(got, "✍️ <b>Signing:</b> 🟢 98% under 10 ms over 21m") {
			t.Fatalf("%s", got)
		}
	})
	for _, tc := range []struct {
		name             string
		ok, fast, failed int64
		want             string
	}{
		{"a hundred percent is all of them", 1000, 999, 0, "🟢 99% under 10 ms"},
		{"mostly slow", 1000, 400, 0, "🟡 40% under 10 ms"},
		{"some failed", 1000, 999, 3, "🔴 3 failed · 99% under 10 ms"},
		{"all failed", 0, 0, 5, "🔴 all 5 failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newFibreRig(t)
			r.cc.Alerts.FibreInstance = "fibre-host-1"
			f := r.feed(time.Minute)
			f.r.c.ingestFibreMetrics(f.statusData(statusTotals{signOK: tc.ok, signFast: tc.fast, signFailed: tc.failed, signSec: 1}))
			if got := r.status(); !strings.Contains(got, "✍️ <b>Signing:</b> "+tc.want+"\n") {
				t.Fatalf("%s", got)
			}
		})
	}
}

func TestFibreStatusInStatusDetail(t *testing.T) {
	r := statusRig(t, true, nil)
	alarms.AllAlarms["Chain"] = map[string]time.Time{"Severity: critical\nRPC node http://b has been down for > 3 minutes on celestia": time.Now()}
	r.cc.Nodes = []*NodeConfig{{Url: "http://a"}}
	detail := r.cc.statusDetail("Chain", r.clock.now)
	rpc, fibre, active := strings.Index(detail, "<b>RPC:</b>"), strings.Index(detail, "<b>Fibre</b>"), strings.Index(detail, "<b>Active alerts</b>")
	if rpc < 0 || fibre < rpc || active < fibre {
		t.Fatalf("Fibre is not between the RPC line and the alerts:\n%s", detail)
	}
	if !strings.Contains(detail, r.status()) {
		t.Fatalf("the detail has other Fibre lines than the report:\n%s", detail)
	}
}

func TestFibreStatusOnlyForWatchedChains(t *testing.T) {
	for _, tc := range []struct {
		name  string
		chain func(cc *ChainConfig)
	}{
		{"fibre alerts off", func(cc *ChainConfig) { cc.Alerts.FibreAlerts = false }},
		{"no x/valaddr", func(cc *ChainConfig) { cc.fibreUnsupported = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := statusRig(t, true, nil)
			tc.chain(r.cc)
			if r.cc.fibreReport(r.clock.now) != nil {
				t.Fatal("a report was made")
			}
			if strings.Contains(r.cc.statusDetail("Chain", r.clock.now), "Fibre") {
				t.Fatal("/status shows Fibre")
			}
			status := r.cc.dashboardStatus()
			b, err := json.Marshal(status)
			if err != nil {
				t.Fatal(err)
			}
			if status.Fibre != nil || strings.Contains(string(b), `"fibre"`) {
				t.Fatalf("the dashboard has Fibre: %s", b)
			}
		})
	}
}

func TestFibreStatusBeforeTheFirstCheck(t *testing.T) {
	r := statusRig(t, true, nil)
	r.cc.fibreLoaded = false
	if got := r.status(); got != "🧵 <b>Fibre</b>\n⏳ Not checked yet, the first check runs within a minute\n<b>Endorsed (24h):</b> none seen yet" {
		t.Fatalf("%q", got)
	}
	if status := r.cc.dashboardStatus(); status.Fibre != nil {
		t.Fatalf("the dashboard has Fibre before it was checked: %+v", status.Fibre)
	}
}

func TestFibreStatusJSON(t *testing.T) {
	r := statusRig(t, true, nil)
	b, err := json.Marshal(r.cc.dashboardStatus())
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Fibre map[string]interface{} `json:"fibre"`
	}
	if err := json.Unmarshal(b, &got); err != nil || got.Fibre == nil {
		t.Fatalf("no fibre in %s: %v", b, err)
	}
	var status dash.ChainStatus
	if err := json.Unmarshal(b, &status); err != nil {
		t.Fatal(err)
	}
	f := status.Fibre
	if !f.Registered || f.Host != statusHost || !f.Reachable || f.Identity != "verified" || f.Version != "v10.4.0-mocha" || f.DownSince != 0 {
		t.Fatalf("provider %+v", f)
	}
	if !f.InActiveSet || f.Rows != 148 || f.TotalRows != 4096 || !f.MinimumApplies || f.ChainBudget != 79_456_894_976 || f.RowPercent < 3.61 || f.RowPercent > 3.62 {
		t.Fatalf("share %+v", f)
	}
	if !f.MetricsReceived || !f.BudgetReported || f.FibreBudget != 79_456_894_976 || f.Unlimited || !f.OccupancyKnown || f.Occupancy != 12<<20 ||
		f.LastUploadAt != r.clock.now.Add(-18*time.Second).Unix() || f.MetricsAt != r.clock.now.Add(-34*time.Second).Unix() {
		t.Fatalf("metrics %+v", f)
	}
	w := f.Last10m
	if w.Seconds != 600 || w.UploadsOK != 42 || w.UploadsFailed != 0 || w.Signatures != 50 || w.SignaturesFailed != 0 ||
		w.SignFastPercent == nil || *w.SignFastPercent != 98 || w.AvgUploadMs == nil || !near(*w.AvgUploadMs, 20) || w.AvgSignMs == nil || !near(*w.AvgSignMs, 4) {
		t.Fatalf("10 minutes %+v", w)
	}
	// omit unmeasured averages and report observed coverage
	if f.Last24h.Seconds != 634 || f.Last24h.UploadsOK != 42 {
		t.Fatalf("24 hours %+v", f.Last24h)
	}
	window := got.Fibre["last_24h"].(map[string]interface{})
	for _, key := range []string{"avg_download_ms", "avg_put_ms", "avg_prune_ms"} {
		if _, ok := window[key]; ok {
			t.Errorf("%s is in %v with nothing measured", key, window)
		}
	}
	for _, key := range []string{"avg_upload_ms", "sign_fast_percent", "uploads_ok", "seconds"} {
		if _, ok := window[key]; !ok {
			t.Errorf("%s is not in %v", key, window)
		}
	}

	r = statusRig(t, false, nil)
	f = r.cc.dashboardStatus().Fibre
	if f == nil || f.MetricsReceived || f.Rows != 148 || f.ChainBudget != 79_456_894_976 || f.Last10m.Seconds != 0 || f.Last24h.UploadsOK != 0 {
		t.Fatalf("without metrics %+v", f)
	}
	// inactive validators have no rows or chain budget
	r.cc.valInfo.VotingPower = 0
	if f = r.cc.dashboardStatus().Fibre; f.InActiveSet || f.Rows != 0 || f.ChainBudget != 0 || f.MinimumApplies {
		t.Fatalf("not in the active set %+v", f)
	}
	// unlimited is Fibre's own report of a budget of 0
	r = statusRig(t, true, func(f *fibreFeed) { f.budget = i64(0) })
	if f = r.cc.dashboardStatus().Fibre; !f.Unlimited || !f.BudgetReported || f.FibreBudget != 0 {
		t.Fatalf("unlimited %+v", f)
	}
	r.cc.fibre.Reachable, r.cc.fibre.DownSince, r.cc.fibre.DownReason = false, r.clock.now.Add(-5*time.Minute), "connection refused"
	if f = r.cc.dashboardStatus().Fibre; f.Reachable || f.DownSince != r.clock.now.Add(-5*time.Minute).Unix() || f.DownReason != "connection refused" {
		t.Fatalf("unreachable %+v", f)
	}
}
