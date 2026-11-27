import { getResponse, type RequestHandler } from 'msw'
import { apiError } from './shared'
import { chatHandlers } from './handlers/chat'
import { coreHandlers } from './handlers/core'
import { gatewayHandlers } from './handlers/gateways'
import { securityHandlers } from './handlers/security'
import { taskHandlers } from './handlers/tasks'

const handlers: RequestHandler[] = [...coreHandlers, ...taskHandlers, ...chatHandlers, ...securityHandlers, ...gatewayHandlers]

const LATENCY_MS = 120

function abortable(response: Response, signal: AbortSignal | null | undefined): Response {
  if (!signal || !response.body) return response
  const reader = response.body.getReader()
  const body = new ReadableStream<Uint8Array>({
    start(controller) {
      const onAbort = () => {
        try {
          controller.error(signal.reason ?? new DOMException('The operation was aborted.', 'AbortError'))
        } catch {
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
      }
    },
    cancel(reason) {
      return reader.cancel(reason)
    },
  })
  return new Response(body, { status: response.status, statusText: response.statusText, headers: response.headers })
}

export async function handleDemoRequest(request: Request): Promise<Response> {
  if (request.signal.aborted) throw new DOMException('The operation was aborted.', 'AbortError')
  await new Promise((resolve) => setTimeout(resolve, LATENCY_MS))
  const response = (await getResponse(handlers, request)) ?? apiError(501, 'This endpoint is not available in the demo')
  return abortable(response, request.signal)
}

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
