import { describe, expect, it, vi } from 'vitest'
import { agentSchema, agentSystemPromptSchema, type Agent } from '@/schemas/agent'
import { agentRuntimeListSchema, agentRuntimeSchema, runtimePoolListSchema } from '@/schemas/runtime'
import { toolListItemSchema, toolSchema } from '@/schemas/tool'
import { scriptChatTurn, SUGGESTED_PROMPTS } from '../data/chat'
import { handleDemoRequest } from '../server'
import { AGENTS } from '../world'

// Chat's create tools hand off to the task domain; stub it so these tests
// cover only chat's side of the contract.
vi.mock('./tasks', async (importOriginal) => ({
  ...(await importOriginal<typeof import('./tasks')>()),
  createDemoTask: vi.fn(() => 'review-pr-1486-abc12'),
}))

const call = (path: string, init?: RequestInit) => handleDemoRequest(new Request(`http://localhost/api/v1${path}`, init))
const json = async (path: string, init?: RequestInit) => (await call(path, init)).json()

function parseSSE(text: string) {
  return text
    .split('\n\n')
    .filter(Boolean)
    .map((block) => {
      const event = block.match(/^event: (.*)$/m)?.[1]
      const data = block.match(/^data: (.*)$/m)?.[1]
      return { event, data: data ? JSON.parse(data) : undefined }
    })
}

const chat = (message: string, extra: Record<string, unknown> = {}) =>
  call('/chat', { method: 'POST', body: JSON.stringify({ message, namespace: 'default', ...extra }) })

describe('demo agents', () => {
  it('lists every world agent with a valid spec', async () => {
    const body = await json('/agents?namespace=default')
    const agents = body.items.map((item: unknown) => agentSchema.parse(item))
    expect(agents.map((agent: { metadata: { name: string } }) => agent.metadata.name)).toEqual(
      expect.arrayContaining(Object.values(AGENTS).map((agent) => agent.name)),
    )
    expect((await json('/agents?namespace=orka-system')).items).toEqual([])
  })

  it('serves the ConfigMap text of every agent whose system prompt references one', async () => {
    const agents: Agent[] = (await json('/agents?namespace=default')).items
    const referencing = agents.filter((agent) => agent.spec.systemPrompt?.configMapRef)
    expect(referencing.length).toBeGreaterThan(0)
    for (const agent of referencing) {
      const prompt = agentSystemPromptSchema.parse(await json(`/agents/${agent.metadata.name}/system-prompt?namespace=default`))
      expect(prompt.configMapRef).toEqual(agent.spec.systemPrompt?.configMapRef)
      expect(prompt.content).not.toBe('')
    }
    expect((await call(`/agents/${AGENTS.triage.name}/system-prompt?namespace=default`)).status).toBe(404)
  })

  it('creates, reads, and deletes an agent in memory', async () => {
    const spec = { runtime: { type: 'claude' }, model: { name: 'claude-sonnet-4-5' } }
    const created = await call('/agents', { method: 'POST', body: JSON.stringify({ name: 'demo-helper', namespace: 'default', spec }) })
    expect(created.status).toBe(201)
    expect(agentSchema.parse(await json('/agents/demo-helper?namespace=default')).spec.runtime).toEqual(spec.runtime)
    expect((await call('/agents', { method: 'POST', body: JSON.stringify({ name: 'demo-helper', spec }) })).status).toBe(409)
    expect((await call('/agents/demo-helper?namespace=default', { method: 'DELETE' })).status).toBe(204)
    expect((await call('/agents/demo-helper?namespace=default')).status).toBe(404)
  })

  it('rejects an invalid agent spec', async () => {
    const response = await call('/agents', { method: 'POST', body: JSON.stringify({ name: 'bad-opencode', spec: { runtime: { type: 'opencode' } } }) })
    expect(response.status).toBe(422)
  })
})

describe('demo tools', () => {
  it('lists built-in and custom tools', async () => {
    const items = (await json('/tools?namespace=default')).items.map((item: unknown) => toolListItemSchema.parse(item))
    expect(items.filter((tool: { builtin: boolean }) => tool.builtin)).toHaveLength(7)
    expect(items.filter((tool: { builtin: boolean }) => !tool.builtin).map((tool: { name: string }) => tool.name)).toEqual([
      'github_search', 'pagerduty_incident', 'datadog_query', 'slack_post',
    ])
  })

  it('serves built-in and custom tool details', async () => {
    const builtin = await json('/tools/code_exec?namespace=default')
    expect(builtin).toMatchObject({ name: 'code_exec', builtin: true })
    expect(builtin.parameters.required).toEqual(['language', 'code'])
    expect(toolSchema.parse(await json('/tools/slack_post?namespace=default')).spec.http?.url).toContain('slack.com')
    expect((await call('/tools/nope?namespace=default')).status).toBe(404)
  })
})

describe('demo runtimes', () => {
  it('lists runtime pools and external runtimes that parse', async () => {
    const pools = runtimePoolListSchema.parse(await json('/runtime-pools?namespace=default&limit=100'))
    expect(new Set(pools.items.map((pool) => pool.spec.runtime.profile.providerKind))).toEqual(new Set(['claude', 'codex', 'copilot', 'opencode']))
    expect(pools.items.every((pool) => /@sha256:[0-9a-f]{64}$/.test(pool.spec.runtime.image))).toBe(true)
    const runtimes = agentRuntimeListSchema.parse(await json('/agent-runtimes?namespace=default&limit=100'))
    expect(runtimes.items).toHaveLength(1)
    const goose = agentRuntimeSchema.parse(await json(`/agent-runtimes/${runtimes.items[0].metadata.name}?namespace=default`))
    expect(goose.spec.contractVersion).toBe('orka.harness.v2')
  })
})

describe('demo chat', () => {
  it('routes every suggested prompt to a scripted scenario', () => {
    const context = { sessionId: 'chat-test', namespace: 'default', agents: () => [] }
    for (const prompt of SUGGESTED_PROMPTS) {
      expect(scriptChatTurn(prompt, context).some((step) => step.kind === 'tool'), prompt).toBe(true)
    }
  })

  it('streams a turn in the order the UI parses', async () => {
    const events = parseSSE(await (await chat('Which agents can I use?')).text())
    expect(events.map((event) => event.event)).toEqual(['status', 'tool_call', 'tool_result', 'message', 'done'])
    expect(events[0].data).toMatchObject({ provider: 'anthropic', model: 'claude-sonnet-4-5' })
    expect(events[0].data.sessionId).toMatch(/^chat-/)
    expect(events[1].data.id).toBe(events[2].data.id)
    expect(events[2].data.result.success).toBe(true)
    expect(events[3].data.content).toContain('code-reviewer')
    expect(events[4].data.usage).toMatchObject({ llmCalls: 2, toolCalls: 1, tasksCreated: 0 })
  }, 15_000)

  it('reports tasks created by a create tool', async () => {
    const events = parseSSE(await (await chat('Review PR #1486 in checkout-service', { sessionId: 'chat-keep' })).text())
    expect(events[0].data.sessionId).toBe('chat-keep')
    const result = events.find((event) => event.event === 'tool_result')!.data
    expect(result.name).toBe('create_agent_task')
    expect(result.result).toEqual({ success: true, data: expect.objectContaining({ name: 'review-pr-1486-abc12' }) })
    expect(events.at(-1)!.data.usage.tasksCreated).toBe(1)
  }, 15_000)

  it('answers status questions from the live task list', async () => {
    const events = parseSSE(await (await chat("What's running right now?")).text())
    expect(events.find((event) => event.event === 'tool_result')!.data.result.success).toBe(true)
    expect(events.find((event) => event.event === 'message')!.data.content).toMatch(/active right now|Nothing is running/)
  }, 15_000)

  it('ends a cancelled turn with an error event', async () => {
    const response = await chat('Why did the latest tasks fail?', { sessionId: 'chat-cancel' })
    setTimeout(() => void call('/chat/chat-cancel?namespace=default', { method: 'DELETE' }), 300)
    const events = parseSSE(await response.text())
    expect(events.at(-1)).toEqual({ event: 'error', data: { error: 'chat turn cancelled' } })
  }, 15_000)
})
