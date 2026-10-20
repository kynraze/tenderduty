const test = require('node:test')
const assert = require('node:assert/strict')
const { normalize, definitions, headline, resourceDefinition, missedRows, summaryFacts, reachSlots, reachColor, leadingUnrecorded, chainGroups } = require('../td2/static/fibre.js')

test('Fibre panel settings recover from missing, duplicate and unknown panels', () => {
    const layout = normalize({ range: 'invalid', panels: [{ id: 'storage', show: false, wide: true }, { id: 'storage' }, { id: 'unknown' }, null] })
    assert.equal(layout.range, '24h')
    assert.equal(layout.panels.length, 10)
    assert.deepEqual(layout.panels[1], { id: 'storage', show: false, wide: true, chart: 'auto' })
    assert.equal(new Set(layout.panels.map(p => p.id)).size, 10)
})

test('Fibre preferences retain valid ranges and use defaults for corrupt storage', () => {
    assert.equal(normalize(null).panels.length, 10)
    assert.equal(normalize({ panels: 'broken' }).range, '24h')
    assert.equal(normalize({ range: '7d', panels: [] }).range, '7d')
    assert.equal(normalize({ panels: [{ id: 'uploads', show: 'false', wide: 'true' }] }).panels[1].wide, false)
})


test('Old Fibre layouts append visible latency and resources panels in order', () => {
    const ids = ['downloads', 'reach', 'uploads', 'received', 'signing', 'storage', 'errors']
    const layout = normalize({ range: '7d', panels: ids.map(id => ({ id, show: false, wide: true })) })
    assert.deepEqual(layout.panels.slice(1, 8).map(p => p.id), ids)
    assert.deepEqual(layout.panels.slice(8), [
        { id: 'latency', show: true, wide: true, chart: 'auto' },
        { id: 'resources', show: true, wide: false, chart: 'auto' }
    ])
    assert.equal(layout.range, '7d')
    assert.equal(layout.panels[1].show, false)
})

test('Fibre chart types keep a valid choice and fall back to auto', () => {
    const layout = normalize({ panels: [{ id: 'uploads', chart: 'area' }, { id: 'signing', chart: 'pie' }] })
    assert.equal(layout.panels[1].chart, 'area')
    assert.equal(layout.panels[2].chart, 'auto')
    assert.equal(normalize(null).panels[0].chart, 'auto')
})

const fs = require('node:fs')
const vm = require('node:vm')
const fibreSource = fs.readFileSync(require.resolve('../td2/static/fibre.js'), 'utf8')
const statusSource = fs.readFileSync(require.resolve('../td2/static/status.js'), 'utf8')

function budgetFacts(fibre) {
    const context = {
        number: String, data: null, range: '24h', summaryFacts,
        document: { createTextNode: text => ({ text }) },
        $: () => ({ replaceChildren: (...facts) => { context.facts = facts } }),
        node: (tag, text) => ({ tag, text, style: {}, append(...children) { this.children = children; this.lastChild = children.at(-1) } })
    }
    vm.createContext(context)
    const compact = fibreSource.slice(fibreSource.indexOf('    const value ='), fibreSource.indexOf('    let data ='))
    const cards = fibreSource.slice(fibreSource.indexOf('    function cards('), fibreSource.indexOf('    async function refresh()'))
    const bytes = statusSource.slice(statusSource.indexOf('function bytesText('), statusSource.indexOf('\n}', statusSource.indexOf('function bytesText(')) + 2)
    const provider = statusSource.slice(statusSource.indexOf('function fibreProviderFacts('), statusSource.indexOf('function appendFact('))
    vm.runInContext(compact + cards + bytes + provider, context)
    context.fibre = { rows: 148, total_rows: 4096, row_percent: 3.61, ...fibre }
    vm.runInContext('cards(fibre); provider = fibreProviderFacts({ fibre, total_power: 0 })', context)
    return {
        panel: context.facts.flatMap(section => section.children.map(f => f.children.map(c => c.text))),
        detail: context.provider
    }
}

test('Endorsing status stays on the tab and colours detail endorsements without metrics', () => {
    for (const endorsing of [
        { stopped: true, text: 'stopped since 10:01 · 0 of 140 blobs' },
        { stopped: false, text: 'last endorsed 2m ago' }
    ]) {
        const facts = budgetFacts({ endorsing, metrics_received: false })
        assert.equal(facts.panel.find(([label]) => label === 'Last endorsed')[1], endorsing.text.replace('last endorsed ', ''))
        assert.equal(facts.detail.some(([label]) => label === 'Endorsing'), false)
        assert.equal(facts.detail.find(([label]) => label === 'Endorsed (24h)')[1], 'none seen yet')
    }
    assert.match(fibreSource, /\['Last endorsed'.*f\.endorsing\?\.stopped \? 'critical'/)
    assert.match(statusSource, /label === 'Endorsed \(24h\)' && fibre\.endorsing\?\.stopped.*var\(--critical\)/)
})

test('Shard budget combines occupancy and chain budget on both Fibre views', () => {
    assert.equal(definitions.find(d => d.id === 'storage').title, 'Shards stored')
    const gib = 1024 ** 3
    for (const [values, expected] of [
        [{ occupancy_known: true, occupancy: 0, chain_budget: 74 * gib, budget_reported: true, fibre_budget: 74 * gib }, '0 B of 74 GiB'],
        [{ occupancy_known: true, occupancy: gib, chain_budget: 74 * gib, budget_reported: true, fibre_budget: 80 * gib }, '1 GiB of 74 GiB · Fibre reports 80 GiB'],
        [{ occupancy_known: true, occupancy: 0, chain_budget: 74 * gib, budget_reported: true, unlimited: true, fibre_budget: 0 }, '0 B of 74 GiB · Fibre unlimited'],
        [{ chain_budget: 74 * gib }, 'Unknown of 74 GiB'],
        [{}, 'Unknown of Unknown'],
        [{ occupancy_known: true, occupancy: 0, budget_reported: true, fibre_budget: 80 * gib }, '0 B of Unknown · Fibre reports 80 GiB']
    ]) {
        for (const [view, facts] of Object.entries(budgetFacts(values))) {
            const budgets = facts.filter(([label]) => label === (view === 'panel' ? 'Shards stored' : 'Shards'))
            assert.equal(budgets.length, 1)
            const detail = expected.includes(' · ') ? expected.replace(' · ', ' budget · ') : `${expected} budget`
            assert.equal(view === 'panel' ? `${budgets[0][1]} ${budgets[0][2]}` : budgets[0][1], view === 'panel' ? expected : detail)
            assert.equal(facts.some(([label]) => ['Chain budget', 'Used / reported budget', 'Storage used'].includes(label)), false)
        }
    }
})

const header = (id, metrics) => headline(definitions.find(d => d.id === id), metrics.map(metrics => ({ metrics }))).map(([text]) => text).join(' \u00b7 ')

test('Upload and download headers show whole-range success and omit percent at zero', () => {
    assert.equal(header('uploads', [{ uploads_ok: 10, uploads_failed: 20, uploads_rejected: 0 }, { uploads_ok: 3, uploads_failed: 3, uploads_rejected: 0 }]), '13 ok \u00b7 23 failed \u00b7 0 rejected \u00b7 36% ok')
    assert.equal(header('downloads', [{ downloads_ok: 58, downloads_failed: 41, bytes_served: 6501171 }]), '58 ok \u00b7 41 failed \u00b7 59% ok \u00b7 6.2 MiB served')
    assert.equal(header('uploads', [{ uploads_ok: 0, uploads_failed: 0, uploads_rejected: 0 }]), '0 ok \u00b7 0 failed \u00b7 0 rejected')
    assert.equal(header('downloads', [{ downloads_ok: 0, downloads_failed: 0, bytes_served: 0 }]), '0 ok \u00b7 0 failed \u00b7 0 B served')
    assert.equal(header('uploads', [{}]), '')
})

test('Split latency preferences merge at the first saved position', () => {
    for (const ids of [['request_latency', 'internal_latency'], ['internal_latency', 'request_latency'], ['latency', 'request_latency']]) {
        const layout = normalize({ panels: [{ id: 'uploads' }, { id: ids[0], show: false, wide: true, chart: 'line' }, { id: 'storage' }, { id: ids[1] }] })
        assert.deepEqual(layout.panels.slice(1, 4).map(p => p.id), ['uploads', 'latency', 'storage'])
        assert.deepEqual(layout.panels[2], { id: 'latency', show: false, wide: true, chart: 'line' })
        assert.equal(new Set(layout.panels.map(p => p.id)).size, 10)
    }
})

test('Latency headlines total signing performance over the range', () => {
    const def = definitions.find(d => d.id === 'latency')
    const points = (...metrics) => metrics.map(metrics => ({ metrics }))
    assert.deepEqual(headline(def, points({ signatures_fast: 2, signatures_slow: 0 }, { signatures_fast: 8, signatures_slow: 0 })), [['signing 100% under 10 ms', 'good']])
    assert.deepEqual(headline(def, points({ signatures_fast: 2, signatures_slow: 2 }, { signatures_fast: 6, signatures_slow: 0, signatures_failed: 3 })), [['signing 80% under 10 ms', 'warning'], ['3 signing failed', 'critical']])
    assert.deepEqual(headline(def, points({ signatures_fast: 999, signatures_slow: 1 })), [['signing 99% under 10 ms', 'warning']])
    assert.deepEqual(headline(def, points({ signatures_failed: 2 })), [['2 signing failed', 'critical']])
    assert.deepEqual(headline(def, points({})), [])
    assert.deepEqual(headline(def, []), [])
    assert.deepEqual(headline(def, points({ signatures_fast: 0, signatures_slow: 0, signatures_failed: 0 })), [])
})

test('Downloads chart counts operations and keeps bytes in the tooltip', () => {
    const def = definitions.find(d => d.id === 'downloads')
    assert.deepEqual(def.fields, ['downloads_ok', 'downloads_failed'])
    assert.equal(def.unit, 'count')
    assert.equal(def.stacked, true)
    assert.ok(def.tooltip.includes('bytes_served'))
})

test('Resources header uses latest runtime values with and without a limit', () => {
    const metrics = { go_goroutines: 56, go_processor_limit: 12, go_gogc: 100 }
    assert.equal(header('resources', [metrics]), '56 goroutines \u00b7 GOMAXPROCS 12 \u00b7 GOGC 100')
    assert.equal(header('resources', [metrics, { ...metrics, go_memory_limit_bytes: 1.5 * 1024 ** 3 }]), '56 goroutines \u00b7 GOMAXPROCS 12 \u00b7 GOGC 100 \u00b7 limit 1.5 GiB')
    assert.equal(header('resources', [{ ...metrics, go_memory_limit_bytes: 100 }, { ...metrics, go_memory_no_limit: 1 }]), '56 goroutines \u00b7 GOMAXPROCS 12 \u00b7 GOGC 100')
    assert.deepEqual(resourceDefinition([{ metrics: { go_memory_used_bytes: 10 } }]).fields, ['go_memory_used_bytes'])
    const split = resourceDefinition([{ metrics: { go_memory_heap_bytes: 8, go_memory_stack_bytes: 2 } }])
    assert.deepEqual(split.fields, ['go_memory_heap_bytes', 'go_memory_stack_bytes'])
    assert.deepEqual(split.lines, ['go_gc_goal_bytes'])
    assert.ok(split.tooltip.includes('go_goroutines'))
})

test('Latency plots p50 and p95 and expose adaptive sample counts', () => {
    for (const id of ['latency']) {
        const def = definitions.find(d => d.id === id)
        for (let i = 0; i < def.fields.length; i += 2) {
            assert.match(def.fields[i], /_p50_ms$/)
            assert.equal(def.fields[i + 1], def.fields[i].replace('_p50_', '_p95_'))
            assert.ok(def.tooltip.includes(def.fields[i].replace('_p50_ms', '_samples')))
        }
    }
})

function chartContext(span = '1h') {
    const node = (tag, text, cls) => ({ tag, text, cls, style: {}, children: [], listeners: {},
        append(...children) { this.children.push(...children) },
        replaceChildren(...children) { this.children = children },
        setAttribute() {}, addEventListener(event, listener) { this.listeners[event] = listener }
    })
    const context = { node, svg: (tag, attributes) => ({ ...node(tag), attributes }), range: span, selected: 'Chain', document: { createTextNode: text => node('text', text) } }
    vm.createContext(context)
    const helpers = fibreSource.slice(fibreSource.indexOf('    const value ='), fibreSource.indexOf("    if (typeof module"))
    const charts = fibreSource.slice(fibreSource.indexOf('    // rounds up'), fibreSource.indexOf('    function drawPanels()'))
    vm.runInContext('const definitions = ' + JSON.stringify(definitions) + ';\n' + fibreSource.slice(fibreSource.indexOf('    const tooltipLabels ='), fibreSource.indexOf('    const ranges =')) + helpers + charts, context)
    return context
}

test('Resources render stacked memory with separate GC goal and red limit lines', () => {
    const context = chartContext('7d')
    context.points = [{ time: 1000, metrics: { go_memory_used_bytes: 150, go_memory_heap_bytes: 100, go_memory_stack_bytes: 50, go_memory_max_bytes: 1000, go_memory_heap_max_bytes: 800, go_memory_stack_max_bytes: 200, go_gc_goal_bytes: 1200, go_memory_limit_bytes: 1500, go_goroutines: 5, go_goroutines_max: 10, go_allocation_bytes_per_second: 100 } }]
    vm.runInContext("def = resourceDefinition(points); chart = drawChart(def, points, 500, 'bars'); key = legend(def)", context)
    const paths = context.chart.children[0].children.filter(n => n.tag === 'path')
    assert.equal(paths.filter(n => n.attributes.fill?.startsWith('color-mix')).length, 2)
    assert.ok(paths.some(n => n.attributes.stroke === 'var(--warning)' && n.attributes['stroke-dasharray'] === '5 4'))
    assert.ok(paths.some(n => n.attributes.stroke === 'var(--critical)' && n.attributes['stroke-dasharray'] === 'none'))
    const hit = context.chart.children[0].children.find(n => n.attributes?.tabindex === '0')
    hit.listeners.focus()
    const tip = context.chart.children[1]
    assert.ok(tip.children.some(n => n.children?.some(c => c.text === '800 B')))
    assert.ok(tip.children.some(n => n.children?.some(c => c.text === '10')))
    assert.ok(tip.children.some(n => n.children?.some(c => c.text === '100 B/s')))
    assert.equal(context.points[0].metrics.go_memory_heap_bytes, 100)
})

test('Resources keep a total memory area for history that lacks a split', () => {
    const context = chartContext()
    context.points = [{ time: 1000, metrics: { go_memory_used_bytes: 500, go_gc_goal_bytes: 800 } }]
    vm.runInContext("def = resourceDefinition(points); chart = drawChart(def, points, 500, 'auto'); key = legend(def)", context)
    assert.ok(context.key.children.some(n => n.children.some(c => c.text === 'Memory used')))
    assert.equal(context.chart.children[0].children.filter(n => n.tag === 'path' && n.attributes.fill?.startsWith('color-mix')).length, 1)
    const mixed = resourceDefinition([{ metrics: { go_memory_used_bytes: 500 } }, { metrics: { go_memory_used_bytes: 150, go_memory_heap_bytes: 100, go_memory_stack_bytes: 50 } }])
    assert.deepEqual(mixed.fields, ['go_memory_used_bytes'])
})

test('Fibre push refresh filters the chain and tab and limits requests to once in five seconds', () => {
    let listener, now = 1000, callback, delay, count = 0
    const context = { document: { addEventListener: (_, fn) => { listener = fn } }, active: () => context.visible,
        visible: true, selected: 'Chain', pushed: null, lastRequestAt: 0, Date: { now: () => now },
        setTimeout: (fn, ms) => { callback = fn; delay = ms; return 1 }, refresh: () => { count++; context.lastRequestAt = now }
    }
    vm.createContext(context)
    const start = fibreSource.indexOf("    document.addEventListener('fibre-update'")
    vm.runInContext(fibreSource.slice(start, fibreSource.indexOf("    window.addEventListener('hashchange'", start)), context)
    listener({ detail: { name: 'Other' } })
    assert.equal(callback, undefined)
    context.visible = false
    listener({ detail: { name: 'Chain' } })
    assert.equal(callback, undefined)
    context.visible = true
    listener({ detail: { name: 'Chain' } })
    assert.equal(delay, 4000)
    now = 5000; callback()
    assert.equal(count, 1)
    listener({ detail: { name: 'Chain' } })
    assert.equal(delay, 5000)
    context.lastRequestAt = 9000
    now = 10000; callback()
    assert.equal(count, 1)
    assert.equal(delay, 4000)
    now = 14000; callback()
    assert.equal(count, 2)
    listener({ detail: { name: 'Chain' } })
    context.selected = 'Other'
    now = 19000; callback()
    assert.equal(count, 2)
})


test('Endorsements are first and visible for defaults and existing layouts', () => {
    for (const layout of [normalize(null), normalize({ panels: [{ id: 'uploads', show: false }] })]) {
        assert.equal(layout.panels[0].id, 'endorsements')
        assert.equal(layout.panels[0].show, true)
    }
})

test('Endorsement header totals the range and omits percentage at zero', () => {
    const def = definitions.find(d => d.id === 'endorsements')
    assert.equal(headline(def, [{ metrics: { endorsements_endorsed: 40, endorsements_missed: 1 } }, { metrics: { endorsements_endorsed: 3, endorsements_missed: 0, endorsements_unknown: 9 } }])[0][0], '43 endorsed · 1 missed · 97.7%')
    assert.equal(headline(def, [])[0][0], '0 endorsed · 0 missed')
    assert.deepEqual(def.colors, ['good', 'critical'])
})

test('Latest missed rows are bounded and show height, short commitment, size and age', () => {
    const blob = { height: 42, commitment: '74656e73' + '00'.repeat(25) + '6c6500', blob_size: 2048, time: 100 }
    const rows = missedRows(Array(20).fill(blob), 220)
    assert.equal(rows.length, 5)
    assert.deepEqual(rows[0], { height: '42', commitment: '74656e73…6c6500', full: blob.commitment, size: '2 KiB', signatures: '', age: '2m ago' })
    assert.deepEqual(missedRows([]), [])
    assert.equal(missedRows([blob], 90)[0].age, '0s ago')
})

test('Endorsement facts are available without Fibre metrics', () => {
    const facts = budgetFacts({ last_24h: { endorsed: 43, missed: 1, unknown: 90 } })
    assert.ok(facts.panel.some(row => row[0] === 'Endorsed' && row[1] === '—' && row[2] === 'none seen'))
    assert.ok(facts.detail.some(row => row[0] === 'Endorsed (24h)' && row[1] === '43 of 44 · 97.7%'))
})

test('Endorsement panel renders its header, chain note and at most five missed rows', () => {
    function element(tag, text) {
        return { tag, text, children: [], dataset: {}, clientWidth: 400,
            getBoundingClientRect() { return { width: context.listWidth } },
            get lastChild() { return this.children.at(-1) },
            append(...items) { this.children.push(...items) },
            replaceChildren(...items) { this.children = items },
            querySelector() { return null }, querySelectorAll() { return [] } }
    }
    const grid = element('div'), blob = { height: 42, commitment: 'ab'.repeat(32), blob_size: 2048, time: 1 }
    const context = { node: element, $: () => grid, definitions, headline, missedRows,
        listWidth: 220, columns: '368px',
        getComputedStyle: () => ({ paddingLeft: '16', paddingRight: '16', gridTemplateColumns: context.columns, columnGap: '22px' }),
        document: { createTextNode: text => element('text', text) },
        legend: () => element('legend'), drawChart: (def, points, width) => ({ ...element('chart'), width }),
        data: { series: [{ metrics: { endorsements_endorsed: 43, endorsements_missed: 1 } }], summary: { latest_missed: Array(20).fill(blob) } },
        layout: { panels: [{ id: 'endorsements', show: true, wide: false, chart: 'auto' }] } }
    vm.createContext(context)
    const panels = fibreSource.slice(fibreSource.indexOf('    function drawPanels()'), fibreSource.indexOf('    function rangeButtons()'))
    vm.runInContext(panels + '\ndrawPanels()', context)
    const panel = grid.children[0], latest = panel.children.at(-1).children[1]
    assert.equal(panel.dataset.panel, 'endorsements')
    assert.equal(panel.children[0].children[0].text, 'Endorsements')
    assert.equal(panel.children[0].children[1].children[0].text, '43 endorsed · 1 missed · 97.7%')
    assert.equal(panel.children[2].text, "From the chain: blobs paid for with or without this validator's signature.")
    assert.equal(latest.children[0].text, 'Latest missed')
    assert.equal(latest.children.length, 6)
    assert.equal(latest.children[1].children[1].tag, 'code')
    assert.equal(latest.children[1].children[1].title, blob.commitment)
    assert.ok(latest.children[1].children[2].text.startsWith('2 KiB \u00b7 '))
    assert.equal(latest.children[1].children[2].text.includes('%'), false)
    assert.equal(panel.children.at(-1).children[0].children[0].width, 368)
    context.columns = '726px 220px'
    context.node = (tag, text) => ({ ...element(tag, text), clientWidth: 1000 })
    vm.runInContext('drawPanels()', context)
    assert.equal(grid.children[0].children.at(-1).children[0].children[0].width, 726)
    context.listWidth = 160
    vm.runInContext('drawPanels()', context)
    assert.equal(grid.children[0].children.at(-1).children[0].children[0].width, 786)
    blob.signed = 40; blob.validators = 84; blob.power = 67.25
    vm.runInContext('drawPanels()', context)
    assert.ok(grid.children[0].children.at(-1).children[1].children[1].children[2].text.startsWith('2 KiB \u00b7 40/84 \u00b7 67% \u00b7 '))
})


test('Backend labels, facts and upload failure note cover Fibre v10.4', () => {
    assert.equal(definitions.find(d => d.id === 'errors').labels[3], 'Storage backend')
    assert.match(fibreSource, /object_storage_bytes: 'Backend bytes read'/)
    assert.match(fibreSource, /object_storage_in_flight: 'Backend reads in flight'/)
    assert.match(fibreSource, /Storage backend \(\$\{data.summary.storage_backend\}\)/)
    assert.ok(fibreSource.includes("Failed includes payments Fibre refused (e.g. the payer's escrow was too low) and uploads under the validator's min_upload_size."))
    const facts = budgetFacts({ storage_backend: 'local' })
    assert.ok(facts.panel.some(([label, text]) => label === 'Storage' && text === 'local'))
    assert.equal(facts.detail.some(([label]) => label === 'Storage backend'), false)
    assert.equal(facts.detail.find(([label]) => label === 'Shards')[1], 'Unknown of Unknown budget · local')
    assert.equal(budgetFacts({ storage_backend: 'object' }).detail.find(([label]) => label === 'Shards')[1], 'Unknown of Unknown budget · object')
    assert.equal(budgetFacts({ storage_backend: 'object, local', occupancy_known: true, occupancy: 605594, chain_budget: 74 * 1024 ** 3 }).detail.find(([label]) => label === 'Shards')[1], '591.4 KiB of 74 GiB budget · object, local')
})


test('Store get misses stay neutral and are excluded from error totals', () => {
    const def = definitions.find(d => d.id === 'errors')
    const points = [{ metrics: { store_put_errors: 0, store_get_errors: 12, prune_errors: 0, object_storage_errors: 0 } }]
    assert.deepEqual(headline(def, points), [['no errors', 'good'], ['12 store get misses', undefined]])
    assert.equal(def.labels[1], 'Store get misses')
    assert.equal(def.colors[1], 'muted')
    assert.equal(new Set(def.colors).size, 4)
    points[0].metrics.store_put_errors = 2
    points[0].metrics.prune_errors = 3
    points[0].metrics.object_storage_errors = 4
    assert.deepEqual(headline(def, points), [['9 errors', 'critical'], ['12 store get misses', undefined]])
})

test('Latency log charts span every decade and preserve real tooltip values', () => {
    for (const id of ['latency']) {
        const context = chartContext()
        const def = definitions.find(d => d.id === id)
        assert.equal(def.log, true)
        context.def = def
        context.points = [{ time: 1000, metrics: { [def.fields[0]]: 0.48, [def.fields[1]]: 22 } }]
        vm.runInContext("chart = drawChart(def, points, 500, 'auto')", context)
        const nodes = context.chart.children[0].children
        const labels = nodes.filter(n => n.tag === 'text').map(n => n.textContent)
        for (const label of ['0.1 ms', '1 ms', '10 ms', '100 ms']) assert.ok(labels.includes(label), label)
        const circles = nodes.filter(n => n.tag === 'circle')
        assert.equal(circles.length, 2)
        assert.ok(circles.every(n => n.attributes.cy > 10 && n.attributes.cy < 158))
        assert.ok(circles[0].attributes.cy > circles[1].attributes.cy)
        const paths = nodes.filter(n => n.tag === 'path')
        assert.equal(paths[0].attributes['stroke-dasharray'], 'none')
        assert.equal(paths[1].attributes['stroke-dasharray'], '5 4')
        nodes.find(n => n.attributes?.tabindex === '0').listeners.focus()
        const tip = context.chart.children[1]
        assert.ok(tip.children.some(n => n.children?.some(c => c.text === '0.48 ms')))
        assert.ok(tip.children.some(n => n.children?.some(c => c.text === '22 ms')))
    }
})

test('Log latency omits zero and negative values and keeps a minimum decade', () => {
    const context = chartContext()
    context.def = definitions.find(d => d.id === 'latency')
    context.points = [-1, 0, 0.01, null, 0.02].map((v, i) => ({ time: 1000 + i * 60, metrics: { upload_p50_ms: v } }))
    vm.runInContext("chart = drawChart(def, points, 500, 'auto')", context)
    const nodes = context.chart.children[0].children
    const path = nodes.find(n => n.tag === 'path')
    assert.equal((path.attributes.d.match(/M/g) || []).length, 2)
    assert.equal(path.attributes.d.includes('L'), false)
    assert.equal(nodes.filter(n => n.tag === 'circle').length, 2)
    const labels = nodes.filter(n => n.tag === 'text').map(n => n.textContent)
    assert.ok(labels.includes('0.01 ms'))
    assert.ok(labels.includes('0.1 ms'))
})

function textOf(element) {
    return [element.text ?? element.textContent ?? '', ...(element.children || []).map(textOf)].join(' ')
}

test('Latency panels render operation minis with separate log axes and tooltips', () => {
    for (const [id, count] of [['latency', 6]]) {
        const context = chartContext()
        const def = definitions.find(d => d.id === id)
        context.def = def
        const metrics = Object.fromEntries(def.fields.map((field, i) => [field, i < 2 ? 0.48 + i * 0.09 : 200 + i]))
        for (const field of def.tooltip) metrics[field] = 50
        metrics.prune_entries = 999
        context.points = [
            { time: 1000, metrics },
            { time: 1060, metrics: {} },
            { time: 1120, metrics }
        ]
        context.latency = Object.fromEntries(def.fields.map(field => [field, 0.57]))
        vm.runInContext("data = { latency }; grid = latencyCharts(def, points, 700); key = legend(def)", context)
        assert.equal(context.grid.children.length, count)
        assert.equal(textOf(context.key).trim(), 'solid p50 \u00b7 dashed p95')
        for (const [i, mini] of context.grid.children.entries()) {
            assert.ok(textOf(mini.children[0]).includes(`${def.labels[i * 2].replace(' p50', '')} \u00b7 p95 0.57 ms`))
            assert.equal(mini.children[0].children[0].style.background, `var(--${def.colors[i * 2]})`)
            const chart = mini.children[1], nodes = chart.children[0].children
            const paths = nodes.filter(n => n.tag === 'path')
            assert.equal(paths.length, 2)
            assert.equal(paths[0].attributes['stroke-dasharray'], 'none')
            assert.equal(paths[1].attributes['stroke-dasharray'], '5 4')
            assert.equal(paths[0].attributes.stroke, paths[1].attributes.stroke)
            assert.ok(paths.every(n => (n.attributes.d.match(/M/g) || []).length === 2))
            const ticks = nodes.filter(n => n.tag === 'text' && n.attributes['text-anchor'] === 'end' && n.attributes.x === 54).map(n => n.textContent)
            assert.deepEqual(Array.from(ticks), i === 0 ? ['0.1 ms', '1 ms'] : ['100 ms', '1000 ms'])
            assert.equal(nodes.filter(n => n.tag === 'text' && n.attributes['font-size'] === 11 && n.attributes.y === 110).length, 3)
            nodes.find(n => n.attributes?.tabindex === '0').listeners.focus()
            const rows = chart.children[1].children.slice(1)
            assert.equal(rows.length, 3)
            assert.deepEqual(Array.from(rows, n => n.children[0].text), ['p50', 'p95', def.labels[i * 2].replace(' p50', '') + ' ops'])
            assert.equal(textOf(chart.children[1]).includes('999'), false)
        }
        vm.runInContext("data = {}; grid = latencyCharts(def, points, 400)", context)
        assert.ok(context.grid.children.every(mini => !textOf(mini.children[0]).includes('p95')))
        assert.ok(context.grid.children.every(mini => mini.children[1].children[0].attributes.viewBox === '0 0 192 120'))
    }
})

test('Latency minis use three columns only for wide panels of at least 840px', () => {
    const context = chartContext()
    context.def = definitions.find(d => d.id === 'latency')
    assert.deepEqual(context.def.labels.filter((_, i) => i % 2 === 0).map(label => label.replace(' p50', '')), ['Upload', 'Download', 'Signing', 'Store put', 'Store get', 'Prune'])
    context.points = [{ time: 1000, metrics: Object.fromEntries(context.def.fields.map(field => [field, 1])) }]
    for (const [width, wide, columns] of [[1000, true, 3], [840, true, 3], [839, true, 2], [1000, false, 2], [400, true, 2]]) {
        context.width = width; context.wide = wide
        vm.runInContext('grid = latencyCharts(def, points, width, wide)', context)
        assert.equal(context.grid.style.gridTemplateColumns, `repeat(${columns}, minmax(0, 1fr))`)
        const chartWidth = Math.max(180, Math.round((width - 16 * (columns - 1)) / columns))
        assert.ok(context.grid.children.every(mini => mini.children[1].children[0].attributes.viewBox === `0 0 ${chartWidth} 120`))
    }
})

test('Resources omit the memory limit legend and line unless a limit is set', () => {
    for (const metrics of [
        { go_memory_used_bytes: 500 },
        { go_memory_used_bytes: 500, go_memory_no_limit: 1, go_memory_limit_bytes: 1500 },
        { go_memory_used_bytes: 500, go_memory_limit_bytes: 1500 }
    ]) {
        const context = chartContext()
        context.points = [{ time: 1000, metrics }]
        vm.runInContext("def = resourceDefinition(points); chart = drawChart(def, points, 500, 'auto'); key = legend(def)", context)
        const hasLimit = metrics.go_memory_limit_bytes != null && metrics.go_memory_no_limit !== 1
        assert.equal(textOf(context.key).includes('Memory limit'), hasLimit)
        assert.equal(context.chart.children[0].children.some(n => n.tag === 'path' && n.attributes.stroke === 'var(--critical)'), hasLimit)
        assert.equal(header('resources', [metrics]).includes('limit 1.5 KiB'), hasLimit)
        assert.equal(header('resources', [metrics]).includes('no memory limit'), false)
    }
})

test('Errors tooltips contain only errors and Downloads includes backend requests', () => {
    const context = chartContext()
    context.def = definitions.find(d => d.id === 'errors')
    context.points = [{ time: 1000, metrics: {
        store_put_errors: 1, store_get_errors: 2, prune_errors: 3, object_storage_errors: 4,
        prune_entries: 5, object_storage_requests: 6, object_storage_in_flight: 7
    } }]
    vm.runInContext("chart = drawChart(def, points, 500, 'auto')", context)
    context.chart.children[0].children.find(n => n.attributes?.tabindex === '0').listeners.focus()
    const rows = context.chart.children[1].children.slice(1)
    assert.deepEqual(Array.from(rows, n => n.children[0].text), context.def.labels)
    assert.equal(rows.length, 4)
    const downloads = definitions.find(d => d.id === 'downloads')
    assert.ok(downloads.tooltip.includes('object_storage_requests'))
    assert.ok(downloads.tooltip.includes('object_storage_in_flight'))
    assert.ok(definitions.every(d => !(d.tooltip || []).includes('prune_entries')))
})


test('24h count bars sum local hours and leave missing and zero hours blank', () => {
    for (const style of ['auto', 'bars']) {
        const context = chartContext('24h')
        const start = new Date(2026, 9, 6, 0, 15).getTime() / 1000
        context.def = definitions.find(d => d.id === 'downloads')
        context.points = Array.from({ length: 288 }, (_, i) => ({ time: start + i * 300, metrics: {
            downloads_ok: 2, downloads_failed: 1, bytes_served: 100,
            object_storage_bytes: 50, object_storage_requests: 3, object_storage_in_flight: i
        } }))
        for (const p of context.points) {
            const hour = new Date(p.time * 1000).getHours()
            if (hour === 2) p.metrics = {}
            if (hour === 3) p.metrics = { downloads_ok: 0, downloads_failed: 0 }
        }
        context.style = style
        vm.runInContext('chart = drawChart(def, points, 500, style)', context)
        const nodes = context.chart.children[0].children
        const hits = nodes.filter(n => n.attributes?.tabindex === '0')
        assert.equal(hits.length, 25)
        assert.equal(nodes.filter(n => n.tag === 'path').length, 0)
        const bars = nodes.filter(n => n.tag === 'rect' && ['good', 'critical'].some(color => n.attributes.fill === `color-mix(in srgb, var(--${color}) 82%, var(--panel))`))
        assert.equal(bars.length, 46)
        assert.ok(bars.every(n => n.attributes.height > 0))
        hits[1].listeners.focus()
        const tip = context.chart.children[1]
        assert.ok(textOf(tip.children[0]).includes('\u2013'))
        assert.deepEqual(Array.from(tip.children.slice(1), n => n.children.at(-1).text), ['24', '12', '1.2 KiB', '600 B', '36', '20'])
        hits[0].listeners.focus()
        assert.equal(tip.children[1].children.at(-1).text, '18')
        hits[2].listeners.focus()
        assert.equal(textOf(tip).includes('No observation'), true)
        hits[3].listeners.focus()
        assert.equal(tip.children[1].children.at(-1).text, '0')
        const labels = nodes.filter(n => n.tag === 'text' && n.attributes.y === 180)
        assert.deepEqual(Array.from(labels, n => n.textContent), [0, 12, 24].map(hour => new Date(2026, 9, 6, hour).toLocaleTimeString(undefined, { hour: 'numeric' })))
        assert.deepEqual(Array.from(labels, n => n.attributes.x), [0, 12, 24].map(i => hits[i].attributes.x + hits[i].attributes.width / 2))
        assert.equal(context.points[0].metrics.downloads_ok, 2)
    }
})

test('7d and 30d count bars align to local six-hour and midnight buckets; 1h stays native', () => {
    for (const [range, hours, count] of [['7d', 6, 168], ['30d', 24, 720], ['1h', 0, 12]]) {
        const context = chartContext(range)
        const start = new Date(2026, 8, 1, 0, 0).getTime() / 1000
        context.def = definitions.find(d => d.id === 'uploads')
        context.points = Array.from({ length: count }, (_, i) => ({ time: start + i * (hours ? 3600 : 300), metrics: { uploads_ok: 1, uploads_failed: 2, uploads_rejected: 3 } }))
        vm.runInContext("chart = drawChart(def, points, 500, 'auto')", context)
        const hits = context.chart.children[0].children.filter(n => n.attributes?.tabindex === '0')
        assert.equal(hits.length, hours ? count / hours : count)
        for (const hit of hits) {
            hit.listeners.focus()
            assert.deepEqual(Array.from(context.chart.children[1].children.slice(1), n => n.children.at(-1).text), [1, 2, 3].map(n => String(n * (hours || 1))))
        }
        const tip = textOf(context.chart.children[1].children[0])
        assert.equal(tip.includes('\u2013'), Boolean(hours))
    }
})

test('Area and line styles retain native intervals on longer count charts', () => {
    for (const style of ['area', 'line']) {
        const context = chartContext('24h')
        context.def = definitions.find(d => d.id === 'endorsements')
        context.points = Array.from({ length: 288 }, (_, i) => ({ time: 1000 + i * 300, metrics: { endorsements_endorsed: 2, endorsements_missed: 1 } }))
        context.style = style
        vm.runInContext('chart = drawChart(def, points, 500, style)', context)
        const nodes = context.chart.children[0].children
        assert.equal(nodes.filter(n => n.attributes?.tabindex === '0').length, 288)
        assert.ok(nodes.some(n => n.tag === 'path' && n.attributes.d.includes('L')))
        nodes.find(n => n.attributes?.tabindex === '0').listeners.focus()
        assert.equal(textOf(context.chart.children[1].children[0]).includes('\u2013'), false)
    }
})

test('Signing buckets sum observed slow signatures and retain the latest average', () => {
    const context = chartContext('24h')
    context.def = definitions.find(d => d.id === 'signing')
    const time = new Date(2026, 9, 6, 1).getTime() / 1000
    context.points = [
        { time, metrics: { signatures_ok: 10, signatures_fast: 7, signatures_failed: 1, avg_sign_ms: 2 } },
        { time: time + 300, metrics: { signatures_ok: 5, signatures_fast: 4, signatures_failed: 0, avg_sign_ms: 8 } },
        { time: time + 600, metrics: {} }
    ]
    vm.runInContext("chart = drawChart(def, points, 500, 'auto')", context)
    context.chart.children[0].children.find(n => n.attributes?.tabindex === '0').listeners.focus()
    assert.deepEqual(Array.from(context.chart.children[1].children.slice(1), n => n.children.at(-1).text), ['11', '4', '1', '8 ms'])
    assert.ok(context.chart.children[0].attributes['aria-label'].includes('73% under 10 ms'))
})

test('Only the bucket containing the counting interval is faded', () => {
    const context = chartContext('24h')
    context.def = definitions.find(d => d.id === 'received')
    const now = new Date(2026, 9, 7, 1, 17).getTime()
    context.now = now
    vm.runInContext('Date.now = () => now', context)
    context.points = [0, 300, 3600, 3900].map(offset => ({ time: now / 1000 - 4020 + offset, metrics: { bytes_received: 100 } }))
    vm.runInContext("chart = drawChart(def, points, 500, 'auto')", context)
    const nodes = context.chart.children[0].children
    const hits = nodes.filter(n => n.attributes?.tabindex === '0')
    const faded = nodes.filter(n => n.attributes?.['fill-opacity'] === 0.6)
    assert.equal(faded.length, 1)
    assert.equal(faded[0].attributes.x, hits.at(-1).attributes.x)
    hits.at(-1).listeners.focus()
    assert.equal(textOf(context.chart.children[1]).includes('Still counting'), true)
    hits[0].listeners.focus()
    assert.equal(textOf(context.chart.children[1]).includes('Still counting'), false)
})


test('24h count axes have headroom and whole-number ticks while byte axes keep three ticks', () => {
    const start = new Date(2026, 9, 6, 0, 15).getTime() / 1000
    for (const id of ['endorsements', 'uploads', 'signing', 'errors', 'downloads', 'received']) {
        for (const max of [1, 4]) {
            const context = chartContext('24h')
            context.def = definitions.find(d => d.id === id)
            context.points = Array.from({ length: 288 }, (_, i) => ({ time: start + i * 300, metrics: {
                [context.def.fields[0]]: i % 12 === 0 ? max : 0
            } }))
            vm.runInContext("chart = drawChart(def, points, 500, 'auto')", context)
            const ticks = context.chart.children[0].children.filter(n => n.tag === 'text' && n.attributes.x === 54).map(n => n.textContent)
            // count tops: 1 -> 2, 4 -> 5 (2.5 dropped)
            assert.deepEqual(Array.from(ticks), id === 'received' ? ['0 B', max === 1 ? '0.5 B' : '2 B', max + ' B'] : max === 1 ? ['0', '1', '2'] : ['0', '5'])
        }
    }
})

function detailFacts(fibre) {
    const surfaces = new Map()
    const node = (tag, text = '') => ({
        tag, text, children: [], style: {},
        appendChild(child) { this.children.push(child); this.lastChild = child },
        replaceChildren() { this.children = []; this.textContent = '' }
    })
    const context = vm.createContext({
        chain: { name: 'Chain', total_power: 100, fibre }, number: String, age: at => `${at}s ago`,
        element: (tag, cls, text) => node(tag, text),
        chainDialog: { close() { context.closed = true } },
        document: { getElementById(id) {
            if (!surfaces.has(id)) surfaces.set(id, node('div'))
            return surfaces.get(id)
        } }
    })
    const code = statusSource.slice(statusSource.indexOf('function bytesText('), statusSource.indexOf('// render() redraws'))
    vm.runInContext(code + 'fillFibre(chain)', context)
    return { surfaces, context, labels: surfaces.get('detail-fibre-facts').children.map(f => f.children[0].text) }
}

test('Detail Fibre shows exactly nine facts and links to the selected chain', () => {
    const fibre = { registered: true, reachable: true, identity: 'verified', host: 'host', version: 'v10.4', metrics_received: true, metrics_at: 10, last_upload_at: 20, endorsing: { stopped: true } }
    const { surfaces, context, labels } = detailFacts(fibre)
    assert.deepEqual(labels, ['Provider', 'Reachability', 'Identity', 'Fibre version', 'Share of rows', 'Shards', 'Endorsed (24h)', 'Last successful upload', 'Metrics received'])
    assert.equal(surfaces.get('detail-fibre-facts').children[6].lastChild.style.color, 'var(--critical)')
    const note = surfaces.get('detail-fibre-note')
    assert.equal(note.textContent, '')
    assert.equal(note.children.length, 1)
    const link = note.children[0]
    assert.equal(link.text, 'More in the Fibre tab →')
    assert.equal(link.href, '#fibre')
    const select = context.document.getElementById('fibre-chain')
    let selected
    select.onchange = event => { selected = event.target.value }
    link.onclick()
    assert.equal(selected, 'Chain')
    assert.equal(context.closed, true)
})

test('Detail Fibre retains missing-metrics note and hides unknown provider facts', () => {
    const { surfaces, labels } = detailFacts({ registered: false, metrics_received: false })
    assert.deepEqual(labels, ['Provider', 'Share of rows', 'Shards', 'Endorsed (24h)', 'Fibre metrics'])
    assert.equal(surfaces.get('detail-fibre-facts').lastChild.lastChild.text, 'Not received, see docs/fibre.md')
    assert.match(surfaces.get('detail-fibre-note').textContent, /^Share and budget come from the chain/)
    assert.equal(surfaces.get('detail-fibre-note').children.at(-1).href, '#fibre')
})


test('Fibre defaults follow content order and preserve saved widths', () => {
    const panels = normalize(null).panels
    assert.deepEqual(panels.map(p => p.id), ['endorsements', 'uploads', 'downloads', 'latency', 'storage', 'received', 'errors', 'reach', 'resources', 'signing'])
    assert.deepEqual(panels.filter(p => p.wide).map(p => p.id), ['endorsements', 'latency', 'storage'])
    assert.deepEqual(panels.filter(p => !p.show).map(p => p.id), ['signing'])
    assert.ok(panels.every(p => p.chart === 'auto'))
    const saved = normalize({ panels: [{ id: 'endorsements', wide: false }, { id: 'storage', wide: true }] })
    assert.equal(saved.panels[0].wide, false)
    assert.equal(saved.panels[1].wide, true)
})

test('Summary percentages use observed range history and retain outage and budget notes', () => {
    const points = [{ reachable: 1, metrics: { endorsements_endorsed: 8, endorsements_missed: 2, uploads_ok: 7, uploads_failed: 2, uploads_rejected: 1 } }, { reachable: null, metrics: {} }, { reachable: 0.5, metrics: { endorsements_endorsed: 9, endorsements_missed: 1 } }]
    const facts = summaryFacts({ reachable: true, occupancy_known: true, occupancy: 1024, chain_budget: 2048, last_upload_at: 880 }, points, '7d', 1000)
    assert.equal(facts[0].sub, '75% over 7d')
    assert.equal(facts[1].value, '85.0%')
    assert.equal(facts[1].sub, '3 missed of 20')
    assert.equal(facts[2].value, '70%')
    assert.equal(facts[2].failed, 2)
    assert.equal(facts[3].value, '2m ago')
    assert.equal(facts[4].value, '1 KiB')
    assert.equal(facts[4].sub, 'of 2 KiB')
    const empty = summaryFacts({ reachable: false, down_since: 280, down_reason: 'timeout' }, [], '1h', 1000)
    assert.equal(empty[0].sub, 'down 12m · timeout')
    assert.equal(empty[1].value, '—')
    assert.equal(empty[1].sub, 'none seen')
    assert.equal(empty[2].value, '—')
    assert.equal(empty[3].value, 'none yet')
    assert.equal(summaryFacts({ reachable: true }, [], '24h')[0].sub, '')
    assert.equal(summaryFacts({ pending: true }, [], '24h')[0].value, 'Checking…')
})

test('Reachability slots average observations, exclude boundaries and use status thresholds', () => {
    for (const [range, count, seconds] of [['1h', 12, 300], ['24h', 48, 1800], ['7d', 42, 14400], ['30d', 30, 86400]]) {
        const slots = reachSlots([{ time: 100, reachable: 1 }, { time: 101, reachable: null }, { time: 100 + seconds, reachable: 1 }, { time: 101 + seconds, reachable: 0 }], range)
        assert.equal(slots.length, count)
        assert.equal(slots[0].reachable, 1)
        assert.equal(slots[0].color, 'good')
        assert.equal(slots[1].reachable, 0.5)
        assert.equal(slots[1].color, 'warning')
        assert.equal(slots[2].reachable, null)
        assert.equal(slots[2].color, 'line')
        assert.equal(slots.at(-1).end, 100 + count * seconds)
    }
    assert.deepEqual([null, 0, 0.499, 0.5, 0.998, 0.999, 1].map(reachColor), ['line', 'critical', 'critical', 'warning', 'warning', 'good', 'good'])
})

test('Leading not-recorded detection distinguishes zero observations from missing history', () => {
    const def = definitions.find(d => d.id === 'uploads')
    const points = Array.from({ length: 100 }, (_, i) => ({ time: i, metrics: i < 40 || i === 60 ? {} : { uploads_ok: 0 } }))
    assert.equal(leadingUnrecorded(def, points), 0.4)
    assert.equal(leadingUnrecorded(def, points.slice(37)), 3 / 63)
    assert.equal(leadingUnrecorded(def, points.slice(39)), 0)
    assert.equal(leadingUnrecorded(def, []), 0)
    const context = chartContext('1h'); context.def = def; context.points = points
    vm.runInContext("chart = drawChart(def, points, 500, 'line')", context)
    assert.ok(context.chart.children[0].children.some(n => n.tag === 'defs'))
    assert.ok(context.chart.children[0].children.some(n => n.tag === 'text' && n.textContent === 'Not recorded yet'))
})

test('Chain picker groups networks and switches the single-chain name and TESTNET tag', () => {
    const surfaces = new Map(), node = (tag, text) => ({ tag, text, children: [], append(...children) { this.children.push(...children) }, replaceChildren(...children) { this.children = children } })
    const get = id => { if (!surfaces.has(id)) surfaces.set(id, node('div')); return surfaces.get(id) }
    const context = vm.createContext({ node, $: get, chainGroups, chains: [{ name: 'Mocha', testnet: true }, { name: 'Celestia' }], selected: 'Mocha' })
    const picker = fibreSource.slice(fibreSource.indexOf('    function chainPicker('), fibreSource.indexOf('    function cards('))
    vm.runInContext(picker + "chainPicker('fibre', chains, selected)", context)
    assert.deepEqual(get('fibre-chain').children.map(g => g.label), ['Mainnet', 'Testnet'])
    assert.equal(get('fibre-chain').children[1].children[0].selected, true)
    assert.equal(get('fibre-testnet').hidden, false)
    assert.equal(get('fibre-chain-name').hidden, true)
    context.chains = [{ name: 'Celestia' }]; context.selected = 'Celestia'
    vm.runInContext("chainPicker('bridge', chains, selected)", context)
    assert.equal(get('bridge-chain').children.length, 1)
    assert.equal(get('bridge-chain-label').hidden, true)
    assert.equal(get('bridge-chain-name').textContent, 'Celestia')
    assert.equal(get('bridge-testnet').hidden, true)
    assert.equal(get('bridge-heading').textContent, 'Bridge')
})


test('Reachability renders full-height slots and interval tooltips including no data', () => {
    const context = chartContext('1h')
    context.def = definitions.find(d => d.id === 'reach')
    context.points = [{ time: 1000, reachable: 1 }, { time: 1300, reachable: 0.5 }, { time: 1600, reachable: 0 }]
    vm.runInContext("chart = drawChart(def, points, 500, 'auto'); key = legend(def)", context)
    const nodes = context.chart.children[0].children
    const bars = nodes.filter(n => n.tag === 'rect' && n.attributes.rx === 2)
    assert.equal(bars.length, 12)
    assert.deepEqual(Array.from(bars.slice(0, 3), n => n.attributes.fill), ['var(--good)', 'var(--warning)', 'var(--critical)'])
    assert.ok(bars.slice(3).every(n => n.attributes.fill.startsWith('color-mix')))
    assert.ok(bars.every(n => n.attributes.height === 110))
    const hits = nodes.filter(n => n.attributes?.tabindex === '0')
    hits[0].listeners.focus()
    assert.ok(textOf(context.chart.children[1]).includes('100% reachable'))
    assert.ok(textOf(context.chart.children[1].children[0]).includes('\u2013'))
    hits[3].listeners.focus()
    assert.ok(textOf(context.chart.children[1]).includes('No data'))
    assert.equal(nodes.some(n => n.tag === 'defs'), false)
    assert.ok(textOf(context.key).includes('Partly down'))
})
