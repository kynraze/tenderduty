const test = require('node:test')
const assert = require('node:assert/strict')
const { presets, buildTheme, normalize, contrast } = require('../td2/static/appearance.js')

test('every preset keeps text and status labels readable on all surfaces', () => {
    for (const [name, colors] of Object.entries(presets)) {
        const theme = buildTheme(colors)
        assert.equal(theme.error, undefined, name)
        const backgrounds = ['base', 'panel', 'raised'].map(key => theme.tokens[key])
        for (const key of ['text', 'muted', 'subtle', 'accent', 'good', 'warning', 'critical', 'precommit', 'prevote']) {
            for (const background of backgrounds) {
                assert.ok(contrast(theme.tokens[key], background) >= 4.5, `${name}: ${key} on ${background}`)
            }
        }
    }
})

test('a valid custom palette is retained without accepting invalid CSS input', () => {
    const preference = { theme: 'custom', colors: { background: '#101820', panel: '#192631', accent: '#a9c9f5' } }
    assert.deepEqual(normalize(preference).colors, preference.colors)
    assert.equal(normalize(preference).theme, 'custom')
    assert.ok(buildTheme({ ...preference.colors, accent: 'url(example)' }).error)
})

test('unreadable combinations cannot be applied', () => {
    assert.ok(buildTheme({ background: '#000000', panel: '#ffffff', accent: '#ff0000' }).error)
    assert.ok(buildTheme({ ...presets.paper, accent: '#eeeeee' }).error)
})

test('old light preferences migrate and corrupt preferences use a safe default', () => {
    assert.equal(normalize(null, 'light').theme, 'paper')
    assert.equal(normalize({ theme: 'missing', colors: {} }).theme, 'graphite')
    assert.equal(normalize({ theme: 'custom', colors: { background: '#bad' } }).theme, 'graphite')
})
