const test = require('node:test')
const assert = require('node:assert/strict')
const fs = require('node:fs')
const vm = require('node:vm')

const source = fs.readFileSync(require.resolve('../td2/static/status.js'), 'utf8')

function detail(chain, surfaces = new Map()) {
    const element = (tag, className, text) => ({
        tag, className, text, children: [],
        appendChild(child) { this.children.push(child) },
        replaceChildren() { this.children = [] }
    })
    const context = {
        chains: new Map([['Chain', chain]]),
        document: { getElementById(id) {
            if (!surfaces.has(id)) surfaces.set(id, element('div'))
            return surfaces.get(id)
        } },
        element, stateFor: () => ({ label: 'Healthy' }), number: String,
        age: () => 'now', jailBudget: () => '', votingPower: () => ({}),
        fillFibre: () => {}, blockTrace: () => element('div'), newBlockAge: () => 0,
        blockSummary: () => ({ total: 0, text: '' })
    }
    vm.createContext(context)
    const append = source.slice(source.indexOf('function appendFact('), source.indexOf('function fillFibre('))
    const fill = source.slice(source.indexOf('function fillDetail('), source.indexOf('function showDetail('))
    vm.runInContext(append + fill + "fillDetail('Chain')", context)
    return surfaces.get('detail-facts').children
        .filter(row => row.children[0].text === 'RPC node')
        .map(row => row.children[1].text)
}

test('Chain RPC list renders own software versions and endpoint health', () => {
    assert.deepEqual(detail({ rpc_nodes: [
        { endpoint: 'localhost:26657', version: 'v10.4.0', down: true },
        { endpoint: 'rpc.kynraze.com', version: 'v11.0.0', down: false },
        { endpoint: 'public.example', down: false }
    ] }), [
        'localhost:26657 · v10.4.0 · down',
        'rpc.kynraze.com · v11.0.0 · healthy',
        'public.example · healthy'
    ])
})

test('Chain detail tolerates hidden node lists and displays unreadable versions', () => {
    assert.deepEqual(detail({}), [])
    assert.deepEqual(detail({ rpc_nodes: [{ endpoint: 'localhost:26657', version: 'version unknown', down: true }] }),
        ['localhost:26657 · version unknown · down'])
})

test('Chain detail uses filtered alert lines without raw proposal alarms', () => {
    const surfaces = new Map()
    detail({ last_error: 'Proposal #1\nValidator has not voted', alerts: [{ text: 'RPC unavailable', detail: 'Endpoint down' }] }, surfaces)
    assert.equal(surfaces.get('detail-error').textContent, 'RPC unavailable\nEndpoint down')
    detail({ last_error: 'Proposal #1\nValidator has not voted' }, surfaces)
    assert.equal(surfaces.get('detail-error').textContent, '')
})

test('Block summary counts the history, not since start', () => {
    const context = { number: String }
    vm.createContext(context)
    const names = source.slice(source.indexOf('const statusNames ='), source.indexOf('let filter'))
    const summary = source.slice(source.indexOf('function blockSummary('), source.indexOf('function blockTrace('))
    vm.runInContext(names + summary + 'globalThis.out = blockSummary([3, 4, 2, 1, 0, 5, -1, 3])', context)
    assert.deepEqual({ ...context.out }, { total: 7, text: '4 signed · 1 proposed · 3 missed · 1 missed proposals' })
})

function state(chain) {
    const context = { chain }
    vm.createContext(context)
    const code = source.slice(source.indexOf('function stateFor('), source.indexOf('function jailBudget('))
    vm.runInContext(code + 'globalThis.out = stateFor(chain)', context)
    return { ...context.out }
}

test('Notice-only chains are blue and mixed alarms need attention', () => {
    const chain = { height: 100, active_alerts: 2, notice_alerts: 2, notice_label: 'Upgrade scheduled' }
    assert.deepEqual(state(chain), { label: 'Upgrade scheduled', tone: 'blue', rank: 3 })
    assert.deepEqual(state({ ...chain, notice_label: 'Notice' }), { label: 'Notice', tone: 'blue', rank: 3 })
    assert.deepEqual(state({ ...chain, notice_alerts: 1 }), { label: 'Needs attention', tone: 'red', rank: 2 })
    assert.deepEqual(state({ height: 100, active_alerts: 1 }), { label: 'Needs attention', tone: 'red', rank: 2 })
    assert.equal(state({ ...chain, jailed: true }).label, 'Jailed')
    assert.equal(state({ ...chain, height: 0 }).label, 'No data')
})

