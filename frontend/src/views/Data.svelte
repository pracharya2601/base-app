<script>
  import { onMount } from 'svelte'
  import { api } from '../lib/api.js'

  let collections = []
  let selected = ''
  let columns = []
  let rows = []
  let note = ''
  let err = ''

  async function loadCollections() {
    err = ''
    try {
      const r = await api('/api/collections?perPage=200')
      collections = r.items || []
    } catch (e) { err = e.message }
  }

  async function loadRecords() {
    rows = []; columns = []; note = ''; err = ''
    if (!selected) return
    try {
      const r = await api(`/api/collections/${encodeURIComponent(selected)}/records?perPage=50&sort=-created`)
      const items = r.items || []
      const col = collections.find((c) => c.name === selected)
      let cols = ['id']
      if (col && col.fields) cols = cols.concat(col.fields.map((f) => f.name).filter((n) => n !== 'password' && n !== 'tokenKey'))
      columns = cols
      rows = items
      const more = r.totalItems > items.length ? ` (first 50 of ${r.totalItems})` : ''
      note = `${items.length} record(s)${more}`
    } catch (e) { err = e.message }
  }

  function cell(rec, c) {
    let v = rec[c]
    if (v === undefined || v === null) return ''
    if (typeof v === 'object') v = JSON.stringify(v)
    else v = String(v)
    return v.length > 80 ? v.slice(0, 77) + '…' : v
  }

  onMount(loadCollections)
</script>

<h1>Data</h1>
<p class="sub">Read-only browse of your collections — so you never need the PocketBase dashboard to look at records. To <em>change</em> data, use the Ops command box (Orchestrator) or the records API.</p>

<div class="card">
  <div class="topbar">
    <h2 style="margin:0">Records</h2>
    <div style="display:flex; gap:8px; align-items:center">
      <select style="width:auto" bind:value={selected} on:change={loadRecords}>
        <option value="">choose a collection…</option>
        {#each collections as c}
          <option value={c.name}>{c.name}{c.system ? ' (system)' : ''}</option>
        {/each}
      </select>
      <button class="ghost" on:click={() => { loadCollections().then(loadRecords) }}>Refresh</button>
    </div>
  </div>

  {#if err}<div class="msg err" style="margin-top:10px">{err}</div>{/if}
  {#if note}<div class="msg ok" style="margin-top:10px">{note}</div>{/if}

  {#if selected && rows.length}
    <div style="overflow:auto; margin-top:8px">
      <table>
        <thead><tr>{#each columns as c}<th>{c}</th>{/each}</tr></thead>
        <tbody>
          {#each rows as rec}
            <tr>{#each columns as c}<td>{cell(rec, c)}</td>{/each}</tr>
          {/each}
        </tbody>
      </table>
    </div>
  {:else if selected}
    <p class="sub" style="margin-top:10px">No records.</p>
  {/if}
</div>
