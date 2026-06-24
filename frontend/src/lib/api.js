// Thin fetch wrapper over the same superuser-gated endpoints the old console used.
// The token is the PocketBase superuser auth token, kept in sessionStorage.
const KEY = 'pb_admin_token'

export function getToken() { return sessionStorage.getItem(KEY) || '' }
export function setToken(t) { sessionStorage.setItem(KEY, t) }
export function clearToken() { sessionStorage.removeItem(KEY) }

export async function api(path, opts = {}) {
  const headers = { 'Content-Type': 'application/json', ...(opts.headers || {}) }
  const t = getToken()
  if (t) headers['Authorization'] = t
  const res = await fetch(path, { ...opts, headers })
  const text = await res.text()
  let data = null
  try { data = text ? JSON.parse(text) : null } catch { data = text }
  if (!res.ok) {
    const msg = (data && data.message) || res.statusText || 'request failed'
    throw new Error(msg)
  }
  return data
}

export async function login(identity, password) {
  const r = await api('/api/collections/_superusers/auth-with-password', {
    method: 'POST',
    body: JSON.stringify({ identity, password }),
  })
  setToken(r.token)
  return r
}
