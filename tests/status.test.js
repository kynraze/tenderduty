const test = require('node:test')
const assert = require('node:assert/strict')
const fs = require('node:fs')
const vm = require('node:vm')

const source = fs.readFileSync(require.resolve('../td2/static/status.js'), 'utf8')
const stateCode = source.slice(source.indexOf('function stateFor('), source.indexOf('function jailBudget('))

function state(chain) {
    const context = vm.createContext({ chain })
    vm.runInContext(stateCode + 'globalThis.out = stateFor(chain)', context)
    return { ...context.out }
}

function summary(values, logsEnabled = true) {
    const surfaces = new Map()
    const opened = []
    let filtered = false
    const node = tag => ({
        tag, children: [], events: {}, attributes: {},
        appendChild(child) { this.children.push(child) },
        replaceChildren() { this.children = [] },
        setAttribute(name, value) { this.attributes[name] = value },
        addEventListener(name, fn) { this.events[name] = fn }
    })
    const context = vm.createContext({
        chains: new Map(values.map(chain => [chain.name, chain])), logsEnabled,
        validatorLabel: count => count === 1 ? 'validator' : 'validators', age: () => 'now',
        showDetail: name => opened.push(name),
        document: {
            createElement: node,
            getElementById(id) {
                if (!surfaces.has(id)) surfaces.set(id, node('div'))
                return surfaces.get(id)
            },
            querySelector(selector) {
                assert.equal(selector, '[data-filter="attention"]')
                return { click() { filtered = true } }
            }
        }
    })
    const elementCode = source.slice(source.indexOf('function element('), source.indexOf('function number('))
    const summaryCode = source.slice(source.indexOf('function renderSummary('), source.indexOf('function renderRow('))
    vm.runInContext(elementCode + stateCode + summaryCode + 'renderSummary()', context)
    return { surfaces, opened, context, filtered: () => filtered, list: surfaces.get('priority-list').children }
}

const chain = (name, extra = {}) => ({ name, height: 100, active_alerts: 1, nodes: 3, healthy_nodes: 2, ...extra })

test('No data with action alerts is red; absent or notice-only alerts stay grey', () => {
    for (const missing of [{ height: 0 }, { height: -1 }, { no_nodes: true, height: 100 }]) {
        assert.deepEqual(state({ ...missing, active_alerts: 2, notice_alerts: 1 }), { label: 'Needs attention', tone: 'red', rank: 1 })
        assert.deepEqual(state({ ...missing, active_alerts: 1 }), { label: 'Needs attention', tone: 'red', rank: 1 })
        assert.deepEqual(state(missing), { label: 'No data', tone: 'unknown', rank: 1 })
        assert.deepEqual(state({ ...missing, active_alerts: 1, notice_alerts: 1 }), { label: 'No data', tone: 'unknown', rank: 1 })
    }
})

test('A down endpoint without an alert leaves the chain healthy', () => {
    const healthy = { height: 100, active_alerts: 0, nodes: 3, healthy_nodes: 2, window: 100, bonded: true, last_block_at: Date.now() / 1000 }
    assert.deepEqual(state(healthy), { label: 'Healthy', tone: 'good', rank: 4 })
})

test('Priority lists three red chains in rank/name order and opens remaining alerts', () => {
    const alerts = Array.from({ length: 5 }, (_, i) => ({ text: `Alarm ${i}`, detail: `Detail ${i}` }))
    const out = summary([
        chain('Zulu'), chain('Beta', { height: 0, alerts, last_error: 'raw error' }),
        chain('Alpha', { no_nodes: true }), chain('Jailed', { jailed: true }),
        chain('Notice', { notice_alerts: 1 }), chain('Unknown', { height: 0, active_alerts: 0 })
    ])
    assert.equal(out.surfaces.get('attention-count').textContent, 4)
    assert.equal(out.surfaces.get('coverage-count').textContent, '3/6')
    assert.deepEqual(out.list.slice(0, 3).map(item => item.children[0].children[0].textContent), ['Jailed', 'Alpha', 'Beta'])
    const beta = out.list[2]
    assert.equal(beta.children[0].children[1].textContent, 'Needs attention')
    const lines = beta.children.filter(node => node.className === 'priority-alert')
    assert.deepEqual(lines.map(node => node.textContent), ['Alarm 0', 'Alarm 1', 'Alarm 2'])
    assert.deepEqual(lines.map(node => node.children[0].textContent), ['Detail 0', 'Detail 1', 'Detail 2'])
    const more = beta.children.find(node => node.tag === 'button')
    assert.equal(more.textContent, '+2 more')
    assert.equal(more.type, 'button')
    more.events.click()
    assert.deepEqual(out.opened, ['Beta'])
    assert.equal(beta.children.at(-1).textContent, '1 of 3 RPC endpoints unreachable')
    assert.equal(out.list[3].textContent, '+1 more validators need attention')
    out.list[3].events.click()
    assert.equal(out.filtered(), true)
    assert.equal(out.surfaces.get('priority-context').textContent, '2/3 RPC endpoints healthy')
    assert.ok(source.includes("if (filter === 'attention') return stateFor(chain).tone === 'red'"))
    assert.ok(source.includes("if (filter === 'unavailable') return chain.height <= 0 || chain.no_nodes === true"))
})

test('Fallback avoids raw errors and hides unreachable counts when logs are hidden', () => {
    for (const alerts of [undefined, []]) {
        const out = summary([chain('Hidden', { alerts, last_error: 'private node error' })], false)
        const item = out.list[0]
        assert.deepEqual(item.children.slice(1).map(node => node.textContent), [
            '2 of 3 configured RPC endpoints responding', 'RPC endpoints unreachable'
        ])
    }
    const out = summary([chain('Connected', { healthy_nodes: 3, alerts: [{ text: '<script>alert(1)</script>' }] })])
    assert.equal(out.list[0].children.length, 2)
    assert.equal(out.list[0].children[1].textContent, '<script>alert(1)</script>')
    assert.equal(out.list[0].children[1].children.length, 0)
})

test('No red chains hides and clears the priority list', () => {
    const out = summary([chain('Action')])
    out.context.chains.clear()
    out.context.chains.set('Notice', chain('Notice', { notice_alerts: 1 }))
    vm.runInContext('renderSummary()', out.context)
    assert.equal(out.surfaces.get('attention-count').textContent, 0)
    assert.equal(out.surfaces.get('attention-section').hidden, true)
    assert.equal(out.surfaces.get('priority-list').children.length, 0)
})

test('Proposal-only alarms are notices and do not enter Priority', () => {
    const pending = chain('Pending', { active_alerts: 2, notice_alerts: 2, notice_label: 'Vote pending', proposals: [{ id: 1, voted: false }] })
    assert.deepEqual(state(pending), { label: 'Vote pending', tone: 'blue', rank: 3 })
    const out = summary([pending])
    assert.equal(out.surfaces.get('attention-section').hidden, true)
    assert.equal(out.list.length, 0)
})
