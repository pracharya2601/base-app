<script>
  import { getToken, clearToken, login } from './lib/api.js'
  import Orchestrator from './views/Orchestrator.svelte'
  import Data from './views/Data.svelte'
  import Providers from './views/Providers.svelte'
  import Images from './views/Images.svelte'
  import Keys from './views/Keys.svelte'

  let token = getToken()
  let view = 'orchestrator'
  let email = 'admin@example.com'
  let password = ''
  let loginErr = ''
  let busy = false

  async function doLogin() {
    busy = true; loginErr = ''
    try { await login(email, password); token = getToken() }
    catch (e) { loginErr = e.message }
    finally { busy = false }
  }
  function logout() { clearToken(); token = '' }

  const nav = [
    { id: 'orchestrator', label: 'Orchestrator', ico: '🤖' },
    { id: 'data', label: 'Data', ico: '▦' },
    { id: 'providers', label: 'AI Providers', ico: '✦' },
    { id: 'images', label: 'Images', ico: '🖼' },
    { id: 'keys', label: 'API Keys', ico: '⚿' },
  ]
</script>

{#if !token}
  <div class="overlay">
    <div class="card">
      <h2>base-app admin</h2>
      {#if loginErr}<div class="msg err">{loginErr}</div>{/if}
      <div class="field"><label>Email</label>
        <input type="email" bind:value={email} autocomplete="username" /></div>
      <div class="field"><label>Password</label>
        <input type="password" bind:value={password} autocomplete="current-password"
          on:keydown={(e) => e.key === 'Enter' && doLogin()} /></div>
      <button style="width:100%" disabled={busy} on:click={doLogin}>{busy ? 'Logging in…' : 'Log in'}</button>
    </div>
  </div>
{:else}
  <div class="layout">
    <aside class="sidebar">
      <div class="brand"><span class="dot"></span> base-app</div>
      <nav class="nav">
        {#each nav as n}
          <a class:active={view === n.id} on:click={() => (view = n.id)}><span class="ico">{n.ico}</span> {n.label}</a>
        {/each}
      </nav>
      <div class="sidefoot">
        <button class="ghost" style="width:100%" on:click={logout}>Log out</button>
      </div>
    </aside>
    <main class="content"><div class="content-inner">
      {#if view === 'orchestrator'}<Orchestrator />{/if}
      {#if view === 'data'}<Data />{/if}
      {#if view === 'providers'}<Providers />{/if}
      {#if view === 'images'}<Images />{/if}
      {#if view === 'keys'}<Keys />{/if}
    </div></main>
  </div>
{/if}
