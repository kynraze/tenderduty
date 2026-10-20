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
let networkFilter = 'all'
let search = ''
let attentionExpanded = false
let governanceExpanded = false
// retain drawn heights so redraws continue the new-block animation
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
    const urgent = urgentAlerts(chain)
    if (chain.height <= 0 || chain.no_nodes === true) {
        if (urgent > 0) return { label: 'Needs attention', tone: 'red', rank: 1 }
        return { label: 'No data', tone: 'unknown', rank: 1 }
    }
    if (urgent > 0) return { label: 'Needs attention', tone: 'red', rank: 2 }
    if (chain.notice_alerts > 0) return { label: chain.notice_label || 'Notice', tone: 'blue', rank: 3 }
    if (chain.monitoring === false) return { label: 'Monitoring interrupted', tone: 'amber', rank: 3 }
    if (chain.validator_stale) return { label: 'Validator info stale', tone: 'unknown', rank: 3 }
    if (chain.signing_stale) return { label: 'Signing info stale', tone: 'unknown', rank: 3 }
    if (chain.last_block_at && Date.now() / 1000 - chain.last_block_at > 600) {
        return { label: 'No recent blocks', tone: 'amber', rank: 3 }
    }
    if (chain.window <= 0) return { label: 'Signing info unavailable', tone: 'unknown', rank: 3 }
    if (!chain.bonded) return { label: 'Inactive', tone: 'unknown', rank: 3 }
    return { label: 'Healthy', tone: 'good', rank: 4 }
}

// only once the validator set was read; a chain with no data is not bonded either
function outOfSet(chain) {
    return chain.active_set > 0 && chain.height > 0 && chain.no_nodes !== true && !chain.bonded && !chain.jailed && !chain.tombstoned
}

// full text, or only the kind with hide_logs
function inactiveAlert(alert) {
    return / is no longer in the active set on /.test(alert.text) || alert.text === 'Not in the active set'
}

// out of the active set still alerts, but the dashboard shows it as Inactive
function urgentAlerts(chain) {
    let count = (chain.active_alerts || 0) - (chain.notice_alerts || 0)
    if (outOfSet(chain)) count -= Array.isArray(chain.alerts) ? chain.alerts.filter(inactiveAlert).length : 1
    return Math.max(0, count)
}

function shownAlerts(chain) {
    const alerts = Array.isArray(chain.alerts) ? chain.alerts : []
    return outOfSet(chain) ? alerts.filter(a => !inactiveAlert(a)) : alerts
}

function jailBudget(chain) {
    if (!(chain.max_missed > 0)) return ''
    return `${number(Math.max(0, chain.max_missed - chain.missed))} left before jail`
}

function votingPower(chain) {
    if (!chain.voting_power && !chain.stake) {
        return { value: '—', share: '—', rank: chain.active_set ? 'Not in active set' : 'Unavailable', detail: chain.active_set ? 'not in active set' : 'not available' }
    }
    const share = chain.total_power > 0 ? `${(100 * chain.voting_power / chain.total_power).toFixed(2)}%` : '—'
    const rank = chain.max_validators > chain.active_set ? `${chain.rank} / ${chain.active_set} (limit ${chain.max_validators})` : `${chain.rank} / ${chain.active_set}`
    return { value: chain.stake || number(chain.voting_power), share, rank, detail: `${share} · rank ${rank}` }
}

// counts from the block history, which survives a restart
function blockSummary(values) {
    const c = { 'missed-proposal': 0, proposed: 0, signed: 0, precommit: 0, prevote: 0, missed: 0, unobserved: 0 }
    for (const value of (Array.isArray(values) ? values : []).slice(0, 512)) c[statusNames[value] || 'unobserved']++
    const total = Object.values(c).reduce((a, b) => a + b, 0) - c.unobserved
    const missed = c.missed + c.precommit + c.prevote
    let text = `${number(c.signed + c.proposed + c['missed-proposal'])} signed · ${number(c.proposed)} proposed · ${number(missed)} missed`
    if (c['missed-proposal'] > 0) text += ` · ${number(c['missed-proposal'])} missed proposals`
    return { total, text }
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
    trace.setAttribute('aria-label', `${label}: ${counts.signed} signed, ${counts.proposed} proposed, ${counts['missed-proposal']} missed proposals, ${counts.missed} missed, ${counts.precommit} late precommits, ${counts.prevote} prevote only, ${counts.unobserved} without data`)
    return trace
}

function setConnection(connected) {
    const monitor = document.getElementById('monitor-state')
    monitor.textContent = connected ? 'Monitor live' : 'Monitor disconnected'
    monitor.classList.toggle('offline', !connected)
    const values = [...chains.values()]
    const networks = new Map(values.map(chain => [chain.chain_id, Boolean(chain.testnet)]))
    document.getElementById('monitor-detail').textContent = `${chains.size} ${validatorLabel(chains.size)} · ${networks.size} ${networks.size === 1 ? 'chain' : 'chains'}`
    const testnets = [...networks.values()].filter(Boolean).length
    const detail = document.getElementById('monitor-networks')
    detail.hidden = testnets === 0
    detail.textContent = `${networks.size - testnets} mainnet · ${testnets} testnet`
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

function summaryValues(values) {
    let healthy = 0, total = 0
    let closest = null
    for (const chain of values) {
        healthy += chain.healthy_nodes || 0
        total += chain.nodes || 0
        // inactive validators don't sign, so can't be jailed for downtime
        if (chain.bonded && chain.max_missed > 0 && chain.window > 0 && (!closest || chain.max_missed - chain.missed < closest.max_missed - closest.missed)) closest = chain
    }
    return { healthy, total, closest }
}

function avatar(name) {
    const initials = () => element('span', 'avatar', name.slice(0, 2).toUpperCase())
    if (!chains.get(name)?.logo) return initials()
    const img = element('img', 'avatar logo')
    img.alt = ''
    img.loading = 'lazy'
    img.decoding = 'async'
    img.addEventListener('error', () => img.replaceWith(initials()), { once: true })
    img.src = `logo?chain=${encodeURIComponent(name)}`
    return img
}

function cardRow(name, text, title, open) {
    const row = element('button', 'card-row')
    row.type = 'button'
    row.addEventListener('click', () => open ? open(row) : showDetail(name))
    row.appendChild(avatar(name))
    const body = element('span', 'card-row-body')
    body.appendChild(element('strong', '', name))
    const subtitle = element('span', 'card-row-subtitle', text)
    subtitle.title = title
    body.appendChild(subtitle)
    row.appendChild(body)
    return row
}

function renderMore(id, count, expanded, toggle) {
    const button = document.getElementById(id)
    button.hidden = count <= 3
    button.textContent = expanded ? 'Show less' : `Show ${count - 3} more`
    button.setAttribute('aria-expanded', String(expanded))
    button.onclick = toggle
}

function renderSummary() {
    const values = [...chains.values()]
    const summary = summaryValues(values)
    document.getElementById('rpc-value').textContent = `${number(summary.healthy)} / ${number(summary.total)}`
    document.getElementById('rpc-up-bar').style.width = `${summary.total ? 100 * summary.healthy / summary.total : 0}%`
    document.getElementById('rpc-down-bar').style.width = `${summary.total ? 100 * (summary.total - summary.healthy) / summary.total : 0}%`
    const closest = summary.closest
    document.getElementById('jail-value').textContent = closest ? `${number(Math.max(0, closest.max_missed - closest.missed))} blocks left` : '—'
    document.getElementById('jail-foot').textContent = closest ? `${closest.name} · ${number(closest.missed)} / ${number(closest.max_missed)} missed` : 'No signing window available'
    const attention = values.filter(chain => stateFor(chain).tone === 'red')
        .sort((left, right) => stateFor(left).rank - stateFor(right).rank || left.name.localeCompare(right.name))
    const badge = document.getElementById('attention-count')
    badge.textContent = attention.length
    badge.hidden = attention.length === 0
    const counts = { all: values.length, attention: attention.length, notice: values.filter(chain => stateFor(chain).tone === 'blue').length, unavailable: values.filter(chain => chain.height <= 0 || chain.no_nodes === true).length }
    for (const [key, count] of Object.entries(counts)) document.getElementById(`filter-count-${key}`).textContent = count
    const testnets = values.filter(chain => chain.testnet).length
    document.getElementById('network-filters').hidden = testnets === 0
    if (testnets === 0 && networkFilter !== 'all') {
        networkFilter = 'all'
        for (const button of document.querySelectorAll('[data-network]')) {
            const selected = button.dataset.network === 'all'
            button.classList.toggle('current', selected)
            button.setAttribute('aria-pressed', String(selected))
        }
    }
    document.getElementById('network-count-mainnet').textContent = values.length - testnets
    document.getElementById('network-count-testnet').textContent = testnets
    document.getElementById('attention-section').hidden = attention.length === 0
    document.getElementById('priority-count').textContent = `${attention.length} ${validatorLabel(attention.length)}`
    const list = document.getElementById('priority-list')
    list.replaceChildren()
    for (const chain of attention.slice(0, attentionExpanded ? attention.length : 3)) {
        const alerts = shownAlerts(chain)
        const first = alerts[0]
        // older servers send only the count
        const count = urgentAlerts(chain)
        const text = first ? first.text : count > 0 ? `${count} active alert${count === 1 ? '' : 's'}` : stateFor(chain).label
        const row = cardRow(chain.name, text, alerts.map(alert => alert.text).join('\n') || text)
        const severity = chain.tombstoned ? 'tombstoned' : chain.jailed ? 'jailed' : first && first.severity || ''
        const side = element('span', 'card-row-side')
        side.appendChild(element('strong', severity === 'warning' ? 'severity warning' : severity === 'info' ? 'severity info' : 'severity critical', severity))
        if (first && first.since) side.appendChild(element('span', '', durationText(Date.now() / 1000 - first.since)))
        row.appendChild(side)
        list.appendChild(row)
    }
    renderMore('attention-more', attention.length, attentionExpanded, () => { attentionExpanded = !attentionExpanded; renderSummary() })
    updateAttentionGrid()
}

function updateAttentionGrid() {
    const attention = document.getElementById('attention-section').hidden
    const governance = document.getElementById('governance-section').hidden
    const grid = document.getElementById('attention-grid')
    grid.hidden = attention && governance
    grid.classList.toggle('single', attention || governance)
}

function renderGovernance() {
    const rows = []
    for (const chain of chains.values()) {
        for (const proposal of chain.proposals || []) rows.push({ ...proposal, name: chain.name })
    }
    rows.sort((left, right) => Number(left.voted) - Number(right.voted) || left.end - right.end || left.name.localeCompare(right.name) || left.id - right.id)
    document.getElementById('governance-section').hidden = rows.length === 0
    document.getElementById('governance-count').textContent = `${rows.length} open · ${rows.filter(row => !row.voted).length} not voted`
    const list = document.getElementById('governance-list')
    list.replaceChildren()
    const now = Date.now() / 1000
    for (const proposal of rows.slice(0, governanceExpanded ? rows.length : 3)) {
        const row = cardRow(proposal.name, `#${proposal.id} ${proposal.title}`, proposal.title, row => showProposal(proposal.name, proposal.id, row))
        row.dataset.proposalChain = proposal.name
        row.dataset.proposalId = String(proposal.id)
        const side = element('span', 'card-row-side')
        const left = proposal.end - now
        const tone = proposal.voted ? 'good' : left < 86400 ? 'red' : 'amber'
        side.appendChild(element('span', `status governance-pill ${tone}`, proposal.voted ? 'Voted' : 'Not voted'))
        side.appendChild(element('span', 'governance-end', left <= 0 ? 'ended' : `${durationText(left)} left`))
        row.appendChild(side)
        list.appendChild(row)
    }
    renderMore('governance-more', rows.length, governanceExpanded, () => { governanceExpanded = !governanceExpanded; renderGovernance() })
    updateAttentionGrid()
}

function renderRow(chain) {
    const state = stateFor(chain)
    const row = element('tr')

    const identity = element('td')
    const open = element('button', 'chain-button', chain.name)
    open.type = 'button'
    open.dataset.chain = chain.name
    open.title = chain.name
    open.addEventListener('click', () => showDetail(chain.name))
    const label = element('div', 'chain-label')
    const heading = element('div', 'chain-name')
    heading.appendChild(open)
    if (chain.testnet) heading.appendChild(element('span', 'network-tag', 'TESTNET'))
    label.appendChild(heading)
    // one operator's fleet repeats the same moniker, so show it only when they differ
    const id = chain.chain_id || 'Unknown chain', moniker = chain.moniker || 'Unknown validator'
    const monikers = new Set([...chains.values()].map(c => c.moniker).filter(Boolean))
    const detail = element('small', 'cell-detail', monikers.size > 1 ? `${id} · ${moniker}` : id)
    detail.title = `${id} · ${moniker}`
    label.appendChild(detail)
    const identityBody = element('div', 'chain-identity')
    identityBody.appendChild(avatar(chain.name))
    identityBody.appendChild(label)
    identity.appendChild(identityBody)
    row.appendChild(identity)

    const status = element('td')
    status.appendChild(element('span', `status ${state.tone}`, state.label))
    status.lastChild.title = state.label
    row.appendChild(status)

    const power = votingPower(chain)
    const vp = element('td')
    vp.appendChild(element('span', 'mono', power.value))
    vp.appendChild(element('small', 'cell-detail', (chain.voting_power || chain.stake) ? `${power.share} · #${chain.rank} of ${chain.active_set}` : power.detail))
    row.appendChild(vp)

    const height = element('td')
    height.appendChild(element('span', 'mono', chain.height > 0 ? number(chain.height) : '—'))
    height.appendChild(element('small', 'cell-detail', chain.last_block_at ? age(chain.last_block_at) : 'not yet observed'))
    row.appendChild(height)

    const trace = element('td')
    const tape = blockTrace(chain.blocks, 160, `${chain.name} recent blocks`, newBlockAge(chain.name))
    tape.classList.add('fill')
    trace.appendChild(tape)
    row.appendChild(trace)

    const missed = element('td')
    missed.appendChild(element('span', 'mono', chain.window > 0 ? `${number(chain.missed)} / ${number(chain.window)}` : '—'))
    missed.appendChild(element('small', 'cell-detail', chain.signing_stale ? 'last known signing window' : (jailBudget(chain).replace(' before jail', '') || 'current signing window')))
    if (chain.missed_proposals > 0) {
        missed.appendChild(element('small', 'cell-detail missed-proposals', `${number(chain.missed_proposals)} missed ${chain.missed_proposals === 1 ? 'proposal' : 'proposals'}`))
    }
    row.appendChild(missed)

    const rpc = element('td')
    // a down endpoint without alert_if_down marks only this cell
    const degraded = chain.healthy_nodes < chain.nodes
    const count = element('span', degraded ? 'mono rpc-degraded' : 'mono', `${chain.healthy_nodes}/${chain.nodes}`)
    if (degraded) count.title = `${chain.nodes - chain.healthy_nodes} of ${chain.nodes} RPC endpoints down`
    rpc.appendChild(count)
    const dots = element('span', 'rpc-dots')
    const endpoints = Array.isArray(chain.rpc_nodes) && chain.rpc_nodes.length ? chain.rpc_nodes : Array.from({ length: chain.nodes || 0 }, (_, index) => ({ down: index >= chain.healthy_nodes }))
    for (const node of endpoints.slice(0, 5)) {
        const dot = element('i', node.down ? 'rpc-dot down' : 'rpc-dot')
        dot.title = `${node.endpoint ? node.endpoint + ': ' : ''}endpoint ${node.down ? 'down' : 'up'}`
        dots.appendChild(dot)
    }
    rpc.appendChild(dots)
    row.appendChild(rpc)
    return row
}

function matchesFilters(chain) {
    if (filter === 'attention' && stateFor(chain).tone !== 'red') return false
    if (filter === 'notice' && stateFor(chain).tone !== 'blue') return false
    if (filter === 'unavailable' && !(chain.height <= 0 || chain.no_nodes === true)) return false
    if (networkFilter === 'testnet' && !chain.testnet) return false
    if (networkFilter === 'mainnet' && chain.testnet) return false
    return [chain.name, chain.chain_id, chain.moniker].some(value => String(value || '').toLowerCase().includes(search.toLowerCase()))
}

function renderDate() {
    const date = new Date()
    document.getElementById('overview-date').textContent = `${date.toLocaleDateString('en-GB', { weekday: 'long' })} ${date.getDate()} ${date.toLocaleDateString('en-GB', { month: 'short' })} · ${date.toLocaleTimeString('en-GB', { hour: '2-digit', minute: '2-digit' })}`
}

function render() {
    const focusedChain = document.activeElement && document.activeElement.dataset.chain
    const ordered = statusOrder.map(name => chains.get(name)).filter(Boolean)
    for (const chain of ordered) {
        const drawn = drawnHeights.get(chain.name)
        if (!drawn) drawnHeights.set(chain.name, { height: chain.height, at: 0 })
        else if (chain.height > drawn.height) drawnHeights.set(chain.name, { height: chain.height, at: Date.now() })
    }
    renderDate()
    renderSummary()
    renderGovernance()
    statusTable.replaceChildren()
    const visible = ordered.filter(matchesFilters)
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

function fibreProviderFacts(chain) {
    const fibre = chain.fibre
    if (fibre.pending) return [['Provider', 'Checking…']]
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
    let budget = `${fibre.occupancy_known ? bytesText(fibre.occupancy) : 'Unknown'} of ${fibre.chain_budget > 0 ? bytesText(fibre.chain_budget) : 'Unknown'} budget`
    if (fibre.budget_reported) {
        if (fibre.unlimited) budget += ' · Fibre unlimited'
        else if (fibre.fibre_budget !== fibre.chain_budget) budget += ` · Fibre reports ${bytesText(fibre.fibre_budget)}`
    }
    if (fibre.storage_backend) budget += ` · ${fibre.storage_backend}`
    rows.push(['Shards', budget])
    const endorsed = fibre.last_24h?.endorsed || 0, missed = fibre.last_24h?.missed || 0
    rows.push(['Endorsed (24h)', endorsed + missed ? `${number(endorsed)} of ${number(endorsed + missed)} · ${(100 * endorsed / (endorsed + missed)).toFixed(1)}%` : 'none seen yet'])
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

function fillBridge(chain) {
    const section = document.getElementById('detail-bridge')
    section.hidden = !chain.bridge
    if (!chain.bridge) return
    const bridge = chain.bridge
    document.getElementById('detail-bridge-title').textContent = bridge.title || 'Bridge'
    const facts = document.getElementById('detail-bridge-facts')
    facts.replaceChildren()
    for (const fact of bridge.facts || [{ label: 'Status', value: bridge.text, tone: bridge.tone }]) {
        appendFact(facts, fact.label, fact.value)
        if (fact.tone === 'critical' || fact.tone === 'warning') {
            facts.lastChild.lastChild.style.color = `var(--${fact.tone})`
        }
    }
}

function fillFibre(chain) {
    const section = document.getElementById('detail-fibre')
    section.hidden = !chain.fibre
    if (!chain.fibre) return
    const fibre = chain.fibre
    const facts = document.getElementById('detail-fibre-facts')
    const note = document.getElementById('detail-fibre-note')
    facts.replaceChildren()
    for (const [label, value] of fibreProviderFacts(chain)) {
        appendFact(facts, label, value)
        if (label === 'Endorsed (24h)' && fibre.endorsing?.stopped) facts.lastChild.lastChild.style.color = 'var(--critical)'
    }
    note.replaceChildren()
    if (!fibre.metrics_received) {
        appendFact(facts, 'Fibre metrics', 'Not received, see docs/fibre.md')
        note.textContent = 'Share and budget come from the chain. Fibre reports what it did and how full its storage is once it sends metrics to tenderduty.'
        note.appendChild(element('br'))
    } else {
        appendFact(facts, 'Last successful upload', fibre.last_upload_at ? age(fibre.last_upload_at) : '—')
        appendFact(facts, 'Metrics received', age(fibre.metrics_at))
    }
    const link = element('a', '', 'More in the Fibre tab →')
    link.href = '#fibre'
    link.onclick = () => {
        const select = document.getElementById('fibre-chain')
        select.value = chain.name
        select.onchange({ target: select })
        chainDialog.close()
    }
    note.appendChild(link)
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
        [`Last ${number(blockSummary(chain.blocks).total)} blocks`, blockSummary(chain.blocks).text]
    ]
    for (const [label, value] of rows) {
        const item = element('div', 'detail-fact')
        item.appendChild(element('small', '', label))
        item.appendChild(element('strong', '', value))
        facts.appendChild(item)
    }
    for (const node of chain.rpc_nodes || []) {
        appendFact(facts, 'RPC node', `${node.endpoint}${node.version ? ` · ${node.version}` : ''} · ${node.down ? 'down' : 'healthy'}`)
    }
    fillBridge(chain)
    fillFibre(chain)
    const history = document.getElementById('detail-blocks')
    history.replaceChildren()
    history.appendChild(blockTrace(chain.blocks, 512, `${name} block history`, newBlockAge(name)))
    document.getElementById('detail-error').textContent = (chain.alerts || []).map(alert => alert.detail ? `${alert.text}\n${alert.detail}` : alert.text).join('\n')
    return true
}

function showDetail(name) {
    chainDialog.dataset.proposal = ''
    document.getElementById('detail-proposal').hidden = true
    document.getElementById('detail-chain-extra').hidden = false
    document.getElementById('detail-vote').hidden = true
    if (!fillDetail(name)) return
    chainDialog.dataset.chain = name
    chainDialog.showModal()
}

function proposalDate(seconds) {
    return seconds ? new Date(seconds * 1000).toLocaleString('en-GB', { dateStyle: 'medium', timeStyle: 'short' }) : '\u2014'
}

function fillProposalHeader(proposal) {
    document.getElementById('detail-heading').textContent = `${proposal.chain} \u00b7 ${proposal.chain_id || 'Unknown chain'} \u00b7 Proposal #${proposal.id}`
    document.getElementById('detail-subtitle').textContent = proposal.title || ''
    const left = proposal.end - Date.now() / 1000
    const vote = document.getElementById('detail-vote')
    vote.hidden = false
    vote.className = `status governance-pill ${proposal.voted ? 'good' : left < 86400 ? 'red' : 'amber'}`
    vote.textContent = proposal.voted ? 'Voted' : 'Not voted'
    const facts = document.getElementById('detail-facts')
    facts.replaceChildren()
    appendFact(facts, 'Voting ends', [proposalDate(proposal.end), left <= 0 ? 'ended' : `${durationText(left)} left`])
    appendFact(facts, 'Voting started', proposalDate(proposal.start))
    appendFact(facts, 'Your vote', `${proposal.moniker || 'Validator'} ${proposal.voted ? 'voted' : 'has not voted'}`)
}

function fillProposalSummary(summary) {
    const section = document.getElementById('proposal-summary')
    section.replaceChildren()
    const text = element('p', 'proposal-summary')
    const characters = Array.from(summary || '')
    let expanded = false
    const draw = () => { text.textContent = characters.length ? (expanded || characters.length <= 400 ? summary : characters.slice(0, 400).join('') + '\u2026') : 'No summary on chain' }
    draw()
    section.appendChild(text)
    if (characters.length > 400) {
        const toggle = element('button', 'tool', 'Show full summary')
        toggle.type = 'button'
        toggle.setAttribute('aria-expanded', 'false')
        toggle.addEventListener('click', () => {
            expanded = !expanded
            draw()
            toggle.textContent = expanded ? 'Show less' : 'Show full summary'
            toggle.setAttribute('aria-expanded', String(expanded))
        })
        section.appendChild(toggle)
    }
}

function fillProposalTally(proposal) {
    const section = document.getElementById('proposal-tally')
    section.replaceChildren()
    if (!proposal.tally) {
        section.appendChild(element('p', 'footnote', proposal.tally_error || 'tally unavailable from this node'))
        return
    }
    const bar = element('div', 'proposal-tally-bar')
    bar.setAttribute('aria-hidden', 'true')
    const legend = element('div', 'proposal-tally-legend')
    for (const [key, label, color] of [['yes', 'Yes', 'good'], ['abstain', 'Abstain', 'muted'], ['no', 'No', 'critical'], ['veto', 'Veto', 'warning']]) {
        const value = proposal.tally[key] || 0
        const segment = element('span')
        segment.style.width = `${value}%`
        segment.style.background = `var(--${color})`
        bar.appendChild(segment)
        const item = element('span', '', `${label} ${value.toFixed(1)}%`)
        const swatch = element('i')
        swatch.style.background = `var(--${color})`
        item.appendChild(swatch)
        legend.appendChild(item)
    }
    section.appendChild(bar)
    section.appendChild(legend)
    if (proposal.tally.turnout != null) section.appendChild(element('p', 'footnote', `Turnout ${proposal.tally.turnout.toFixed(1)}% of bonded stake`))
}

async function showProposal(name, id, origin) {
    const chain = chains.get(name)
    const proposal = chain?.proposals?.find(item => item.id === id)
    if (!proposal) return
    const token = {}
    chainDialog.proposalRequest = token
    chainDialog.proposalOrigin = origin
    chainDialog.dataset.chain = ''
    chainDialog.dataset.proposal = String(id)
    chainDialog.dataset.proposalChain = name
    document.getElementById('detail-chain-extra').hidden = true
    document.getElementById('detail-proposal').hidden = false
    fillProposalHeader({ ...proposal, chain: name, chain_id: chain.chain_id, moniker: chain.moniker })
    for (const section of ['proposal-tally', 'proposal-summary']) document.getElementById(section).replaceChildren(element('p', 'footnote', 'Loading\u2026'))
    chainDialog.showModal()
    try {
        const response = await fetch(`proposal?chain=${encodeURIComponent(name)}&id=${encodeURIComponent(id)}`, { cache: 'no-store' })
        if (!response.ok) throw new Error('proposal request failed')
        const details = await response.json()
        if (!chainDialog.open || !chainDialog.dataset.proposal || chainDialog.proposalRequest !== token) return
        fillProposalHeader(details)
        fillProposalTally(details)
        fillProposalSummary(details.summary)
    } catch (_) {
        if (!chainDialog.open || !chainDialog.dataset.proposal || chainDialog.proposalRequest !== token) return
        for (const section of ['proposal-tally', 'proposal-summary']) document.getElementById(section).replaceChildren(element('p', 'footnote', 'Could not load proposal details'))
    }
}

function restoreProposalFocus() {
    if (!chainDialog.dataset.proposal) return
    const row = Array.from(document.getElementById('governance-list').children).find(item => item.dataset.proposalChain === chainDialog.dataset.proposalChain && item.dataset.proposalId === chainDialog.dataset.proposal)
    const target = row || chainDialog.proposalOrigin
    if (target?.isConnected) target.focus()
    chainDialog.dataset.proposal = ''
    chainDialog.proposalRequest = null
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
            if (message.msgType === 'fibre') document.dispatchEvent(new CustomEvent('fibre-update', { detail: message }))
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
for (const button of document.querySelectorAll('[data-network]')) {
    button.addEventListener('click', () => {
        networkFilter = button.dataset.network
        for (const option of document.querySelectorAll('[data-network]')) {
            option.classList.toggle('current', option === button)
            option.setAttribute('aria-pressed', String(option === button))
        }
        render()
    })
}
const searchInput = document.getElementById('chain-search')
searchInput.addEventListener('input', () => { search = searchInput.value.trim(); render() })
searchInput.addEventListener('keydown', event => {
    if (event.key === 'Escape') { event.preventDefault(); searchInput.value = ''; search = ''; render() }
})
document.addEventListener('keydown', event => {
    const target = event.target
    if (event.key === '/' && !event.ctrlKey && !event.metaKey && !event.altKey && !chainDialog.open && !document.querySelector('dialog[open]') && !['INPUT', 'TEXTAREA', 'SELECT'].includes(target.tagName) && !target.isContentEditable) {
        event.preventDefault()
        searchInput.focus()
    }
})
renderDate()
document.getElementById('detail-close').addEventListener('click', () => chainDialog.close())
chainDialog.addEventListener('close', restoreProposalFocus)
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
