<script>
  import { onMount } from 'svelte'
  import { api, getToken } from '../lib/api.js'

  let catalog = []
  let rows = []
  let listErr = ''

  let pProvider = '', pEnabled = true, pDefaultModel = '', pBaseUrl = '', pApiKey = ''
  let saveMsg = '', saveErr = '', saving = false

  let tProvider = '', tModel = '', tPrompt = 'Say hello in one short sentence.'
  let testMsg = '', testErr = '', testOut = '', testing = false

  async function loadCatalog() {
    try {
      const { providers } = await api('/api/ai/catalog')
      providers.sort((a, b) => a.provider.localeCompare(b.provider))
      catalog = providers
      if (!pProvider && providers.length) pProvider = providers[0].provider
      if (!tProvider && providers.length) tProvider = providers[0].provider
    } catch { /* dropdowns empty; table still renders */ }
  }
  async function loadProviders() {
    listErr = ''
    try {
      const d = await api('/api/collections/_aiProviders/records?perPage=200')
      const byName = {}; d.items.forEach((r) => (byName[r.provider] = r))
      const names = Array.from(new Set([...catalog.map((c) => c.provider), ...Object.keys(byName)])).sort()
      rows = names.map((name) => ({ name, r: byName[name] }))
    } catch (e) { listErr = e.message }
  }
  async function save() {
    saveMsg = ''; saveErr = ''; saving = true
    try {
      const payload = { provider: pProvider, enabled: pEnabled, defaultModel: pDefaultModel.trim(), baseUrl: pBaseUrl.trim() }
      if (pApiKey.trim()) payload.apiKeyEnc = pApiKey.trim() // raw in -> save-hook encrypts
      const existing = await api(`/api/collections/_aiProviders/records?filter=(provider='${encodeURIComponent(pProvider)}')`)
      let rec, created
      if (existing.items.length) { rec = await api(`/api/collections/_aiProviders/records/${existing.items[0].id}`, { method: 'PATCH', body: JSON.stringify(payload) }); created = false }
      else { rec = await api('/api/collections/_aiProviders/records', { method: 'POST', body: JSON.stringify(payload) }); created = true }
      saveMsg = `Saved ${rec.provider} (${created ? 'created' : 'updated'}) — has key: ${!!rec.apiKeyEnc}`
      pApiKey = ''
      loadProviders()
    } catch (e) { saveErr = e.message } finally { saving = false }
  }
  function reqBody() { const b = { prompt: tPrompt }; if (tModel.trim()) b.model = tModel.trim(); return b }
  async function generate() {
    testing = true; testMsg = 'Calling…'; testErr = ''; testOut = ''
    try {
      const d = await api(`/api/ai/${tProvider}/generate`, { method: 'POST', body: JSON.stringify(reqBody()) })
      testMsg = `OK — ${d.usage.totalTokens} tokens (${d.usage.promptTokens} in / ${d.usage.completionTokens} out)`
      testOut = d.text
    } catch (e) { testMsg = ''; testErr = e.message } finally { testing = false }
  }
  async function stream() {
    testing = true; testMsg = 'Streaming…'; testErr = ''; testOut = ''
    try {
      const res = await fetch(`/api/ai/${tProvider}/stream`, { method: 'POST', headers: { 'Content-Type': 'application/json', Authorization: getToken() }, body: JSON.stringify(reqBody()) })
      if (!res.ok) throw new Error((await res.text()) || 'HTTP ' + res.status)
      const reader = res.body.getReader(); const dec = new TextDecoder(); let buf = ''
      while (true) {
        const { done, value } = await reader.read(); if (done) break
        buf += dec.decode(value, { stream: true })
        const frames = buf.split('\n\n'); buf = frames.pop()
        for (const f of frames) {
          const line = f.split('\n').find((l) => l.startsWith('data:')); if (!line) continue
          const evt = JSON.parse(line.slice(5).trim())
          if (evt.delta) testOut += evt.delta
          else if (evt.done) testMsg = `Done — ${evt.usage.totalTokens} tokens`
          else if (evt.error) testErr = evt.error
        }
      }
    } catch (e) { testMsg = ''; testErr = e.message } finally { testing = false }
  }
  onMount(async () => { await loadCatalog(); loadProviders() })
</script>

<h1>AI Providers</h1>
<p class="sub">Configure provider keys (encrypted at rest) and test the proxy. Keys are write-only — they're never returned.</p>

<div class="card">
  <div class="topbar"><h2 style="margin:0">Configured providers</h2><button class="ghost" on:click={loadProviders}>Refresh</button></div>
  {#if listErr}<div class="msg err" style="margin-top:10px">{listErr}</div>{/if}
  <table style="margin-top:8px">
    <thead><tr><th>Provider</th><th>Enabled</th><th>Has key</th><th>Default model</th><th>Base URL</th></tr></thead>
    <tbody>
      {#each rows as { name, r }}
        <tr>
          <td><b>{name}</b></td>
          <td><span class="pill {r && r.enabled ? 'yes' : 'no'}">{r && r.enabled ? 'yes' : 'no'}</span></td>
          <td><span class="pill {r && r.apiKeyEnc ? 'yes' : 'no'}">{r && r.apiKeyEnc ? 'yes' : 'no'}</span></td>
          <td>{(r && r.defaultModel) || '—'}</td>
          <td>{(r && r.baseUrl) || '—'}</td>
        </tr>
      {/each}
    </tbody>
  </table>
</div>

<div class="card">
  <h2>Add / update a provider</h2>
  {#if saveErr}<div class="msg err">{saveErr}</div>{/if}
  {#if saveMsg}<div class="msg ok">{saveMsg}</div>{/if}
  <div class="row">
    <div class="field"><label>Provider</label>
      <select bind:value={pProvider}>{#each catalog as c}<option value={c.provider}>{c.provider}</option>{/each}</select></div>
    <div class="field"><label>Default model</label><input type="text" bind:value={pDefaultModel} placeholder="(optional)" /></div>
  </div>
  <div class="row">
    <div class="field"><label>Base URL</label><input type="text" bind:value={pBaseUrl} placeholder="(optional, for self-hosted)" /></div>
    <div class="field"><label>API key (write-only)</label><input type="password" bind:value={pApiKey} placeholder="leave blank to keep existing" /></div>
  </div>
  <label class="check" style="max-width:260px"><input type="checkbox" bind:checked={pEnabled} /> <span>Enabled</span></label>
  <div class="row" style="margin-top:12px"><button disabled={saving} on:click={save}>{saving ? 'Saving…' : 'Save provider'}</button></div>
</div>

<div class="card">
  <h2>Test generation</h2>
  {#if testErr}<div class="msg err">{testErr}</div>{/if}
  {#if testMsg}<div class="msg ok">{testMsg}</div>{/if}
  <div class="row">
    <div class="field"><label>Provider</label>
      <select bind:value={tProvider}>{#each catalog as c}<option value={c.provider}>{c.provider}</option>{/each}</select></div>
    <div class="field"><label>Model</label><input type="text" bind:value={tModel} placeholder="(provider default)" /></div>
  </div>
  <div class="field"><label>Prompt</label><textarea rows="2" bind:value={tPrompt}></textarea></div>
  <div class="row">
    <button disabled={testing} on:click={generate}>Generate</button>
    <button class="ghost" disabled={testing} on:click={stream}>Stream</button>
  </div>
  {#if testOut}<pre>{testOut}</pre>{/if}
</div>
