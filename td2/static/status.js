const chains = new Map()
const activity = []
const statusOrder = []
const statusTable = document.getElementById('status-table')
const chainDialog = document.getElementById('chain-dialog')
const statusNames = {
    5: 'missed-proposal',
    4: 'proposed',
    3: 'signed',
    2: 'precommit',
    1: 'prevote',
    0: 'missed',
    '-1': 'unobserved'
}
let filter = 'all'
// drawn height per chain and when it moved; a redraw continues the new-block animation instead of restarting it
const drawnHeights = new Map()
const newBlockMs = 700

function newBlockAge(name) {
    const drawn = drawnHeights.get(name)
    const elapsed = drawn && drawn.at ? Date.now() - drawn.at : Infinity
    return elapsed < newBlockMs ? elapsed : null
}
let socket
let reconnectDelay = 3000
let logsEnabled = false

function element(tag, className, value) {
    const node = document.createElement(tag)
    if (className) node.className = className
    if (value !== undefined) node.textContent = value
    return node
}

function number(value) {
    return Number(value).toLocaleString('en-US')
}

function validatorLabel(count) {
    return count === 1 ? 'validator' : 'validators'
}

function age(timestamp) {
    if (!timestamp) return 'not yet observed'
    const seconds = Math.max(0, Math.floor(Date.now() / 1000 - timestamp))
    if (seconds < 60) return `${seconds}s ago`
    if (seconds < 3600) return `${Math.floor(seconds / 60)}m ago`
    if (seconds < 86400) return `${Math.floor(seconds / 3600)}h ago`
    return `${Math.floor(seconds / 86400)}d ago`
}

function stateFor(chain) {
    if (chain.tombstoned) return { label: 'Tombstoned', tone: 'red', rank: 0 }
    if (chain.jailed) return { label: 'Jailed', tone: 'red', rank: 0 }
    if (chain.height <= 0 || chain.no_nodes === true) {
        return { label: 'No data', tone: 'unknown', rank: 1 }
    }
    if (chain.active_alerts > 0) return { label: 'Needs attention', tone: 'red', rank: 2 }
    if (chain.monitoring === false) return { label: 'Monitoring interrupted', tone: 'amber', rank: 3 }
    if (chain.validator_stale) return { label: 'Validator info stale', tone: 'unknown', rank: 3 }
    if (chain.signing_stale) return { label: 'Signing info stale', tone: 'unknown', rank: 3 }
    if (chain.last_block_at && Date.now() / 1000 - chain.last_block_at > 600) {
        return { label: 'No recent blocks', tone: 'amber', rank: 3 }
    }
    if (chain.healthy_nodes < chain.nodes) return { label: 'RPC degraded', tone: 'amber', rank: 3 }
    if (chain.window <= 0) return { label: 'Signing info unavailable', tone: 'unknown', rank: 3 }
    if (!chain.bonded) return { label: 'Inactive', tone: 'unknown', rank: 3 }
    return { label: 'Healthy', tone: 'good', rank: 4 }
}

function jailBudget(chain) {
    if (!(chain.max_missed > 0)) return ''
    return `${number(Math.max(0, chain.max_missed - chain.missed))} left before jail`
}

function votingPower(chain) {
    if (!chain.voting_power) {
        return { value: '—', share: '—', rank: chain.active_set ? 'Not in active set' : 'Unavailable', detail: chain.active_set ? 'not in active set' : 'not available' }
    }
    const share = chain.total_power > 0 ? `${(100 * chain.voting_power / chain.total_power).toFixed(2)}%` : '—'
    const rank = chain.max_validators > chain.active_set ? `${chain.rank} / ${chain.active_set} (limit ${chain.max_validators})` : `${chain.rank} / ${chain.active_set}`
    return { value: number(chain.voting_power), share, rank, detail: `${share} · rank ${rank}` }
}

function blockTrace(values, limit, label, newAge) {
    const trace = element('div', 'block-tape')
    const recent = (Array.isArray(values) ? values : []).slice(0, limit)
    const counts = { 'missed-proposal': 0, proposed: 0, signed: 0, precommit: 0, prevote: 0, missed: 0, unobserved: 0 }
    for (const value of recent) {
        const name = statusNames[value] || 'unobserved'
        counts[name]++
        trace.appendChild(element('i', name))
    }
    if (newAge !== null && newAge !== undefined && trace.firstChild) {
        trace.firstChild.classList.add('new')
        trace.firstChild.style.animationDelay = `-${newAge}ms`
    }
    trace.setAttribute('role', 'img')
    trace.setAttribute('aria-label', `${label}: ${counts.signed} signed, ${counts.proposed} proposed, ${counts['missed-proposal']} missed proposals, ${counts.missed} missed, ${counts.precommit} precommit seen, ${counts.prevote} prevote seen, ${counts.unobserved} without data`)
    return trace
}

function setConnection(connected) {
    const label = connected ? 'Live' : 'Disconnected · data is stale'
    document.getElementById('connection-state').textContent = label
    document.getElementById('connection-state').classList.toggle('offline', !connected)
    document.getElementById('monitor-state').textContent = connected ? 'Monitor online' : 'Monitor disconnected'
    const networks = new Set([...chains.values()].map(chain => chain.chain_id)).size
    document.getElementById('monitor-detail').textContent = `${chains.size} ${validatorLabel(chains.size)} · ${networks} ${networks === 1 ? 'chain' : 'chains'}`
}

function setStatus(update) {
    if (!update || !Array.isArray(update.Status)) return
    for (const chain of update.Status) {
        if (!chain || !chain.name) continue
        chains.set(chain.name, chain)
    }
    if (statusOrder.length === 0) {
        statusOrder.push(...[...chains.values()]
            .sort((left, right) => stateFor(left).rank - stateFor(right).rank || left.name.localeCompare(right.name))
            .map(chain => chain.name))
    }
    for (const name of chains.keys()) {
        if (!statusOrder.includes(name)) statusOrder.push(name)
    }
    setConnection(socket && socket.readyState === WebSocket.OPEN)
    render()
    document.dispatchEvent(new CustomEvent("fibre-state", { detail: [...chains.values()] }))
}

function renderSummary() {
    const values = [...chains.values()]
    const attention = values.filter(chain => stateFor(chain).rank < 4)
    const unavailable = values.filter(chain => stateFor(chain).label === 'No data')
    const degraded = values.filter(chain => chain.nodes > 0 && chain.healthy_nodes < chain.nodes)
    document.getElementById('chain-count').textContent = values.length
    document.getElementById('attention-count').textContent = attention.length
    document.getElementById('rpc-count').textContent = degraded.length
    document.getElementById('chain-count-label').textContent = validatorLabel(values.length)
    document.getElementById('attention-count-label').textContent = validatorLabel(attention.length)
    document.getElementById('rpc-count-label').textContent = validatorLabel(degraded.length)
    document.getElementById('coverage-count').textContent = `${values.length - unavailable.length}/${values.length}`
    const priority = attention.sort((left, right) => stateFor(left).rank - stateFor(right).rank || left.name.localeCompare(right.name))[0]
    const section = document.getElementById('attention-section')
    section.hidden = !priority
    if (!priority) return
    document.getElementById('priority-count').textContent = `${attention.length} ${validatorLabel(attention.length)} ${attention.length === 1 ? 'needs' : 'need'} attention`
    document.getElementById('priority-chain').textContent = priority.name
    document.getElementById('priority-status').textContent = stateFor(priority).label
    document.getElementById('priority-status').className = `status ${stateFor(priority).tone}`
    document.getElementById('priority-detail').textContent = priority.last_error || `${priority.healthy_nodes} of ${priority.nodes} configured RPC endpoints responding`
    document.getElementById('priority-context').textContent = `${priority.healthy_nodes}/${priority.nodes} RPC endpoints healthy`
    document.getElementById('priority-context-detail').textContent = priority.last_block_at ? `Last block observed ${age(priority.last_block_at)}` : 'No block has been observed yet'
}

function renderRow(chain) {
    const state = stateFor(chain)
    const row = element('tr')

    const identity = element('td')
    const open = element('button', 'chain-button', chain.name)
    open.type = 'button'
    open.dataset.chain = chain.name
    open.addEventListener('click', () => showDetail(chain.name))
    identity.appendChild(open)
    identity.appendChild(element('small', 'cell-detail', `${chain.chain_id || 'Unknown chain'} · ${chain.moniker || 'Unknown validator'}`))
    row.appendChild(identity)

    const status = element('td')
    status.appendChild(element('span', `status ${state.tone}`, state.label))
    row.appendChild(status)

    const power = votingPower(chain)
    const vp = element('td')
    vp.appendChild(element('span', 'mono', power.value))
    vp.appendChild(element('small', 'cell-detail', power.detail))
    row.appendChild(vp)

    const height = element('td')
    height.appendChild(element('span', 'mono', chain.height > 0 ? number(chain.height) : '—'))
    height.appendChild(element('small', 'cell-detail', chain.last_block_at ? `block ${age(chain.last_block_at)}` : 'not yet observed'))
    row.appendChild(height)

    const trace = element('td')
    const tape = blockTrace(chain.blocks, 160, `${chain.name} recent blocks`, newBlockAge(chain.name))
    tape.classList.add('fill')
    trace.appendChild(tape)
    row.appendChild(trace)

    const missed = element('td')
    missed.appendChild(element('span', 'mono', chain.window > 0 ? `${number(chain.missed)} / ${number(chain.window)}` : '—'))
    missed.appendChild(element('small', 'cell-detail', chain.signing_stale ? 'last known signing window' : (jailBudget(chain) || 'current signing window')))
    if (chain.missed_proposals > 0) {
        missed.appendChild(element('small', 'cell-detail missed-proposals', `${number(chain.missed_proposals)} missed ${chain.missed_proposals === 1 ? 'proposal' : 'proposals'}`))
    }
    row.appendChild(missed)

    const rpc = element('td')
    rpc.appendChild(element('span', 'mono', `${chain.healthy_nodes} / ${chain.nodes}`))
    row.appendChild(rpc)
    return row
}

function render() {
    const focusedChain = document.activeElement && document.activeElement.dataset.chain
    const ordered = statusOrder.map(name => chains.get(name)).filter(Boolean)
    for (const chain of ordered) {
        const drawn = drawnHeights.get(chain.name)
        if (!drawn) drawnHeights.set(chain.name, { height: chain.height, at: 0 })
        else if (chain.height > drawn.height) drawnHeights.set(chain.name, { height: chain.height, at: Date.now() })
    }
    renderSummary()
    statusTable.replaceChildren()
    const visible = ordered.filter(chain => {
        if (filter === 'attention') return stateFor(chain).rank < 4
        if (filter === 'unavailable') return stateFor(chain).label === 'No data'
        return true
    })
    if (visible.length === 0) {
        const row = element('tr')
        const cell = element('td', 'empty', chains.size ? 'No validators match this view.' : 'Waiting for chain data…')
        cell.colSpan = 7
        row.appendChild(cell)
        statusTable.appendChild(row)
    } else {
        statusTable.append(...visible.map(renderRow))
    }
    if (chainDialog.open && chainDialog.dataset.chain) fillDetail(chainDialog.dataset.chain)
    if (focusedChain) {
        const target = [...statusTable.querySelectorAll('[data-chain]')].find(node => node.dataset.chain === focusedChain)
        if (target) target.focus({ preventScroll: true })
    }
}

function bytesText(value) {
    const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB']
    let size = Number(value)
    let unit = 0
    while (size >= 1024 && unit < units.length - 1) {
        size /= 1024
        unit++
    }
    return `${Number(size.toFixed(1))} ${units[unit]}`
}

function durationText(seconds) {
    const total = Math.max(0, Math.floor(seconds))
    if (total < 60) return `${total}s`
    if (total < 3600) return `${Math.floor(total / 60)}m`
    if (total < 86400) return `${Math.floor(total / 3600)}h ${Math.floor(total % 3600 / 60)}m`
    return `${Math.floor(total / 86400)}d ${Math.floor(total % 86400 / 3600)}h`
}

function msText(value) {
    if (value === null || value === undefined) return '—'
    return `${value < 10 ? value.toFixed(1) : Math.round(value)} ms`
}

// what Fibre did in a window: a label and the text for each fact
const fibreWindowFacts = [
    ['Uploads ok / failed / rejected', w => `${number(w.uploads_ok)} / ${number(w.uploads_failed)} / ${number(w.uploads_rejected)}`],
    ['Data received', w => bytesText(w.bytes_received)],
    ['Average upload time', w => msText(w.avg_upload_ms)],
    ['Downloads ok / failed', w => `${number(w.downloads_ok)} / ${number(w.downloads_failed)}`],
    ['Data served', w => bytesText(w.bytes_served)],
    ['Average download time', w => msText(w.avg_download_ms)],
    ['Signatures / failed', w => `${number(w.signatures)} / ${number(w.signatures_failed)}`],
    ['Signed within 10 ms', w => w.sign_fast_percent === null || w.sign_fast_percent === undefined ? '—' : `${Math.floor(w.sign_fast_percent)}%`],
    ['Average signing time', w => msText(w.avg_sign_ms)],
    ['Store put / get errors', w => `${number(w.store_put_errors)} / ${number(w.store_get_errors)}`],
    ['Average store put / get', w => `${msText(w.avg_put_ms)} / ${msText(w.avg_get_ms)}`],
    ['Prune errors / entries pruned', w => `${number(w.prune_errors)} / ${number(w.entries_pruned)}`],
    ['Average prune time', w => msText(w.avg_prune_ms)],
    ['Object storage errors', w => `${number(w.object_storage_errors)} of ${number(w.object_storage_requests)} requests`]
]

function fibreProviderFacts(chain) {
    const fibre = chain.fibre
    const rows = [['Provider', fibre.registered ? fibre.host : 'Not registered']]
    if (fibre.registered) {
        let reach = 'Reachable'
        if (!fibre.reachable) {
            reach = 'Unreachable'
            if (fibre.down_since) reach += ` for ${durationText(Date.now() / 1000 - fibre.down_since)}`
            if (fibre.down_reason) reach += ` · ${fibre.down_reason}`
        }
        rows.push(['Reachability', reach])
        const identity = { verified: 'Verified', failed: `Not verified${fibre.identity_error ? ` · ${fibre.identity_error}` : ''}` }
        rows.push(['Identity', identity[fibre.identity] || 'Not checked'])
    }
    if (fibre.version) rows.push(['Fibre version', fibre.version])
    let share = 'Unavailable'
    if (chain.total_power > 0) {
        share = 'Not in the active set, no rows'
        if (fibre.in_active_set) {
            const note = fibre.minimum_applies ? ' · minimum' : (fibre.rows === fibre.total_rows ? ' · maximum' : '')
            share = `${number(fibre.rows)} / ${number(fibre.total_rows)} rows · ${fibre.row_percent.toFixed(2)}%${note}`
        }
    }
    rows.push(['Share of rows', share])
    let budget = fibre.in_active_set ? 'Unavailable' : 'None'
    if (fibre.unlimited) {
        budget = 'Unlimited (--unlimited-budget)'
    } else if (fibre.chain_budget > 0) {
        budget = bytesText(fibre.chain_budget)
        if (fibre.budget_reported && fibre.fibre_budget !== fibre.chain_budget) budget += ` · Fibre reports ${bytesText(fibre.fibre_budget)}`
    } else if (fibre.budget_reported) {
        budget = `Fibre reports ${bytesText(fibre.fibre_budget)}`
    }
    rows.push(['Storage budget', budget])
    return rows
}

function appendFact(container, label, values) {
    const item = element('div', 'detail-fact')
    item.appendChild(element('small', '', label))
    for (const value of Array.isArray(values) ? values : [values]) {
        item.appendChild(element('strong', Array.isArray(values) ? 'pair' : '', value))
    }
    container.appendChild(item)
}

function fillFibre(chain) {
    const section = document.getElementById('detail-fibre')
    section.hidden = !chain.fibre
    if (!chain.fibre) return
    const fibre = chain.fibre
    const facts = document.getElementById('detail-fibre-facts')
    const note = document.getElementById('detail-fibre-note')
    facts.replaceChildren()
    for (const [label, value] of fibreProviderFacts(chain)) appendFact(facts, label, value)
    if (!fibre.metrics_received) {
        appendFact(facts, 'Fibre metrics', 'Not received, see docs/fibre.md')
        note.textContent = 'Share and budget come from the chain. Fibre reports what it did and how full its storage is once it sends metrics to tenderduty.'
        return
    }
    if (fibre.occupancy_known) {
        const used = fibre.budget_reported && fibre.fibre_budget > 0 ? ` · ${(100 * fibre.occupancy / fibre.fibre_budget).toFixed(2)}% of the budget` : ''
        appendFact(facts, 'Storage used', `${bytesText(fibre.occupancy)}${used}`)
    }
    appendFact(facts, 'Metrics received', age(fibre.metrics_at))
    appendFact(facts, 'Last successful upload', fibre.last_upload_at ? age(fibre.last_upload_at) : '—')
    const noun = (count, one) => `${number(count)} ${count === 1 ? one : `${one}s`}`
    appendFact(facts, 'In flight now', `${noun(fibre.uploads_in_flight, 'upload')} · ${noun(fibre.downloads_in_flight, 'download')}`)
    const short = fibre.last_24h.seconds < 86400
    for (const [label, text] of fibreWindowFacts) {
        appendFact(facts, label, [`10 min · ${text(fibre.last_10m)}`, `24 h · ${text(fibre.last_24h)}`])
    }
    note.textContent = short
        ? `The 24 h figures cover ${durationText(fibre.last_24h.seconds)} so far, since tenderduty started: Fibre's history is kept in memory.`
        : 'Figures are what Fibre reported to tenderduty.'
}

// render() redraws it while the dialog is open
function fillDetail(name) {
    const chain = chains.get(name)
    if (!chain) return false
    document.getElementById('detail-heading').textContent = name
    document.getElementById('detail-subtitle').textContent = `${chain.chain_id || 'Unknown chain'} · ${chain.moniker || 'Unknown validator'}`
    const facts = document.getElementById('detail-facts')
    facts.replaceChildren()
    const rows = [
        ['Status', stateFor(chain).label],
        ['Height', chain.height > 0 ? number(chain.height) : 'Unavailable'],
        ['Last observed block', age(chain.last_block_at)],
        [chain.signing_stale ? 'Missed / signing window (last known)' : 'Missed / signing window', chain.window > 0 ? `${number(chain.missed)} / ${number(chain.window)}${jailBudget(chain) ? ` · ${jailBudget(chain)}` : ''}` : 'Unavailable'],
        ['Voting power', votingPower(chain).value],
        ['Share of voting power', votingPower(chain).share],
        ['Rank in active set', votingPower(chain).rank],
        ['Healthy RPC endpoints', `${chain.healthy_nodes} / ${chain.nodes}`],
        ['Active alerts', String(chain.active_alerts || 0)],
        ['Signed since start', number(chain.signed || 0)],
        ['Proposed since start', number(chain.proposed || 0)],
        ['Missed proposals since start', number(chain.missed_proposals || 0)]
    ]
    for (const [label, value] of rows) {
        const item = element('div', 'detail-fact')
        item.appendChild(element('small', '', label))
        item.appendChild(element('strong', '', value))
        facts.appendChild(item)
    }
    fillFibre(chain)
    const history = document.getElementById('detail-blocks')
    history.replaceChildren()
    history.appendChild(blockTrace(chain.blocks, 512, `${name} block history`, newBlockAge(name)))
    document.getElementById('detail-error').textContent = chain.last_error || ''
    return true
}

function showDetail(name) {
    if (!fillDetail(name)) return
    chainDialog.dataset.chain = name
    chainDialog.showModal()
}

function addActivity(entry) {
    if (!entry || !entry.msg) return
    if (activity.some(item => item.ts === entry.ts && item.msg === entry.msg)) return
    activity.unshift(entry)
    if (activity.length > 20) activity.pop()
    const list = document.getElementById('activity-list')
    list.replaceChildren()
    for (const item of activity.slice(0, 8)) {
        const row = element('li', 'activity-item')
        row.appendChild(element('time', 'mono', item.ts ? new Date(item.ts * 1000).toLocaleTimeString('en-US', { hour: '2-digit', minute: '2-digit' }) : '—'))
        row.appendChild(element('span', '', item.msg))
        list.appendChild(row)
    }
}

async function loadInitial() {
    try {
        const [settings, snapshot] = await Promise.all([fetch('logsenabled'), fetch('state')])
        if (!settings.ok || !snapshot.ok) throw new Error('Could not load monitor state')
        logsEnabled = (await settings.json()).enabled === true
        document.getElementById('log-section').hidden = !logsEnabled
        document.getElementById('activity-nav').hidden = !logsEnabled
        setStatus(await snapshot.json())
        if (logsEnabled) {
            const response = await fetch('logs')
            if (response.ok) {
                const recent = await response.json()
                if (Array.isArray(recent)) recent.reverse().forEach(addActivity)
            }
        }
    } catch (error) {
        document.getElementById('monitor-detail').textContent = error.message
    }
}

function connect() {
    const protocol = location.protocol === 'https:' ? 'wss:' : 'ws:'
    socket = new WebSocket(`${protocol}//${location.host}/ws`)
    socket.addEventListener('open', () => {
        reconnectDelay = 3000
        setConnection(true)
        loadInitial()
    })
    socket.addEventListener('message', event => {
        try {
            const message = JSON.parse(event.data)
            if (message.msgType === 'update') setStatus(message)
            if (message.msgType === 'log' && logsEnabled) addActivity(message)
        } catch (error) {
            console.error('Could not read monitor update', error)
        }
    })
    socket.addEventListener('close', () => {
        setConnection(false)
        setTimeout(connect, reconnectDelay)
        reconnectDelay = Math.min(reconnectDelay * 2, 30000)
    })
    socket.addEventListener('error', () => socket.close())
}

for (const button of document.querySelectorAll('[data-filter]')) {
    button.addEventListener('click', () => {
        filter = button.dataset.filter
        for (const option of document.querySelectorAll('[data-filter]')) {
            const selected = option === button
            option.classList.toggle('current', selected)
            option.setAttribute('aria-pressed', String(selected))
        }
        render()
    })
}
document.getElementById('detail-close').addEventListener('click', () => chainDialog.close())
document.getElementById('sort-urgency').addEventListener('click', () => {
    statusOrder.splice(0, statusOrder.length, ...[...chains.values()]
        .sort((left, right) => stateFor(left).rank - stateFor(right).rank || left.name.localeCompare(right.name))
        .map(chain => chain.name))
    render()
})
document.addEventListener('visibilitychange', () => {
    if (!document.hidden) loadInitial()
})
setInterval(() => {
    setConnection(socket && socket.readyState === WebSocket.OPEN)
    render()
    document.dispatchEvent(new CustomEvent("fibre-state", { detail: [...chains.values()] }))
}, 30000)
loadInitial()
connect()
