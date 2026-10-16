import { HttpResponse } from 'msw'

// Helpers shared by the demo API handlers. The demo build serves the dashboard
// from a static host with no controller behind it, so every /api/v1 request is
// answered in the browser from the in-memory fixtures under src/demo.

/** The one namespace that holds demo data; every other namespace is empty. */
export const DEMO_NAMESPACE = 'default'

/** Fixture timestamps are relative to page load so the data always looks fresh. */
export const NOW = Date.now()

/** ISO timestamp `minutes` before page load (negative values are in the future). */
export const ago = (minutes: number) => new Date(NOW - minutes * 60_000).toISOString()

/** ISO timestamp for the current moment, for objects created during the demo. */
export const now = () => new Date().toISOString()

/** MSW path pattern for an API route, matching any origin and base path. */
export const api = (path: string) => `*/api/v1${path}`

/** Whether a request targets the demo namespace (no namespace means the default). */
export function inDemoNamespace(request: Request): boolean {
  const namespace = new URL(request.url).searchParams.get('namespace')
  return !namespace || namespace === DEMO_NAMESPACE
}

/** Query parameter value, or undefined when absent or empty. */
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

/** Deterministic UUID-shaped id derived from a seed string. */
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

/** Whether a name is a lowercase RFC 1123 label, as Kubernetes object names here must be. */
export const isDNSLabel = (name: string) => name.length <= 63 && /^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/.test(name)

/** A task's metadata.uid; usage records carry it to link back to the live task. */
export const taskUID = (name: string, createdMs: number) => uid(`task:${name}:${createdMs}`)

/** Deterministic hex digest-like string (e.g. git SHAs, sha256 digests). */
export function digest(seed: string, length = 40): string {
  let out = ''
  let i = 0
  while (out.length < length) out += uid(`${seed}:${i++}`).replace(/-/g, '')
  return out.slice(0, length)
}

const sleep = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms))

/**
 * A streaming response that writes each chunk after its delay. Used for SSE
 * (execution events, chat) and slow log streams; the stream closes after the
 * last chunk unless `holdOpen` is set, in which case it stays open until the
 * client aborts.
 */
export function streamResponse(
  chunks: Array<{ data: string; delayMs?: number }>,
  { contentType = 'text/event-stream', holdOpen = false, signal }: { contentType?: string; holdOpen?: boolean; signal?: AbortSignal } = {},
) {
  const encoder = new TextEncoder()
  const body = new ReadableStream<Uint8Array>({
    async start(controller) {
      try {
        for (const chunk of chunks) {
          if (chunk.delayMs) await sleep(chunk.delayMs)
          if (signal?.aborted) return
          controller.enqueue(encoder.encode(chunk.data))
        }
        if (holdOpen) {
          // Heartbeats keep an idle stream alive the way the controller does.
          while (!signal?.aborted) {
            await sleep(15_000)
            if (signal?.aborted) break
            controller.enqueue(encoder.encode(': heartbeat\n\n'))
          }
          return
        }
        controller.close()
      } catch {
        // The reader went away; nothing to clean up.
      }
    },
  })
  return new HttpResponse(body, { headers: { 'Content-Type': contentType, 'Cache-Control': 'no-cache' } })
}

/** One SSE block. */
export const sseFrame = (event: string, data: unknown) => `event: ${event}\ndata: ${JSON.stringify(data)}\n\n`
