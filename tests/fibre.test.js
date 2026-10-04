const test = require('node:test')
const assert = require('node:assert/strict')
const { normalize } = require('../td2/static/fibre.js')

test('Fibre panel settings recover from missing, duplicate and unknown panels', () => {
    const layout = normalize({ range: 'invalid', panels: [{ id: 'storage', show: false, wide: true }, { id: 'storage' }, { id: 'unknown' }, null] })
    assert.equal(layout.range, '24h')
    assert.equal(layout.panels.length, 9)
    assert.deepEqual(layout.panels[0], { id: 'storage', show: false, wide: true, chart: 'auto' })
    assert.equal(new Set(layout.panels.map(p => p.id)).size, 9)
})

test('Fibre preferences retain valid ranges and use defaults for corrupt storage', () => {
    assert.equal(normalize(null).panels.length, 9)
    assert.equal(normalize({ panels: 'broken' }).range, '24h')
    assert.equal(normalize({ range: '7d', panels: [] }).range, '7d')
    assert.equal(normalize({ panels: [{ id: 'uploads', show: 'false', wide: 'true' }] }).panels[0].wide, false)
})


test('Old Fibre layouts append visible latency and resources panels in order', () => {
    const ids = ['downloads', 'reach', 'uploads', 'received', 'signing', 'storage', 'errors']
    const layout = normalize({ range: '7d', panels: ids.map(id => ({ id, show: false, wide: true })) })
    assert.deepEqual(layout.panels.slice(0, 7).map(p => p.id), ids)
    assert.deepEqual(layout.panels.slice(7), [
        { id: 'latency', show: true, wide: false, chart: 'auto' },
        { id: 'resources', show: true, wide: false, chart: 'auto' }
    ])
    assert.equal(layout.range, '7d')
    assert.equal(layout.panels[0].show, false)
})

test('Fibre chart types keep a valid choice and fall back to auto', () => {
    const layout = normalize({ panels: [{ id: 'uploads', chart: 'area' }, { id: 'signing', chart: 'pie' }] })
    assert.equal(layout.panels[0].chart, 'area')
    assert.equal(layout.panels[1].chart, 'auto')
    assert.equal(normalize(null).panels[0].chart, 'auto')
})
