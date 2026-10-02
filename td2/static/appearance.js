(function (root) {
    const presets = {
        graphite: { background: '#0d151b', panel: '#152027', accent: '#63d6c4' },
        midnight: { background: '#080e1e', panel: '#111b31', accent: '#90b4f0' },
        paper: { background: '#f6f9f8', panel: '#ffffff', accent: '#0b766f' },
        'warm-stone': { background: '#f5f1e9', panel: '#fffaf2', accent: '#795b39' }
    }
    const storageKey = 'tenderduty-appearance'

    function validColor(value) {
        return typeof value === 'string' && /^#[0-9a-f]{6}$/i.test(value)
    }

    function channels(color) {
        return color.slice(1).match(/../g).map(value => parseInt(value, 16))
    }

    function luminance(color) {
        const values = channels(color).map(value => {
            value /= 255
            return value <= 0.04045 ? value / 12.92 : ((value + 0.055) / 1.055) ** 2.4
        })
        return values[0] * 0.2126 + values[1] * 0.7152 + values[2] * 0.0722
    }

    function contrast(left, right) {
        const values = [luminance(left), luminance(right)].sort((a, b) => b - a)
        return (values[0] + 0.05) / (values[1] + 0.05)
    }

    function blend(left, right, amount) {
        const other = channels(right)
        return '#' + channels(left).map((value, index) => Math.round(value * (1 - amount) + other[index] * amount).toString(16).padStart(2, '0')).join('')
    }

    function readable(candidates, backgrounds) {
        return candidates.find(color => backgrounds.every(background => contrast(color, background) >= 4.5))
    }

    function buildTheme(colors) {
        if (!colors || !['background', 'panel', 'accent'].every(key => validColor(colors[key]))) {
            return { error: 'Enter a six-digit HEX color, such as #17212B.' }
        }
        const { background, panel, accent } = colors
        const text = readable(['#e8f1f2', '#17292f', '#ffffff', '#000000'], [background, panel])
        if (!text) return { error: 'Choose background and panel colors with similar brightness so labels stay readable.' }
        const dark = luminance(text) > luminance(background)
        const raised = blend(panel, text, dark ? 0.04 : 0.025)
        const surfaces = [background, panel, raised]
        if (!readable([text], surfaces)) return { error: 'This background and panel combination makes labels difficult to read.' }
        if (!readable([accent], surfaces)) return { error: 'Choose a brighter or darker accent so links and controls stay readable.' }
        const muted = readable([blend(text, background, 0.3), blend(text, background, 0.2), text], surfaces)
        const signal = (light, deep, fallback) => readable([light, deep, fallback, dark ? '#ffffff' : '#000000'], surfaces)
        return {
            scheme: dark ? 'dark' : 'light',
            tokens: {
                base: background, panel, raised, accent, text, muted, subtle: muted,
                line: blend(panel, text, dark ? 0.14 : 0.16), focus: accent,
                good: signal('#63d6c4', '#0b766f', dark ? '#b4ffe2' : '#002d25'),
                warning: signal('#f1b663', '#955800', dark ? '#ffe1a1' : '#301700'),
                critical: signal('#f27c76', '#b33b44', dark ? '#ffb6b6' : '#300009'),
                precommit: signal('#83bee4', '#2c668b', dark ? '#c4e9ff' : '#001c30'),
                prevote: signal('#c5a3dc', '#775495', dark ? '#edd5ff' : '#24002f')
            }
        }
    }

    function normalize(value, legacy) {
        if (value && (presets[value.theme] || value.theme === 'custom')) {
            const colors = value.theme === 'custom' ? value.colors : presets[value.theme]
            if (!buildTheme(colors).error) return { version: 1, theme: value.theme, colors: { ...colors } }
        }
        const theme = legacy === 'light' ? 'paper' : 'graphite'
        return { version: 1, theme, colors: { ...presets[theme] } }
    }

    const api = { presets, buildTheme, contrast, normalize }
    if (typeof module !== 'undefined' && module.exports) module.exports = api
    if (!root.document) return
    root.TenderdutyAppearance = api

    const dialog = document.getElementById('appearance-dialog')
    const selector = document.getElementById('appearance-theme')
    const controls = document.getElementById('custom-colors')
    const error = document.getElementById('appearance-error')
    const apply = document.getElementById('appearance-apply')
    const feedback = document.getElementById('appearance-feedback')
    let saved, legacy
    try {
        saved = JSON.parse(localStorage.getItem(storageKey))
        legacy = localStorage.getItem('tenderduty-theme')
    } catch (_) {}
    let current = normalize(saved, legacy)
    let draft = { ...current, colors: { ...current.colors } }

    function showTheme(preference) {
        const theme = buildTheme(preference.colors)
        if (theme.error) return theme.error
        for (const [name, color] of Object.entries(theme.tokens)) document.documentElement.style.setProperty(`--${name}`, color)
        document.documentElement.style.colorScheme = theme.scheme
        document.documentElement.dataset.appearance = preference.theme
        document.querySelector('meta[name="theme-color"]').content = preference.colors.background
        return ''
    }

    function preview() {
        const problem = showTheme(draft)
        error.textContent = problem
        error.hidden = !problem
        apply.disabled = Boolean(problem)
    }

    function fillControls() {
        selector.value = draft.theme
        controls.hidden = draft.theme !== 'custom'
        for (const key of ['background', 'panel', 'accent']) {
            document.getElementById(`appearance-${key}`).value = draft.colors[key]
            document.getElementById(`appearance-${key}-hex`).value = draft.colors[key]
        }
        preview()
    }

    selector.addEventListener('change', () => {
        draft.theme = selector.value
        if (presets[draft.theme]) draft.colors = { ...presets[draft.theme] }
        fillControls()
    })
    for (const key of ['background', 'panel', 'accent']) {
        const picker = document.getElementById(`appearance-${key}`)
        const hex = document.getElementById(`appearance-${key}-hex`)
        picker.addEventListener('input', () => {
            draft.colors[key] = picker.value
            hex.value = picker.value
            preview()
        })
        hex.addEventListener('input', () => {
            draft.colors[key] = hex.value.trim().toLowerCase()
            if (validColor(draft.colors[key])) picker.value = draft.colors[key]
            preview()
        })
    }
    document.getElementById('customize-appearance').addEventListener('click', () => {
        draft = { ...current, colors: { ...current.colors } }
        fillControls()
        dialog.showModal()
    })
    document.getElementById('appearance-reset').addEventListener('click', () => {
        draft = normalize(null)
        fillControls()
    })
    document.getElementById('appearance-cancel').addEventListener('click', () => dialog.close())
    dialog.addEventListener('close', () => showTheme(current))
    apply.addEventListener('click', () => {
        if (buildTheme(draft.colors).error) return
        current = normalize(draft)
        feedback.textContent = 'Appearance saved in this browser.'
        try {
            localStorage.setItem(storageKey, JSON.stringify(current))
            localStorage.removeItem('tenderduty-theme')
        } catch (_) {
            feedback.textContent = 'Appearance applied for this session. Browser storage is unavailable.'
        }
        dialog.close()
    })
    showTheme(current)
})(typeof window !== 'undefined' ? window : globalThis)
