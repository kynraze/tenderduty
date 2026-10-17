const test = require('node:test')
const assert = require('node:assert/strict')
const { dashboard } = require('./support/status.cjs')
const now = 10000
const proposal = (id, extra = {}) => ({ id, title: `Proposal ${id}`, end: now + 104400, voted: false, ...extra })
const body = row => row.children[1].children
const side = row => row.children[2].children

test('Governance sorts by vote, end, chain and id and expands after three', () => {
    const out = dashboard([
        { name: 'Zulu', proposals: [proposal(1), proposal(2, { end: now + 200 })] },
        { name: 'Alpha', proposals: [proposal(3), proposal(2), proposal(1, { voted: true, end: now - 1 })] },
        { name: 'Beta', proposals: [proposal(4), proposal(5)] }
    ])
    out.run('renderGovernance()')
    const list = out.get('governance-list')
    assert.equal(list.children.length, 3)
    assert.equal(out.get('governance-count').textContent, '7 open · 6 not voted')
    assert.equal(out.get('governance-more').textContent, 'Show 4 more')
    out.get('governance-more').onclick()
    out.run('renderGovernance()')
    assert.equal(out.get('governance-more').textContent, 'Show less')
    assert.deepEqual(list.children.map(row => body(row).map(node => node.textContent)), [
        ['Zulu', '#2 Proposal 2'], ['Alpha', '#2 Proposal 2'], ['Alpha', '#3 Proposal 3'],
        ['Beta', '#4 Proposal 4'], ['Beta', '#5 Proposal 5'], ['Zulu', '#1 Proposal 1'], ['Alpha', '#1 Proposal 1']
    ])
    list.children[0].events.click()
    assert.deepEqual(out.opened, ['Zulu'])
    out.get('governance-more').onclick()
    assert.equal(list.children.length, 3)
})

test('Governance uses deadline pills, duration and safe titles', () => {
    const out = dashboard([{ name: '<Chain>', proposals: [
        proposal(1, { title: '<script>alert(1)</script>' }),
        proposal(2, { end: now + 86400 }), proposal(3, { end: now + 86399 }),
        proposal(4, { end: now - 1 }), proposal(5, { voted: true, end: now + 100 })
    ] }])
    out.run('renderGovernance()')
    out.get('governance-more').onclick()
    const rows = new Map(out.get('governance-list').children.map(row => [body(row)[1].textContent.split(' ')[0], row]))
    for (const [id, tone, label] of [[1, 'amber', 'Not voted'], [2, 'amber', 'Not voted'], [3, 'red', 'Not voted'], [4, 'red', 'Not voted'], [5, 'good', 'Voted']]) {
        assert.equal(side(rows.get(`#${id}`))[0].className, `status governance-pill ${tone}`)
        assert.equal(side(rows.get(`#${id}`))[0].textContent, label)
    }
    assert.equal(side(rows.get('#1'))[1].textContent, '1d 5h left')
    assert.equal(side(rows.get('#4'))[1].textContent, 'ended')
    const title = body(rows.get('#1'))[1]
    assert.equal(title.title, '<script>alert(1)</script>')
    assert.equal(title.children.length, 0)
})

test('Empty governance clears rows and shares full width with attention', () => {
    const out = dashboard([{ name: 'Chain', proposals: [proposal(1)] }])
    out.get('attention-section').hidden = true
    out.run('renderGovernance()')
    assert.equal(out.get('attention-grid').hidden, false)
    out.context.chains.clear()
    out.run('renderGovernance()')
    assert.equal(out.get('governance-section').hidden, true)
    assert.equal(out.get('governance-list').children.length, 0)
    assert.equal(out.get('governance-more').hidden, true)
    assert.equal(out.get('attention-grid').hidden, true)
})
