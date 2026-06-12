const BASE = import.meta.env.VITE_API_URL || 'http://localhost:8080'

async function req(path, { method = 'GET', body, headers = {} } = {}) {
  const res = await fetch(BASE + path, {
    method,
    headers: { 'Content-Type': 'application/json', ...headers },
    body: body === undefined ? undefined : JSON.stringify(body),
  })
  const data = await res.json().catch(() => ({}))
  if (!res.ok) throw new Error(data.error || `Request failed (${res.status})`)
  return data
}

export const api = {
  overview: () => req('/api/overview'),
  meta: () => req('/api/meta/fields'),
  preview: (intent) => req('/api/segments/preview', { method: 'POST', body: { intent } }),
  previewSpec: (definition) => req('/api/segments/preview-spec', { method: 'POST', body: { definition } }),
  draft: (intent, definition) => req('/api/campaigns/draft', { method: 'POST', body: { intent, definition } }),
  launch: (payload, key) => req('/api/campaigns', { method: 'POST', body: payload, headers: { 'Idempotency-Key': key } }),
  campaigns: () => req('/api/campaigns'),
  campaign: (id) => req(`/api/campaigns/${id}`),
  stats: (id) => req(`/api/campaigns/${id}/stats`),
  events: (id) => req(`/api/campaigns/${id}/events`),
  narrate: (id) => req(`/api/campaigns/${id}/narrate`),
  simulateOrder: (id) => req('/api/demo/simulate-order', { method: 'POST', body: { campaign_id: id } }),
  recipients: (id) => req(`/api/campaigns/${id}/recipients`),
  comm: (id) => req(`/api/communications/${id}`),
}

export const inr = (n) =>
  '₹' + Number(n || 0).toLocaleString('en-IN', { maximumFractionDigits: 0 })
