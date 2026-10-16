import { afterEach, describe, expect, it, vi } from 'vitest'
import { taskSchema } from '@/schemas/task'
import {
  forkTaskResponseSchema,
  listExecutionEventsResponseSchema,
  listTaskApprovalsResponseSchema,
  taskTraceSchema,
} from '@/schemas/execution-event'
import { listArtifactsResponseSchema } from '@/schemas/artifact'
import { sessionListItemSchema, sessionSchema, transcriptMessageSchema } from '@/schemas/session'
import { SSEFrameBuffer } from '@/lib/execution-events'
import { handleDemoRequest } from '../server'
import { TASKS } from '../world'
import { cancelDemoTask, createDemoTask } from './tasks'

const BASE = 'http://localhost/api/v1'

async function call(path: string, init?: RequestInit) {
  const url = `${BASE}${path}${path.includes('?') ? '&' : '?'}namespace=default`
  return handleDemoRequest(new Request(url, init))
}

async function json<T = unknown>(path: string, init?: RequestInit): Promise<T> {
  const response = await call(path, init)
  expect(response.ok, `${path} -> ${response.status}`).toBe(true)
  return (await response.json()) as T
}

const post = (body: unknown): RequestInit => ({ method: 'POST', body: JSON.stringify(body), headers: { 'Content-Type': 'application/json' } })

/** Moves the demo clock forward; only Date is faked so request latency still elapses. */
function advanceClock(ms: number) {
  vi.setSystemTime(Date.now() + ms)
}

afterEach(() => {
  vi.useRealTimers()
})

describe('demo task API', () => {
  it('serves every catalog task, schema-valid, with valid detail endpoints', async () => {
    const list = await json<{ items: unknown[] }>('/tasks')
    const tasks = list.items.map((item) => taskSchema.parse(item))
    expect(tasks.map((t) => t.metadata.name)).toEqual(expect.arrayContaining(TASKS.map((seed) => seed.name)))

    await Promise.all(tasks.map(async (task) => {
      const name = task.metadata.name
      const [events, trace, approvals, artifacts, children] = await Promise.all([
        json(`/tasks/${name}/events?after=0&limit=1000`),
        json(`/tasks/${name}/trace`),
        json(`/tasks/${name}/approvals`),
        json(`/tasks/${name}/artifacts`),
        json<{ items: unknown[] }>(`/tasks/${name}/children`),
      ])
      listExecutionEventsResponseSchema.parse(events)
      taskTraceSchema.parse(trace)
      listTaskApprovalsResponseSchema.parse(approvals)
      listArtifactsResponseSchema.parse(artifacts)
      children.items.forEach((child) => taskSchema.parse(child))
    }))
  })

  it('matches each seed phase and finished tasks carry their results', async () => {
    const { items } = await json<{ items: Array<{ metadata: { name: string }; status?: { phase?: string } }> }>('/tasks')
    for (const seed of TASKS) {
      const task = items.find((item) => item.metadata.name === seed.name)
      expect(task?.status?.phase, seed.name).toBe(seed.phase)
    }
    const result = await json<{ result: string }>('/tasks/review-pr-1479-b7n2k/result')
    expect(JSON.parse(result.result).verdict).toBe('APPROVE')
    expect((await call('/tasks/db-migrate-orders-v42-e2l9o/result')).status).toBe(404)
  })

  it('links coordinator children and scheduled runs to their parents', async () => {
    const plan = taskSchema.parse(await json('/tasks/release-v2-14-0-plan-r2w8z'))
    expect(plan.status?.childTasks?.map((c) => c.name).sort()).toEqual(['release-v2-14-0-blockers-p3c5v', 'release-v2-14-0-notes-h4t6n'])
    expect(plan.plan?.progressPct).toBeGreaterThan(0)

    const parent = taskSchema.parse(await json('/tasks/deps-weekly-bump'))
    const runs = await json<{ items: unknown[] }>('/tasks/deps-weekly-bump/children')
    const runTasks = runs.items.map((item) => taskSchema.parse(item))
    expect(runTasks).toHaveLength(2)
    expect(runTasks.every((run) => run.metadata.ownerReferences?.[0]?.uid === parent.metadata.uid)).toBe(true)
    expect(parent.status?.nextScheduleTime).toBeTruthy()
  })

  it('serves logs as JSON while running, text when finished, and 404 for runtime-pool tasks', async () => {
    const running = await json<{ logs: string }>('/tasks/checkout-latency-investigation-7k2qp/logs?tailLines=200')
    expect(running.logs).toContain('model request')
    const finished = await call('/tasks/terraform-drift-check-u5r1a/logs')
    expect(await finished.text()).toContain('No changes. Your infrastructure matches the configuration.')
    expect((await call('/tasks/fix-cart-rounding-bug-x9m4d/logs')).status).toBe(404)
  })

  it('streams events and ends a finished task with stream_complete', async () => {
    const response = await call('/tasks/triage-issue-3391-m2x7c/stream?after=0')
    const text = await response.text()
    const frames = new SSEFrameBuffer().push(text)
    const events = frames.filter((frame) => frame.kind === 'event')
    const complete = frames.find((frame) => frame.kind === 'complete')
    expect(events.length).toBeGreaterThan(5)
    expect(complete && complete.kind === 'complete' && complete.complete.type).toBe('TaskSucceeded')
  })

  it('holds the incident on its approval until it is decided, then finishes', async () => {
    vi.useFakeTimers({ toFake: ['Date'], now: Date.now() })
    const name = 'checkout-latency-investigation-7k2qp'
    const before = taskSchema.parse(await json(`/tasks/${name}`))
    expect(before.status?.conditions?.some((c) => c.type === 'WaitingForApproval' && c.status === 'True')).toBe(true)

    advanceClock(10 * 60_000)
    expect(taskSchema.parse(await json(`/tasks/${name}`)).status?.phase).toBe('Running')

    const { approvals } = listTaskApprovalsResponseSchema.parse(await json(`/tasks/${name}/approvals`))
    const pending = approvals.find((a) => a.status === 'pending')
    expect(pending?.id).toBe('apr-rollback-checkout')

    const decided = await json<{ status: string }>(`/tasks/${name}/approvals/${pending!.id}/decision`, post({ decision: 'approve' }))
    expect(decided.status).toBe('approved')
    expect((await call(`/tasks/${name}/approvals/${pending!.id}/decision`, post({ decision: 'approve' }))).status).toBe(409)

    advanceClock(3 * 60_000)
    const after = taskSchema.parse(await json(`/tasks/${name}`))
    expect(after.status?.phase).toBe('Succeeded')
    expect((await json<{ result: string }>(`/tasks/${name}/result`)).result).toContain('resolved')
  })

  it('runs a task created through POST /tasks from Pending to Succeeded', async () => {
    vi.useFakeTimers({ toFake: ['Date'], now: Date.now() })
    const created = await call('/tasks', post({ name: 'demo-created-task', namespace: 'default', type: 'ai', agentRef: { name: 'issue-triager' }, ai: { prompt: 'Summarize open P1 bugs' } }))
    expect(created.status).toBe(201)
    expect(taskSchema.parse(await created.json()).status?.phase).toBe('Pending')
    expect((await call('/tasks', post({ name: 'demo-created-task', type: 'ai' }))).status).toBe(409)
    expect((await call('/tasks', post({ name: 'other-ns', namespace: 'orka-system', type: 'ai' }))).status).toBe(403)

    advanceClock(10_000)
    expect(taskSchema.parse(await json('/tasks/demo-created-task')).status?.phase).toBe('Running')

    advanceClock(2 * 60_000)
    const done = taskSchema.parse(await json('/tasks/demo-created-task'))
    expect(done.status?.phase).toBe('Succeeded')
    expect((await json<{ result: string }>('/tasks/demo-created-task/result')).result).toContain('Summarize open P1 bugs')

    expect((await call('/tasks/demo-created-task', { method: 'DELETE' })).status).toBe(204)
    expect((await call('/tasks/demo-created-task')).status).toBe(404)
  })

  it('creates tasks for other demo domains and lists their session', async () => {
    const name = createDemoTask({ agent: 'coordinator', prompt: 'Draft the Q4 roadmap update', session: 'chat-demo-session', createdBy: 'chat' })
    taskSchema.parse(await json(`/tasks/${name}`))
    const sessions = await json<{ items: unknown[] }>('/sessions')
    const items = sessions.items.map((item) => sessionListItemSchema.parse(item))
    expect(items.find((s) => s.name === 'chat-demo-session')?.activeTask).toBe(name)
  })

  it('cancels a running task for other demo domains', async () => {
    vi.useFakeTimers({ toFake: ['Date'], now: Date.now() })
    const name = createDemoTask({ agent: 'fixer', prompt: 'Implement issue #2219', repo: 'checkout', createdBy: 'monitor' })
    advanceClock(10_000)
    expect(taskSchema.parse(await json(`/tasks/${name}`)).status?.phase).toBe('Running')

    cancelDemoTask(name, 'Cancelled: repository monitor stopped by command')
    advanceClock(5 * 60_000)
    const task = taskSchema.parse(await json(`/tasks/${name}`))
    expect(task.status?.phase).toBe('Cancelled')
    const { events } = listExecutionEventsResponseSchema.parse(await json(`/tasks/${name}/events`))
    expect(events.at(-1)).toMatchObject({ type: 'TaskCancelled', summary: 'Cancelled: repository monitor stopped by command' })
  })

  it('forks a finished container task with its own container and keeps its outcome', async () => {
    const source = 'db-migrate-orders-v42-e2l9o'
    const fork = forkTaskResponseSchema.parse(await json(`/tasks/${source}/fork`, post({})))
    const forked = taskSchema.parse(await json(`/tasks/${fork.newTaskName}`))
    expect(forked.spec).toMatchObject({ type: 'container', image: 'ghcr.io/northwind-labs/checkout-migrate:v42', command: ['migrate', 'up'] })

    // The fork events land after the outcome; the stream still reports the outcome.
    const frames = new SSEFrameBuffer().push(await (await call(`/tasks/${source}/stream?after=0`)).text())
    const events = frames.flatMap((frame) => (frame.kind === 'event' ? [frame.event] : []))
    const complete = frames.find((frame) => frame.kind === 'complete')
    expect(events.at(-1)?.type).toBe('TaskForkCreated')
    expect(complete && complete.kind === 'complete' && complete.complete).toEqual({ lastSeq: events.at(-1)?.seq, type: 'TaskFailed' })
  })

  it('rejects task names the API could not route to', async () => {
    expect((await call('/tasks', post({ name: 'My Task/1', type: 'ai', agentRef: { name: 'issue-triager' } }))).status).toBe(400)
    expect((await call('/tasks/review-pr-1479-b7n2k/fork', post({ newTaskName: 'bad name' }))).status).toBe(400)
  })

  it('forks a task into a new one that records its source', async () => {
    const response = forkTaskResponseSchema.parse(await json('/tasks/review-pr-1479-b7n2k/fork', post({ afterSeq: 5 })))
    expect(response.forkContext.events.every((e) => e.seq <= 5)).toBe(true)
    const fork = taskSchema.parse(await json(`/tasks/${response.newTaskName}`))
    expect(fork.metadata.annotations?.['orka.ai/fork-source-task']).toBe('review-pr-1479-b7n2k')
  })

  it('serves sessions with transcripts and merged event streams', async () => {
    const { items } = await json<{ items: unknown[] }>('/sessions')
    for (const item of items) {
      const listItem = sessionListItemSchema.parse(item)
      const session = sessionSchema.parse(await json(`/sessions/${listItem.name}`))
      session.transcript?.split('\n').filter(Boolean).forEach((line) => transcriptMessageSchema.parse(JSON.parse(line)))
      const events = listExecutionEventsResponseSchema.parse(await json(`/sessions/${listItem.name}/events?after=0&limit=1000`))
      expect(events.events.map((e) => e.seq)).toEqual(events.events.map((_, i) => i + 1))
    }
    const incidents = listExecutionEventsResponseSchema.parse(await json('/sessions/slack-incidents-c7f3e1/events'))
    expect(new Set(incidents.events.map((e) => e.taskStreamID)).size).toBe(2)
  })

  it('keeps other namespaces empty', async () => {
    const response = await handleDemoRequest(new Request(`${BASE}/tasks?namespace=orka-system`))
    expect(((await response.json()) as { items: unknown[] }).items).toEqual([])
    expect((await handleDemoRequest(new Request(`${BASE}/tasks/review-pr-1479-b7n2k?namespace=orka-system`))).status).toBe(404)
  })
})
