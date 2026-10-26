import ForceGraph from 'force-graph'
import { forceCollide } from 'd3-force'
import './style.css'
import { inject } from '@vercel/analytics'

inject()

// ---- API types (mirror internal/api/graph.go) ----
type Kind = 'EOA' | 'REALM' | 'PACKAGE' | 'UNKNOWN'
interface GNode {
  id: string
  address: string
  kind: Kind
  pkgPath?: string
  label: string
  txCount: number
  volumeUgnot: number
  degree: number
  size: number
  x?: number
  y?: number
}
interface GLink {
  source: string | GNode
  target: string | GNode
  weight: number
  volumeUgnot: number
  kinds: string[]
}
interface GraphResp {
  nodes: GNode[]
  links: GLink[]
  seed: string
  sync: { chainId: string; lastHeight: number }
  warning?: string
}
interface NodeDetail extends GNode {
  neighbors: (GNode & { weight: number; linkVolumeUgnot: number; kinds: string[] })[]
  recentTransfers: { hash: string; height: number; amount: number; denom: string; from: string; to: string }[]
  recentCalls: { hash: string; height: number; func: string; pkgPath: string; send?: string; caller: string; realm: string }[]
  deploys: { hash: string; height: number; pkgPath: string; creator: string }[]
}

// dev → local API; prod build → same origin (served by bubblegno from web/dist) unless VITE_API_BASE set
const API = (import.meta.env.VITE_API_BASE ?? (import.meta.env.DEV ? 'http://localhost:8090' : '')).replace(/\/$/, '')

const COLORS: Record<Kind, string> = {
  EOA: '#4cc9f0',
  REALM: '#f72585',
  PACKAGE: '#ffb703',
  UNKNOWN: '#6c757d',
}
const color = (k: Kind) => COLORS[k] ?? COLORS.UNKNOWN

const $ = <T extends HTMLElement>(id: string) => document.getElementById(id) as T
const seedInput = $<HTMLInputElement>('seed')
const depthSel = $<HTMLSelectElement>('depth')
const limitSel = $<HTMLSelectElement>('limit')
const minWeightSel = $<HTMLSelectElement>('minWeight')
const kindBtns = [...document.querySelectorAll<HTMLButtonElement>('.kind-toggle')]
const ALL_KINDS = ['EOA', 'REALM', 'PACKAGE']
const selectedKinds = () => kindBtns.filter((b) => b.classList.contains('on')).map((b) => b.dataset.kind!)
function setKinds(kinds: string[]) {
  for (const b of kindBtns) {
    const on = kinds.includes(b.dataset.kind!)
    b.classList.toggle('on', on)
    b.setAttribute('aria-pressed', String(on))
  }
}
const statusEl = $<HTMLSpanElement>('status')
const loaderEl = $<HTMLDivElement>('loader')
let loadSeq = 0
let loadCtl: AbortController | undefined
const panel = $<HTMLElement>('panel')

const esc = (s: string) => s.replace(/[&<>"']/g, (c) => `&#${c.charCodeAt(0)};`)
const gnot = (u: number) => {
  const g = u / 1e6
  if (g >= 1e6) return `${(g / 1e6).toFixed(2)}M GNOT`
  if (g >= 1e3) return `${(g / 1e3).toFixed(1)}k GNOT`
  return `${g.toLocaleString(undefined, { maximumFractionDigits: 2 })} GNOT`
}
const short = (a: string) => (a.startsWith('g1') && a.length > 16 ? `${a.slice(0, 8)}…${a.slice(-4)}` : a.replace(/^gno\.land/, ''))
const idOf = (n: string | GNode) => (typeof n === 'string' ? n : n.id)

// ---- graph ----
let selected: string | null = null
let hovered: GNode | null = null
let neighborsOfHover = new Set<string>()
let current: GraphResp | null = null

const graph = new ForceGraph<GNode, GLink>($('graph'))
  .backgroundColor('rgba(0,0,0,0)')
  .nodeId('id')
  .nodeVal((n) => n.size * n.size)
  .nodeRelSize(1)
  .nodeLabel((n) =>
    `<b>${esc(n.label || n.address)}</b><br/><span style="color:#8a90a6">${n.kind} · ${n.txCount} tx · deg ${n.degree} · ${gnot(n.volumeUgnot)}</span>`,
  )
  .linkColor((l) => {
    const active = hovered && (idOf(l.source) === hovered.id || idOf(l.target) === hovered.id)
    if (active) return 'rgba(255,255,255,0.55)'
    if (l.kinds.includes('transfer')) return 'rgba(76,201,240,0.18)'
    if (l.kinds.includes('deploy')) return 'rgba(255,183,3,0.22)'
    return 'rgba(247,37,133,0.16)'
  })
  .linkWidth((l) => Math.min(6, 0.5 + Math.log2(1 + l.weight)))
  .linkDirectionalParticles((l) =>
    hovered && (idOf(l.source) === hovered.id || idOf(l.target) === hovered.id) ? 2 : 0,
  )
  .linkDirectionalParticleWidth(2)
  .cooldownTicks(200)
  .nodeCanvasObject((n, ctx, scale) => {
    const r = n.size
    const x = n.x ?? 0
    const y = n.y ?? 0
    const dim = hovered && hovered.id !== n.id && !neighborsOfHover.has(n.id)
    const c = color(n.kind)
    ctx.globalAlpha = dim ? 0.18 : 1
    const grad = ctx.createRadialGradient(x - r * 0.3, y - r * 0.3, r * 0.1, x, y, r)
    grad.addColorStop(0, c + 'ee')
    grad.addColorStop(1, c + '55')
    ctx.beginPath()
    ctx.arc(x, y, r, 0, 2 * Math.PI)
    ctx.fillStyle = grad
    ctx.fill()
    ctx.lineWidth = (n.id === selected ? 3 : 1) / scale
    ctx.strokeStyle = n.id === selected ? '#ffffff' : c
    ctx.stroke()
    // labels: big bubbles always, others when zoomed in or hovered
    const show = n.id === selected || hovered?.id === n.id || r * scale > 22
    if (show) {
      const fs = Math.max(10 / scale, Math.min(r / 2.5, 14 / scale))
      ctx.font = `600 ${fs}px Inter, system-ui, sans-serif`
      ctx.textAlign = 'center'
      ctx.textBaseline = 'middle'
      ctx.fillStyle = '#fff'
      ctx.fillText(short(n.label || n.address), x, y)
    }
    ctx.globalAlpha = 1
  })
  .nodePointerAreaPaint((n, c, ctx) => {
    ctx.fillStyle = c
    ctx.beginPath()
    ctx.arc(n.x ?? 0, n.y ?? 0, n.size, 0, 2 * Math.PI)
    ctx.fill()
  })
  .onNodeHover((n) => {
    hovered = n ?? null
    neighborsOfHover = new Set()
    if (n && current) {
      for (const l of current.links) {
        if (idOf(l.source) === n.id) neighborsOfHover.add(idOf(l.target))
        if (idOf(l.target) === n.id) neighborsOfHover.add(idOf(l.source))
      }
    }
    $('graph').style.cursor = n ? 'pointer' : ''
  })
  .onNodeClick((n) => openPanel(n.id))
  .onBackgroundClick(() => closePanel())

graph.d3Force('collide', forceCollide<GNode>((n) => n.size + 10).strength(0.9) as any)
graph.d3Force('charge')?.strength((n: GNode) => -120 - n.size * 15).distanceMax(600)
graph.d3Force('link')?.distance((l: GLink) => {
  const s = typeof l.source === 'object' ? l.source.size : 8
  const t = typeof l.target === 'object' ? l.target.size : 8
  return s + t + 80
})

window.addEventListener('resize', () => graph.width(innerWidth).height(innerHeight))

// ---- data ----
async function load() {
  closeSuggest()
  syncClear()
  const seed = seedInput.value.trim()
  const qs = new URLSearchParams({ depth: depthSel.value, limit: limitSel.value })
  if (minWeightSel.value !== '1') qs.set('minWeight', minWeightSel.value)
  const kinds = selectedKinds()
  if (kinds.length < ALL_KINDS.length) qs.set('kinds', kinds.join(','))
  if (seed) qs.set('seed', seed)
  statusEl.innerHTML = '<span class="mini-spin"></span>loading…'
  const seq = ++loadSeq
  loadCtl?.abort()
  loadCtl = new AbortController()
  loaderEl.classList.remove('hidden')
  document.body.classList.add('is-loading')
  try {
    const res = await fetch(`${API}/v0/graph?${qs}`, { signal: loadCtl.signal })
    const data: GraphResp = await res.json()
    if (seq !== loadSeq) return
    current = data
    graph.graphData({ nodes: data.nodes, links: data.links })
    if (data.nodes.length) setTimeout(() => graph.zoomToFit(600, 80), 900)
    // server resolves seed (address or pkgPath) → canonical id: prefill it
    if (seed && data.seed && data.seed !== seed) seedInput.value = data.seed
    const warn = data.warning ? ` · <span class="warn">${esc(data.warning)}</span>` : ''
    statusEl.innerHTML = `${data.nodes.length} nodes · ${data.links.length} links · <span id="syncH">${syncText(data.sync.chainId, data.sync.lastHeight)}</span>${warn}`
    syncClear()
    const canon = seedInput.value.trim()
    const hash = canon ? `#${encodeSeed(canon)}` : ''
    if (location.hash !== hash || location.search) {
      // drop ?seed= once consumed; hash is the canonical shareable form
      history.replaceState(null, '', location.pathname + hash)
      lastHash = location.hash
    }
  } catch (e) {
    if (seq !== loadSeq) return
    statusEl.innerHTML = `<span class="warn">API unreachable (${esc(API)})</span>`
  } finally {
    if (seq === loadSeq) {
      loaderEl.classList.add('hidden')
      document.body.classList.remove('is-loading')
    }
  }
}

async function openPanel(id: string) {
  selected = id
  panel.classList.remove('hidden')
  panel.innerHTML = `<p class="muted">loading ${esc(short(id))}…</p>`
  try {
    const res = await fetch(`${API}/v0/node/${id.split('/').map(encodeURIComponent).join('/')}`)
    if (!res.ok) throw new Error((await res.json()).error ?? res.statusText)
    renderPanel((await res.json()) as NodeDetail)
  } catch (e) {
    panel.innerHTML = `<button class="close">×</button><p class="warn">${esc(String(e))}</p>`
    panel.querySelector('.close')?.addEventListener('click', closePanel)
  }
}

function closePanel() {
  selected = null
  panel.classList.add('hidden')
}

function gnoLandURL(a: string): string {
  if (a.startsWith('gno.land/')) return `https://${a}`
  if (a.startsWith('g1')) return `https://gno.land/u/${a}`
  return `https://gno.land/${a}`
}
function gnoscanURL(a: string): string {
  if (a.startsWith('gno.land/')) return `https://gnoscan.io/realms/details?path=${encodeURIComponent(a)}`
  return `https://gnoscan.io/account/${encodeURIComponent(a)}`
}
const ext = (a: string) =>
  `<a class="ext" href="${esc(gnoLandURL(a))}" target="_blank" rel="noreferrer" title="gno.land">gno</a>` +
  `<a class="ext" href="${esc(gnoscanURL(a))}" target="_blank" rel="noreferrer" title="gnoscan.io">scan</a>`
const link = (a: string) =>
  a
    ? `<span class="addr"><a data-id="${esc(a)}" title="${esc(a)}">${esc(short(a))}</a> ${ext(a)}</span>`
    : '<span class="muted">?</span>'


function renderPanel(d: NodeDetail) {
  panel.innerHTML = `
    <div class="panel-head">
    <button class="close">×</button>
    <span class="kind" style="background:${color(d.kind)}">${d.kind}</span>
    <h2>${esc(d.label || d.address)}</h2>
    <div class="muted mono" style="word-break:break-all;font-size:11px">${esc(d.address)} ${ext(d.address)}</div>
    <div class="stats">
      <div class="stat"><b>${d.txCount}</b><small>tx</small></div>
      <div class="stat"><b>${d.degree}</b><small>neighbors</small></div>
      <div class="stat"><b>${gnot(d.volumeUgnot)}</b><small>volume</small></div>
    </div>
    <div class="actions">
      <button id="explore">Explore from here</button>
      <a class="ghost" href="${esc(gnoLandURL(d.address))}" target="_blank" rel="noreferrer" style="align-self:center">gno.land ↗</a>
      <a class="ghost" href="${esc(gnoscanURL(d.address))}" target="_blank" rel="noreferrer" style="align-self:center">gnoscan ↗</a>
    </div>
    </div>
    <div class="panel-body">
    <h3>Top neighbors</h3>
    <ul>${d.neighbors.slice(0, 20).map((n) => `<li><span class="mono">${link(n.id)}</span><span class="muted">${n.weight}× · ${n.kinds.join('/')}</span></li>`).join('') || '<li class="muted">none</li>'}</ul>
    <h3>Recent transfers</h3>
    <ul>${d.recentTransfers.map((t) => `<li><span class="mono">${link(t.from)} → ${link(t.to)}</span><span class="muted">${t.denom === 'ugnot' ? gnot(t.amount) : `${t.amount} ${esc(t.denom)}`}</span></li>`).join('') || '<li class="muted">none</li>'}</ul>
    <h3>Recent calls</h3>
    <ul>${d.recentCalls.map((c) => `<li><span class="mono">${link(c.caller)} → ${esc(c.func)}()</span><span class="muted">#${c.height}</span></li>`).join('') || '<li class="muted">none</li>'}</ul>
    ${d.deploys.length ? `<h3>Deploys</h3><ul>${d.deploys.map((x) => `<li><span class="mono">${link(x.pkgPath)}</span><span class="muted">#${x.height}</span></li>`).join('')}</ul>` : ''}
    </div>
  `
  panel.querySelector('.close')?.addEventListener('click', closePanel)
  panel.querySelector('#explore')?.addEventListener('click', () => {
    seedInput.value = d.address
    load()
  })
  panel.querySelectorAll<HTMLAnchorElement>('a[data-id]').forEach((a) =>
    a.addEventListener('click', () => {
      const id = a.dataset.id!
      const n = current?.nodes.find((x) => x.id === id)
      if (n?.x != null) graph.centerAt(n.x, n.y, 600)
      openPanel(id)
    }),
  )
}

// ---- UI wiring ----
$('search').addEventListener('submit', (e) => {
  e.preventDefault()
  load()
})
$('reset').addEventListener('click', () => {
  seedInput.value = ''
  load()
})
$('refresh').addEventListener('click', load)
let selTimer: ReturnType<typeof setTimeout> | undefined
const autoLoad = () => { clearTimeout(selTimer); selTimer = setTimeout(load, 100) }
depthSel.addEventListener('change', autoLoad)
limitSel.addEventListener('change', autoLoad)
minWeightSel.addEventListener('change', autoLoad)
for (const b of kindBtns) {
  b.addEventListener('click', () => {
    const on = !b.classList.contains('on')
    if (!on && selectedKinds().length === 1) return // keep at least one kind
    b.classList.toggle('on', on)
    b.setAttribute('aria-pressed', String(on))
    autoLoad()
  })
}

// ---- search / autocomplete ----
interface SearchHit { address: string; kind: Kind; pkgPath?: string; label?: string; activity: number; match: string }
const suggestEl = $<HTMLUListElement>('suggest')
let hits: SearchHit[] = []
let active = -1
let debounce: number | undefined
let searchCtl: AbortController | null = null
let lastQuery = ''

const hl = (text: string, q: string) => {
  const i = text.toLowerCase().indexOf(q.toLowerCase())
  if (!q || i < 0) return esc(text)
  return esc(text.slice(0, i)) + '<mark>' + esc(text.slice(i, i + q.length)) + '</mark>' + esc(text.slice(i + q.length))
}

function closeSuggest() {
  clearTimeout(debounce)
  searchCtl?.abort()
  suggestEl.classList.add('hidden')
  seedInput.setAttribute('aria-expanded', 'false')
  active = -1
}

function renderSuggest(q: string) {
  if (!hits.length) {
    suggestEl.innerHTML = `<li class="empty">no match for “${esc(q)}”</li>`
  } else {
    suggestEl.innerHTML = hits
      .map((h, i) => {
        const main = h.label ? `${hl(h.label, q)}<small>${hl(short(h.address), q)}</small>` : hl(h.address, q)
        return `<li role="option" data-i="${i}" class="${i === active ? 'active' : ''}" title="${esc(h.address)}">
          <span class="dot" style="background:${color(h.kind)}"></span>
          <span class="main">${main}</span>
          <span class="side">${h.activity.toLocaleString()} tx</span></li>`
      })
      .join('')
  }
  suggestEl.classList.remove('hidden')
  seedInput.setAttribute('aria-expanded', 'true')
  suggestEl.querySelector('li.active')?.scrollIntoView({ block: 'nearest' })
}

async function runSearch(q: string) {
  searchCtl?.abort()
  searchCtl = new AbortController()
  try {
    const res = await fetch(`${API}/v0/search?${new URLSearchParams({ q, limit: '20' })}`, { signal: searchCtl.signal })
    const data: { hits: SearchHit[] } = await res.json()
    if (seedInput.value.trim() !== q) return // stale
    hits = data.hits ?? []
    active = hits.length ? 0 : -1
    lastQuery = q
    if (document.activeElement === seedInput) renderSuggest(q)
  } catch (e) {
    if ((e as Error).name !== 'AbortError') closeSuggest()
  }
}

function pick(i: number) {
  const h = hits[i]
  if (!h) return
  seedInput.value = h.address // prefill with canonical id
  load()
  seedInput.blur()
}

// ---- clear (×) button inside the search input ----
const clearBtn = $<HTMLButtonElement>('clear')
function syncClear() {
  clearBtn.classList.toggle('hidden', seedInput.value === '')
}
clearBtn.addEventListener('mousedown', (e) => e.preventDefault()) // keep focus, avoid blur race
clearBtn.addEventListener('click', () => {
  clearTimeout(debounce)
  seedInput.value = ''
  hits = []
  lastQuery = ''
  closeSuggest()
  syncClear()
  if (location.hash || location.search) {
    history.replaceState(null, '', location.pathname)
    lastHash = ''
  }
  load()
})

seedInput.addEventListener('input', () => {
  syncClear()
  const q = seedInput.value.trim()
  clearTimeout(debounce)
  if (q.length < 2) {
    closeSuggest()
    return
  }
  debounce = window.setTimeout(() => runSearch(q), 250)
})
seedInput.addEventListener('focus', () => {
  const q = seedInput.value.trim()
  if (q.length >= 2 && q === lastQuery && hits.length) renderSuggest(q)
})
seedInput.addEventListener('blur', () => setTimeout(() => suggestEl.classList.add('hidden'), 150))
seedInput.addEventListener('keydown', (e) => {
  const open = !suggestEl.classList.contains('hidden') && hits.length > 0
  if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
    if (!open) return
    e.preventDefault()
    active = (active + (e.key === 'ArrowDown' ? 1 : -1) + hits.length) % hits.length
    renderSuggest(lastQuery)
  } else if (e.key === 'Enter' && open && active >= 0) {
    e.preventDefault()
    pick(active)
  } else if (e.key === 'Tab' && open && active >= 0) {
    e.preventDefault() // autocomplete without loading
    seedInput.value = hits[active].address
    syncClear()
    closeSuggest()
  } else if (e.key === 'Escape') {
    closeSuggest()
  }
})
// mousedown (not click) so it fires before input blur
suggestEl.addEventListener('mousedown', (e) => {
  const li = (e.target as HTMLElement).closest<HTMLLIElement>('li[data-i]')
  if (!li) return
  e.preventDefault()
  pick(Number(li.dataset.i))
})

// ---- prefill from URL: #<seed> (canonical) or ?seed=&depth=&limit= ----
function encodeSeed(s: string) {
  return s.split('/').map(encodeURIComponent).join('/')
}
let lastHash = location.hash
function seedFromURL(): string {
  if (location.hash.length > 1) {
    try {
      return decodeURIComponent(location.hash.slice(1))
    } catch {
      return location.hash.slice(1)
    }
  }
  return new URLSearchParams(location.search).get('seed') ?? new URLSearchParams(location.search).get('q') ?? ''
}
function setSel(sel: HTMLSelectElement, v: string | null) {
  if (v && [...sel.options].some((o) => (o.value || o.text) === v)) sel.value = v
}
const params = new URLSearchParams(location.search)
setSel(depthSel, params.get('depth'))
setSel(limitSel, params.get('limit'))
setSel(minWeightSel, params.get('minWeight'))
{
  const k = (params.get('kinds') ?? '').toUpperCase().split(',').map((s) => s.trim()).filter((s) => ALL_KINDS.includes(s))
  if (k.length) setKinds(k)
}
seedInput.value = seedFromURL()
window.addEventListener('hashchange', () => {
  if (location.hash === lastHash) return
  lastHash = location.hash
  seedInput.value = seedFromURL()
  syncClear()
  load()
})
syncClear()
load()

// ---- live sync height (polls /v0/sync, independent of graph reload) ----
let liveSync: { chainId: string; lastHeight: number; tip?: number } | null = null
function syncText(chainId: string, h: number): string {
  const s = liveSync && liveSync.lastHeight >= h ? liveSync : { chainId, lastHeight: h }
  return `${esc(s.chainId)} @ ${s.lastHeight.toLocaleString()}`
}
async function pollSync() {
  try {
    const res = await fetch(`${API}/v0/sync`, { cache: 'no-store' })
    if (res.ok) {
      liveSync = await res.json()
      const el = document.getElementById('syncH')
      if (el && liveSync) el.innerHTML = syncText(liveSync.chainId, liveSync.lastHeight)
    }
  } catch {}
}
pollSync()
setInterval(pollSync, 2500)
