/* Fibre charts leave unobserved intervals blank. */
(() => {
    'use strict'
    const definitions = [
        // fields: left axis; right: right axis; tooltip: hover only
        { id: 'endorsements', title: 'Endorsements', fields: ['endorsements_endorsed', 'endorsements_missed'], labels: ['Endorsed', 'Missed'], colors: ['good', 'critical'], stacked: true, unit: 'count' },
        { id: 'uploads', title: 'Uploads', fields: ['uploads_ok', 'uploads_failed', 'uploads_rejected'], labels: ['OK', 'Failed', 'Rejected'], colors: ['good', 'critical', 'warning'], stacked: true, unit: 'count' },
        { id: 'downloads', title: 'Downloads', fields: ['downloads_ok', 'downloads_failed'], labels: ['OK', 'Failed'], colors: ['good', 'critical'], stacked: true, unit: 'count', tooltip: ['bytes_served', 'object_storage_bytes', 'object_storage_requests', 'object_storage_in_flight'] },
        { id: 'latency', title: 'Latency', fields: ['upload_p50_ms', 'upload_p95_ms', 'download_p50_ms', 'download_p95_ms', 'sign_p50_ms', 'sign_p95_ms', 'store_put_p50_ms', 'store_put_p95_ms', 'store_get_p50_ms', 'store_get_p95_ms', 'prune_p50_ms', 'prune_p95_ms'], labels: ['Upload p50', 'Upload p95', 'Download p50', 'Download p95', 'Signing p50', 'Signing p95', 'Store put p50', 'Store put p95', 'Store get p50', 'Store get p95', 'Prune p50', 'Prune p95'], colors: ['precommit', 'precommit', 'prevote', 'prevote', 'precommit', 'precommit', 'warning', 'warning', 'prevote', 'prevote', 'critical', 'critical'], unit: 'ms', log: true, tooltip: ['upload_samples', 'download_samples', 'sign_samples', 'store_put_samples', 'store_get_samples', 'prune_samples'] },
        { id: 'storage', title: 'Shards stored', fields: ['occupancy'], labels: ['Used'], colors: ['precommit'], unit: 'bytes', tooltip: ['budget'] },
        { id: 'received', title: 'Data received', fields: ['bytes_received'], labels: ['Received'], colors: ['precommit'], stacked: true, unit: 'bytes' },
        { id: 'errors', title: 'Errors', fields: ['store_put_errors', 'store_get_errors', 'prune_errors', 'object_storage_errors'], labels: ['Store put', 'Store get misses', 'Prune', 'Storage backend'], colors: ['critical', 'muted', 'prevote', 'precommit'], stacked: true, unit: 'count' },
        { id: 'reach', title: 'Reachability', fields: ['reachable'], labels: ['Reachable'], colors: ['good'], strip: true },
        { id: 'resources', title: 'Resources', fields: ['go_memory_used_bytes'], labels: ['Memory used'], colors: ['precommit'], stacked: true, area: true, unit: 'bytes', lines: ['go_gc_goal_bytes', 'go_memory_limit_bytes'], lineLabels: ['GC goal', 'Memory limit'], lineColors: ['warning', 'critical'], tooltip: ['go_goroutines', 'go_allocation_bytes_per_second', 'go_schedule_p95_ms', 'go_memory_allocations'] },
        { id: 'signing', title: 'Signing', fields: ['signatures_fast', 'signatures_slow', 'signatures_failed'], labels: ['Under 10 ms', 'Over 10 ms', 'Failed'], colors: ['good', 'warning', 'critical'], stacked: true, unit: 'count', tooltip: ['avg_sign_ms'] }
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
    const defaults = () => ({ range: '24h', panels: definitions.map(d => ({ id: d.id, show: d.id !== 'signing', wide: ['endorsements', 'latency', 'storage'].includes(d.id), chart: 'auto' })) })
    function normalize(value) {
        const result = defaults()
        if (!value || !Array.isArray(value.panels)) return result
        result.range = ranges.includes(value.range) ? value.range : '24h'
        const seen = new Set()
        const panels = value.panels.map(p => ['request_latency', 'internal_latency'].includes(p?.id) ? { ...p, id: 'latency' } : p)
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
    const shown = (field, n) => n === null ? 'No observation' : field.endsWith('_ms') ? ms(n) : field === 'reachable' ? `${Math.round(100 * n)}%` : field === 'budget' && n === 0 ? 'unlimited' : field === 'go_allocation_bytes_per_second' ? `${compact(n, true)}/s` : (field.endsWith('_bytes') || byteFields.includes(field)) ? compact(n, true) : exact(n)
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
    const rangeSeconds = { '1h': 3600, '24h': 86400, '7d': 604800, '30d': 2592000 }
    const reachIntervals = { '1h': [12, 300, '5 minutes'], '24h': [48, 1800, '30 minutes'], '7d': [42, 14400, '4 hours'], '30d': [30, 86400, '1 day'] }
    const reachColor = v => v === null ? 'line' : v >= 0.999 ? 'good' : v >= 0.5 ? 'warning' : 'critical'
    function reachSlots(points, range, start = points[0]?.time ?? Date.now() / 1000 - rangeSeconds[range]) {
        const [count, seconds] = reachIntervals[range]
        return Array.from({ length: count }, (_, i) => {
            const time = start + i * seconds, end = time + seconds
            const observed = points.filter(p => p.time >= time && p.time < end && value(p, 'reachable') !== null)
            const reachable = observed.length ? observed.reduce((total, p) => total + value(p, 'reachable'), 0) / observed.length : null
            return { time, end, reachable, color: reachColor(reachable) }
        })
    }
    function leadingUnrecorded(def, points) {
        const fields = [...def.fields, ...(def.lines || []), ...(def.right || [])]
        const first = points.findIndex(p => fields.some(f => value(p, f) !== null))
        const fraction = first < 0 ? 0 : first / points.length
        return fraction > 0.03 ? fraction : 0
    }
    const chainGroups = chains => ['Mainnet', 'Testnet'].map(label => ({ label, chains: chains.filter(c => Boolean(c.testnet) === (label === 'Testnet')) })).filter(group => group.chains.length)
    const ageText = (at, now = Date.now() / 1000) => {
        const seconds = Math.max(0, Math.floor(now - at))
        return seconds < 60 ? `${seconds}s` : seconds < 3600 ? `${Math.floor(seconds / 60)}m` : seconds < 86400 ? `${Math.floor(seconds / 3600)}h` : `${Math.floor(seconds / 86400)}d`
    }
    function summaryFacts(f, points, range, now = Date.now() / 1000) {
        const n = v => Number(v).toLocaleString()
        const endorsed = sumOf(points, 'endorsements_endorsed') || 0, missed = sumOf(points, 'endorsements_missed') || 0
        const ok = sumOf(points, 'uploads_ok') || 0, failed = sumOf(points, 'uploads_failed') || 0, rejected = sumOf(points, 'uploads_rejected') || 0
        const observed = points.map(p => value(p, 'reachable')).filter(v => v !== null)
        let budget = f.chain_budget > 0 ? `of ${compact(f.chain_budget, true)}` : f.budget_reported && f.unlimited ? 'no budget limit' : 'of Unknown'
        if (f.budget_reported && f.chain_budget > 0 && f.unlimited) budget += ' · Fibre unlimited'
        else if (f.budget_reported && !f.unlimited && f.fibre_budget !== f.chain_budget) budget += ` · Fibre reports ${compact(f.fibre_budget, true)}`
        return [
            f.pending ? { label: 'Reachable', value: 'Checking…', tone: '', sub: '' } : { label: 'Reachable', value: f.reachable ? 'Yes' : 'No', tone: f.reachable ? 'good' : 'critical', sub: !f.reachable ? [f.down_since ? `down ${ageText(f.down_since, now)}` : 'down', f.down_reason].filter(Boolean).join(' · ') : observed.length ? `${Math.round(100 * observed.reduce((a, b) => a + b, 0) / observed.length)}% over ${range}` : '' },
            { label: 'Ready', value: f.pending ? 'Checking…' : f.ready === 'yes' ? 'Yes' : f.ready === 'no' ? 'No' : 'Unknown', tone: !f.pending && f.ready === 'yes' ? 'good' : !f.pending && f.ready === 'no' ? 'warning' : '', sub: !f.pending && (!f.ready || f.ready === 'unknown') ? 'not reported (Fibre before v10.5)' : '' },
            { label: 'Endorsed', value: endorsed + missed ? `${(100 * endorsed / (endorsed + missed)).toFixed(1)}%` : '—', sub: endorsed + missed ? `${n(missed)} missed of ${n(endorsed + missed)}` : 'none seen' },
            { label: 'Uploads ok', value: ok + failed + rejected ? `${Math.round(100 * ok / (ok + failed + rejected))}%` : '—', sub: `${n(failed)} failed · ${n(rejected)} rejected`, failed },
            { label: 'Last upload', value: f.last_upload_at ? `${ageText(f.last_upload_at, now)} ago` : 'none yet', sub: f.last_upload_at ? new Date(f.last_upload_at * 1000).toLocaleString() : '' },
            { label: 'Shards stored', value: f.occupancy_known ? compact(f.occupancy, true) : 'Unknown', sub: budget }
        ]
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
        case 'latency': {
            const fast = sumOf(points, 'signatures_fast') || 0, slow = sumOf(points, 'signatures_slow') || 0, failed = sumOf(points, 'signatures_failed') || 0
            if (fast + slow > 0) add(`signing ${Math.floor(100 * fast / (fast + slow))}% under 10 ms`, slow === 0 ? 'good' : 'warning')
            if (failed > 0) add(`${n(failed)} signing failed`, 'critical')
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
    if (typeof module !== 'undefined') module.exports = { normalize, definitions, headline, resourceDefinition, missedRows, summaryFacts, reachSlots, reachColor, leadingUnrecorded, chainGroups }
    if (typeof window !== 'undefined') window.tdCharts = { drawChart, legend, chainPicker }
    if (typeof document === 'undefined') return
    // v2 resets defaults for the merged latency and content-led layout.
    const storageKey = 'tenderduty-fibre-panels-v2'
    let layout = defaults()
    try { layout = normalize(JSON.parse(localStorage.getItem(storageKey))) } catch (_) { /* storage is optional */ }
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
        try { localStorage.setItem(storageKey, JSON.stringify(layout)) } catch (_) { /* session preferences work without storage */ }
    }
    function active() { return location.hash === '#fibre' && !document.hidden && watched.length > 0 }
    function route() {
        const on = location.hash === '#fibre' && watched.length > 0
        $('overview').hidden = on || location.hash === '#bridge' && !$('bridge-nav').hidden
        $('fibre').hidden = !on
        for (const link of document.querySelectorAll('.nav a')) {
            const current = on ? link.id === 'fibre-nav' : location.hash === '#bridge' && !$('bridge-nav').hidden ? link.id === 'bridge-nav' : link.hash === '#overview'
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
        chainPicker('fibre', watched, selected)
        if (!watched.length || !timer) route()
        else if (selected !== previous) { data = null; refresh() }
    })
    function chainPicker(tab, chains, selected) {
        $(`${tab}-chain`).replaceChildren(...chainGroups(chains).map(group => {
            const optgroup = node('optgroup'); optgroup.label = group.label
            optgroup.append(...group.chains.map(c => {
                const option = node('option', c.name); option.value = c.name; option.selected = c.name === selected; return option
            }))
            return optgroup
        }))
        $(`${tab}-chain`).value = selected
        $(`${tab}-chain-label`).hidden = chains.length < 2
        $(`${tab}-chain-name`).textContent = selected
        $(`${tab}-chain-name`).hidden = chains.length !== 1
        $(`${tab}-testnet`).hidden = !chains.find(c => c.name === selected)?.testnet
        $(`${tab}-heading`).textContent = tab === 'fibre' ? 'Fibre' : 'Bridge'
    }
    function cards(f) {
        if (!f) { $('fibre-facts').replaceChildren(node('p', 'Provider check pending.')); return }
        const grid = node('div', undefined, 'summary-grid')
        grid.append(...summaryFacts(f, data?.series || [], range).map(fact => {
            const cell = node('div', undefined, 'summary-cell'), strong = node('strong', fact.value), sub = node('span', fact.sub, 'summary-sub')
            strong.title = fact.value
            if (fact.tone) strong.style.color = `var(--${fact.tone})`
            if (fact.failed > 0) {
                const failed = node('span', fact.sub.split(' · ')[0]); failed.style.color = 'var(--critical)'
                sub.replaceChildren(failed, document.createTextNode(` · ${fact.sub.split(' · ')[1]}`))
            }
            cell.append(node('small', fact.label), strong, sub); return cell
        }))
        const meta = node('dl', undefined, 'summary-meta')
        const facts = [
            ['Provider', f.pending ? 'checking…' : f.host || 'Not registered', '', f.pending ? '' : 'mono'],
            ['Identity', f.pending ? 'checking…' : f.identity === 'failed' ? `failed${f.identity_error ? ` · ${f.identity_error}` : ''}` : f.identity || 'unchecked', f.identity === 'verified' ? 'good' : f.identity === 'failed' ? 'critical' : ''],
            ['Version', f.version || 'Unknown'],
            ...(f.storage_backend ? [['Storage', f.storage_backend]] : []),
            ['Rows', f.pending ? 'checking…' : `${Number(f.rows || 0).toLocaleString()} / ${Number(f.total_rows || 0).toLocaleString()} · ${Number(f.row_percent || 0).toFixed(2)}%`],
            ['Last endorsed', (f.endorsing?.text || 'none seen yet').replace(/^last endorsed /i, ''), f.endorsing?.stopped ? 'critical' : ''],
            ['Metrics', f.metrics_received ? `${ageText(f.metrics_at)} ago` : 'not received']
        ]
        meta.append(...facts.map(([label, text, tone, cls]) => {
            const pair = node('div'), dd = node('dd', text, cls)
            if (tone) dd.style.color = `var(--${tone})`
            pair.append(node('dt', label), dd); return pair
        }))
        $('fibre-facts').replaceChildren(grid, meta)
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
    function drawChart(def, points, chartWidth, chartStyle, view = { range, selected, headline, tooltipLabels }) {
        if (def.id === 'resources' && ['7d', '30d'].includes(view.range)) points = points.map(p => ({ ...p, metrics: { ...p.metrics,
            ...Object.fromEntries([['go_memory_used_bytes', 'go_memory_max_bytes'], ['go_memory_heap_bytes', 'go_memory_heap_max_bytes'], ['go_memory_stack_bytes', 'go_memory_stack_max_bytes'], ['go_goroutines', 'go_goroutines_max']].filter(([, peak]) => p.metrics?.[peak] != null).map(([field, peak]) => [field, p.metrics[peak]]))
        } }))
        const axisPoints = points
        const seconds = points.length > 1 ? points[1].time - points[0].time : 60
        const counting = points.length > 0 && Date.now() / 1000 < points.at(-1).time + seconds
        const hours = def.stacked && !def.area && !def.strip && !def.log && (!chartStyle || ['auto', 'bars'].includes(chartStyle)) ? { '24h': 1, '7d': 6, '30d': 24 }[view.range] : 0
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
        if (def.strip) points = reachSlots(points, view.range)
        const right = def.right || []
        const lines = def.lines || []
        const all = [...def.fields, ...lines, ...right]
        const body = node('div', undefined, 'fibre-chart')
        if (!def.strip && !points.some(p => all.some(f => value(p, f) !== null))) { body.append(node('p', 'No observations in this range', 'empty')); return body }
        // drawn at actual width so labels don't scale with the view box
        const width = Math.max(def.mini ? 180 : 280, Math.round(chartWidth))
        const height = def.strip ? 152 : def.mini ? 120 : 190, left = 62, end = width - (right.length ? 40 : 8), top = 10, bottom = height - 32
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
        const chart = svg('svg', { viewBox: `0 0 ${width} ${height}`, role: 'img', 'aria-label': `${def.title}, ${view.selected}, ${view.range}: ${view.headline(def, axisPoints).map(([t]) => t).join(', ') || 'no observations'}` })
        const ticks = def.log ? Array.from({ length: logTop - logBottom + 1 }, (_, i) => 10 ** (logBottom + i)) : [0, top1 / 2, top1].filter(tick => def.unit === 'bytes' || def.unit === 'ms' || def.strip || Number.isInteger(tick))
        if (!def.strip) for (const tick of ticks) {
            const fraction = tick / top1
            const at = y(tick)
            chart.append(svg('line', { x1: left, x2: end, y1: at, y2: at, stroke: 'color-mix(in srgb, var(--line) 55%, var(--panel))' }))
            const label = svg('text', { x: left - 8, y: at + 4, 'text-anchor': 'end', fill: 'var(--subtle)', 'font-size': 11 })
            label.textContent = axisText(def, tick); chart.append(label)
            if (right.length) {
                const second = svg('text', { x: end + 6, y: at + 4, fill: 'var(--subtle)', 'font-size': 11 })
                second.textContent = compact(top2 * fraction, false); chart.append(second)
            }
        }
        const leading = leadingUnrecorded(def, axisPoints) * (end - left)
        if (leading > 0 && !def.strip) {
            const id = `not-recorded-${def.id}-${view.range}${def.mini ? `-${def.fields[0]}` : ''}`
            const pattern = svg('pattern', { id, width: 8, height: 8, patternUnits: 'userSpaceOnUse' })
            pattern.append(svg('path', { d: 'M-2,2 L2,-2 M0,8 L8,0 M6,10 L10,6', stroke: 'color-mix(in srgb, var(--line) 65%, var(--panel))', 'stroke-width': 1 }))
            const defs = svg('defs', {}); defs.append(pattern); chart.append(defs)
            chart.append(svg('rect', { x: left, y: top, width: leading, height: bottom - top, fill: `url(#${id})` }))
            if (leading >= 120) {
                const label = svg('text', { x: left + leading / 2, y: (top + bottom) / 2, 'text-anchor': 'middle', fill: 'var(--subtle)', 'font-size': 11.5 })
                label.textContent = 'Not recorded yet'; chart.append(label)
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
                    chart.append(svg('path', { d: `M${upper.map(at).join(' L')} L${under.map(at).join(' L')} Z`, fill: `color-mix(in srgb, ${color} 82%, var(--panel))`, stroke: 'none' }))
                    // outline only where this band is thicker than zero, or an empty top band paints the whole edge
                    let edge = []
                    const flush = () => { if (edge.length > 1) chart.append(svg('path', { d: `M${edge.map(at).join(' L')}`, fill: 'none', stroke: color, 'stroke-width': 1.6, 'stroke-linejoin': 'round' })); edge = [] }
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
                    if (!def.strip && (v === null || v === 0)) return
                    const box = def.strip
                        ? { y: top, height: bottom - top, fill: v === null ? 'color-mix(in srgb, var(--line) 55%, var(--panel))' : `var(--${reachColor(v)})` }
                        : { y: y(base + v), height: Math.max(1, v / top1 * (bottom - top)), fill: `color-mix(in srgb, var(--${def.colors[j]}) 82%, var(--panel))` }
                    // overlap narrow bars to avoid visible seams
                    const dense = step < 4 && !def.strip
                    chart.append(svg('rect', dense
                        ? { x: x(i) - step / 2, width: step + 0.5, ...box }
                        : { x: x(i) - step / 2 + (def.strip ? 1 : 0.5), width: step - (def.strip ? 2 : 1), rx: def.strip ? 2 : step > 6 ? 1.5 : 0, ...box }))
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
                chart.append(svg('path', { d: path, fill: 'none', stroke: `var(--${color})`, 'stroke-width': 1.6, 'stroke-linejoin': 'round', 'stroke-dasharray': dashed ? '5 4' : 'none' }))
            }
            if (mode === 'line') def.fields.forEach((f, j) => line(f, def.colors[j], false))
            lines.forEach((f, j) => line(f, def.lineColors[j], false))
            right.forEach((f, j) => line(f, def.rightColors[j], true))
        }
        const last = points.length - 1
        if (counting && !def.strip) chart.append(svg('rect', { x: x(last) - step / 2, y: top, width: end - x(last) + step / 2, height: bottom - top + 1, fill: 'var(--panel)', 'fill-opacity': 0.6 }))
        for (const i of [0, Math.floor(points.length / 2), points.length - 1]) {
            const label = svg('text', { x: x(i), y: bottom + 22, 'text-anchor': i === 0 ? 'start' : i === points.length - 1 ? 'end' : 'middle', fill: 'var(--subtle)', 'font-size': 11 })
            const date = new Date(points[i].time * 1000)
            label.textContent = ['7d', '30d'].includes(view.range) ? date.toLocaleDateString(undefined, { month: 'short', day: 'numeric' }) : date.toLocaleTimeString(undefined, hours ? { hour: 'numeric' } : { hour: '2-digit', minute: '2-digit' })
            chart.append(label)
        }
        const tip = node('div', undefined, 'fibre-tip'); tip.hidden = true; tip.setAttribute('role', 'status')
        const show = i => {
            const p = points[i]
            const rows = [...def.fields.map((f, j) => [def.labels[j], f]), ...lines.map((f, j) => [def.lineLabels[j], f]), ...right.map((f, j) => [def.rightLabels[j], f]), ...(def.tooltip || []).map(f => [view.tooltipLabels[f] || f.replaceAll('_', ' '), f])]
                .map(([label, f]) => [label, f === 'go_memory_limit_bytes' && value(p, f) === null && value(p, 'go_memory_used_bytes') !== null ? 'no limit' : value(p, f) === null ? null : shown(f, value(p, f))])
                .filter(([, text]) => text !== null)
            tip.replaceChildren(node('strong', new Date(p.time * 1000).toLocaleString(undefined, { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' }) + (p.end ? `\u2013${new Date(p.end * 1000).toLocaleString(undefined, { ...(hours === 24 ? { month: 'short', day: 'numeric' } : {}), hour: '2-digit', minute: '2-digit' })}` : '')))
            if (def.strip) { rows.length = 0; tip.append(node('span', p.reachable === null ? 'No data' : `${Math.round(100 * p.reachable)}% reachable`)) }
            else if (!rows.length) tip.append(node('span', 'No observation'))
            if (counting && !def.strip && i === last) tip.append(node('span', 'Still counting'))
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
    function latencyCharts(def, points, width, wide = false) {
        const grid = node('div', undefined, 'fibre-latency-grid')
        const columns = wide && width >= 840 ? 3 : 2
        grid.style.gridTemplateColumns = `repeat(${columns}, minmax(0, 1fr))`
        for (let i = 0; i < def.fields.length; i += 2) {
            const title = def.labels[i].replace(' p50', '')
            const mini = { ...def, title, mini: true, fields: def.fields.slice(i, i + 2), labels: ['p50', 'p95'], colors: def.colors.slice(i, i + 2), tooltip: [def.tooltip[i / 2]] }
            const box = node('section', undefined, 'fibre-latency-mini')
            const p95 = data?.latency?.[mini.fields[1]]
            const heading = node('h3', undefined, 'fibre-latency-title'), dot = node('i')
            dot.style.background = `var(--${mini.colors[0]})`
            heading.append(dot, document.createTextNode(`${title}${p95 == null ? '' : ` \u00b7 p95 ${ms(p95)}`}`))
            box.append(heading, drawChart(mini, points, (width - 16 * (columns - 1)) / columns, 'line'))
            grid.append(box)
        }
        return grid
    }
    function legend(def, view = { range, selected, headline, tooltipLabels }) {
        const box = node('div', undefined, 'fibre-legend')
        const entry = (label, color, note) => {
            const item = node('span'); const dot = node('i'); dot.style.background = `var(--${color})`
            item.append(dot, document.createTextNode(label)); if (note) item.append(node('small', note)); box.append(item)
        }
        if (def.strip) {
            for (const [label, color] of [['Reachable', 'good'], ['Partly down', 'warning'], ['Down', 'critical'], ['No data', 'line']]) entry(label, color)
        } else if (def.unit === 'ms' && def.log) {
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
            for (const [text, tone] of headline(def, points)) { const stat = node('span', text); if (tone) stat.className = tone; stats.append(stat) }
            head.append(node('h2', def.title), stats)
            const notes = []
            if (def.id === 'endorsements') notes.push("From the chain: blobs paid for with or without this validator's signature.")
            if (def.id === 'uploads') notes.push("Failed includes payments Fibre refused (e.g. the payer's escrow was too low) and uploads under the validator's min_upload_size.")
            if (def.id === 'downloads') notes.push("Failed includes shards this validator doesn't hold.")
            if (def.strip) notes.push(`Each bar is ${reachIntervals[range][2]}`)
            panel.replaceChildren(head, legend(def), ...notes.map(text => node('p', text, 'fibre-note')))
            if (def.id === 'endorsements') {
                const list = node('div', undefined, 'fibre-missed'); list.append(node('h3', 'Latest missed'))
                const rows = missedRows(data.summary?.latest_missed)
                if (!rows.length) list.append(node('p', 'None seen yet', 'fibre-note'))
                for (const row of rows) {
                    const item = node('p', undefined, 'fibre-note'), commitment = node('code', row.commitment); commitment.title = row.full
                    item.append(node('span', `${row.height} · `), commitment, node('span', `${row.size} · ${row.signatures ? `${row.signatures} · ` : ''}${row.age}`, 'fibre-missed-meta')); list.append(item)
                }
                const split = node('div', undefined, 'fibre-endorsements')
                const chart = node('div'); split.append(chart, list); panel.append(split)
                const splitStyle = getComputedStyle(split)
                const chartWidth = splitStyle.gridTemplateColumns.split(' ').length > 1 ? width - list.getBoundingClientRect().width - parseFloat(splitStyle.columnGap) : width
                chart.replaceChildren(drawChart(def, points, chartWidth, preference.chart))
            } else panel.append(def.log ? latencyCharts(def, points, width, preference.wide) : drawChart(def, points, width, preference.chart))
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
    $('fibre-chain').onchange = event => { if (pushed !== null) { clearTimeout(pushed); pushed = null }; selected = event.target.value; chainPicker('fibre', watched, selected); data = null; refresh() }
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
