const test = require('node:test')
const assert = require('node:assert/strict')
const { readFileSync } = require('node:fs')
const vm = require('node:vm')
const { normalize, definitions, headline } = require('../td2/static/bridge.js')
const def = id => definitions.find(d => d.id === id)
const points = (...metrics) => metrics.map(m => ({ metrics: m }))

test('Bridge defaults show eight panels and hide memory and restarts', () => {
    const layout = normalize(null)
    assert.equal(layout.range, '24h')
    assert.equal(layout.panels.length, 10)
    assert.deepEqual(layout.panels.filter(p => !p.show).map(p => p.id), ['memory', 'restarts'])
    assert.equal(def('memory').area, true)
})

test('Bridge layouts drop unknown and duplicate ids and recover invalid fields', () => {
    const layout = normalize({ range: 'bad', panels: [null, { id: 'unknown' }, { id: 'responses', show: false, wide: true, chart: 'line' }, { id: 'responses' }, { id: 'memory', show: false, chart: 'bad' }] })
    assert.equal(layout.range, '24h')
    assert.equal(layout.panels.length, definitions.length)
    assert.deepEqual(layout.panels[0], { id: 'responses', show: false, wide: true, chart: 'line' })
    assert.equal(layout.panels[1].show, false)
    assert.equal(layout.panels[1].chart, 'auto')
    assert.equal(normalize({ panels: 'bad' }).panels.length, 10)
    for (const range of ['1h', '24h', '7d', '30d']) assert.equal(normalize({ range, panels: [] }).range, range)
})

test('Bridge responses total all groups and highlight only internal errors', () => {
    const result = headline(def('responses'), points({ responses_success: 18, responses_internal_err: 2, responses_peer_err: 5 }, { responses_success: 15 }))
    assert.equal(result[0][0], '40 total')
    assert.deepEqual(result[1], ['5% internal errors', 'critical'])
    assert.equal(headline(def('responses'), points({ responses_success: 0 }))[1][0], '0% internal errors')
    assert.deepEqual(headline(def('responses'), points(null)), [])
})

test('Bridge pruning uses latest retained time and failures and totals reclaimed bytes', () => {
    const result = headline(def('pruning'), points({ retained_hours: 4, reclaimed_bytes: 1024, failed_prunes: 0 }, { retained_hours: 169, reclaimed_bytes: 2048, failed_prunes: 2 }))
    assert.equal(result[0][0], '7d 1h kept')
    assert.equal(result[1][0], '3 KiB reclaimed')
    assert.deepEqual(result[2], ['2 failed prunes', 'critical'])
})

test('Bridge restart versions preserve order and collapse repeated observations', () => {
    const result = headline(def('restarts'), [{ metrics: { restarts: 0 }, version: '0.34.2' }, { metrics: { restarts: 1 }, version: '0.34.2' }, { metrics: null, version: 'v0.34.3' }])
    assert.equal(result[0][0], '1 restart')
    assert.equal(result[1][0], 'v0.34.2 → v0.34.3')
})

test('Bridge latency headlines use whole-range weighted averages', () => {
    for (const [id, seconds, count] of [['da_lag', 'da_lag_seconds', 'da_blocks'], ['serve_latency', 'serve_seconds', 'serve_requests'], ['disk_latency', 'put_seconds', 'puts']]) {
        const result = headline(def(id), points({ [seconds]: 1, [count]: 2 }, { [seconds]: 1, [count]: 8, put_failures: 3 }))
        assert.equal(result[0][0], '200 ms')
        if (id === 'da_lag') assert.equal(result[1][0], '10 blocks')
        if (id === 'disk_latency') assert.deepEqual(result[1], ['3 failed', 'critical'])
        assert.deepEqual(headline(def(id), points({ [seconds]: 0, [count]: 0 })).filter(([text]) => text.endsWith(' ms')), [])
    }
})

test('Bridge sync stays neutral and peers warn below two full peers', () => {
    assert.deepEqual(headline(def('sync'), points({ head_lag: 3, head_lag_max: 10, sample_lag: 4 }, { head_lag: 1, head_lag_max: 2, sample_lag: 0 })), [['1 behind · 0 sampling behind', undefined], ['max 10', undefined]])
    assert.equal(headline(def('peers'), points({ full_peers: 1, archival_peers: 5 }))[0][1], 'warning')
    assert.deepEqual(headline(def('peers'), points(null)), [])
})

test('Bridge starts with defaults when stored JSON is corrupt', () => {
    const source = readFileSync(require.resolve('../td2/static/bridge.js'), 'utf8')
    const init = source.slice(0, source.indexOf('    let range = layout.range'))
    const context = vm.createContext({ module: { exports: {} }, document: {}, localStorage: { getItem: () => '{invalid' } })
    vm.runInContext(init + '\nmodule.exports.layout = layout\n})()', context)
    assert.equal(context.module.exports.layout.range, '24h')
    assert.equal(context.module.exports.layout.panels.filter(p => p.show).length, 8)
})

test('Shared charts use the Bridge view, average legends and byte tooltips', () => {
    const source = readFileSync(require.resolve('../td2/static/fibre.js'), 'utf8')
    const node = (tag, text, cls) => ({ tag, text, cls, style: {}, children: [], listeners: {}, append(...children) { this.children.push(...children) }, replaceChildren(...children) { this.children = children }, setAttribute() {}, addEventListener(event, listener) { this.listeners[event] = listener } })
    const context = vm.createContext({ node, svg: (tag, attributes) => ({ ...node(tag), attributes }), range: '30d', selected: 'Fibre', document: { createTextNode: text => node('text', text) } })
    const helpers = source.slice(source.indexOf('    const value ='), source.indexOf('    if (typeof module'))
    const charts = source.slice(source.indexOf('    // rounds up'), source.indexOf('    function drawPanels()'))
    vm.runInContext('const definitions = []; const tooltipLabels = {};\n' + helpers + charts, context)
    context.def = def('pruning')
    context.points = [{ time: 1000, metrics: { retained_hours: 169, reclaimed_bytes: 2048 } }]
    context.view = { range: '1h', selected: 'Bridge chain', headline, tooltipLabels: { reclaimed_bytes: 'Reclaimed' } }
    vm.runInContext("chart = drawChart(def, points, 500, 'line', view)", context)
    const svg = context.chart.children[0]
    assert.match(svg.attributes['aria-label'], /Pruning, Bridge chain, 1h: 7d 1h kept/)
    svg.children.find(n => n.listeners.pointerenter)?.listeners.pointerenter()
    assert.ok(context.chart.children[1].children.some(n => n.children.some(child => child.text === '2 KiB')))
    context.def = def('serve_latency')
    vm.runInContext('key = legend(def, view)', context)
    assert.ok(!context.key.children.some(n => n.text?.includes('p95')))
})

test('Bridge routing follows watched chains, refreshes history and aborts on leave', async () => {
    const handlers = {}, windowHandlers = {}, elements = new Map(), fetches = [], intervals = new Map()
    const element = tag => ({ tag, hidden: false, style: {}, dataset: {}, children: [], clientWidth: 600, attributes: {}, classList: { toggle() {} }, append(...nodes) { this.children.push(...nodes) }, replaceChildren(...nodes) { this.children = nodes }, setAttribute(key, value) { this.attributes[key] = value }, removeAttribute(key) { delete this.attributes[key] }, addEventListener() {}, querySelectorAll() { return this.children.filter(n => n.dataset.panel) } })
    const get = id => { if (!elements.has(id)) { const e = element('div'); e.id = id; elements.set(id, e) } return elements.get(id) }
    const links = ['overview', 'fibre', 'bridge'].map(id => { const e = get(`${id}-nav`); e.hash = `#${id}`; return e })
    get('fibre-nav').hidden = true
    const location = { hash: '#bridge' }
    const window = { addEventListener(event, handler) { windowHandlers[event] = handler }, tdCharts: { legend: () => element('legend'), drawChart: () => element('chart') } }
    const context = vm.createContext({ window, location, document: { hidden: false, getElementById: get, createElement: element, createTextNode: text => ({ text }), querySelectorAll: () => links, addEventListener(event, handler) { handlers[event] = handler } }, localStorage: { getItem: () => null, setItem() {} }, AbortController, getComputedStyle: () => ({ paddingLeft: '16', paddingRight: '16' }), setInterval(fn, delay) { intervals.set(fn, delay); return fn }, clearInterval(fn) { intervals.delete(fn) }, setTimeout, clearTimeout, async fetch(url, options) { fetches.push({ url, options }); return { ok: true, async json() { return { chains: [{ name: 'Chain', series: [{ time: 1, metrics: { served_bytes: 0 } }] }] } } } } })
    vm.runInContext(readFileSync(require.resolve('../td2/static/bridge.js'), 'utf8'), context)
    assert.equal(get('overview').hidden, false)
    const watched = [{ name: 'Chain', bridge: { facts: [{ label: 'Head', value: '100', tone: 'warning' }] } }]
    handlers['fibre-state']({ detail: watched })
    await new Promise(resolve => setImmediate(resolve))
    assert.equal(get('bridge-nav').hidden, false)
    assert.equal(get('bridge').hidden, false)
    assert.equal(get('overview').hidden, true)
    assert.equal(get('bridge-chain-label').hidden, true)
    assert.equal(get('bridge-heading').textContent, 'Bridge · Chain')
    assert.equal(fetches[0].url, 'bridge?range=24h&chain=Chain')
    assert.deepEqual([...intervals.values()], [60000])
    assert.equal(get('bridge-panels').children.length, 8)
    assert.equal(get('bridge-facts').children[0].children[1].style.color, 'var(--warning)')
    assert.equal(get('bridge-message').children.length, 0)
    // Begin another request, then leave before its continuation runs.
    windowHandlers.hashchange()
    location.hash = '#overview'
    windowHandlers.hashchange()
    assert.equal(fetches.at(-1).options.signal.aborted, true)
    assert.equal(get('overview').hidden, false)
    assert.equal(get('bridge').hidden, true)
    assert.equal(intervals.size, 0)
    assert.equal(get('overview-nav').attributes['aria-current'], 'page')
    location.hash = '#activity'
    windowHandlers.hashchange()
    assert.equal(get('overview').hidden, false)
    assert.equal(get('overview-nav').attributes['aria-current'], 'page')
    location.hash = '#bridge'
    handlers['fibre-state']({ detail: [] })
    assert.equal(get('overview').hidden, false)
    assert.equal(get('bridge-nav').hidden, true)
})
