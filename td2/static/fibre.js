/* Fibre charts use observation intervals: missing values are never joined across gaps. */
(() => {
    'use strict'
    const definitions = [
        // fields: left axis; right: right axis; tooltip: hover only
        { id: 'uploads', title: 'Uploads', fields: ['uploads_ok', 'uploads_failed', 'uploads_rejected'], labels: ['OK', 'Failed', 'Rejected'], colors: ['good', 'critical', 'warning'], stacked: true, unit: 'count' },
        { id: 'received', title: 'Data received', fields: ['bytes_received'], labels: ['Received'], colors: ['precommit'], stacked: true, unit: 'bytes' },
        { id: 'signing', title: 'Signing', fields: ['signatures_fast', 'signatures_slow', 'signatures_failed'], labels: ['Under 10 ms', 'Over 10 ms', 'Failed'], colors: ['good', 'warning', 'critical'], stacked: true, unit: 'count', tooltip: ['avg_sign_ms'] },
        { id: 'storage', title: 'Storage used', fields: ['occupancy'], labels: ['Used'], colors: ['precommit'], unit: 'bytes', tooltip: ['budget'] },
        { id: 'errors', title: 'Errors', fields: ['store_put_errors', 'store_get_errors', 'prune_errors', 'object_storage_errors'], labels: ['Store put', 'Store get', 'Prune', 'Object storage'], colors: ['critical', 'warning', 'prevote', 'precommit'], stacked: true, unit: 'count', tooltip: ['prune_entries', 'object_storage_requests', 'object_storage_in_flight'] },
        { id: 'downloads', title: 'Downloads served', fields: ['bytes_served'], labels: ['Served'], colors: ['precommit'], stacked: true, unit: 'bytes', tooltip: ['downloads_ok', 'downloads_failed', 'object_storage_bytes', 'object_storage_in_flight'] },
        { id: 'reach', title: 'Reachability', fields: ['reachable'], labels: ['Reachable'], colors: ['good'], strip: true },
        { id: 'latency', title: 'Latency', fields: ['upload_p50_ms', 'upload_p95_ms', 'sign_p50_ms', 'sign_p95_ms', 'store_put_p50_ms', 'store_put_p95_ms'], labels: ['Upload p50', 'Upload p95', 'Signing p50', 'Signing p95', 'Store put p50', 'Store put p95'], colors: ['precommit', 'precommit', 'prevote', 'prevote', 'warning', 'warning'], unit: 'ms', tooltip: ['upload_samples', 'sign_samples', 'store_put_samples', 'download_p50_ms', 'download_p95_ms', 'store_get_p50_ms', 'store_get_p95_ms', 'prune_p50_ms', 'prune_p95_ms'] },
        { id: 'resources', title: 'Resources', fields: ['go_memory_used_bytes', 'go_gc_goal_bytes'], labels: ['Memory used', 'GC goal'], colors: ['precommit', 'warning'], unit: 'bytes', right: ['go_goroutines'], rightLabels: ['Goroutines'], rightColors: ['prevote'], tooltip: ['go_allocation_bytes_per_second', 'go_schedule_p95_ms', 'go_memory_limit_bytes', 'go_memory_allocations'] }
    ]
    const tooltipLabels = {
        upload_samples: 'Upload ops', sign_samples: 'Signing ops', store_put_samples: 'Store put ops',
        download_p50_ms: 'Download p50', download_p95_ms: 'Download p95', store_get_p50_ms: 'Store get p50', store_get_p95_ms: 'Store get p95', prune_p50_ms: 'Prune p50', prune_p95_ms: 'Prune p95',
        go_allocation_bytes_per_second: 'Allocation rate', go_schedule_p95_ms: 'Schedule p95', go_memory_limit_bytes: 'Memory limit', go_memory_allocations: 'Allocations',
        prune_entries: 'Entries pruned', object_storage_requests: 'Object requests', object_storage_in_flight: 'Object requests in flight', object_storage_bytes: 'Object storage bytes',
        avg_sign_ms: 'Average signing', budget: 'Budget', downloads_ok: 'Downloads OK', downloads_failed: 'Downloads failed'
    }
    const ranges = ['1h', '24h', '7d', '30d']
    const chartStyles = ['auto', 'bars', 'area', 'line']
    const defaults = () => ({ range: '24h', panels: definitions.map(d => ({ id: d.id, show: true, wide: d.id === 'storage', chart: 'auto' })) })
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
    if (typeof module !== 'undefined') module.exports = { normalize }
    if (typeof document === 'undefined') return
    let layout = defaults()
    try { layout = normalize(JSON.parse(localStorage.getItem('tenderduty-fibre-panels'))) } catch (_) { /* storage is optional */ }
    let range = layout.range
    let watched = []
    let selected = ''
    let data = null
    let timer = null
    let request = null
    let dragID = null
    const $ = id => document.getElementById(id)
    const node = (tag, text, cls) => {
        const n = document.createElement(tag)
        if (text !== undefined) n.textContent = text
        if (cls) n.className = cls
        return n
    }
    const svg = (tag, attributes) => {
        const n = document.createElementNS('http://www.w3.org/2000/svg', tag)
        for (const [k, v] of Object.entries(attributes)) n.setAttribute(k, v)
        return n
    }
    const value = (p, field) => {
        if (['occupancy', 'budget', 'reachable'].includes(field)) return p[field] ?? null
        if (field === 'signatures_slow') {
            const ok = p.metrics?.signatures_ok ?? null, fast = p.metrics?.signatures_fast ?? null
            return ok === null || fast === null ? null : Math.max(0, ok - fast)
        }
        return p.metrics?.[field] ?? null
    }
    // total of a counter, latest of a gauge; null when nothing was observed
    const sumOf = (points, field) => points.reduce((total, p) => { const v = value(p, field); return v === null ? total : (total || 0) + v }, null)
    const lastOf = (points, field) => { for (let i = points.length - 1; i >= 0; i--) { const v = value(points[i], field); if (v !== null) return v } return null }
    const ms = v => `${Number(v.toFixed(v < 1 ? 2 : v < 10 ? 1 : 0))} ms`
    const exact = n => n === null ? 'No observation' : Number(n).toLocaleString(undefined, { maximumFractionDigits: 6 })
    const byteFields = ['bytes_received', 'bytes_served', 'occupancy', 'budget', 'object_storage_bytes', 'go_memory_used_bytes', 'go_memory_limit_bytes', 'go_gc_goal_bytes', 'go_memory_max_bytes']
    const shown = (field, n) => n === null ? 'No observation' : field.endsWith('_ms') ? ms(n) : field === 'reachable' ? `${Math.round(100 * n)}%` : field === 'budget' && n === 0 ? 'unlimited' : field === 'go_allocation_bytes_per_second' ? `${compact(n, true)}/s` : byteFields.includes(field) ? compact(n, true) : exact(n)
    const compact = (n, bytes) => {
        if (bytes) {
            let unit = 0
            while (n >= 1024 && unit < 4) { n /= 1024; unit++ }
            return `${Number(n.toFixed(1))} ${['B', 'KiB', 'MiB', 'GiB', 'TiB'][unit]}`
        }
        return Number(n.toFixed(1)).toLocaleString()
    }
    function save() {
        try { localStorage.setItem('tenderduty-fibre-panels', JSON.stringify(layout)) } catch (_) { /* session preferences still work */ }
    }
    function active() { return location.hash === '#fibre' && !document.hidden && watched.length > 0 }
    function route() {
        const on = location.hash === '#fibre' && watched.length > 0
        $('overview').hidden = on
        $('fibre').hidden = !on
        for (const link of document.querySelectorAll('.nav a')) {
            const current = on ? link.id === 'fibre-nav' : link.hash === (location.hash === '#activity' ? '#activity' : '#overview')
            link.classList.toggle('active', current)
            if (current) link.setAttribute('aria-current', 'page')
            else link.removeAttribute('aria-current')
        }
        if (timer) clearInterval(timer)
        timer = null
        // a hidden page only stops the refresh timer; leaving the Fibre tab drops the request and data
        // load once even in a background tab so the page isn't empty when opened
        if (on && !request && (!data || active())) refresh()
        if (active()) timer = setInterval(refresh, 60000)
        else if (request && !on) { request.abort(); request = null }
    }
    document.addEventListener('fibre-state', event => {
        const previous = selected
        watched = event.detail.filter(c => c.fibre)
        $('fibre-nav').hidden = watched.length === 0
        if (!watched.some(c => c.name === selected)) selected = watched[0]?.name || ''
        $('fibre-chain').replaceChildren(...watched.map(c => {
            const option = node('option', c.name); option.value = c.name; option.selected = c.name === selected; return option
        }))
        $('fibre-chain-label').hidden = watched.length < 2
        $('fibre-heading').textContent = selected ? `Fibre · ${selected}` : 'Fibre'
        if (!watched.length || !timer) route()
        else if (selected !== previous) { data = null; refresh() }
    })
    function cards(f) {
        if (!f) { $('fibre-facts').replaceChildren(node('p', 'Provider check pending.')); return }
        const facts = [
            ['Provider', f.host || 'Not registered'],
            ['Reachable', f.reachable ? '● Yes' : '● No'], ['Identity', f.identity || 'Unchecked'],
            ['Assigned rows', `${f.rows}/${f.total_rows} · ${Number(f.row_percent.toFixed(2))}%`],
            ['Chain budget', f.chain_budget ? compact(f.chain_budget, true) : 'Unknown'],
            ['Used / reported budget', `${f.occupancy_known ? compact(f.occupancy, true) : 'Unknown'} / ${f.budget_reported ? f.unlimited ? 'Unlimited' : compact(f.fibre_budget, true) : 'Unknown'}`],
            ['Metrics last received', f.metrics_received ? new Date(f.metrics_at * 1000).toLocaleString() : 'Not received'],
            ['Fibre version', f.version || 'Unknown'],
            // the Go runtime reports a limit only when one is set
            ['Fibre process memory (Go)', f.resources?.go_memory_used_bytes == null ? 'Not reported' : `${shown('go_memory_used_bytes', f.resources.go_memory_used_bytes)} · ${f.resources.go_memory_limit_bytes == null ? 'no memory limit set' : `limit ${shown('go_memory_limit_bytes', f.resources.go_memory_limit_bytes)}`}`]
        ]
        $('fibre-facts').replaceChildren(...facts.map(([label, text]) => {
            const div = node('div', undefined, 'detail-fact'); div.append(node('small', label), node('strong', text))
            if (label === 'Reachable') div.lastChild.style.color = `var(--${f.reachable ? 'good' : 'critical'})`
            return div
        }))
    }
    async function refresh() {
        if (location.hash !== '#fibre' || !watched.length || !selected) return
        if (request) request.abort()
        const controller = new AbortController(); request = controller
        // only on first load: text on a refresh would shift the page
        if (!data) { $('fibre-message').textContent = 'Loading Fibre history…'; $('fibre-facts').replaceChildren(); $('fibre-panels').replaceChildren() }
        try {
            const response = await fetch(`fibre?range=${range}&chain=${encodeURIComponent(selected)}`, { cache: 'no-store', signal: controller.signal })
            if (!response.ok) throw new Error(`Fibre history unavailable (${response.status})`)
            const result = await response.json()
            if (request !== controller) return
            data = result.chains[0]
            cards(data.summary)
            $('fibre-message').replaceChildren()
            if (!data.summary?.metrics_received) {
                $('fibre-message').append(node('span', 'No Fibre metrics received. Configure the OTLP receiver: '))
                const link = node('a', 'docs/fibre.md'); link.href = 'https://github.com/kynraze/tenderduty/blob/main/docs/fibre.md'; $('fibre-message').append(link)
            }
            drawPanels()
        } catch (error) {
            if (error.name !== 'AbortError') $('fibre-message').textContent = `${error.message}. Will retry in 60 seconds.`
        } finally { if (request === controller) request = null }
    }
    function headline(def, points) {
        const n = v => Number(v).toLocaleString()
        const out = []
        const add = (text, tone) => out.push([text, tone])
        switch (def.id) {
        case 'uploads': {
            const ok = sumOf(points, 'uploads_ok'), failed = sumOf(points, 'uploads_failed'), rejected = sumOf(points, 'uploads_rejected')
            if (ok !== null) add(`${n(ok)} ok`)
            if (failed !== null) add(`${n(failed)} failed`, failed > 0 ? 'critical' : '')
            if (rejected !== null) add(`${n(rejected)} rejected`, rejected > 0 ? 'warning' : '')
            break
        }
        case 'received': case 'downloads': {
            const total = sumOf(points, def.fields[0])
            if (total !== null) add(compact(total, true))
            if (def.id === 'downloads') {
                const ok = sumOf(points, 'downloads_ok'), failed = sumOf(points, 'downloads_failed')
                if (ok !== null) add(`${n(ok)} ok`)
                if (failed) add(`${n(failed)} failed`, 'critical')
            }
            break
        }
        case 'signing': {
            const ok = sumOf(points, 'signatures_ok'), fast = sumOf(points, 'signatures_fast'), failed = sumOf(points, 'signatures_failed')
            // rounded down so 100% means all
            if (ok && fast !== null) add(`${Math.floor(100 * fast / ok)}% under 10 ms`, 2 * fast < ok ? 'warning' : '')
            if (failed !== null) add(`${n(failed)} failed`, failed > 0 ? 'critical' : '')
            break
        }
        case 'storage': {
            const used = lastOf(points, 'occupancy'), budget = lastOf(points, 'budget')
            if (used !== null) add(`${compact(used, true)} used`)
            if (budget === 0) add('no budget limit')
            else if (budget !== null) add(`${used !== null ? `${Number((100 * used / budget).toFixed(2))}% of ` : ''}${compact(budget, true)}`)
            break
        }
        case 'errors': {
            const totals = def.fields.map(f => sumOf(points, f)).filter(v => v !== null)
            if (totals.length) { const total = totals.reduce((a, b) => a + b, 0); add(total ? `${n(total)} errors` : 'no errors', total ? 'critical' : 'good') }
            break
        }
        case 'reach': {
            const share = lastOf(points, 'reachable')
            if (share !== null) add(`${Math.round(100 * share)}% reachable`, share < 1 ? 'warning' : 'good')
            break
        }
        case 'latency': {
            // whole-range percentiles: a quiet minute's p95 swings with one slow operation
            const whole = data?.latency || {}
            if (whole.upload_p95_ms != null) add(`upload p95 ${ms(whole.upload_p95_ms)}`)
            if (whole.sign_p95_ms != null) add(`signing p95 ${ms(whole.sign_p95_ms)}`)
            break
        }
        case 'resources': {
            const memory = lastOf(points, 'go_memory_used_bytes'), goroutines = lastOf(points, 'go_goroutines')
            if (memory !== null) add(compact(memory, true))
            if (goroutines !== null) add(`${n(goroutines)} goroutines`)
            break
        }
        }
        return out
    }
    // rounds up to 1, 2, 4 or 5 of a power of ten (of 1024 for bytes): 400 MiB, not 383.1
    function niceTop(v, bytes) {
        if (!(v > 0)) return 1
        let unit = 1
        if (bytes) while (v / unit >= 1024) unit *= 1024
        const scaled = v / unit, power = 10 ** Math.floor(Math.log10(scaled))
        const step = [1, 2, 4, 5, 10].find(s => s * power >= scaled * 0.999)
        return step * power * unit
    }
    // clip spikes above twice the typical worst interval so they don't flatten the other lines; the tooltip has the real value
    function robustTop(values) {
        const sorted = values.filter(v => v > 0).sort((a, b) => a - b)
        if (sorted.length < 10) return sorted.at(-1) || 0
        const typical = sorted[Math.floor(sorted.length * 0.9)]
        return Math.min(sorted.at(-1), typical * 2)
    }
    function axisText(def, v) {
        return def.unit === 'ms' ? ms(v) : compact(v, def.unit === 'bytes')
    }
    // built off the page at a pre-measured width, then swapped in whole so the page doesn't jump on refresh
    function drawChart(def, points, chartWidth, chartStyle) {
        const right = def.right || []
        const all = [...def.fields, ...right]
        const body = node('div', undefined, 'fibre-chart')
        if (!points.some(p => all.some(f => value(p, f) !== null))) { body.append(node('p', 'No observations in this range', 'empty')); return body }
        // drawn at actual width so labels don't scale with the view box
        const width = Math.max(280, Math.round(chartWidth))
        const height = 190, left = 62, end = width - (right.length ? 40 : 8), top = 10, bottom = 158
        const step = (end - left) / points.length
        const x = i => left + (i + 0.5) * step
        // auto: bars while an interval is wide enough to read as one
        const mode = def.strip ? 'strip' : !def.stacked ? 'line' : chartStyle === 'auto' || !chartStyle ? (step >= 6 ? 'bars' : 'area') : chartStyle
        const sum = p => def.fields.reduce((total, f) => total + (value(p, f) || 0), 0)
        const highest = def.unit === 'ms'
            ? robustTop(points.flatMap(p => def.fields.map(f => value(p, f) || 0)))
            : Math.max(0, ...points.map(p => def.stacked && mode !== 'line' ? sum(p) : Math.max(0, ...def.fields.map(f => value(p, f) || 0))))
        const top1 = def.strip ? 1 : niceTop(Math.max(def.unit === 'ms' ? 0.1 : 1, highest), def.unit === 'bytes')
        // twice the headroom so the right line runs below the left one
        const top2 = niceTop(2 * Math.max(1, ...points.map(p => Math.max(0, ...right.map(f => value(p, f) || 0)))), false)
        const y = (v, onRight) => bottom - Math.min(v, onRight ? top2 : top1) / (onRight ? top2 : top1) * (bottom - top)
        const chart = svg('svg', { viewBox: `0 0 ${width} ${height}`, role: 'img', 'aria-label': `${def.title}, ${selected}, ${range}: ${headline(def, points).map(([t]) => t).join(', ') || 'no observations'}` })
        if (!def.strip) for (const fraction of [0, 0.5, 1]) {
            const at = bottom - fraction * (bottom - top)
            chart.append(svg('line', { x1: left, x2: end, y1: at, y2: at, stroke: 'var(--line)' }))
            const label = svg('text', { x: left - 8, y: at + 4, 'text-anchor': 'end', fill: 'var(--subtle)', 'font-size': 12 })
            label.textContent = axisText(def, top1 * fraction); chart.append(label)
            if (right.length) {
                const second = svg('text', { x: end + 6, y: at + 4, fill: 'var(--subtle)', 'font-size': 12 })
                second.textContent = compact(top2 * fraction, false); chart.append(second)
            }
        }
        if (mode === 'area') {
            // bands stack per run of observed intervals; a run spans the outer edges of its first and last interval
            const observed = i => def.fields.some(f => value(points[i], f) !== null)
            const totals = points.map(() => 0)
            const at = ([position, v]) => `${position.toFixed(1)},${y(v).toFixed(1)}`
            def.fields.forEach((f, j) => {
                if (!points.some(p => (value(p, f) || 0) > 0)) return
                const lower = totals.slice()
                points.forEach((p, i) => { totals[i] += value(p, f) || 0 })
                for (let i = 0; i < points.length; i++) {
                    if (!observed(i)) continue
                    let k = i
                    while (k + 1 < points.length && observed(k + 1)) k++
                    const run = (values) => {
                        const out = [[x(i) - step / 2, values[i]]]
                        for (let m = i; m <= k; m++) out.push([x(m), values[m]])
                        out.push([x(k) + step / 2, values[k]])
                        return out
                    }
                    const upper = run(totals), under = run(lower).reverse()
                    const color = `var(--${def.colors[j]})`
                    chart.append(svg('path', { d: `M${upper.map(at).join(' L')} L${under.map(at).join(' L')} Z`, fill: color, 'fill-opacity': 0.45, stroke: 'none' }))
                    chart.append(svg('path', { d: `M${upper.map(at).join(' L')}`, fill: 'none', stroke: color, 'stroke-width': 1.5, 'stroke-linejoin': 'round' }))
                    i = k
                }
            })
        } else if (mode === 'bars' || mode === 'strip') {
            points.forEach((p, i) => {
                let base = 0
                def.fields.forEach((f, j) => {
                    const v = value(p, f)
                    if (v === null || v === 0 && !def.strip) return
                    const box = def.strip
                        ? { y: top + 40, height: bottom - top - 80, fill: v >= 1 ? 'var(--good)' : v === 0 ? 'var(--critical)' : 'var(--warning)' }
                        : { y: y(base + v), height: Math.max(1, v / top1 * (bottom - top)), fill: `var(--${def.colors[j]})` }
                    // overlap narrow bars: 1px gaps or antialiased seams read as a barcode
                    const dense = step < 4
                    chart.append(svg('rect', dense
                        ? { x: x(i) - step / 2, width: step + 0.5, ...box }
                        : { x: x(i) - step / 2 + 0.5, width: step - 1, rx: step > 6 ? 1.5 : 0, ...box }))
                    base += v
                })
            })
        } else {
            const line = (field, color, onRight) => {
                let path = '', joined = false
                points.forEach((p, i) => {
                    const v = value(p, field)
                    if (v === null) { joined = false; return }
                    path += `${joined ? 'L' : 'M'}${x(i).toFixed(1)},${y(v, onRight).toFixed(1)} `
                    // mark a lone observation
                    const before = i > 0 && value(points[i - 1], field) !== null, after = i < points.length - 1 && value(points[i + 1], field) !== null
                    if (!before && !after) chart.append(svg('circle', { cx: x(i), cy: y(v, onRight), r: 2.5, fill: `var(--${color})` }))
                    joined = true
                })
                const dashed = field.endsWith('_p95_ms') || field === 'go_gc_goal_bytes'
                chart.append(svg('path', { d: path, fill: 'none', stroke: `var(--${color})`, 'stroke-width': 2, 'stroke-linejoin': 'round', 'stroke-dasharray': dashed ? '5 4' : 'none' }))
            }
            def.fields.forEach((f, j) => line(f, def.colors[j], false))
            right.forEach((f, j) => line(f, def.rightColors[j], true))
        }
        const last = points.length - 1, seconds = points.length > 1 ? points[1].time - points[0].time : 60
        const counting = Date.now() / 1000 < points[last].time + seconds
        if (counting && !def.strip) chart.append(svg('rect', { x: x(last) - step / 2, y: top, width: end - x(last) + step / 2, height: bottom - top + 1, fill: 'var(--panel)', 'fill-opacity': 0.6 }))
        for (const i of [0, Math.floor(points.length / 2), points.length - 1]) {
            const label = svg('text', { x: x(i), y: bottom + 22, 'text-anchor': i === 0 ? 'start' : i === points.length - 1 ? 'end' : 'middle', fill: 'var(--subtle)', 'font-size': 12 })
            const date = new Date(points[i].time * 1000)
            label.textContent = ['7d', '30d'].includes(range) ? date.toLocaleDateString(undefined, { month: 'short', day: 'numeric' }) : date.toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit' })
            chart.append(label)
        }
        const tip = node('div', undefined, 'fibre-tip'); tip.hidden = true; tip.setAttribute('role', 'status')
        const show = i => {
            const p = points[i]
            const rows = [...def.fields.map((f, j) => [def.labels[j], f]), ...right.map((f, j) => [def.rightLabels[j], f]), ...(def.tooltip || []).map(f => [tooltipLabels[f] || f.replaceAll('_', ' '), f])]
                .map(([label, f]) => [label, f === 'go_memory_limit_bytes' && value(p, f) === null && value(p, 'go_memory_used_bytes') !== null ? 'no limit' : value(p, f) === null ? null : shown(f, value(p, f))])
                .filter(([, text]) => text !== null)
            tip.replaceChildren(node('strong', new Date(p.time * 1000).toLocaleString(undefined, { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' })))
            if (!rows.length) tip.append(node('span', 'No observation'))
            if (counting && i === last) tip.append(node('span', 'Still counting'))
            for (const [label, text] of rows) { const row = node('span'); row.append(node('em', label), document.createTextNode(text)); tip.append(row) }
            tip.hidden = false
            const share = x(i) / width
            tip.style.left = share > 0.6 ? 'auto' : `${Math.max(0, share * 100)}%`
            tip.style.right = share > 0.6 ? `${Math.max(0, (1 - share) * 100)}%` : 'auto'
        }
        points.forEach((p, i) => {
            const hit = svg('rect', { x: x(i) - step / 2, y: top, width: step, height: bottom - top, fill: 'transparent', tabindex: '0', 'aria-label': new Date(p.time * 1000).toLocaleString() })
            for (const event of ['pointerenter', 'click', 'focus']) hit.addEventListener(event, () => show(i))
            hit.addEventListener('blur', () => { tip.hidden = true })
            chart.append(hit)
        })
        chart.addEventListener('pointerleave', () => { tip.hidden = true })
        body.append(chart, tip)
        return body
    }
    function legend(def) {
        const box = node('div', undefined, 'fibre-legend')
        const entry = (label, color, note) => {
            const item = node('span'); const dot = node('i'); dot.style.background = `var(--${color})`
            item.append(dot, document.createTextNode(label)); if (note) item.append(node('small', note)); box.append(item)
        }
        if (def.id === 'latency') {
            // line style tells p50 from p95
            for (let i = 0; i < def.fields.length; i += 2) entry(def.labels[i].replace(' p50', ''), def.colors[i])
            box.append(node('span', 'solid p50 · dashed p95', 'note'))
        } else if (def.fields.length > 1 || def.right) {
            def.fields.forEach((f, i) => entry(def.labels[i], def.colors[i], f === 'go_gc_goal_bytes' ? 'dashed' : ''))
            ;(def.right || []).forEach((f, i) => entry(def.rightLabels[i], def.rightColors[i], 'right axis'))
        }
        return box
    }
    function drawPanels() {
        const grid = $('fibre-panels')
        if (!data) { grid.replaceChildren(); return }
        const points = data.series || []
        const shown = layout.panels.filter(p => p.show)
        if (!shown.length) { grid.replaceChildren(node('p', 'All panels are hidden. Open Customize panels to show them.')); return }
        // update existing panels in place
        const existing = new Map([...grid.querySelectorAll('.fibre-panel')].map(panel => [panel.dataset.panel, panel]))
        const panels = shown.map(preference => {
            const panel = existing.get(preference.id) || node('section')
            panel.className = `fibre-panel${preference.wide ? ' wide' : ''}`
            panel.dataset.panel = preference.id
            return panel
        })
        if (panels.some((panel, i) => grid.children[i] !== panel) || grid.children.length !== panels.length) grid.replaceChildren(...panels)
        shown.forEach((preference, i) => {
            const def = definitions.find(d => d.id === preference.id), panel = panels[i]
            const style = getComputedStyle(panel)
            const width = panel.querySelector('.fibre-chart')?.clientWidth || panel.clientWidth - parseFloat(style.paddingLeft) - parseFloat(style.paddingRight)
            const head = node('header', undefined, 'fibre-panel-head')
            const stats = node('p', undefined, 'fibre-stats')
            for (const [text, tone] of headline(def, points)) { const stat = node('span', text); if (tone) stat.className = tone; stats.append(stat) }
            head.append(node('h2', def.title), stats)
            panel.replaceChildren(head, legend(def), drawChart(def, points, width, preference.chart))
        })
    }
    function rangeButtons() {
        $('fibre-ranges').replaceChildren(...ranges.map(r => {
            const button = node('button', r, `tool${range === r ? ' current' : ''}`); button.type = 'button'; button.setAttribute('aria-pressed', String(range === r))
            button.onclick = () => { range = r; data = null; rangeButtons(); refresh() }; return button
        }))
    }
    function move(id, index) {
        const from = layout.panels.findIndex(p => p.id === id)
        const [panel] = layout.panels.splice(from, 1); layout.panels.splice(index, 0, panel)
        save(); customize(); drawPanels()
        const button = $('fibre-layout-list').querySelector(`[data-panel="${id}"] button:not(:disabled)`); if (button) button.focus()
    }
    function customize() {
        $('fibre-default-range').value = layout.range
        $('fibre-layout-list').replaceChildren(...layout.panels.map((p, i) => {
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
    $('fibre-customize').onclick = () => { customize(); $('fibre-layout').showModal() }
    $('fibre-layout-close').onclick = () => $('fibre-layout').close()
    $('fibre-default-range').onchange = event => { layout.range = event.target.value; save() }
    $('fibre-layout-reset').onclick = () => { layout = defaults(); range = layout.range; data = null; save(); customize(); rangeButtons(); refresh() }
    $('fibre-chain').onchange = event => { selected = event.target.value; $('fibre-heading').textContent = `Fibre · ${selected}`; data = null; refresh() }
    window.addEventListener('hashchange', route)
    let resized = null
    window.addEventListener('resize', () => { clearTimeout(resized); resized = setTimeout(() => { if (active()) drawPanels() }, 200) })
    document.addEventListener('visibilitychange', route)
    rangeButtons(); route()
})()
