import { HttpResponse } from 'msw'

export const DEMO_NAMESPACE = 'default'

export const NOW = Date.now()

export const ago = (minutes: number) => new Date(NOW - minutes * 60_000).toISOString()

export const now = () => new Date().toISOString()

export const api = (path: string) => `*/api/v1${path}`

export function inDemoNamespace(request: Request): boolean {
  const namespace = new URL(request.url).searchParams.get('namespace')
  return !namespace || namespace === DEMO_NAMESPACE
}

export function query(request: Request, key: string): string | undefined {
  return new URL(request.url).searchParams.get(key) || undefined
}

export function list<T>(items: T[]) {
  return HttpResponse.json({ items, metadata: {} })
}

export function apiError(status: number, message: string) {
  return HttpResponse.json({ error: { code: status, message } }, { status })
}

export const notFound = (what: string) => apiError(404, `${what} not found`)

export const noContent = () => new HttpResponse(null, { status: 204 })

export function uid(seed: string): string {
  let h1 = 0x811c9dc5
  let h2 = 0x01000193
  for (let i = 0; i < seed.length; i++) {
    const c = seed.charCodeAt(i)
    h1 = Math.imul(h1 ^ c, 0x01000193) >>> 0
    h2 = Math.imul(h2 ^ c, 0x5bd1e995) >>> 0
  }
  const hex = (h1.toString(16).padStart(8, '0') + h2.toString(16).padStart(8, '0')).repeat(2)
  return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-4${hex.slice(13, 16)}-a${hex.slice(17, 20)}-${hex.slice(20, 32)}`
}

export const isDNSLabel = (name: string) => name.length <= 63 && /^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/.test(name)

export const taskUID = (name: string, createdMs: number) => uid(`task:${name}:${createdMs}`)

export function digest(seed: string, length = 40): string {
  let out = ''
  let i = 0
  while (out.length < length) out += uid(`${seed}:${i++}`).replace(/-/g, '')
  return out.slice(0, length)
}

export const sseFrame = (event: string, data: unknown) => `event: ${event}\ndata: ${JSON.stringify(data)}\n\n`
