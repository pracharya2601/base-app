<script>
  import { onMount } from 'svelte'
  import { api } from '../lib/api.js'

  let scopes = []   // [{scope, description, checked}]
  let roles = []    // [{id, name, permissions, checked}]
  let keys = []
  let listErr = ''

  let keyName = '', expiresInDays = 0
  let minted = null // { name, key, warning }
  let mintErr = '', minting = false, copied = false

  async function loadScopes() {
    try {
      const r = await api('/api/superadmin/scopes')
      r.scopes.sort((a, b) => a.scope.localeCompare(b.scope))
      scopes = r.scopes.map((s) => ({ ...s, checked: s.scope === 'admin' }))
    } catch (e) { listErr = e.message }
  }
  async function loadRoles() {
    try {
      const r = await api('/api/superadmin/roles')
      roles = (r.roles || []).map((x) => ({ ...x, checked: false }))
    } catch { roles = [] }
  }
  async function loadKeys() {
    listErr = ''
    try {
      const r = await api('/api/superadmin/apikeys')
      keys = r.apiKeys || []
    } catch (e) { listErr = e.message }
  }
  async function mint() {
    mintErr = ''; minted = null; copied = false
    if (!keyName.trim()) { mintErr = 'Name is required.'; return }
    minting = true
    try {
      const body = {
        name: keyName.trim(),
        scopes: scopes.filter((s) => s.checked).map((s) => s.scope),
        roles: roles.filter((r) => r.checked).map((r) => r.id),
        expiresInDays: parseInt(expiresInDays, 10) || 0,
      }
      const r = await api('/api/superadmin/apikeys', { method: 'POST', body: JSON.stringify(body) })
      minted = r
      keyName = ''
      loadKeys()
    } catch (e) { mintErr = e.message } finally { minting = false }
  }
  async function revoke(id) {
    if (!confirm('Revoke this key? This is immediate and irreversible.')) return
    try { await api('/api/superadmin/apikeys/' + id, { method: 'DELETE' }); loadKeys() }
    catch (e) { listErr = e.message }
  }
  function copy() { if (minted) { navigator.clipboard.writeText(minted.key); copied = true } }
  onMount(() => { loadScopes(); loadRoles(); loadKeys() })
</script>

<h1>API Keys</h1>
<p class="sub">Mint scoped, roled, revocable keys for machine clients. The plaintext key is shown once at mint time.</p>

<div class="card">
  <h2>Mint a key</h2>
  {#if mintErr}<div class="msg err">{mintErr}</div>{/if}
  {#if minted}
    <div class="keyout">
      <strong>Key minted: {minted.name}</strong>
      <code class="mono">{minted.key}</code>
      <button class="ghost" on:click={copy}>{copied ? 'Copied' : 'Copy'}</button>
      <p class="warn">⚠ {minted.warning || 'Store this key now — it cannot be retrieved again.'}</p>
    </div>
  {/if}
  <div class="row">
    <div class="field"><label>Name</label><input type="text" bind:value={keyName} placeholder="e.g. mcp-client" /></div>
    <div class="field"><label>Expires in days (0 = never)</label><input type="number" bind:value={expiresInDays} min="0" /></div>
  </div>
  <div class="field"><label>Scopes (control plane)</label>
    <div class="checks">
      {#each scopes as s}
        <label class="check"><input type="checkbox" bind:checked={s.checked} />
          <span><code>{s.scope}</code><br /><span class="desc">{s.description}</span></span></label>
      {/each}
    </div>
  </div>
  <div class="field"><label>Roles (data plane — the service account's RBAC)</label>
    {#if roles.length}
      <div class="checks">
        {#each roles as r}
          <label class="check"><input type="checkbox" bind:checked={r.checked} />
            <span><code>{r.name}</code><br /><span class="desc">{(r.permissions || []).join(', ') || '—'}</span></span></label>
        {/each}
      </div>
    {:else}<span class="desc">No roles defined.</span>{/if}
  </div>
  <div class="row" style="margin-top:8px"><button disabled={minting} on:click={mint}>{minting ? 'Minting…' : 'Mint key'}</button></div>
</div>

<div class="card">
  <div class="topbar"><h2 style="margin:0">Keys</h2><button class="ghost" on:click={loadKeys}>Refresh</button></div>
  {#if listErr}<div class="msg err" style="margin-top:10px">{listErr}</div>{/if}
  <table style="margin-top:8px">
    <thead><tr><th>Name</th><th>Prefix</th><th>Scopes</th><th>Roles</th><th>Status</th><th></th></tr></thead>
    <tbody>
      {#each keys as k}
        <tr>
          <td>{k.name}</td>
          <td class="mono">{k.prefix || ''}…</td>
          <td>{#each k.scopes || [] as s}<span class="pill">{s}</span> {/each}</td>
          <td>{#each k.roles || [] as r}<span class="pill">{r}</span> {:else}—{/each}</td>
          <td>{#if k.revoked}<span class="pill revoked">revoked</span>{:else}<span class="pill">active</span>{/if}</td>
          <td>{#if !k.revoked}<button class="danger" on:click={() => revoke(k.id)}>Revoke</button>{/if}</td>
        </tr>
      {:else}
        <tr><td colspan="6" class="muted">No keys yet.</td></tr>
      {/each}
    </tbody>
  </table>
</div>
