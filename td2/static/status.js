const chains = new Map()
const activity = []
const statusOrder = []
const statusTable = document.getElementById('status-table')
const chainDialog = document.getElementById('chain-dialog')
const statusNames = {
    4: 'proposed',
    3: 'signed',
    2: 'precommit',
    1: 'prevote',
    0: 'missed',
    '-1': 'unobserved'
}
let filter = 'all'
let socket
let reconnectDelay = 3000
let lastSnapshot = 0
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
    if (chain.last_block_at && Date.now() / 1000 - chain.last_block_at > 600) {
        return { label: 'No recent blocks', tone: 'amber', rank: 3 }
    }
    if (chain.healthy_nodes < chain.nodes) return { label: 'RPC degraded', tone: 'amber', rank: 3 }
    if (chain.window <= 0) return { label: 'Signing info unavailable', tone: 'unknown', rank: 3 }
    if (!chain.bonded) return { label: 'Inactive', tone: 'unknown', rank: 3 }
    return { label: 'Healthy', tone: 'good', rank: 4 }
}

function blockTrace(values, limit, label) {
    const trace = element('div', 'block-tape')
    const recent = (Array.isArray(values) ? values : []).slice(0, limit)
    const counts = { proposed: 0, signed: 0, precommit: 0, prevote: 0, missed: 0, unobserved: 0 }
    for (const value of recent) {
        const name = statusNames[value] || 'unobserved'
        counts[name]++
        trace.appendChild(element('i', name))
    }
    trace.setAttribute('role', 'img')
    trace.setAttribute('aria-label', `${label}: ${counts.signed} signed, ${counts.proposed} proposed, ${counts.missed} missed, ${counts.precommit} precommit seen, ${counts.prevote} prevote seen, ${counts.unobserved} without data`)
    return trace
}

function setConnection(connected) {
    const label = connected ? `Live · snapshot ${age(lastSnapshot)}` : `Disconnected · last snapshot ${age(lastSnapshot)}`
    document.getElementById('connection-state').textContent = label
    document.getElementById('connection-state').classList.toggle('offline', !connected)
    document.getElementById('monitor-state').textContent = connected ? 'Monitor online' : 'Monitor disconnected'
    const networks = new Set([...chains.values()].map(chain => chain.chain_id)).size
    document.getElementById('monitor-detail').textContent = `${chains.size} ${validatorLabel(chains.size)} · ${networks} ${networks === 1 ? 'chain' : 'chains'} · snapshot ${age(lastSnapshot)}`
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
    lastSnapshot = Date.now() / 1000
    setConnection(socket && socket.readyState === WebSocket.OPEN)
    render()
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

    const height = element('td')
    height.appendChild(element('span', 'mono', chain.height > 0 ? number(chain.height) : '—'))
    height.appendChild(element('small', 'cell-detail', chain.last_block_at ? `block ${age(chain.last_block_at)}` : 'not yet observed'))
    row.appendChild(height)

    const trace = element('td')
    trace.appendChild(blockTrace(chain.blocks, 48, `${chain.name} recent blocks`))
    row.appendChild(trace)

    const missed = element('td')
    missed.appendChild(element('span', 'mono', chain.window > 0 ? `${number(chain.missed)} / ${number(chain.window)}` : '—'))
    missed.appendChild(element('small', 'cell-detail', 'current signing window'))
    row.appendChild(missed)

    const rpc = element('td')
    rpc.appendChild(element('span', 'mono', `${chain.healthy_nodes} / ${chain.nodes}`))
    row.appendChild(rpc)
    return row
}

function render() {
    const focusedChain = document.activeElement && document.activeElement.dataset.chain
    const ordered = statusOrder.map(name => chains.get(name)).filter(Boolean)
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
        cell.colSpan = 6
        row.appendChild(cell)
        statusTable.appendChild(row)
    } else {
        statusTable.append(...visible.map(renderRow))
    }
    if (focusedChain) {
        const target = [...statusTable.querySelectorAll('[data-chain]')].find(node => node.dataset.chain === focusedChain)
        if (target) target.focus({ preventScroll: true })
    }
}

function showDetail(name) {
    const chain = chains.get(name)
    if (!chain) return
    document.getElementById('detail-heading').textContent = name
    document.getElementById('detail-subtitle').textContent = `${chain.chain_id || 'Unknown chain'} · ${chain.moniker || 'Unknown validator'}`
    const facts = document.getElementById('detail-facts')
    facts.replaceChildren()
    const rows = [
        ['Status', stateFor(chain).label],
        ['Height', chain.height > 0 ? number(chain.height) : 'Unavailable'],
        ['Last observed block', age(chain.last_block_at)],
        ['Missed / signing window', chain.window > 0 ? `${number(chain.missed)} / ${number(chain.window)}` : 'Unavailable'],
        ['Healthy RPC endpoints', `${chain.healthy_nodes} / ${chain.nodes}`],
        ['Active alerts', String(chain.active_alerts || 0)]
    ]
    for (const [label, value] of rows) {
        const item = element('div', 'detail-fact')
        item.appendChild(element('small', '', label))
        item.appendChild(element('strong', '', value))
        facts.appendChild(item)
    }
    const history = document.getElementById('detail-blocks')
    history.replaceChildren()
    history.appendChild(blockTrace(chain.blocks, 512, `${name} block history`))
    document.getElementById('detail-error').textContent = chain.last_error || ''
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
}, 30000)
loadInitial()
connect()
