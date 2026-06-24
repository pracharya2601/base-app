<script>
  import { onMount } from 'svelte'
  import { api } from '../lib/api.js'

  let status = null
  let tasks = []
  let filter = ''
  let detail = null // { task, runs, lineage, proposedActions }
  let opsInput = ''
  let opsMsg = ''
  let feedback = ''
  let err = ''

  // DB-driven config (Settings panel)
  let cfg = { provider: '', model: '', maxTokens: '', dailyTokenBudget: '', maxRevisions: '', intervalSeconds: '' }
  let cfgMsg = ''

  async function loadConfig() {
    try {
      const c = await api('/api/orchestrator/config')
      cfg = {
        provider: c.provider || '', model: c.model || '',
        maxTokens: c.maxTokens || '', dailyTokenBudget: c.dailyTokenBudget || '',
        maxRevisions: c.maxRevisions || '', intervalSeconds: c.intervalSeconds || '',
      }
    } catch (e) { err = e.message }
  }
  async function saveConfig() {
    const num = (v) => { const n = parseInt(v, 10); return Number.isFinite(n) ? n : undefined }
    const body = {
      provider: cfg.provider || undefined, model: cfg.model || undefined,
      maxTokens: num(cfg.maxTokens), dailyTokenBudget: num(cfg.dailyTokenBudget),
      maxRevisions: num(cfg.maxRevisions), intervalSeconds: num(cfg.intervalSeconds),
    }
    try {
      await api('/api/orchestrator/config', { method: 'POST', body: JSON.stringify(body) })
      cfgMsg = 'Saved — applies on the next tick.'
      loadConfig(); loadStatus()
    } catch (e) { cfgMsg = 'Save failed: ' + e.message }
  }

  async function loadStatus() {
    try { status = await api('/api/orchestrator/status') } catch (e) { err = e.message }
  }
  async function loadTasks() {
    try {
      const q = filter ? '?state=' + encodeURIComponent(filter) : ''
      const r = await api('/api/orchestrator/tasks' + q)
      tasks = r.items || []
    } catch (e) { err = e.message }
  }
  async function toggleAutopilot() {
    if (!status) return
    if (!status.autopilot && !confirm('Turn autopilot ON? The loop will auto-approve drafts and spend tokens autonomously (bounded by the daily budget).')) return
    try { await api('/api/orchestrator/autopilot', { method: 'POST', body: JSON.stringify({ enabled: !status.autopilot }) }); loadStatus() }
    catch (e) { alert('Failed: ' + e.message) }
  }
  async function sendOps() {
    const text = opsInput.trim()
    if (!text) { opsMsg = 'Type a request first.'; return }
    const title = text.length > 60 ? text.slice(0, 57) + '…' : text
    try {
      await api('/api/orchestrator/tasks', { method: 'POST', body: JSON.stringify({ role: 'ops', kind: 'ops_command', title, description: text }) })
      opsMsg = 'Queued. The ops agent will draft a change and stop for your approval — see Tasks (needs_review), then open it to review.'
      opsInput = ''
      loadTasks()
    } catch (e) { opsMsg = 'Failed: ' + e.message }
  }
  async function openTask(id) {
    try { detail = await api('/api/orchestrator/tasks/' + id) } catch (e) { alert('Load failed: ' + e.message) }
  }
  async function act(id, action) {
    const body = (action === 'revise' || action === 'reject') ? { feedback } : {}
    try {
      await api(`/api/orchestrator/tasks/${id}/${action}`, { method: 'POST', body: JSON.stringify(body) })
      detail = null; feedback = ''; loadTasks(); loadStatus()
    } catch (e) { alert(action + ' failed: ' + e.message) }
  }

  function summarize(raw) {
    try {
      const p = JSON.parse(raw || '{}')
      return Object.entries(p).map(([k, v]) => `${k}: ${typeof v === 'object' ? JSON.stringify(v) : v}`).join(', ')
    } catch { return raw || '' }
  }

  $: pending = detail ? (detail.proposedActions || []).filter((p) => p.status === 'proposed') : []

  onMount(() => { loadStatus(); loadConfig(); loadTasks() })
</script>

<h1>Orchestrator</h1>
<p class="sub">The always-on AI agent company — status, the agentic ops command, the task queue, and human approvals.</p>
{#if err}<div class="msg err">{err}</div>{/if}

<!-- Status -->
<div class="card">
  <div class="topbar"><h2 style="margin:0">Status</h2><button class="ghost" on:click={loadStatus}>Refresh</button></div>
  {#if status}
    <div class="row" style="margin-top:12px">
      <div class="field"><label>Enabled</label><div><span class="pill {status.enabled ? 'yes' : 'no'}">{status.enabled ? 'yes' : 'no'}</span></div></div>
      <div class="field"><label>Autopilot</label><div>
        <span class="pill {status.autopilot ? 'yes' : 'no'}">{status.autopilot ? 'on' : 'off'}</span>
        <button class="ghost" style="margin-left:8px" on:click={toggleAutopilot}>{status.autopilot ? 'Turn off' : 'Turn on'}</button>
      </div></div>
      <div class="field"><label>Tokens today / budget</label><div>{status.tokensUsedToday} / {status.dailyTokenBudget}</div></div>
    </div>
    <div style="margin-top:6px">
      {#each Object.entries(status.tasks || {}) as [k, v]}<span class="pill" style="margin-right:6px">{k}: {v}</span>{/each}
    </div>
  {:else}<p class="sub" style="margin-top:12px">—</p>{/if}
</div>

<!-- Settings (DB-driven config) -->
<div class="card">
  <div class="topbar"><h2 style="margin:0">Settings</h2><button class="ghost" on:click={loadConfig}>Refresh</button></div>
  <p class="sub" style="margin-top:-8px">DB-driven config (system tenant). Saved values apply on the next tick; interval applies on restart. Leave a number blank to use the env default.</p>
  {#if cfgMsg}<div class="msg ok">{cfgMsg}</div>{/if}
  <div class="row">
    <div class="field"><label>Provider</label><input type="text" bind:value={cfg.provider} placeholder="anthropic" /></div>
    <div class="field"><label>Model</label><input type="text" bind:value={cfg.model} placeholder="(provider default)" /></div>
  </div>
  <div class="row">
    <div class="field"><label>Max tokens / call</label><input type="number" bind:value={cfg.maxTokens} min="1" /></div>
    <div class="field"><label>Daily token budget</label><input type="number" bind:value={cfg.dailyTokenBudget} min="1" /></div>
  </div>
  <div class="row">
    <div class="field"><label>Max revisions</label><input type="number" bind:value={cfg.maxRevisions} min="1" /></div>
    <div class="field"><label>Interval (seconds · restart)</label><input type="number" bind:value={cfg.intervalSeconds} min="1" /></div>
  </div>
  <div class="row" style="margin-top:10px"><button on:click={saveConfig}>Save settings</button></div>
</div>

<!-- Ops command -->
<div class="card">
  <h2>Ops command</h2>
  <p class="sub" style="margin-top:-8px">Tell the platform what to do in plain English — e.g. "add a blog_posts collection with title and body, rbac on", "create a support-readonly role that reads orders", or "make alice@example.com a support-readonly user". The ops agent drafts the change and <strong>queues it for approval</strong>.</p>
  {#if opsMsg}<div class="msg ok">{opsMsg}</div>{/if}
  <div class="field"><textarea rows="3" bind:value={opsInput} placeholder="Describe the change you want…"></textarea></div>
  <div class="row"><button on:click={sendOps}>Ask the ops agent</button></div>
</div>

<!-- Tasks -->
<div class="card">
  <div class="topbar"><h2 style="margin:0">Tasks</h2>
    <div style="display:flex; gap:8px; align-items:center">
      <select style="width:auto" bind:value={filter} on:change={loadTasks}>
        <option value="">all states</option>
        {#each ['pending', 'working', 'needs_review', 'rejected', 'done', 'failed'] as s}<option value={s}>{s}</option>{/each}
      </select>
      <button class="ghost" on:click={loadTasks}>Refresh</button>
    </div>
  </div>
  <table style="margin-top:8px">
    <thead><tr><th>Title</th><th>Role / Agent</th><th>State</th><th>Draft</th><th></th></tr></thead>
    <tbody>
      {#each tasks as t}
        <tr>
          <td>{t.title}</td>
          <td>{t.role || ''} / {t.agent || ''}</td>
          <td><span class="pill">{t.state}</span></td>
          <td>{t.hasOutput ? '✓' : '—'}</td>
          <td><button class="ghost" on:click={() => openTask(t.id)}>Open</button></td>
        </tr>
      {:else}
        <tr><td colspan="5" class="muted">No tasks.</td></tr>
      {/each}
    </tbody>
  </table>
</div>

<!-- Detail -->
{#if detail}
  {@const t = detail.task}
  <div class="card">
    <div class="topbar"><h2 style="margin:0">{t.title}</h2><button class="ghost" on:click={() => (detail = null)}>Close</button></div>
    <div class="field"><label>State</label><div><span class="pill">{t.state}</span> &nbsp; {t.role || ''} / {t.agent || ''}</div></div>
    {#if t.errorMsg}<div class="field"><label>Feedback / error</label><div class="msg err">{t.errorMsg}</div></div>{/if}
    <div class="field"><label>Draft output</label><pre>{t.output || '—'}</pre></div>

    {#if (detail.proposedActions || []).length}
      <div class="field"><label>Proposed actions</label>
        <div class="msg {pending.length ? 'warn' : ''}" style="margin-bottom:0">
          {pending.length ? `⚠ Approving will execute ${pending.length} pending action(s):` : 'Proposed actions:'}
          <ul style="margin:8px 0 0; padding-left:18px">
            {#each detail.proposedActions as p}
              <li>
                <span class="pill {p.status === 'proposed' ? 'pending' : (p.status === 'executed' ? 'done' : 'no')}">{p.status}</span>
                <strong>{p.actionKind}</strong> — {summarize(p.params)}
                {#if p.result}<span class="muted">({p.result})</span>{/if}
              </li>
            {/each}
          </ul>
        </div>
      </div>
    {/if}

    {#if t.state === 'needs_review'}
      <div class="field"><label>Feedback (for revise / reject)</label><textarea rows="2" bind:value={feedback} placeholder="What needs to change?"></textarea></div>
      <div class="row">
        <button on:click={() => act(t.id, 'approve')}>{pending.length ? 'Approve → run actions' : 'Approve'}</button>
        <button class="ghost" on:click={() => act(t.id, 'revise')}>Revise</button>
        <button class="danger" on:click={() => act(t.id, 'reject')}>Reject</button>
      </div>
      {#if pending.length}<p style="color:var(--warning); font-size:13px; margin-top:8px">⚠ Approving also executes the {pending.length} pending action(s) above. Revise and Reject discard them.</p>{/if}
    {/if}
  </div>
{/if}
