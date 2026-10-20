const fs = require('node:fs')
const vm = require('node:vm')
const source = fs.readFileSync(require.resolve('../../td2/static/status.js'), 'utf8')
function dashboard(values, options = {}) {
    const surfaces = new Map(), opened = []
    const node = tag => ({
        tag, isConnected: true, focus() { this.focused = true }, showModal() { this.open = true }, close() { this.open = false; this.events.close?.() }, children: [], events: {}, attributes: {}, style: {}, dataset: {}, hidden: false,
        classList: { add() {}, toggle() {} },
        appendChild(child) { this.children.push(child) },
        replaceChildren(...children) { this.children = children },
        setAttribute(name, value) { this.attributes[name] = value },
        addEventListener(name, fn) { this.events[name] = fn },
        get firstChild() { return this.children[0] },
        get lastChild() { return this.children.at(-1) }
    })
    const get = id => { if (!surfaces.has(id)) surfaces.set(id, node('div')); return surfaces.get(id) }
    const context = vm.createContext({ fetch: options.fetch || (() => new Promise(() => {})), encodeURIComponent, chainDialog: get('chain-dialog'), chains: new Map(values.map(chain => [chain.name, chain])), showDetail: name => opened.push(name), document: { createElement: node, getElementById: get, querySelectorAll: () => [] } })
    const helpers = source.slice(source.indexOf('function element('), source.indexOf('function setConnection('))
    const cards = source.slice(source.indexOf('function summaryValues('), source.indexOf('function renderDate('))
    const duration = source.slice(source.indexOf('function durationText('), source.indexOf('function fibreProviderFacts('))
    const panel = source.slice(source.indexOf('function proposalDate('), source.indexOf('function addActivity('))
    const facts = source.slice(source.indexOf('function appendFact('), source.indexOf('function fillBridge('))
    vm.runInContext("let attentionExpanded = false, governanceExpanded = false, filter = 'all', networkFilter = 'all', search = ''; const drawnHeights = new Map(); const newBlockMs = 700; const statusNames = {5:'missed-proposal',4:'proposed',3:'signed',2:'precommit',1:'prevote',0:'missed','-1':'unobserved'}; function newBlockAge() { return null };" + helpers + duration + cards + facts + panel + 'Date.now = () => 10000000', context)
    return { context, get, opened, run: code => vm.runInContext(code, context), visible: () => Array.from(vm.runInContext('[...chains.values()].filter(matchesFilters).map(chain => chain.name)', context)) }
}
module.exports = { dashboard, source }
