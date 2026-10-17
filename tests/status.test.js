const test = require('node:test')
const assert = require('node:assert/strict')
const vm = require('node:vm')
const { dashboard, source } = require('./support/status.cjs')
function state(chain) {
    const context = vm.createContext({ chain })
    vm.runInContext(source.slice(source.indexOf('function stateFor('), source.indexOf('function jailBudget(')) + 'globalThis.out = stateFor(chain)', context)
    return { ...context.out }
}
const chain = (name, extra = {}) => ({ name, height: 100, active_alerts: 1, nodes: 3, healthy_nodes: 2, window: 100, bonded: true, ...extra })

test('No data with action alerts is red; absent or notice-only alerts stay grey', () => {
    for (const missing of [{ height: 0 }, { height: -1 }, { no_nodes: true, height: 100 }]) {
        assert.deepEqual(state({ ...missing, active_alerts: 2, notice_alerts: 1 }), { label: 'Needs attention', tone: 'red', rank: 1 })
        assert.deepEqual(state({ ...missing, active_alerts: 1 }), { label: 'Needs attention', tone: 'red', rank: 1 })
        assert.deepEqual(state(missing), { label: 'No data', tone: 'unknown', rank: 1 })
        assert.deepEqual(state({ ...missing, active_alerts: 1, notice_alerts: 1 }), { label: 'No data', tone: 'unknown', rank: 1 })
    }
})

test('Out of the active set is Inactive, other alerts still need attention', () => {
    const out = { height: 100, window: 100, active_set: 100, bonded: false, active_alerts: 1 }
    assert.equal(state(out).label, 'Inactive')
    const inactive = { text: 'Kynraze is no longer in the active set on mantra-1' }
    assert.equal(state({ ...out, alerts: [inactive] }).label, 'Inactive')
    assert.equal(state({ ...out, alerts: [{ text: 'Not in the active set' }] }).label, 'Inactive')
    assert.equal(state({ ...out, active_alerts: 2, alerts: [inactive, { text: 'RPC down' }] }).label, 'Needs attention')
    assert.equal(state({ ...out, height: 0 }).label, 'Needs attention')
    assert.equal(state({ ...out, active_set: 0 }).label, 'Needs attention')
})

test('A down endpoint without an alert leaves the chain healthy', () => {
    const healthy = { height: 100, active_alerts: 0, nodes: 3, healthy_nodes: 2, window: 100, bonded: true, last_block_at: Date.now() / 1000 }
    assert.deepEqual(state(healthy), { label: 'Healthy', tone: 'good', rank: 4 })
})


test('Summary aggregates endpoints and closest jail budget', () => {
    const out = dashboard([
        chain('Alpha', { bonded: true, max_missed: 50, missed: 3 }),
        chain('Beta', { bonded: true, nodes: 2, healthy_nodes: 2, max_missed: 20, missed: 5 }),
        chain('Stale', { bonded: true, window: 0, max_missed: 1, missed: 1 }),
        chain('Inactive', { bonded: false, nodes: 0, healthy_nodes: 0, max_missed: 10, missed: 0 })
    ])
    out.run('renderSummary()')
    assert.equal(out.get('rpc-value').textContent, '6 / 8')
    assert.equal(out.get('rpc-up-bar').style.width, '75%')
    assert.equal(out.get('rpc-down-bar').style.width, '25%')
    assert.equal(out.get('jail-value').textContent, '15 blocks left')
    assert.equal(out.get('jail-foot').textContent, 'Beta · 5 / 20 missed')
    out.context.chains.clear()
    out.run('renderSummary()')
    assert.equal(out.get('jail-value').textContent, '—')
})

test('Attention rows sort, show severity and elapsed time, and retain expansion', () => {
    const out = dashboard([
        chain('Zulu'), chain('Beta', { height: 0, alerts: [{ text: 'RPC down', severity: 'warning', since: 10000 - 7500 }, { text: 'Another alarm' }] }),
        chain('Alpha', { no_nodes: true }), chain('Jailed', { jailed: true }),
        chain('Notice', { notice_alerts: 1 })
    ])
    out.run('renderSummary()')
    const list = out.get('priority-list')
    assert.deepEqual(list.children.map(row => row.children[1].children[0].textContent), ['Jailed', 'Alpha', 'Beta'])
    assert.equal(list.children[0].children[2].children[0].textContent, 'jailed')
    const beta = list.children[2]
    assert.equal(beta.children[1].children[1].textContent, 'RPC down')
    assert.equal(beta.children[1].children[1].title, 'RPC down\nAnother alarm')
    assert.equal(beta.children[2].children[0].className, 'severity warning')
    assert.equal(beta.children[2].children[1].textContent, '2h 5m')
    beta.events.click()
    assert.deepEqual(out.opened, ['Beta'])
    assert.equal(out.get('attention-more').textContent, 'Show 1 more')
    out.get('attention-more').onclick()
    out.run('renderSummary()')
    assert.equal(list.children.length, 4)
    assert.equal(out.get('attention-more').textContent, 'Show less')
    out.get('attention-more').onclick()
    assert.equal(list.children.length, 3)
})

test('Attention fallback is safe and unknown start time is omitted', () => {
    const out = dashboard([chain('Hidden', { last_error: 'private error' })])
    out.run('renderSummary()')
    const row = out.get('priority-list').children[0]
    assert.equal(row.children[1].children[1].textContent, '1 active alert')
    assert.equal(row.children[2].children.length, 1)
    out.context.chains.clear()
    out.context.chains.set('Notice', chain('Notice', { notice_alerts: 1 }))
    out.run('renderSummary(); renderGovernance()')
    assert.equal(out.get('priority-list').children.length, 0)
    assert.equal(out.get('attention-count').hidden, true)
    assert.equal(out.get('attention-grid').hidden, true)
})

test('Search, network and status filters combine without changing counts', () => {
    const out = dashboard([
        chain('Alpha', { testnet: true, chain_id: 'test-1', moniker: 'Keeper' }),
        chain('Beta', { chain_id: 'main-1', moniker: 'Keeper' }),
        chain('Notice', { testnet: true, notice_alerts: 1, chain_id: 'test-1' }),
        chain('Unknown', { testnet: true, height: 0, active_alerts: 0 })
    ])
    out.run("filter = 'attention'; networkFilter = 'testnet'; search = 'KEEPER'")
    assert.deepEqual(out.visible(), ['Alpha'])
    out.run("search = 'TEST-1'; filter = 'notice'")
    assert.deepEqual(out.visible(), ['Notice'])
    out.run("filter = 'unavailable'; search = ''")
    assert.deepEqual(out.visible(), ['Unknown'])
    out.run("networkFilter = 'mainnet'; filter = 'all'; search = 'beta'; renderSummary()")
    assert.deepEqual(out.visible(), ['Beta'])
    assert.equal(out.get('filter-count-all').textContent, 4)
    assert.equal(out.get('filter-count-notice').textContent, 1)
    assert.equal(out.get('network-count-testnet').textContent, 3)
    assert.equal(out.get('network-filters').hidden, false)
})

test('Table uses newest 160 blocks, compact voting rank, testnet tag and endpoint dots', () => {
    const out = dashboard([chain('Alpha', { testnet: true, chain_id: 'alpha-1', moniker: 'Kynraze', blocks: Array(512).fill(3), voting_power: 10, total_power: 100, rank: 2, active_set: 4, max_validators: 10, rpc_nodes: [{ down: true }, { down: false }, { down: false }] })])
    out.run("globalThis.row = renderRow(chains.get('Alpha'))")
    const cells = out.context.row.children
    assert.equal(cells[0].children[0].children[1].children[0].children[1].textContent, 'TESTNET')
    // a single moniker stays in the tooltip, two show in the row
    assert.equal(cells[0].children[0].children[1].children[1].textContent, 'alpha-1')
    out.context.chains.set('Beta', chain('Beta', { moniker: 'Other' }))
    out.run("globalThis.row = renderRow(chains.get('Alpha'))")
    assert.equal(out.context.row.children[0].children[0].children[1].children[1].textContent, 'alpha-1 · Kynraze')
    assert.equal(cells[2].children[1].textContent, '10.00% · #2 of 4')
    assert.equal(cells[4].children[0].children.length, 160)
    assert.match(cells[4].children[0].attributes['aria-label'], /160 signed/)
    assert.deepEqual(cells[6].children[1].children.map(dot => dot.className), ['rpc-dot down', 'rpc-dot', 'rpc-dot'])
})

test('Network filter recovers when no testnets remain', () => {
    const out = dashboard([{ name: 'Mainnet', nodes: 1, healthy_nodes: 1 }])
    out.run("networkFilter = 'testnet'; renderSummary()")
    assert.equal(out.get('network-filters').hidden, true)
    assert.deepEqual(out.visible(), ['Mainnet'])
})

test('Voting power prefers token stake and keeps consensus share and rank', () => {
    const out = dashboard([chain('Cheqd', { stake: '19.8K CHEQ', voting_power: 19800000, total_power: 198000000, rank: 29, active_set: 100 })])
    out.run("globalThis.row = renderRow(chains.get('Cheqd'))")
    const cells = out.context.row.children
    assert.equal(cells[2].children[0].textContent, '19.8K CHEQ')
    assert.equal(cells[2].children[1].textContent, '10.00% \u00b7 #29 of 100')
    out.run("chains.get('Cheqd').stake = ''; globalThis.row = renderRow(chains.get('Cheqd'))")
    assert.equal(out.context.row.children[2].children[0].textContent, '19,800,000')
})
