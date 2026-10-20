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
    assert.deepEqual(out.opened, [])
    assert.equal(out.get('chain-dialog').open, true)
    assert.equal(out.get('detail-heading').textContent, 'Zulu \u00b7 Unknown chain \u00b7 Proposal #2')
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

const unsafe = '<img src=x onerror=alert(1)>'
const details = { chain: 'Akash', chain_id: 'akashnet-2', moniker: 'Validator', ...proposal(343), title: unsafe, start: now - 3600, summary: unsafe + '\n' + 'word '.repeat(120), tally_error: 'tally unavailable from this node' }
function proposalDashboard(options = {}) {
    return dashboard([{ name: details.chain, chain_id: details.chain_id, moniker: details.moniker, proposals: [proposal(details.id, { title: unsafe })] }], options)
}

test('Governance opens proposal immediately, renders untrusted text and toggles summary', async () => {
    let resolve
    const response = new Promise(done => { resolve = done })
    let url
    const out = proposalDashboard({ fetch: path => { url = path; return response } })
    out.run('renderGovernance()')
    const row = out.get('governance-list').children[0]
    const loading = row.events.click()
    assert.equal(out.get('chain-dialog').open, true)
    assert.deepEqual(out.opened, [])
    assert.equal(out.get('detail-subtitle').textContent, unsafe)
    assert.equal(out.get('detail-subtitle').children.length, 0)
    assert.equal(out.get('proposal-tally').children[0].textContent, 'Loading\u2026')
    assert.equal(url, 'proposal?chain=Akash&id=343')
    resolve({ ok: true, json: async () => details })
    await loading
    assert.equal(out.get('proposal-tally').children[0].textContent, details.tally_error)
    const [text, toggle] = out.get('proposal-summary').children
    assert.equal(text.textContent, Array.from(details.summary).slice(0, 400).join('') + '\u2026')
    assert.equal(text.children.length, 0)
    toggle.events.click()
    assert.equal(text.textContent, details.summary)
    assert.equal(toggle.textContent, 'Show less')
    toggle.events.click()
    assert.equal(toggle.textContent, 'Show full summary')
    // Overview updates replace rows while the dialog is open.
    out.run('renderGovernance()')
    out.run('restoreProposalFocus()')
    assert.equal(out.get('governance-list').children[0].focused, true)
})

test('Proposal fetch failure keeps header and explains missing details', async () => {
    const out = proposalDashboard({ fetch: async () => ({ ok: false }) })
    out.run('renderGovernance()')
    await out.get('governance-list').children[0].events.click()
    assert.equal(out.get('detail-subtitle').textContent, unsafe)
    for (const id of ['proposal-tally', 'proposal-summary']) assert.equal(out.get(id).children[0].textContent, 'Could not load proposal details')
})

test('Proposal tally uses all four vote shares and optional turnout', () => {
    const out = proposalDashboard()
    out.run('fillProposalTally({ tally: { yes: 70, abstain: 20, no: 9, veto: 1, turnout: 46.94 } }); fillProposalSummary("")')
    const [bar, legend, turnout] = out.get('proposal-tally').children
    assert.deepEqual(bar.children.map(item => item.style.width), ['70%', '20%', '9%', '1%'])
    assert.deepEqual(legend.children.map(item => item.textContent), ['Yes 70.0%', 'Abstain 20.0%', 'No 9.0%', 'Veto 1.0%'])
    assert.equal(turnout.textContent, 'Turnout 46.9% of bonded stake')
    assert.equal(out.get('proposal-summary').children[0].textContent, 'No summary on chain')
})

test('Late proposal responses cannot overwrite another panel', async () => {
    let resolve
    const out = proposalDashboard({ fetch: () => new Promise(done => { resolve = done }) })
    out.run('renderGovernance()')
    const loading = out.get('governance-list').children[0].events.click()
    out.get('chain-dialog').dataset.proposal = ''
    out.get('detail-subtitle').textContent = 'Other panel'
    resolve({ ok: true, json: async () => details })
    await loading
    assert.equal(out.get('detail-subtitle').textContent, 'Other panel')
})

test('Proposal header retains vote deadline tones and an unknown start date', () => {
    const out = proposalDashboard()
    for (const [voted, end, tone] of [[false, 96400, 'amber'], [false, 96399, 'red'], [true, 10001, 'good']]) {
        out.context.header = { ...details, voted, end, start: 0 }
        out.run('fillProposalHeader(header)')
        assert.equal(out.get('detail-vote').className, `status governance-pill ${tone}`)
        assert.equal(out.get('detail-facts').children[1].children[1].textContent, '\u2014')
    }
})
