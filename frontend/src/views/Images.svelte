<script>
  import { onMount } from 'svelte'
  import { api } from '../lib/api.js'

  let catalog = []
  let images = []
  let listErr = ''

  let iProvider = '', iModel = '', iPrompt = '', iSize = '', iCount = 1
  let genMsg = '', genErr = '', generating = false

  async function loadCatalog() {
    try {
      const { providers } = await api('/api/ai/image-catalog')
      providers.sort((a, b) => a.provider.localeCompare(b.provider))
      catalog = providers
      if (!iProvider && providers.length) iProvider = providers[0].provider
    } catch { /* leave empty */ }
  }
  async function loadImages() {
    listErr = ''
    try {
      const d = await api('/api/collections/_aiImages/records?perPage=24&sort=-created')
      images = d.items || []
    } catch (e) { listErr = e.message }
  }
  function fileUrl(r) { return `${location.origin}/api/files/_aiImages/${r.id}/${r.file}` }
  async function generate() {
    genMsg = 'Generating… (can take 10–30s)'; genErr = ''; generating = true
    try {
      const body = { prompt: iPrompt }
      if (iModel.trim()) body.model = iModel.trim()
      if (iSize.trim()) body.size = iSize.trim()
      const c = parseInt(iCount, 10); if (!isNaN(c) && c > 0) body.count = c
      const d = await api(`/api/ai/${iProvider}/image`, { method: 'POST', body: JSON.stringify(body) })
      genMsg = `Generated ${d.count} image(s).`
      loadImages()
    } catch (e) { genMsg = ''; genErr = e.message } finally { generating = false }
  }
  onMount(async () => { await loadCatalog(); loadImages() })
</script>

<h1>Images</h1>
<p class="sub">Generate images through the proxy (openai/google/vertex/azure) — stored as files with preview URLs.</p>

<div class="card">
  <h2>Generate</h2>
  {#if genErr}<div class="msg err">{genErr}</div>{/if}
  {#if genMsg}<div class="msg ok">{genMsg}</div>{/if}
  <div class="row">
    <div class="field"><label>Provider</label>
      <select bind:value={iProvider}>{#each catalog as c}<option value={c.provider}>{c.provider}</option>{/each}</select></div>
    <div class="field"><label>Model</label><input type="text" bind:value={iModel} placeholder="e.g. gpt-image-1" /></div>
  </div>
  <div class="field"><label>Prompt</label><textarea rows="2" bind:value={iPrompt} placeholder="Describe the image…"></textarea></div>
  <div class="row">
    <div class="field"><label>Size</label><input type="text" bind:value={iSize} placeholder="(optional, e.g. 1024x1024)" /></div>
    <div class="field"><label>Count</label><input type="number" bind:value={iCount} min="1" /></div>
  </div>
  <div class="row"><button disabled={generating} on:click={generate}>{generating ? 'Generating…' : 'Generate image'}</button></div>
</div>

<div class="card">
  <div class="topbar"><h2 style="margin:0">Gallery</h2><button class="ghost" on:click={loadImages}>Refresh</button></div>
  {#if listErr}<div class="msg err" style="margin-top:10px">{listErr}</div>{/if}
  {#if images.length}
    <div class="gallery">
      {#each images as r}
        <figure>
          <a href={fileUrl(r)} target="_blank" rel="noopener"><img src={fileUrl(r)} loading="lazy" alt="" /></a>
          <figcaption><b>{r.provider || ''}</b> · {r.model || ''}<br />{(r.prompt || '').slice(0, 90)}</figcaption>
        </figure>
      {/each}
    </div>
  {:else}
    <p class="sub" style="margin-top:10px">No images yet.</p>
  {/if}
</div>
