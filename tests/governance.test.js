const test = require('node:test')
const assert = require('node:assert/strict')
const fs = require('node:fs')
const vm = require('node:vm')

const source = fs.readFileSync(require.resolve('../td2/static/status.js'), 'utf8')
const now = Date.parse('2026-10-13T16:00:00Z') / 1000

function governance(values) {
    const surfaces = new Map()
    const opened = []
    const node = tag => ({
        tag, children: [], events: {},
        appendChild(child) { this.children.push(child) },
        replaceChildren() { this.children = [] },
        addEventListener(name, fn) { this.events[name] = fn }
    })
    const context = vm.createContext({
        chains: new Map(values.map(chain => [chain.name, chain])), now,
        showDetail: name => opened.push(name),
        document: {
            createElement: node,
            getElementById(id) {
                if (!surfaces.has(id)) surfaces.set(id, node('div'))
                return surfaces.get(id)
            }
        }
    })
    const elementCode = source.slice(source.indexOf('function element('), source.indexOf('function number('))
    const renderCode = source.slice(source.indexOf('function renderGovernance('), source.indexOf('function renderRow('))
    const durationCode = source.slice(source.indexOf('function durationText('), source.indexOf('function fibreProviderFacts('))
    vm.runInContext(elementCode + durationCode + renderCode + 'Date.now = () => now * 1000; renderGovernance()', context)
    return { surfaces, opened, context, rows: () => [...surfaces.get('governance-list').children, ...surfaces.get('governance-more-list').children] }
}

const proposal = (id, extra = {}) => ({ id, title: `Proposal ${id}`, end: now + 104400, voted: false, ...extra })

test('Governance sorts by vote, end, chain and id, and splits after five', () => {
    const out = governance([
        { name: 'Zulu', proposals: [proposal(1), proposal(2, { end: now + 200 })] },
        { name: 'Alpha', proposals: [proposal(3), proposal(2), proposal(1, { voted: true, end: now - 1 })] },
        { name: 'Beta', proposals: [proposal(4), proposal(5)] }
    ])
    assert.equal(out.surfaces.get('governance-section').hidden, false)
    assert.equal(out.surfaces.get('governance-count').textContent, '7 open · 6 not voted')
    assert.deepEqual(out.rows().map(row => [row.children[0].textContent, row.children[1].textContent]), [
        ['Zulu', '#2 Proposal 2'], ['Alpha', '#2 Proposal 2'], ['Alpha', '#3 Proposal 3'],
        ['Beta', '#4 Proposal 4'], ['Beta', '#5 Proposal 5'], ['Zulu', '#1 Proposal 1'], ['Alpha', '#1 Proposal 1']
    ])
    assert.equal(out.surfaces.get('governance-list').children.length, 5)
    assert.equal(out.surfaces.get('governance-more-list').children.length, 2)
    assert.equal(out.surfaces.get('governance-more-summary').textContent, 'Show 2 more')
    assert.equal(out.surfaces.get('governance-more').hidden, false)
    const details = out.surfaces.get('governance-more')
    details.open = true
    vm.runInContext('renderGovernance()', out.context)
    assert.equal(out.surfaces.get('governance-more'), details)
    assert.equal(details.open, true)
    out.rows()[0].children[0].events.click()
    assert.deepEqual(out.opened, ['Zulu'])
})

test('Governance formats UTC endings and uses good, warning and critical pills', () => {
    const out = governance([{ name: '<Chain>', proposals: [
        proposal(1, { title: '<script>alert(1)</script>' }),
        proposal(2, { end: now + 86400 }),
        proposal(3, { end: now + 86399 }),
        proposal(4, { end: now - 1 }),
        proposal(5, { voted: true, end: now + 100 })
    ] }])
    const rows = new Map(out.rows().map(row => [row.children[1].textContent.split(' ')[0], row]))
    for (const [id, tone, label] of [[1, 'amber', 'Not voted'], [2, 'amber', 'Not voted'], [3, 'red', 'Not voted'], [4, 'red', 'Not voted'], [5, 'good', 'Voted']]) {
        assert.equal(rows.get(`#${id}`).children[3].className, `status governance-pill ${tone}`)
        assert.equal(rows.get(`#${id}`).children[3].textContent, label)
    }
    assert.equal(rows.get('#1').children[2].textContent, 'Ends 14 Oct 21:00 UTC · 1d 5h left')
    assert.match(rows.get('#4').children[2].textContent, / · ended$/)
    const title = rows.get('#1').children[1]
    assert.equal(title.title, '<script>alert(1)</script>')
    assert.equal(title.textContent, '#1 <script>alert(1)</script>')
    assert.equal(title.children.length, 0)
    assert.equal(rows.get('#1').children[0].textContent, '<Chain>')
    assert.equal(out.surfaces.get('governance-more').hidden, true)
})

test('Governance hides and clears when no proposals remain', () => {
    const out = governance([{ name: 'Chain', proposals: [proposal(1)] }, { name: 'Other' }])
    out.context.chains.clear()
    vm.runInContext('renderGovernance()', out.context)
    assert.equal(out.surfaces.get('governance-section').hidden, true)
    assert.equal(out.rows().length, 0)
    assert.equal(out.surfaces.get('governance-more').hidden, true)
})
