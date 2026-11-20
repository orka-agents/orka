import { getResponse, type RequestHandler } from 'msw'
import { apiError } from './shared'
import { chatHandlers } from './handlers/chat'
import { coreHandlers } from './handlers/core'
import { gatewayHandlers } from './handlers/gateways'
import { securityHandlers } from './handlers/security'
import { taskHandlers } from './handlers/tasks'

const handlers: RequestHandler[] = [...coreHandlers, ...taskHandlers, ...chatHandlers, ...securityHandlers, ...gatewayHandlers]

/** Simulated network latency, so loading states read naturally. */
const LATENCY_MS = 120

/**
 * Re-wraps a mocked streaming body so aborting the request errors the body the
 * way a real fetch does, instead of leaving the reader pending forever.
 */
function abortable(response: Response, signal: AbortSignal | null | undefined): Response {
  if (!signal || !response.body) return response
  const reader = response.body.getReader()
  const body = new ReadableStream<Uint8Array>({
    start(controller) {
      const onAbort = () => {
        try {
          controller.error(signal.reason ?? new DOMException('The operation was aborted.', 'AbortError'))
        } catch {
          // Already closed.
        }
        void reader.cancel().catch(() => {})
      }
      if (signal.aborted) onAbort()
      else signal.addEventListener('abort', onAbort, { once: true })
    },
    async pull(controller) {
      try {
        const { done, value } = await reader.read()
        if (done) controller.close()
        else controller.enqueue(value)
      } catch {
        // Errored by an abort above.
      }
    },
    cancel(reason) {
      return reader.cancel(reason)
    },
  })
  return new Response(body, { status: response.status, statusText: response.statusText, headers: response.headers })
}

/** Answers one API request from the demo fixtures. */
export async function handleDemoRequest(request: Request): Promise<Response> {
  if (request.signal.aborted) throw new DOMException('The operation was aborted.', 'AbortError')
  await new Promise((resolve) => setTimeout(resolve, LATENCY_MS))
  const response = (await getResponse(handlers, request)) ?? apiError(501, 'This endpoint is not available in the demo')
  return abortable(response, request.signal)
}

/** Routes the dashboard's /api/ requests to the demo fixtures; everything else goes to the network. */
export function installDemoFetch() {
  const networkFetch = window.fetch.bind(window)
  window.fetch = async (input: RequestInfo | URL, init?: RequestInit) => {
    const request = new Request(input, init)
    const url = new URL(request.url)
    if (url.origin !== window.location.origin || !url.pathname.startsWith('/api/')) {
      return networkFetch(input, init)
    }
    return handleDemoRequest(request)
  }
}
