/* Bridge history leaves unobserved intervals blank. */
(() => {
    'use strict'
    const definitions = [
        { id: 'sync', title: 'Sync', fields: ['head_lag', 'sample_lag'], labels: ['Head behind chain', 'Sampling behind head'], colors: ['precommit', 'warning'], unit: 'count', tooltip: ['head_lag_max', 'sample_lag_max'] },
        { id: 'da_lag', title: 'Core → DA lag', fields: ['da_lag_ms'], labels: ['Average'], colors: ['precommit'], unit: 'ms', tooltip: ['da_blocks'], note: "Seconds from a block's consensus time to the bridge broadcasting it." },
        { id: 'peers', title: 'Peers', fields: ['full_peers', 'archival_peers'], labels: ['Full', 'Archival'], colors: ['precommit', 'prevote'], unit: 'count', tooltip: ['full_peers_min'] },
        { id: 'served', title: 'Data served', fields: ['served_bytes'], labels: ['Served'], colors: ['precommit'], stacked: true, unit: 'bytes' },
        { id: 'responses', title: 'Responses', fields: ['responses_success', 'responses_not_found', 'responses_rate_limited', 'responses_internal_err', 'responses_peer_err', 'responses_other'], labels: ['Success', 'Not found', 'Rate limited', 'Internal error', 'Peer error', 'Other'], colors: ['good', 'muted', 'warning', 'critical', 'prevote', 'precommit'], stacked: true, unit: 'count', note: 'Internal errors are the bridge failing to read its own store. Peer errors come from the requesting peer or the network.' },
        { id: 'serve_latency', title: 'Serving latency', fields: ['serve_ms'], labels: ['Average'], colors: ['precommit'], unit: 'ms', tooltip: ['serve_requests'] },
        { id: 'disk_latency', title: 'Disk write latency', fields: ['put_ms'], labels: ['Average'], colors: ['precommit'], unit: 'ms', tooltip: ['puts', 'put_failures'] },
        { id: 'pruning', title: 'Pruning', fields: ['retained_hours'], labels: ['Hours kept'], colors: ['precommit'], unit: 'count', tooltip: ['reclaimed_bytes', 'failed_prunes'] },
        { id: 'memory', title: 'Memory', fields: ['memory_bytes'], labels: ['Memory used'], colors: ['precommit'], area: true, unit: 'bytes', tooltip: ['goroutines'] },
        { id: 'restarts', title: 'Restarts and version', fields: ['restarts'], labels: ['Restarts'], colors: ['warning'], stacked: true, unit: 'count' }
    ]
    const ranges = ['1h', '24h', '7d', '30d']
    const chartStyles = ['auto', 'bars', 'area', 'line']
    const defaults = () => ({ range: '24h', panels: definitions.map(d => ({ id: d.id, show: !['memory', 'restarts'].includes(d.id), wide: false, chart: 'auto' })) })
    function normalize(value) {
        const result = defaults()
        if (!value || !Array.isArray(value.panels)) return result
        result.range = ranges.includes(value.range) ? value.range : '24h'
        const seen = new Set()
        result.panels = value.panels.filter(p => p && definitions.some(d => d.id === p.id) && !seen.has(p.id) && seen.add(p.id))
            .map(p => ({ id: p.id, show: p.show !== false, wide: p.wide === true, chart: chartStyles.includes(p.chart) ? p.chart : 'auto' }))
        for (const p of defaults().panels) if (!seen.has(p.id)) result.panels.push(p)
        return result
    }
    const value = (p, field) => p.metrics?.[field] ?? null
    const sumOf = (points, field) => points.reduce((total, p) => { const v = value(p, field); return v === null ? total : (total || 0) + v }, null)
    const lastOf = (points, field) => { for (let i = points.length - 1; i >= 0; i--) { const v = value(points[i], field); if (v !== null) return v } return null }
    const compact = bytes => {
        let unit = 0
        while (bytes >= 1024 && unit < 4) { bytes /= 1024; unit++ }
        return `${Number(bytes.toFixed(1))} ${['B', 'KiB', 'MiB', 'GiB', 'TiB'][unit]}`
    }
    const duration = hours => { const h = Math.floor(hours); return h >= 24 ? `${Math.floor(h / 24)}d ${h % 24}h` : `${h}h` }
    function headline(def, points) {
        const out = [], n = v => Number(v).toLocaleString()
        const add = (text, tone) => out.push([text, tone])
        const last = field => lastOf(points, field), sum = field => sumOf(points, field)
        switch (def.id) {
        case 'sync': {
            if (last('head_lag') !== null || last('sample_lag') !== null) add(`${last('head_lag') === null ? 'Unknown' : n(last('head_lag'))} behind · ${last('sample_lag') === null ? 'Unknown' : n(last('sample_lag'))} sampling behind`)
            const maxima = points.map(p => value(p, 'head_lag_max')).filter(v => v !== null)
            if (maxima.length) add(`max ${n(Math.max(...maxima))}`)
            break
        }
        case 'peers':
            if (last('full_peers') !== null || last('archival_peers') !== null) add(`${last('full_peers') === null ? 'Unknown' : n(last('full_peers'))} full · ${last('archival_peers') === null ? 'Unknown' : n(last('archival_peers'))} archival`, last('full_peers') !== null && last('full_peers') < 2 ? 'warning' : '')
            break
        case 'served': if (sum('served_bytes') !== null) add(compact(sum('served_bytes'))); break
        case 'responses': {
            const totals = def.fields.map(sum).filter(v => v !== null)
            if (totals.length) {
                const total = totals.reduce((a, b) => a + b, 0), errors = sum('responses_internal_err') || 0
                add(`${n(total)} total`)
                add(`${Number((total ? 100 * errors / total : 0).toFixed(1))}% internal errors`, errors > 0 ? 'critical' : '')
            }
            break
        }
        case 'da_lag': case 'serve_latency': case 'disk_latency': {
            const [seconds, count] = { da_lag: ['da_lag_seconds', 'da_blocks'], serve_latency: ['serve_seconds', 'serve_requests'], disk_latency: ['put_seconds', 'puts'] }[def.id]
            const average = sum(count) > 0 && sum(seconds) !== null ? 1000 * sum(seconds) / sum(count) : null
            if (average !== null) add(`${Number(average.toFixed(average < 1 ? 2 : average < 10 ? 1 : 0))} ms`)
            if (def.id === 'da_lag' && sum(count) !== null) add(`${n(sum(count))} blocks`)
            if (def.id === 'disk_latency' && sum('put_failures') !== null) add(`${n(sum('put_failures'))} failed`, sum('put_failures') > 0 ? 'critical' : '')
            break
        }
        case 'pruning':
            if (last('retained_hours') !== null) add(`${duration(last('retained_hours'))} kept`)
            if (sum('reclaimed_bytes') !== null) add(`${compact(sum('reclaimed_bytes'))} reclaimed`)
            if (last('failed_prunes') !== null) add(`${n(last('failed_prunes'))} failed prunes`, last('failed_prunes') > 0 ? 'critical' : '')
            break
        case 'memory':
            if (last('memory_bytes') !== null) add(compact(last('memory_bytes')))
            if (last('goroutines') !== null) add(`${n(last('goroutines'))} goroutines`)
            break
        case 'restarts': {
            if (sum('restarts') !== null) add(`${n(sum('restarts'))} restart${sum('restarts') === 1 ? '' : 's'}`)
            const versions = []
            for (const p of points) if (p.version && versions.at(-1) !== p.version) versions.push(p.version)
            if (versions.length) add(versions.map(v => `v${v.replace(/^v/, '')}`).join(' → '))
            break
        }
        }
        return out
    }
    if (typeof module !== 'undefined') module.exports = { normalize, definitions, headline }
    if (typeof document === 'undefined') return
    let layout = defaults()
    try { layout = normalize(JSON.parse(localStorage.getItem('tenderduty-bridge-panels'))) } catch (_) { /* storage is optional */ }
    let range = layout.range, watched = [], selected = '', data = null, timer = null, request = null, dragID = null
    const $ = id => document.getElementById(id)
    const node = (tag, text, cls) => {
        const n = document.createElement(tag)
        if (text !== undefined) n.textContent = text
        if (cls) n.className = cls
        return n
    }
    const tooltipLabels = { head_lag_max: 'Maximum head lag', sample_lag_max: 'Maximum sampling lag', da_blocks: 'Blocks', full_peers_min: 'Minimum full peers', serve_requests: 'Requests', puts: 'Writes', put_failures: 'Failed writes', reclaimed_bytes: 'Reclaimed', failed_prunes: 'Failed prunes', goroutines: 'Goroutines' }
    function save() {
        try { localStorage.setItem('tenderduty-bridge-panels', JSON.stringify(layout)) } catch (_) { /* session preferences work without storage */ }
    }
    function active() { return location.hash === '#bridge' && !document.hidden && watched.length > 0 }
    function route() {
        const on = location.hash === '#bridge' && watched.length > 0
        $('bridge').hidden = !on
        $('overview').hidden = on || location.hash === '#fibre' && !$('fibre-nav').hidden
        for (const link of document.querySelectorAll('.nav a')) {
            const current = on ? link.id === 'bridge-nav' : location.hash === '#fibre' && !$('fibre-nav').hidden ? link.id === 'fibre-nav' : link.hash === '#overview'
            link.classList.toggle('active', current)
            if (current) link.setAttribute('aria-current', 'page')
            else link.removeAttribute('aria-current')
        }
        if (timer) clearInterval(timer)
        timer = null
        if (on && !request && (!data || active())) refresh()
        if (active()) timer = setInterval(refresh, 60000)
        else if (request && !on) { request.abort(); request = null }
    }
    function cards() {
        const bridge = watched.find(c => c.name === selected)?.bridge
        $('bridge-facts').replaceChildren(...(bridge?.facts || []).map(f => {
            const div = node('div', undefined, 'detail-fact'), strong = node('strong', f.value)
            if (['critical', 'warning'].includes(f.tone)) strong.style.color = `var(--${f.tone})`
            div.append(node('small', f.label), strong)
            return div
        }))
    }
    document.addEventListener('fibre-state', event => {
        const previous = selected
        watched = event.detail.filter(c => c.bridge)
        $('bridge-nav').hidden = watched.length === 0
        if (!watched.some(c => c.name === selected)) selected = watched[0]?.name || ''
        $('bridge-chain').replaceChildren(...watched.map(c => {
            const option = node('option', c.name); option.value = c.name; option.selected = c.name === selected; return option
        }))
        $('bridge-chain-label').hidden = watched.length < 2
        $('bridge-heading').textContent = selected ? `Bridge · ${selected}` : 'Bridge'
        cards()
        if (selected !== previous) data = null
        if (!watched.length || !timer || selected !== previous) route()
    })
    async function refresh() {
        if (location.hash !== '#bridge' || !watched.length || !selected) return
        if (request) request.abort()
        const controller = new AbortController(); request = controller
        if (!data) { $('bridge-message').textContent = 'Loading Bridge history…'; $('bridge-panels').replaceChildren() }
        try {
            const response = await fetch(`bridge?range=${range}&chain=${encodeURIComponent(selected)}`, { cache: 'no-store', signal: controller.signal })
            if (!response.ok) throw new Error(`Bridge history unavailable (${response.status})`)
            const result = await response.json()
            if (request !== controller) return
            data = result.chains[0]
            $('bridge-message').replaceChildren()
            if (!(data?.series || []).some(p => Object.keys(p.metrics || {}).some(k => !['head_lag', 'head_lag_max', 'sample_lag', 'sample_lag_max', 'retained_hours'].includes(k)))) {
                $('bridge-message').append(node('span', 'No bridge metrics received. See '))
                const link = node('a', 'docs/bridge.md'); link.href = 'https://github.com/kynraze/tenderduty/blob/main/docs/bridge.md'; $('bridge-message').append(link)
            }
            drawPanels()
        } catch (error) {
            if (error.name !== 'AbortError') $('bridge-message').textContent = `${error.message}. Will retry in 60 seconds.`
        } finally { if (request === controller) request = null }
    }
    function drawPanels() {
        const grid = $('bridge-panels')
        if (!data) { grid.replaceChildren(); return }
        const points = data.series || [], shown = layout.panels.filter(p => p.show)
        if (!shown.length) { grid.replaceChildren(node('p', 'All panels are hidden. Open Customize panels to show them.')); return }
        const existing = new Map([...grid.querySelectorAll('.fibre-panel')].map(panel => [panel.dataset.panel, panel]))
        const panels = shown.map(p => { const panel = existing.get(p.id) || node('section'); panel.className = `fibre-panel${p.wide ? ' wide' : ''}`; panel.dataset.panel = p.id; return panel })
        if (panels.some((p, i) => grid.children[i] !== p) || grid.children.length !== panels.length) grid.replaceChildren(...panels)
        const view = { range, selected, headline, tooltipLabels }
        shown.forEach((preference, i) => {
            const def = definitions.find(d => d.id === preference.id), panel = panels[i]
            let plot = points
            if (def.id === 'memory' && ['7d', '30d'].includes(range)) plot = points.map(p => ({ ...p, metrics: { ...p.metrics, ...Object.fromEntries([['memory_bytes', 'memory_max_bytes'], ['goroutines', 'goroutines_max']].filter(([, peak]) => p.metrics?.[peak] != null).map(([field, peak]) => [field, p.metrics[peak]])) } }))
            const style = getComputedStyle(panel), width = panel.clientWidth - parseFloat(style.paddingLeft) - parseFloat(style.paddingRight)
            const head = node('header', undefined, 'fibre-panel-head'), stats = node('p', undefined, 'fibre-stats')
            for (const [text, tone] of headline(def, points)) { const stat = node('span', text); if (tone) stat.className = tone; stats.append(stat) }
            head.append(node('h2', def.title), stats)
            panel.replaceChildren(head, window.tdCharts.legend(def, view), ...(def.note ? [node('p', def.note, 'fibre-note')] : []), window.tdCharts.drawChart(def, plot, width, preference.chart, view))
        })
    }
    function rangeButtons() {
        $('bridge-ranges').replaceChildren(...ranges.map(r => {
            const button = node('button', r, `tool${range === r ? ' current' : ''}`); button.type = 'button'; button.setAttribute('aria-pressed', String(range === r))
            button.onclick = () => { range = r; data = null; rangeButtons(); refresh() }; return button
        }))
    }
    function move(id, index) {
        const from = layout.panels.findIndex(p => p.id === id)
        const [panel] = layout.panels.splice(from, 1); layout.panels.splice(index, 0, panel)
        save(); customize(); drawPanels()
        const button = $('bridge-layout-list').querySelector(`[data-panel="${id}"] button:not(:disabled)`); if (button) button.focus()
    }
    function customize() {
        $('bridge-default-range').value = layout.range
        $('bridge-layout-list').replaceChildren(...layout.panels.map((p, i) => {
            const row = node('div', undefined, 'fibre-layout-row'); row.draggable = true; row.dataset.panel = p.id
            const label = node('label'), input = node('input'); input.type = 'checkbox'; input.checked = p.show
            input.onchange = () => { p.show = input.checked; save(); drawPanels() }
            label.append(input, document.createTextNode(definitions.find(d => d.id === p.id).title)); row.append(label)
            const size = node('select'); size.setAttribute('aria-label', `Width of ${label.textContent}`)
            for (const width of ['Normal', 'Wide']) { const option = node('option', width); option.value = width; size.append(option) }
            size.value = p.wide ? 'Wide' : 'Normal'; size.onchange = () => { p.wide = size.value === 'Wide'; save(); drawPanels() }; row.append(size)
            if (definitions.find(d => d.id === p.id).stacked) {
                const kind = node('select'); kind.setAttribute('aria-label', `Chart type of ${label.textContent}`)
                for (const style of chartStyles) { const option = node('option', style[0].toUpperCase() + style.slice(1)); option.value = style; kind.append(option) }
                kind.value = p.chart; kind.onchange = () => { p.chart = kind.value; save(); drawPanels() }; row.append(kind)
            }
            for (const [direction, offset] of [['Up', -1], ['Down', 1]]) {
                const button = node('button', direction); button.type = 'button'; button.setAttribute('aria-label', `Move ${label.textContent} ${direction.toLowerCase()}`); button.disabled = i + offset < 0 || i + offset >= layout.panels.length
                button.onclick = () => move(p.id, i + offset); row.append(button)
            }
            row.ondragstart = event => { dragID = p.id; event.dataTransfer.setData('text/plain', p.id) }
            row.ondragover = event => event.preventDefault()
            row.ondrop = event => { event.preventDefault(); if (dragID && dragID !== p.id) move(dragID, i); dragID = null }
            row.ondragend = () => { dragID = null }
            return row
        }))
    }
    $('bridge-customize').onclick = () => { customize(); $('bridge-layout').showModal() }
    $('bridge-layout-close').onclick = () => $('bridge-layout').close()
    $('bridge-default-range').onchange = event => { layout.range = event.target.value; save() }
    $('bridge-layout-reset').onclick = () => { layout = defaults(); range = layout.range; data = null; save(); customize(); rangeButtons(); refresh() }
    $('bridge-chain').onchange = event => { selected = event.target.value; $('bridge-heading').textContent = `Bridge · ${selected}`; data = null; cards(); refresh() }
    window.addEventListener('hashchange', route)
    let resized = null
    window.addEventListener('resize', () => { clearTimeout(resized); resized = setTimeout(() => { if (active()) drawPanels() }, 200) })
    document.addEventListener('visibilitychange', route)
    rangeButtons(); route()
})()
