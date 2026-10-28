// API 封装：统一错误处理与 JSON 解析
const BASE = ''

async function req(method, url, body) {
  const resp = await fetch(BASE + url, {
    method,
    headers: body ? { 'Content-Type': 'application/json' } : undefined,
    body: body ? JSON.stringify(body) : undefined,
  })
  const text = await resp.text()
  let data = null
  try { data = text ? JSON.parse(text) : null } catch { data = { raw: text } }
  if (!resp.ok) {
    const msg = (data && (data.error || data.message)) || `HTTP ${resp.status}`
    const err = new Error(msg)
    err.status = resp.status
    err.data = data
    throw err
  }
  return data
}

export const api = {
  get: (url) => req('GET', url),
  post: (url, body) => req('POST', url, body),
  put: (url, body) => req('PUT', url, body),
}

// 水位条颜色分档
export function wmClass(v) {
  if (v >= 0.95) return 'wm-bad'
  if (v >= 0.9) return 'wm-warn'
  return 'wm-ok'
}
export function pct(v) { return (v * 100).toFixed(1) + '%' }
export function fmtKbps(k) {
  if (k >= 1_000_000) return (k / 1_000_000).toFixed(1) + ' Gbps'
  if (k >= 1_000) return (k / 1_000).toFixed(0) + ' Mbps'
  return k + ' Kbps'
}
