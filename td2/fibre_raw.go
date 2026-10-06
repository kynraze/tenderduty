package tenderduty

import (
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/prometheus/client_golang/prometheus"
	commonv1 "go.opentelemetry.io/proto/otlp/common/v1"
	metricsv1 "go.opentelemetry.io/proto/otlp/metrics/v1"
)

const (
	// per chain, limits label memory
	fibreRawMaxSeries = 20000
	// as the collector's exporter expires series
	fibreRawStale = 10 * time.Minute
	// skip series exceeding label-count or name-length limits
	fibreRawMaxLabels = 32
	fibreRawMaxName   = 256 // bytes of a metric or label name
	fibreRawMaxValue  = 256 // bytes of a label value, cut
	fibreRawMaxHelp   = 512 // bytes of the help text, cut
)

type fibreRawKind uint8

const (
	fibreRawGauge fibreRawKind = iota
	fibreRawCounter
	fibreRawHistogram
)

// The name and chain_id labels are added when scraped; a chain's id can change on reload.
type fibreRawSeries struct {
	name   string
	help   string
	kind   fibreRawKind
	labels map[string]string
	value  float64
	count  uint64
	sum    float64
	bounds []float64
	counts []uint64 // per bucket as OTLP sends them, len(bounds)+1
	seen   time.Time
}

// OpenTelemetry Collector unit names for the Prometheus exporter
var fibreRawUnits = map[string]string{
	"d": "days", "h": "hours", "min": "minutes", "s": "seconds", "ms": "milliseconds", "us": "microseconds", "ns": "nanoseconds",
	"By": "bytes", "KiBy": "kibibytes", "MiBy": "mebibytes", "GiBy": "gibibytes", "TiBy": "tebibytes",
	"KBy": "kilobytes", "MBy": "megabytes", "GBy": "gigabytes", "TBy": "terabytes",
	"m": "meters", "V": "volts", "A": "amperes", "J": "joules", "W": "watts", "g": "grams", "Cel": "celsius", "Hz": "hertz",
	"1": "", "%": "percent",
}

var fibreRawPerUnits = map[string]string{"s": "second", "m": "minute", "h": "hour", "d": "day", "w": "week", "mo": "month", "y": "year"}

var fibreRawAnnotation = regexp.MustCompile(`\{[^}]*\}`)

// names of other collectors on the endpoint; a clash fails the whole scrape
var fibreRawReserved = []string{"tenderduty_", "promhttp_", "process_", "go_info", "go_threads", "go_goroutines", "go_gc_",
	"go_memstats_", "go_sched_", "go_cpu_", "go_cgo_", "go_godebug_", "go_sync_", "go_memory_classes_"}

func fibreRawTokens(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}

func fibreRawHas(tokens []string, t string) bool {
	for _, have := range tokens {
		if have == t {
			return true
		}
	}
	return false
}

// Use Collector naming: append units, _total for counters and ratio for gauges.
func fibreRawName(name, unit string, counter, gauge bool) string {
	tokens := fibreRawTokens(name)
	if len(tokens) == 0 {
		return ""
	}
	unit = fibreRawAnnotation.ReplaceAllString(unit, "")
	main, per, _ := strings.Cut(unit, "/")
	main, per = strings.TrimSpace(main), strings.TrimSpace(per)
	if word, ok := fibreRawUnits[main]; ok {
		main = word
	}
	if word, ok := fibreRawPerUnits[per]; ok {
		per = word
	}
	for _, t := range fibreRawTokens(main) {
		if !fibreRawHas(tokens, t) {
			tokens = append(tokens, t)
		}
	}
	if per = strings.Join(fibreRawTokens(per), "_"); per != "" && !fibreRawHas(tokens, per) {
		tokens = append(tokens, "per", per)
	}
	if unit == "1" && gauge && !fibreRawHas(tokens, "ratio") {
		tokens = append(tokens, "ratio")
	}
	if counter {
		kept := tokens[:0]
		for _, t := range tokens {
			if t != "total" {
				kept = append(kept, t)
			}
		}
		tokens = append(kept, "total")
	}
	out := strings.Join(tokens, "_")
	if unicode.IsDigit(rune(out[0])) {
		out = "_" + out
	}
	return out
}

func fibreRawLabel(key string) string {
	out := []rune(key)
	for i, r := range out {
		if !(r < unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r))) && r != '_' {
			out[i] = '_'
		}
	}
	label := string(out)
	switch {
	case label == "":
	case unicode.IsDigit(out[0]):
		label = "key_" + label
	case out[0] == '_' && !strings.HasPrefix(label, "__"):
		label = "key" + label
	}
	return label
}

// Sort keys to resolve sanitized-name collisions consistently.
func fibreRawAdd(labels map[string]string, attrs []*commonv1.KeyValue) {
	sorted := append([]*commonv1.KeyValue(nil), attrs...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].GetKey() < sorted[j].GetKey() })
	for _, kv := range sorted {
		if label := fibreRawLabel(kv.GetKey()); label != "" {
			labels[label] = clipBytes(fibreAttrText(kv.GetValue()), fibreRawMaxValue)
		}
	}
}

func fibreRawKey(name string, kind fibreRawKind, labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k, v := range labels {
		keys = append(keys, k+"\xfe"+v)
	}
	sort.Strings(keys)
	return name + "\xff" + string(rune('0'+kind)) + "\xff" + strings.Join(keys, "\xff")
}

func (st *fibreStore) fibreRawNote(key string) bool {
	if st.rawNotes == nil {
		st.rawNotes = make(map[string]bool)
	}
	if st.rawNotes[key] {
		return false
	}
	st.rawNotes[key] = true
	return true
}

// nil when the series is over a limit or the chain is at its series cap.
func (st *fibreStore) rawSeries(chain, name, help string, kind fibreRawKind, labels map[string]string, now time.Time) *fibreRawSeries {
	if len(labels) > fibreRawMaxLabels || len(name) > fibreRawMaxName || fibreRawLongLabel(labels) {
		st.rawSkipped++
		if st.fibreRawNote("limits:" + chain) {
			l("fibre_metrics export_raw: skipping series with more than", fibreRawMaxLabels, "labels or names over", fibreRawMaxName, "bytes from Fibre for", chain+", first was", strconv.Quote(clipBytes(name, 64)))
		}
		return nil
	}
	if st.raw == nil {
		st.raw = make(map[string]map[string]*fibreRawSeries)
	}
	series := st.raw[chain]
	if series == nil {
		series = make(map[string]*fibreRawSeries)
		st.raw[chain] = series
	}
	key := fibreRawKey(name, kind, labels)
	s := series[key]
	if s == nil {
		if len(series) >= fibreRawMaxSeries {
			if st.fibreRawNote("full:" + chain) {
				l("fibre_metrics export_raw: more than", fibreRawMaxSeries, "series from Fibre for", chain+", new ones are dropped until old ones go stale")
			}
			return nil
		}
		s = &fibreRawSeries{name: name, help: clipBytes(help, fibreRawMaxHelp), kind: kind, labels: labels}
		series[key] = s
	}
	s.seen = now
	return s
}

func fibreRawLongLabel(labels map[string]string) bool {
	for label := range labels {
		if len(label) > fibreRawMaxName {
			return true
		}
	}
	return false
}

func (st *fibreStore) dropRaw() {
	st.mu.Lock()
	st.raw = nil
	st.mu.Unlock()
}

// Caller holds the lock.
func (st *fibreStore) sweepRaw(now time.Time) {
	for chain, series := range st.raw {
		for key, s := range series {
			if now.Sub(s.seen) >= fibreRawStale {
				delete(series, key)
			}
		}
		if len(series) == 0 {
			delete(st.raw, chain)
		}
	}
}

func fibreRawGaugeNumber(dp *metricsv1.NumberDataPoint) (float64, bool) {
	if dp.GetFlags()&uint32(metricsv1.DataPointFlags_DATA_POINT_FLAGS_NO_RECORDED_VALUE_MASK) != 0 {
		return 0, false
	}
	return fibreNumber(dp), true
}

func (st *fibreStore) recordRaw(chain string, rm *metricsv1.ResourceMetrics) {
	st.mu.Lock()
	defer st.mu.Unlock()
	now := st.now()
	st.sweepRaw(now)
	resource := make(map[string]string)
	fibreRawAdd(resource, rm.GetResource().GetAttributes())
	labelsFor := func(attrs []*commonv1.KeyValue) map[string]string {
		labels := make(map[string]string, len(resource)+len(attrs))
		for k, v := range resource {
			labels[k] = v
		}
		// data point attributes win over the resource's, as in the collector
		point := make(map[string]string, len(attrs))
		fibreRawAdd(point, attrs)
		for k, v := range point {
			labels[k] = v
		}
		return labels
	}
	for _, sm := range rm.GetScopeMetrics() {
		for _, m := range sm.GetMetrics() {
			switch data := m.GetData().(type) {
			case *metricsv1.Metric_Gauge:
				name := fibreRawName(m.GetName(), m.GetUnit(), false, true)
				for _, dp := range data.Gauge.GetDataPoints() {
					if v, ok := fibreRawGaugeNumber(dp); ok && name != "" {
						if s := st.rawSeries(chain, name, m.GetDescription(), fibreRawGauge, labelsFor(dp.GetAttributes()), now); s != nil {
							s.value = v
						}
					}
				}
			case *metricsv1.Metric_Sum:
				kind := fibreRawGauge
				if data.Sum.GetIsMonotonic() {
					kind = fibreRawCounter
				}
				name := fibreRawName(m.GetName(), m.GetUnit(), kind == fibreRawCounter, kind == fibreRawGauge)
				delta := !temporalityCumulative(data.Sum.GetAggregationTemporality())
				for _, dp := range data.Sum.GetDataPoints() {
					v, ok := fibreRawGaugeNumber(dp)
					if !ok || name == "" {
						continue
					}
					if s := st.rawSeries(chain, name, m.GetDescription(), kind, labelsFor(dp.GetAttributes()), now); s != nil {
						if delta {
							s.value += v
						} else {
							s.value = v
						}
					}
				}
			case *metricsv1.Metric_Histogram:
				name := fibreRawName(m.GetName(), m.GetUnit(), false, false)
				delta := !temporalityCumulative(data.Histogram.GetAggregationTemporality())
				for _, dp := range data.Histogram.GetDataPoints() {
					bounds, counts := dp.GetExplicitBounds(), dp.GetBucketCounts()
					if name == "" || len(counts) != len(bounds)+1 || dp.GetFlags()&uint32(metricsv1.DataPointFlags_DATA_POINT_FLAGS_NO_RECORDED_VALUE_MASK) != 0 {
						continue
					}
					s := st.rawSeries(chain, name, m.GetDescription(), fibreRawHistogram, labelsFor(dp.GetAttributes()), now)
					if s == nil {
						continue
					}
					sum := dp.GetSum()
					if math.IsNaN(sum) {
						sum = 0
					}
					if delta && len(s.counts) == len(counts) && sameFibreBounds(s.bounds, bounds) {
						for i, n := range counts {
							s.counts[i] += n
						}
						s.count += dp.GetCount()
						s.sum += sum
						continue
					}
					// cumulative values replace the baseline on restart
					s.bounds, s.counts = append([]float64(nil), bounds...), append([]uint64(nil), counts...)
					s.count, s.sum = dp.GetCount(), sum
				}
			case *metricsv1.Metric_ExponentialHistogram:
				if st.fibreRawNote("exp:" + m.GetName()) {
					l("fibre_metrics export_raw: skipping", m.GetName()+", exponential histograms are not exported")
				}
			case *metricsv1.Metric_Summary:
				if st.fibreRawNote("summary:" + m.GetName()) {
					l("fibre_metrics export_raw: skipping", m.GetName()+", summaries are not exported")
				}
			}
		}
	}
}

func fibreRawReservedName(name string) bool {
	for _, p := range fibreRawReserved {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// Metric names come from Fibre, so Describe emits no descriptors.
type fibreRawCollector struct {
	config *Config
}

func (*fibreRawCollector) Describe(chan<- *prometheus.Desc) {}

func (fc *fibreRawCollector) Collect(ch chan<- prometheus.Metric) {
	c := fc.config
	if !c.settings().FibreMetrics.ExportRaw {
		return
	}
	chainIDs := make(map[string]string)
	c.chainsMux.RLock()
	for key, cc := range c.Chains {
		if cc == nil {
			continue
		}
		name := cc.name
		if name == "" {
			name = key
		}
		chainIDs[name] = cc.ChainId
	}
	c.chainsMux.RUnlock()
	st := c.fibreData()
	type chainSeries struct {
		name string
		list []fibreRawSeries
	}
	var chains []chainSeries
	st.mu.Lock()
	st.sweepRaw(st.now())
	for chain, series := range st.raw {
		if _, ok := chainIDs[chain]; !ok {
			continue
		}
		cs := chainSeries{name: chain}
		for _, s := range series {
			copied := *s
			copied.counts = append([]uint64(nil), s.counts...)
			cs.list = append(cs.list, copied)
		}
		chains = append(chains, cs)
	}
	st.mu.Unlock()
	// avoid duplicate families and label sets, which fail the scrape
	kinds, helps, seen := make(map[string]fibreRawKind), make(map[string]string), make(map[string]bool)
	for _, cs := range chains {
		for _, s := range cs.list {
			if fibreRawReservedName(s.name) {
				continue
			}
			if kind, ok := kinds[s.name]; ok && kind != s.kind {
				continue
			}
			kinds[s.name] = s.kind
			if _, ok := helps[s.name]; !ok {
				helps[s.name] = s.help
				if s.help == "" {
					helps[s.name] = s.name
				}
			}
			labels := make(map[string]string, len(s.labels)+2)
			for k, v := range s.labels {
				labels[k] = v
			}
			labels["name"], labels["chain_id"] = cs.name, chainIDs[cs.name]
			if _, le := labels["le"]; le && s.kind == fibreRawHistogram {
				continue
			}
			names, values := make([]string, 0, len(labels)), make([]string, 0, len(labels))
			for k := range labels {
				names = append(names, k)
			}
			sort.Strings(names)
			for _, k := range names {
				values = append(values, labels[k])
			}
			id := fibreRawKey(s.name, s.kind, labels)
			if seen[id] {
				continue
			}
			seen[id] = true
			desc := prometheus.NewDesc(s.name, helps[s.name], names, nil)
			var metric prometheus.Metric
			var err error
			switch s.kind {
			case fibreRawHistogram:
				buckets := make(map[float64]uint64, len(s.bounds))
				var cumulative uint64
				for i, bound := range s.bounds {
					cumulative += s.counts[i]
					if !math.IsInf(bound, 0) && !math.IsNaN(bound) {
						buckets[bound] = cumulative
					}
				}
				metric, err = prometheus.NewConstHistogram(desc, s.count, s.sum, buckets, values...)
			case fibreRawCounter:
				metric, err = prometheus.NewConstMetric(desc, prometheus.CounterValue, s.value, values...)
			default:
				metric, err = prometheus.NewConstMetric(desc, prometheus.GaugeValue, s.value, values...)
			}
			if err == nil {
				ch <- metric
			}
		}
	}
}
