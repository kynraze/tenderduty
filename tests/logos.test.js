const test = require('node:test')
const assert = require('node:assert/strict')
const { dashboard } = require('./support/status.cjs')

test('Avatar uses the cached logo and a relative encoded URL', () => {
    const name = 'Chain & Test/One'
    const out = dashboard([{ name, logo: true }])
    const img = out.run(`avatar(${JSON.stringify(name)})`)
    assert.equal(img.tag, 'img')
    assert.equal(img.className, 'avatar logo')
    assert.equal(img.src, `logo?chain=${encodeURIComponent(name)}`)
    assert.equal(img.alt, '')
    assert.equal(img.loading, 'lazy')
    assert.equal(img.decoding, 'async')
    let replacement
    img.replaceWith = node => { replacement = node }
    img.events.error()
    assert.equal(replacement.tag, 'span')
    assert.equal(replacement.className, 'avatar')
    assert.equal(replacement.textContent, 'CH')
})

test('Avatar uses initials for uncached and unknown chains', () => {
    const out = dashboard([{ name: 'Alpha' }, { name: 'Beta', logo: false }])
    for (const name of ['Alpha', 'Beta', 'Unknown']) {
        const avatar = out.run(`avatar(${JSON.stringify(name)})`)
        assert.equal(avatar.tag, 'span')
        assert.equal(avatar.className, 'avatar')
        assert.equal(avatar.textContent, name.slice(0, 2).toUpperCase())
    }
})

test('Table, attention and governance rows share logo avatars', () => {
    const out = dashboard([{ name: 'Alpha', logo: true }])
    assert.equal(out.run("renderRow(chains.get('Alpha'))").children[0].children[0].children[0].tag, 'img')
    assert.equal(out.run("cardRow('Alpha', 'Alert or proposal', '')").children[0].tag, 'img')
})
