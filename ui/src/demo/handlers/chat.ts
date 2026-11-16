import { http, HttpResponse, type RequestHandler } from 'msw'
import { agentSchema, type Agent } from '@/schemas/agent'
import type { ChatRequest } from '@/schemas/chat'
import { scriptChatTurn, type ChatStep } from '../data/chat'
import { demoAgent, promptConfigMaps, seedAgents } from '../data/agents'
import { agentRuntimes, runtimePools } from '../data/runtimes'
import { builtinTools, customTools, toolListItem } from '../data/tools'
import { api, apiError, digest, DEMO_NAMESPACE, inDemoNamespace, list, noContent, notFound, now, sseFrame, isDNSLabel } from '../shared'
import { PROVIDERS } from '../world'

// Chat turns (scripted, streamed), agents, tools, and the ACP runtime fabric.

let agents: Agent[] = seedAgents()

const sleep = (ms: number, signal: AbortSignal) =>
  new Promise<void>((resolve) => {
    if (signal.aborted) return resolve()
    const timer = setTimeout(resolve, ms)
    signal.addEventListener('abort', () => { clearTimeout(timer); resolve() }, { once: true })
  })

/** A little jitter so scripted turns don't feel mechanical. */
const jitter = (base: number) => base + Math.round(Math.random() * base * 0.5)

/** Turns in flight, by chat session, so DELETE /chat/{sessionId} can stop them. */
const activeTurns = new Map<string, AbortController>()

function chatTurn(body: ChatRequest): Response {
  const namespace = body.namespace || DEMO_NAMESPACE
  const sessionId = body.sessionId || `chat-${digest(`${Date.now()}:${Math.random()}`, 6)}`
  const provider = Object.values(PROVIDERS).find((p) => p.name === body.provider) ?? PROVIDERS.anthropic
  const model = body.model || provider.models[0]
  const steps: ChatStep[] = scriptChatTurn(body.message, { sessionId, namespace, agents: () => agents })

  const turn = new AbortController()
  activeTurns.get(sessionId)?.abort()
  activeTurns.set(sessionId, turn)
  const started = Date.now()
  const encoder = new TextEncoder()

  const stream = new ReadableStream<Uint8Array>({
    async start(controller) {
      const send = (event: string, data: unknown) => controller.enqueue(encoder.encode(sseFrame(event, data)))
      const usage = { inputTokens: 2400 + body.message.length, outputTokens: 0, llmCalls: 1, toolCalls: 0, tasksCreated: 0, duration: '' }
      try {
        send('status', { sessionId, provider: provider.name, model })
        for (const [index, step] of steps.entries()) {
          await sleep(jitter(step.kind === 'tool' ? 650 : 900), turn.signal)
          if (turn.signal.aborted) break
          if (step.kind === 'tool') {
            const id = `toolu_${digest(`${sessionId}:${started}:${index}`, 20)}`
            send('tool_call', { id, name: step.name, args: step.args })
            await sleep(jitter(700), turn.signal)
            if (turn.signal.aborted) break
            const result = await step.run()
            send('tool_result', { id, name: step.name, result })
            usage.toolCalls += 1
            usage.llmCalls += 1
            usage.inputTokens += 1800 + JSON.stringify(result).length
            usage.outputTokens += 60
            if (result.success && /create/.test(step.name) && /task/.test(step.name)) usage.tasksCreated += 1
          } else {
            const content = step.content()
            usage.outputTokens += Math.round(content.length / 3.8)
            send('message', { content })
          }
        }
        if (turn.signal.aborted) {
          send('error', { error: 'chat turn cancelled' })
        } else {
          usage.duration = `${((Date.now() - started) / 1000).toFixed(1)}s`
          send('done', { usage })
        }
        controller.close()
      } catch (err) {
        // A failed step ends the turn with an error frame; when the reader
        // went away (navigation or abort) the stream is already closed.
        try {
          send('error', { error: err instanceof Error ? err.message : 'chat turn failed' })
          controller.close()
        } catch {
          // Already closed or cancelled.
        }
      } finally {
        if (activeTurns.get(sessionId) === turn) activeTurns.delete(sessionId)
      }
    },
  })
  return new HttpResponse(stream, { headers: { 'Content-Type': 'text/event-stream', 'Cache-Control': 'no-cache' } })
}

function findAgent(name: string) {
  return agents.find((agent) => agent.metadata.name === name)
}

export const chatHandlers: RequestHandler[] = [
  // Chat
  http.post(api('/chat'), async ({ request }) => {
    const body = (await request.json()) as ChatRequest
    if (!body?.message?.trim()) return apiError(400, 'message is required')
    return chatTurn(body)
  }),
  http.delete(api('/chat/:sessionId'), ({ params }) => {
    activeTurns.get(String(params.sessionId))?.abort()
    return noContent()
  }),

  // Agents
  http.get(api('/agents'), ({ request }) => list(inDemoNamespace(request) ? agents : [])),
  http.get(api('/agents/:name'), ({ request, params }) => {
    const agent = inDemoNamespace(request) ? findAgent(String(params.name)) : undefined
    return agent ? HttpResponse.json(agent) : notFound(`agent "${params.name}"`)
  }),
  http.get(api('/agents/:name/system-prompt'), ({ request, params }) => {
    const agent = inDemoNamespace(request) ? findAgent(String(params.name)) : undefined
    if (!agent) return notFound(`agent "${params.name}"`)
    const ref = agent.spec.systemPrompt?.configMapRef
    if (!ref) return notFound('ConfigMap system prompt')
    const content = promptConfigMaps[ref.name]?.[ref.key]
    return content === undefined ? notFound(`key "${ref.key}" in ConfigMap "${ref.name}"`) : HttpResponse.json({ configMapRef: ref, content })
  }),
  http.post(api('/agents'), async ({ request }) => {
    const body = (await request.json()) as { name?: string; namespace?: string; spec?: unknown }
    const name = body.name?.trim() ?? ''
    if (!isDNSLabel(name)) return apiError(400, `invalid agent name "${name}": must be a lowercase RFC 1123 label`)
    if ((body.namespace || DEMO_NAMESPACE) !== DEMO_NAMESPACE) return apiError(403, `the demo only allows creating agents in namespace "${DEMO_NAMESPACE}"`)
    if (findAgent(name)) return apiError(409, `agents.orka.ai "${name}" already exists`)
    const status: Agent['status'] = {
      activeTasks: 0,
      conditions: [{ type: 'Ready', status: 'True', reason: 'Validated', message: 'Agent configuration is valid', lastTransitionTime: now() }],
    }
    const parsed = agentSchema.safeParse(demoAgent(name, (body.spec ?? {}) as Agent['spec'], now(), status))
    if (!parsed.success) return apiError(422, `Agent "${name}" is invalid: ${parsed.error.issues.map((issue) => issue.message).join('; ')}`)
    agents = [...agents, parsed.data]
    return HttpResponse.json(parsed.data, { status: 201 })
  }),
  http.delete(api('/agents/:name'), ({ request, params }) => {
    const name = String(params.name)
    if (!inDemoNamespace(request) || !findAgent(name)) return notFound(`agent "${name}"`)
    agents = agents.filter((agent) => agent.metadata.name !== name)
    return noContent()
  }),

  // Tools: built-ins are listed in every namespace, custom Tool CRs only in the demo one.
  http.get(api('/tools'), ({ request }) =>
    list([...builtinTools, ...(inDemoNamespace(request) ? customTools.map(toolListItem) : [])]),
  ),
  http.get(api('/tools/:name'), ({ request, params }) => {
    const name = String(params.name)
    const builtin = builtinTools.find((tool) => tool.name === name)
    if (builtin) return HttpResponse.json(builtin)
    const custom = inDemoNamespace(request) ? customTools.find((tool) => tool.metadata.name === name) : undefined
    return custom ? HttpResponse.json(custom) : notFound(`tool "${name}"`)
  }),

  // ACP runtime fabric
  http.get(api('/runtime-pools'), ({ request }) => list(inDemoNamespace(request) ? runtimePools : [])),
  http.get(api('/agent-runtimes'), ({ request }) => list(inDemoNamespace(request) ? agentRuntimes : [])),
  http.get(api('/agent-runtimes/:name'), ({ request, params }) => {
    const runtime = inDemoNamespace(request) ? agentRuntimes.find((item) => item.metadata.name === params.name) : undefined
    return runtime ? HttpResponse.json(runtime) : notFound(`agentruntime "${params.name}"`)
  }),
]
