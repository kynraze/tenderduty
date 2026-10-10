/* Fibre charts leave unobserved intervals blank. */
(() => {
    'use strict'
    const definitions = [
        // fields: left axis; right: right axis; tooltip: hover only
        { id: 'endorsements', title: 'Endorsements', fields: ['endorsements_endorsed', 'endorsements_missed'], labels: ['Endorsed', 'Missed'], colors: ['good', 'critical'], stacked: true, unit: 'count' },
        { id: 'uploads', title: 'Uploads', fields: ['uploads_ok', 'uploads_failed', 'uploads_rejected'], labels: ['OK', 'Failed', 'Rejected'], colors: ['good', 'critical', 'warning'], stacked: true, unit: 'count' },
        { id: 'received', title: 'Data received', fields: ['bytes_received'], labels: ['Received'], colors: ['precommit'], stacked: true, unit: 'bytes' },
        { id: 'signing', title: 'Signing', fields: ['signatures_fast', 'signatures_slow', 'signatures_failed'], labels: ['Under 10 ms', 'Over 10 ms', 'Failed'], colors: ['good', 'warning', 'critical'], stacked: true, unit: 'count', tooltip: ['avg_sign_ms'] },
        { id: 'storage', title: 'Shards stored', fields: ['occupancy'], labels: ['Used'], colors: ['precommit'], unit: 'bytes', tooltip: ['budget'] },
        { id: 'errors', title: 'Errors', fields: ['store_put_errors', 'store_get_errors', 'prune_errors', 'object_storage_errors'], labels: ['Store put', 'Store get misses', 'Prune', 'Storage backend'], colors: ['critical', 'muted', 'prevote', 'precommit'], stacked: true, unit: 'count' },
        { id: 'downloads', title: 'Downloads', fields: ['downloads_ok', 'downloads_failed'], labels: ['OK', 'Failed'], colors: ['good', 'critical'], stacked: true, unit: 'count', tooltip: ['bytes_served', 'object_storage_bytes', 'object_storage_requests', 'object_storage_in_flight'] },
        { id: 'reach', title: 'Reachability', fields: ['reachable'], labels: ['Reachable'], colors: ['good'], strip: true },
        { id: 'request_latency', title: 'Request latency', fields: ['upload_p50_ms', 'upload_p95_ms', 'download_p50_ms', 'download_p95_ms'], labels: ['Upload p50', 'Upload p95', 'Download p50', 'Download p95'], colors: ['precommit', 'precommit', 'prevote', 'prevote'], unit: 'ms', log: true, tooltip: ['upload_samples', 'download_samples'] },
        { id: 'internal_latency', title: 'Internal latency', fields: ['sign_p50_ms', 'sign_p95_ms', 'store_put_p50_ms', 'store_put_p95_ms', 'store_get_p50_ms', 'store_get_p95_ms', 'prune_p50_ms', 'prune_p95_ms'], labels: ['Signing p50', 'Signing p95', 'Store put p50', 'Store put p95', 'Store get p50', 'Store get p95', 'Prune p50', 'Prune p95'], colors: ['precommit', 'precommit', 'warning', 'warning', 'prevote', 'prevote', 'critical', 'critical'], unit: 'ms', log: true, tooltip: ['sign_samples', 'store_put_samples', 'store_get_samples', 'prune_samples'] },
        { id: 'resources', title: 'Resources', fields: ['go_memory_used_bytes'], labels: ['Memory used'], colors: ['precommit'], stacked: true, area: true, unit: 'bytes', lines: ['go_gc_goal_bytes', 'go_memory_limit_bytes'], lineLabels: ['GC goal', 'Memory limit'], lineColors: ['warning', 'critical'], tooltip: ['go_goroutines', 'go_allocation_bytes_per_second', 'go_schedule_p95_ms', 'go_memory_allocations'] }
    ]
    const tooltipLabels = {
        download_samples: 'Download ops', store_get_samples: 'Store get ops', prune_samples: 'Prune ops', bytes_served: 'Bytes served', go_goroutines: 'Goroutines', upload_samples: 'Upload ops', sign_samples: 'Signing ops', store_put_samples: 'Store put ops',
        download_p50_ms: 'Download p50', download_p95_ms: 'Download p95', store_get_p50_ms: 'Store get p50', store_get_p95_ms: 'Store get p95', prune_p50_ms: 'Prune p50', prune_p95_ms: 'Prune p95',
        go_allocation_bytes_per_second: 'Allocation rate', go_schedule_p95_ms: 'Schedule p95', go_memory_limit_bytes: 'Memory limit', go_memory_allocations: 'Allocations',
        prune_entries: 'Entries pruned', object_storage_requests: 'Backend requests', object_storage_in_flight: 'Backend reads in flight', object_storage_bytes: 'Backend bytes read',
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
        const panels = value.panels.flatMap(p => p?.id === 'latency' ? ['request_latency', 'internal_latency'].map(id => ({ ...p, id, show: true })) : [p])
        result.panels = panels.filter(p => p && definitions.some(d => d.id === p.id) && !seen.has(p.id) && seen.add(p.id))
            .map(p => ({ id: p.id, show: p.show !== false, wide: p.wide === true, chart: chartStyles.includes(p.chart) ? p.chart : 'auto' }))
        for (const p of defaults().panels) if (!seen.has(p.id)) { if (p.id === 'endorsements') result.panels.unshift(p); else result.panels.push(p) }
        return result
    }
    const value = (p, field) => {
        if (['occupancy', 'budget', 'reachable'].includes(field)) return p[field] ?? null
        if (field === 'signatures_slow' && p.metrics?.signatures_slow === undefined) {
            const ok = p.metrics?.signatures_ok ?? null, fast = p.metrics?.signatures_fast ?? null
            return ok === null || fast === null ? null : Math.max(0, ok - fast)
        }
        return p.metrics?.[field] ?? null
    }
    // sum counters, use latest gauges; null means unobserved
    const sumOf = (points, field) => points.reduce((total, p) => { const v = value(p, field); return v === null ? total : (total || 0) + v }, null)
    const lastOf = (points, field) => { for (let i = points.length - 1; i >= 0; i--) { const v = value(points[i], field); if (v !== null) return v } return null }
    const ms = v => `${Number(v.toFixed(v < 1 ? 2 : v < 10 ? 1 : 0))} ms`
    const exact = n => n === null ? 'No observation' : Number(n).toLocaleString(undefined, { maximumFractionDigits: 6 })
    const byteFields = ['bytes_received', 'bytes_served', 'occupancy', 'budget', 'object_storage_bytes', 'go_memory_used_bytes', 'go_memory_heap_bytes', 'go_memory_stack_bytes', 'go_memory_limit_bytes', 'go_gc_goal_bytes', 'go_memory_max_bytes']
    const shown = (field, n) => n === null ? 'No observation' : field.endsWith('_ms') ? ms(n) : field === 'reachable' ? `${Math.round(100 * n)}%` : field === 'budget' && n === 0 ? 'unlimited' : field === 'go_allocation_bytes_per_second' ? `${compact(n, true)}/s` : byteFields.includes(field) ? compact(n, true) : exact(n)
    const compact = (n, bytes) => {
        if (bytes) {
            let unit = 0
            while (n >= 1024 && unit < 4) { n /= 1024; unit++ }
            return `${Number(n.toFixed(1))} ${['B', 'KiB', 'MiB', 'GiB', 'TiB'][unit]}`
        }
        return Number(n.toFixed(1)).toLocaleString()
    }
    function missedRows(blobs, now = Date.now() / 1000) {
        return (blobs || []).slice(0, 5).map(blob => {
            const commitment = blob.commitment || ''
            const short = commitment.length > 16 ? `${commitment.slice(0, 8)}…${commitment.slice(-6)}` : commitment
            const seconds = Math.max(0, Math.floor(now - blob.time))
            const age = seconds < 60 ? `${seconds}s ago` : seconds < 3600 ? `${Math.floor(seconds / 60)}m ago` : seconds < 86400 ? `${Math.floor(seconds / 3600)}h ago` : `${Math.floor(seconds / 86400)}d ago`
            return { height: Number(blob.height).toLocaleString(), commitment: short, full: commitment, size: compact(blob.blob_size, true), signatures: blob.validators > 0 ? `${blob.signed || 0}/${blob.validators} · ${Math.round(blob.power || 0)}%` : '', age }
        })
    }
    let data = null
    function headline(def, points) {
        const n = v => Number(v).toLocaleString()
        const out = []
        const add = (text, tone) => out.push([text, tone])
        switch (def.id) {
        case 'endorsements': {
            const endorsed = sumOf(points, 'endorsements_endorsed') || 0, missed = sumOf(points, 'endorsements_missed') || 0
            add(`${n(endorsed)} endorsed · ${n(missed)} missed${endorsed + missed ? ` · ${(100 * endorsed / (endorsed + missed)).toFixed(1)}%` : ''}`)
            break
        }
        case 'uploads': {
            const ok = sumOf(points, 'uploads_ok'), failed = sumOf(points, 'uploads_failed'), rejected = sumOf(points, 'uploads_rejected')
            if (ok !== null) add(`${n(ok)} ok`)
            if (failed !== null) add(`${n(failed)} failed`, failed > 0 ? 'critical' : '')
            if (rejected !== null) add(`${n(rejected)} rejected`, rejected > 0 ? 'warning' : '')
            const total = (ok || 0) + (failed || 0) + (rejected || 0)
            if (total > 0 && ok !== null) add(`${Math.round(100 * ok / total)}% ok`)
            break
        }
        case 'received': {
            const total = sumOf(points, def.fields[0])
            if (total !== null) add(compact(total, true))
            break
        }
        case 'downloads': {
            const ok = sumOf(points, 'downloads_ok'), failed = sumOf(points, 'downloads_failed'), bytes = sumOf(points, 'bytes_served')
            if (ok !== null) add(`${n(ok)} ok`)
            if (failed !== null) add(`${n(failed)} failed`, failed > 0 ? 'critical' : '')
            const total = (ok || 0) + (failed || 0)
            if (total > 0 && ok !== null) add(`${Math.round(100 * ok / total)}% ok`)
            if (bytes !== null) add(`${compact(bytes, true)} served`)
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
            const totals = def.fields.filter(f => f !== 'store_get_errors').map(f => sumOf(points, f)).filter(v => v !== null)
            if (totals.length) { const total = totals.reduce((a, b) => a + b, 0); add(total ? `${n(total)} errors` : 'no errors', total ? 'critical' : 'good') }
            const misses = sumOf(points, 'store_get_errors')
            if (misses > 0) add(`${n(misses)} store get misses`)
            break
        }
        case 'reach': {
            const share = lastOf(points, 'reachable')
            if (share !== null) add(`${Math.round(100 * share)}% reachable`, share < 1 ? 'warning' : 'good')
            break
        }
        case 'request_latency': case 'internal_latency': {
            // whole-range percentiles avoid jumps from intervals with few operations
            const whole = data?.latency || {}
            for (let i = 1; i < def.fields.length; i += 2) {
                const field = def.fields[i]
                if (whole[field] != null) add(`${def.labels[i].toLowerCase()} ${ms(whole[field])}`)
            }
            break
        }
        case 'resources': {
            const latest = field => data?.summary?.resources?.[field] ?? lastOf(points, field)
            const goroutines = latest('go_goroutines'), processors = latest('go_processor_limit'), gc = latest('go_gogc'), limit = latest('go_memory_limit_bytes')
            if (goroutines !== null) add(`${n(goroutines)} goroutines`)
            if (processors !== null) add(`GOMAXPROCS ${n(processors)}`)
            if (gc !== null) add(`GOGC ${n(gc)}`)
            if (latest('go_memory_no_limit') !== 1 && limit !== null) add(`limit ${compact(limit, true)}`)
            break
        }
        }
        return out
    }
    function resourceDefinition(points) {
        let def = definitions.find(d => d.id === 'resources')
        const limit = data?.summary?.resources?.go_memory_limit_bytes ?? lastOf(points, 'go_memory_limit_bytes')
        const unlimited = data?.summary?.resources?.go_memory_no_limit ?? lastOf(points, 'go_memory_no_limit')
        if (unlimited === 1 || limit === null) def = { ...def, lines: def.lines.slice(0, 1), lineLabels: def.lineLabels.slice(0, 1), lineColors: def.lineColors.slice(0, 1) }
        const observed = points.filter(p => value(p, 'go_memory_used_bytes') !== null || value(p, 'go_memory_heap_bytes') !== null)
        if (!observed.length || !observed.every(p => value(p, 'go_memory_heap_bytes') !== null && value(p, 'go_memory_stack_bytes') !== null && (value(p, 'go_memory_max_bytes') === null || (value(p, 'go_memory_heap_max_bytes') || 0) + (value(p, 'go_memory_stack_max_bytes') || 0) === value(p, 'go_memory_max_bytes')))) return def
        return { ...def, fields: ['go_memory_heap_bytes', 'go_memory_stack_bytes'], labels: ['Heap', 'Stack'], colors: ['precommit', 'prevote'], tooltip: [...def.tooltip, 'go_memory_used_bytes'] }
    }
    if (typeof module !== 'undefined') module.exports = { normalize, definitions, headline, resourceDefinition, missedRows }
    if (typeof document === 'undefined') return
    let layout = defaults()
    try { layout = normalize(JSON.parse(localStorage.getItem('tenderduty-fibre-panels'))) } catch (_) { /* storage is optional */ }
    let range = layout.range
    let watched = []
    let selected = ''
    let pushed = null
    let lastRequestAt = -Infinity
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
    function save() {
        try { localStorage.setItem('tenderduty-fibre-panels', JSON.stringify(layout)) } catch (_) { /* session preferences work without storage */ }
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
        if (!active() && pushed !== null) { clearTimeout(pushed); pushed = null }
        // hiding the page pauses refresh; leaving Fibre cancels the request and clears data
        // load background tabs once so they have data when opened
        if (on && !request && (!data || active())) refresh()
        if (active()) timer = setInterval(() => { if (Date.now() - lastRequestAt >= 5000) refresh() }, 60000)
        else if (request && !on) { request.abort(); request = null }
    }
    document.addEventListener('fibre-state', event => {
        const previous = selected
        watched = event.detail.filter(c => c.fibre)
        $('fibre-nav').hidden = watched.length === 0
        if (!watched.some(c => c.name === selected)) selected = watched[0]?.name || ''
        if (selected !== previous && pushed !== null) { clearTimeout(pushed); pushed = null }
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
        let budget = `${f.occupancy_known ? compact(f.occupancy, true) : 'Unknown'} of ${f.chain_budget > 0 ? compact(f.chain_budget, true) : 'Unknown'}`
        if (f.budget_reported) {
            if (f.unlimited) budget += ' · Fibre unlimited'
            else if (f.fibre_budget !== f.chain_budget) budget += ` · Fibre reports ${compact(f.fibre_budget, true)}`
        }
        const facts = [
            ['Provider', f.host || 'Not registered'],
            ['Reachable', f.reachable ? '● Yes' : '● No'], ['Identity', f.identity || 'Unchecked'],
            ['Assigned rows', `${f.rows}/${f.total_rows} · ${Number(f.row_percent.toFixed(2))}%`],
            ['Shard budget', budget],
            ['Endorsing', f.endorsing?.text || 'none seen yet'],
            ['Endorsed (24h)', (f.last_24h?.endorsed || 0) + (f.last_24h?.missed || 0) ? `${f.last_24h.endorsed || 0} of ${(f.last_24h.endorsed || 0) + (f.last_24h.missed || 0)} · ${(100 * (f.last_24h.endorsed || 0) / ((f.last_24h.endorsed || 0) + (f.last_24h.missed || 0))).toFixed(1)}%` : 'none seen yet'],
            ['Metrics last received', f.metrics_received ? new Date(f.metrics_at * 1000).toLocaleString() : 'Not received'],
            ['Fibre version', f.version || 'Unknown']
        ]
        if (f.storage_backend) facts.push(['Storage backend', f.storage_backend])
        facts.push(['Last successful upload', f.last_upload_at ? new Date(f.last_upload_at * 1000).toLocaleString() : 'none yet'])
        $('fibre-facts').replaceChildren(...facts.map(([label, text]) => {
            const div = node('div', undefined, 'detail-fact'); div.append(node('small', label), node('strong', text))
            if (label === 'Endorsing' && f.endorsing?.stopped) div.lastChild.style.color = 'var(--critical)'
            if (label === 'Reachable') div.lastChild.style.color = `var(--${f.reachable ? 'good' : 'critical'})`
            return div
        }))
    }
    async function refresh() {
        if (location.hash !== '#fibre' || !watched.length || !selected) return
        if (request) request.abort()
        lastRequestAt = Date.now()
        const controller = new AbortController(); request = controller
        // show loading text once to avoid shifts on refresh
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
    // rounds up to 1, 2, 4 or 5 times a power of ten, using binary units for bytes
    function niceTop(v, bytes) {
        if (!(v > 0)) return 1
        let unit = 1
        if (bytes) while (v / unit >= 1024) unit *= 1024
        const scaled = v / unit, power = 10 ** Math.floor(Math.log10(scaled))
        const step = [1, 2, 4, 5, 10].find(s => s * power >= scaled * 0.999)
        return step * power * unit
    }
    function countTop(v) {
        const power = 10 ** Math.floor(Math.log10(v))
        return [1, 1.5, 2, 2.5, 3, 4, 5, 6, 8, 10].map(s => s * power).find(t => t >= v * 0.999 && Number.isInteger(t))
    }
    // cap spikes at twice the 90th percentile; tooltips retain actual values
    function robustTop(values) {
        const sorted = values.filter(v => v > 0).sort((a, b) => a - b)
        if (sorted.length < 10) return sorted.at(-1) || 0
        const typical = sorted[Math.floor(sorted.length * 0.9)]
        return Math.min(sorted.at(-1), typical * 2)
    }
    function axisText(def, v) {
        return def.unit === 'ms' ? ms(v) : compact(v, def.unit === 'bytes')
    }
    // build offscreen at the measured width to avoid shifts on refresh
    function drawChart(def, points, chartWidth, chartStyle) {
        if (def.id === 'resources' && ['7d', '30d'].includes(range)) points = points.map(p => ({ ...p, metrics: { ...p.metrics,
            ...Object.fromEntries([['go_memory_used_bytes', 'go_memory_max_bytes'], ['go_memory_heap_bytes', 'go_memory_heap_max_bytes'], ['go_memory_stack_bytes', 'go_memory_stack_max_bytes'], ['go_goroutines', 'go_goroutines_max']].filter(([, peak]) => p.metrics?.[peak] != null).map(([field, peak]) => [field, p.metrics[peak]]))
        } }))
        const axisPoints = points
        const seconds = points.length > 1 ? points[1].time - points[0].time : 60
        const counting = points.length > 0 && Date.now() / 1000 < points.at(-1).time + seconds
        const hours = def.stacked && !def.area && !def.strip && !def.log && (!chartStyle || ['auto', 'bars'].includes(chartStyle)) ? { '24h': 1, '7d': 6, '30d': 24 }[range] : 0
        if (hours && points.length) {
            const buckets = new Map()
            for (const p of points) {
                const date = new Date(p.time * 1000)
                date.setHours(Math.floor(date.getHours() / hours) * hours, 0, 0, 0)
                const time = date.getTime() / 1000
                if (!buckets.has(time)) buckets.set(time, [])
                buckets.get(time).push(p)
            }
            points = []
            const date = new Date(Math.min(...buckets.keys()) * 1000), last = Math.max(...buckets.keys())
            while (date.getTime() / 1000 <= last) {
                const time = date.getTime() / 1000, observed = buckets.get(time) || []
                date.setHours(date.getHours() + hours)
                const metrics = Object.fromEntries([...def.fields, ...(def.tooltip || [])].map(f => [f,
                    f.endsWith('_ms') || f.endsWith('_in_flight') ? lastOf(observed, f) : sumOf(observed, f)]))
                points.push({ time, end: date.getTime() / 1000, metrics })
            }
        }
        const right = def.right || []
        const lines = def.lines || []
        const all = [...def.fields, ...lines, ...right]
        const body = node('div', undefined, 'fibre-chart')
        if (!points.some(p => all.some(f => value(p, f) !== null))) { body.append(node('p', 'No observations in this range', 'empty')); return body }
        // drawn at actual width so labels don't scale with the view box
        const width = Math.max(def.mini ? 180 : 280, Math.round(chartWidth))
        const height = def.mini ? 120 : 190, left = 62, end = width - (right.length ? 40 : 8), top = 10, bottom = height - 32
        const step = (end - left) / points.length
        const x = i => left + (i + 0.5) * step
        // auto uses bars when intervals are wide enough
        const mode = def.area ? 'area' : def.strip ? 'strip' : !def.stacked ? 'line' : chartStyle === 'auto' || !chartStyle ? (hours || step >= 6 ? 'bars' : 'area') : chartStyle
        const sum = p => def.fields.reduce((total, f) => total + (value(p, f) || 0), 0)
        const positive = def.log ? points.flatMap(p => [...def.fields, ...lines].map(f => value(p, f))).filter(v => v > 0) : []
        const logBottom = Math.floor(Math.log10(Math.max(0.01, positive.length ? Math.min(...positive) : 0.01)))
        const logTop = Math.max(logBottom + 1, Math.ceil(Math.log10(Math.max(0.01, ...positive))))
        const highest = def.unit === 'ms'
            ? robustTop(points.flatMap(p => def.fields.map(f => value(p, f) || 0)))
            : Math.max(0, ...points.map(p => def.stacked && mode !== 'line' ? sum(p) : Math.max(0, ...def.fields.map(f => value(p, f) || 0))))
        const peak = Math.max(def.unit === 'ms' ? 0.1 : 1, highest, ...points.flatMap(p => lines.map(f => value(p, f) || 0)))
        // counts: whole-number top with headroom, so one blob an hour is not a full bar
        const top1 = def.strip ? 1 : def.unit === 'count' && !def.log ? countTop(peak * 1.2) : niceTop(peak, def.unit === 'bytes')
        // extra headroom lowers the right-axis line
        const top2 = niceTop(2 * Math.max(1, ...points.map(p => Math.max(0, ...right.map(f => value(p, f) || 0)))), false)
        const y = (v, onRight) => bottom - (def.log && !onRight ? Math.max(0, (Math.log10(v) - logBottom) / (logTop - logBottom)) : Math.min(v, onRight ? top2 : top1) / (onRight ? top2 : top1)) * (bottom - top)
        const chart = svg('svg', { viewBox: `0 0 ${width} ${height}`, role: 'img', 'aria-label': `${def.title}, ${selected}, ${range}: ${headline(def, axisPoints).map(([t]) => t).join(', ') || 'no observations'}` })
        const ticks = def.log ? Array.from({ length: logTop - logBottom + 1 }, (_, i) => 10 ** (logBottom + i)) : [0, top1 / 2, top1].filter(tick => def.unit === 'bytes' || def.unit === 'ms' || def.strip || Number.isInteger(tick))
        if (!def.strip) for (const tick of ticks) {
            const fraction = tick / top1
            const at = y(tick)
            chart.append(svg('line', { x1: left, x2: end, y1: at, y2: at, stroke: 'var(--line)' }))
            const label = svg('text', { x: left - 8, y: at + 4, 'text-anchor': 'end', fill: 'var(--subtle)', 'font-size': 12 })
            label.textContent = axisText(def, tick); chart.append(label)
            if (right.length) {
                const second = svg('text', { x: end + 6, y: at + 4, fill: 'var(--subtle)', 'font-size': 12 })
                second.textContent = compact(top2 * fraction, false); chart.append(second)
            }
        }
        if (mode === 'area') {
            // stack bands across each observed run, from first to last interval edge
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
                    // outline only where this band is thicker than zero, or an empty top band paints the whole edge
                    let edge = []
                    const flush = () => { if (edge.length > 1) chart.append(svg('path', { d: `M${edge.map(at).join(' L')}`, fill: 'none', stroke: color, 'stroke-width': 1.5, 'stroke-linejoin': 'round' })); edge = [] }
                    for (let m = i; m <= k; m++) {
                        if ((value(points[m], f) || 0) > 0) {
                            if (!edge.length) edge.push([x(m) - step / 2, totals[m]])
                            edge.push([x(m), totals[m]])
                        } else flush()
                    }
                    if (edge.length) edge.push([x(k) + step / 2, totals[k]])
                    flush()
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
                    // overlap narrow bars to avoid visible seams
                    const dense = step < 4
                    chart.append(svg('rect', dense
                        ? { x: x(i) - step / 2, width: step + 0.5, ...box }
                        : { x: x(i) - step / 2 + 0.5, width: step - 1, rx: step > 6 ? 1.5 : 0, ...box }))
                    base += v
                })
            })
        }
        {
            const line = (field, color, onRight) => {
                let path = '', joined = false
                points.forEach((p, i) => {
                    const v = value(p, field)
                    if (v === null || def.log && !onRight && v <= 0) { joined = false; return }
                    path += `${joined ? 'L' : 'M'}${x(i).toFixed(1)},${y(v, onRight).toFixed(1)} `
                    const drawable = p => value(p, field) !== null && (!def.log || onRight || value(p, field) > 0)
                    const before = i > 0 && drawable(points[i - 1]), after = i < points.length - 1 && drawable(points[i + 1])
                    if (!before && !after) chart.append(svg('circle', { cx: x(i), cy: y(v, onRight), r: 2.5, fill: `var(--${color})` }))
                    joined = true
                })
                const dashed = field.endsWith('_p95_ms') || field === 'go_gc_goal_bytes'
                chart.append(svg('path', { d: path, fill: 'none', stroke: `var(--${color})`, 'stroke-width': 2, 'stroke-linejoin': 'round', 'stroke-dasharray': dashed ? '5 4' : 'none' }))
            }
            if (mode === 'line') def.fields.forEach((f, j) => line(f, def.colors[j], false))
            lines.forEach((f, j) => line(f, def.lineColors[j], false))
            right.forEach((f, j) => line(f, def.rightColors[j], true))
        }
        const last = points.length - 1
        if (counting && !def.strip) chart.append(svg('rect', { x: x(last) - step / 2, y: top, width: end - x(last) + step / 2, height: bottom - top + 1, fill: 'var(--panel)', 'fill-opacity': 0.6 }))
        for (const i of [0, Math.floor(points.length / 2), points.length - 1]) {
            const label = svg('text', { x: x(i), y: bottom + 22, 'text-anchor': i === 0 ? 'start' : i === points.length - 1 ? 'end' : 'middle', fill: 'var(--subtle)', 'font-size': def.mini ? 11 : 12 })
            const date = new Date(points[i].time * 1000)
            label.textContent = ['7d', '30d'].includes(range) ? date.toLocaleDateString(undefined, { month: 'short', day: 'numeric' }) : date.toLocaleTimeString(undefined, hours ? { hour: 'numeric' } : { hour: '2-digit', minute: '2-digit' })
            chart.append(label)
        }
        const tip = node('div', undefined, 'fibre-tip'); tip.hidden = true; tip.setAttribute('role', 'status')
        const show = i => {
            const p = points[i]
            const rows = [...def.fields.map((f, j) => [def.labels[j], f]), ...lines.map((f, j) => [def.lineLabels[j], f]), ...right.map((f, j) => [def.rightLabels[j], f]), ...(def.tooltip || []).map(f => [tooltipLabels[f] || f.replaceAll('_', ' '), f])]
                .map(([label, f]) => [label, f === 'go_memory_limit_bytes' && value(p, f) === null && value(p, 'go_memory_used_bytes') !== null ? 'no limit' : value(p, f) === null ? null : shown(f, value(p, f))])
                .filter(([, text]) => text !== null)
            tip.replaceChildren(node('strong', new Date(p.time * 1000).toLocaleString(undefined, { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' }) + (p.end ? `\u2013${new Date(p.end * 1000).toLocaleString(undefined, { ...(hours === 24 ? { month: 'short', day: 'numeric' } : {}), hour: '2-digit', minute: '2-digit' })}` : '')))
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
    function latencyCharts(def, points, width) {
        const grid = node('div', undefined, 'fibre-latency-grid')
        for (let i = 0; i < def.fields.length; i += 2) {
            const title = def.labels[i].replace(' p50', '')
            const mini = { ...def, title, mini: true, fields: def.fields.slice(i, i + 2), labels: ['p50', 'p95'], colors: def.colors.slice(i, i + 2), tooltip: [def.tooltip[i / 2]] }
            const box = node('section', undefined, 'fibre-latency-mini')
            const p95 = data?.latency?.[mini.fields[1]]
            const heading = node('h3', undefined, 'fibre-latency-title'), dot = node('i')
            dot.style.background = `var(--${mini.colors[0]})`
            heading.append(dot, document.createTextNode(`${title}${p95 == null ? '' : ` \u00b7 p95 ${ms(p95)}`}`))
            box.append(heading, drawChart(mini, points, width < 520 ? width : (width - 16) / 2, 'line'))
            grid.append(box)
        }
        return grid
    }
    function legend(def) {
        const box = node('div', undefined, 'fibre-legend')
        const entry = (label, color, note) => {
            const item = node('span'); const dot = node('i'); dot.style.background = `var(--${color})`
            item.append(dot, document.createTextNode(label)); if (note) item.append(node('small', note)); box.append(item)
        }
        if (def.unit === 'ms') {
            // line style tells p50 from p95
            box.append(node('span', 'solid p50 · dashed p95', 'note'))
        } else if (def.fields.length > 1 || def.right || def.lines) {
            def.fields.forEach((f, i) => entry(def.labels[i], def.colors[i], f === 'go_gc_goal_bytes' ? 'dashed' : ''))
            ;(def.right || []).forEach((f, i) => entry(def.rightLabels[i], def.rightColors[i], 'right axis'))
        }
        ;(def.lines || []).forEach((f, i) => entry(def.lineLabels[i], def.lineColors[i], f === 'go_gc_goal_bytes' ? 'dashed' : ''))
        return box
    }
    function drawPanels() {
        const grid = $('fibre-panels')
        if (!data) { grid.replaceChildren(); return }
        const points = data.series || []
        const shown = layout.panels.filter(p => p.show)
        if (!shown.length) { grid.replaceChildren(node('p', 'All panels are hidden. Open Customize panels to show them.')); return }
        const existing = new Map([...grid.querySelectorAll('.fibre-panel')].map(panel => [panel.dataset.panel, panel]))
        const panels = shown.map(preference => {
            const panel = existing.get(preference.id) || node('section')
            panel.className = `fibre-panel${preference.wide ? ' wide' : ''}`
            panel.dataset.panel = preference.id
            return panel
        })
        if (panels.some((panel, i) => grid.children[i] !== panel) || grid.children.length !== panels.length) grid.replaceChildren(...panels)
        shown.forEach((preference, i) => {
            const def = preference.id === 'resources' ? resourceDefinition(points) : definitions.find(d => d.id === preference.id), panel = panels[i]
            if (def.id === 'errors') def.labels[3] = data.summary?.storage_backend ? `Storage backend (${data.summary.storage_backend})` : 'Storage backend'
            const style = getComputedStyle(panel)
            const width = panel.clientWidth - parseFloat(style.paddingLeft) - parseFloat(style.paddingRight)
            const head = node('header', undefined, 'fibre-panel-head')
            const stats = node('p', undefined, 'fibre-stats')
            // latency minis title their own p95
            if (!def.id.endsWith('_latency')) for (const [text, tone] of headline(def, points)) { const stat = node('span', text); if (tone) stat.className = tone; stats.append(stat) }
            head.append(node('h2', def.title), stats)
            const notes = []
            if (def.id === 'endorsements') notes.push("From the chain: blobs paid for with or without this validator's signature.")
            if (def.id === 'uploads') notes.push("Failed includes payments Fibre refused (e.g. the payer's escrow was too low) and uploads under the validator's min_upload_size.")
            if (def.id === 'downloads') notes.push("Failed includes shards this validator doesn't hold.")
            panel.replaceChildren(head, legend(def), ...notes.map(text => node('p', text, 'fibre-note')), def.log ? latencyCharts(def, points, width) : drawChart(def, points, width, preference.chart))
            if (def.id === 'endorsements') {
                const list = node('div', undefined, 'fibre-missed'); list.append(node('h3', 'Latest missed'))
                const rows = missedRows(data.summary?.latest_missed)
                if (!rows.length) list.append(node('p', 'None seen yet', 'fibre-note'))
                for (const row of rows) {
                    const item = node('p', undefined, 'fibre-note'), commitment = node('code', row.commitment); commitment.title = row.full
                    item.append(node('span', `Height ${row.height} · `), commitment, document.createTextNode(` · ${row.size} · ${row.signatures ? `${row.signatures} · ` : ''}${row.age}`)); list.append(item)
                }
                panel.append(list)
            }
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
            if (definitions.find(d => d.id === p.id).stacked && p.id !== 'resources') {
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
    $('fibre-chain').onchange = event => { if (pushed !== null) { clearTimeout(pushed); pushed = null }; selected = event.target.value; $('fibre-heading').textContent = `Fibre · ${selected}`; data = null; refresh() }
    document.addEventListener('fibre-update', event => {
        if (!active() || event.detail.name !== selected || pushed !== null) return
        const chain = selected
        const receive = () => {
            pushed = null
            if (!active() || selected !== chain) return
            const delay = 5000 - (Date.now() - lastRequestAt)
            if (delay > 0) pushed = setTimeout(receive, delay)
            else refresh()
        }
        pushed = setTimeout(receive, Math.max(0, 5000 - (Date.now() - lastRequestAt)))
    })
    window.addEventListener('hashchange', route)
    let resized = null
    window.addEventListener('resize', () => { clearTimeout(resized); resized = setTimeout(() => { if (active()) drawPanels() }, 200) })
    document.addEventListener('visibilitychange', route)
    rangeButtons(); route()
})()
