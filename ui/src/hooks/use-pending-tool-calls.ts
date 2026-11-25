import { useEffect, useRef, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { ApiError, api } from '@/lib/api-client'
import { executionEventApiPath } from '@/lib/execution-events'
import { useAuthStore } from '@/stores/auth'
import { useUIStore } from '@/stores/ui'
import type { ListResponse } from '@/lib/list-api'
import type { DecisionView } from '@/schemas/oversight'
import type { Task, TaskEventsResponse } from '@/schemas/task'

/**
 * A tool call Orka's own Task events show as completed, for which no
 * oversight decision has arrived yet. It is provisional: nothing about it is
 * a monitor's finding.
 */
export interface PendingToolCall {
  key: string
  taskName: string
  agentName?: string
  toolName: string
  toolCallID: string
  createdAt: string
}

export interface PendingToolCallsResult {
  /** Newest first, without the ones a decision has matched. */
  pending: PendingToolCall[]
  /** Calls dropped after the expiry with no decision. */
  expired: number
  /** Tasks watched, and how many qualified before the cap. */
  watching: number
  candidates: number
}

export interface PendingToolCallsOptions {
  enabled: boolean
  /** Decisions already loaded or streamed; a matching call is not pending. */
  decisions: DecisionView[]
  taskPollMs?: number
  eventPollMs?: number
  /** Most Tasks watched at once, newest first. */
  maxTasks?: number
  /** A call with no decision after this long is dropped and counted. */
  expiryMs?: number
  /** A finished Task is watched this long after it completes. */
  finishedGraceMs?: number
}

export const pendingKey = (taskName: string, toolCallID: string) => `${taskName}\u0000${toolCallID}`

const ACTIVE_PHASES = new Set(['Pending', 'Running', 'Finalizing'])

interface TaskCursor {
  after: number
  /** Stopped for good: 401/403/404, or done and nothing left to see. */
  stopped: boolean
  /** Consecutive failures, for backoff. */
  failures: number
  nextAt: number
  finishedAt?: number
}

/** Whether a Task is worth watching now: running, or finished within the grace period. */
function watchable(task: Task, now: number, graceMs: number): boolean {
  const phase = task.status?.phase
  if (phase && ACTIVE_PHASES.has(phase)) return true
  const done = task.status?.completionTime ? Date.parse(task.status.completionTime) : NaN
  return Number.isFinite(done) && now - done <= graceMs
}

/**
 * "Seen, scoring…" rows from Orka's own Task events: each ToolCallCompleted
 * with no oversight decision yet for the same (Task, toolCallID). It polls
 * the Task list and the newest Tasks' events only while enabled, backs off on
 * errors, and never retries 401/403/404 for a Task.
 */
export function usePendingToolCalls({
  enabled,
  decisions,
  taskPollMs = 5000,
  eventPollMs = 2000,
  maxTasks = 30,
  expiryMs = 10 * 60 * 1000,
  finishedGraceMs = 2 * 60 * 1000,
}: PendingToolCallsOptions): PendingToolCallsResult {
  const namespace = useUIStore((s) => s.namespace)
  const token = useAuthStore((s) => s.token)
  const tasks = useQuery({
    queryKey: ['oversightPendingTasks', namespace],
    enabled: enabled && Boolean(token),
    queryFn: () => api.get<ListResponse<Task>>('/tasks', { namespace, limit: '200' }),
    retry: (count, error) => !(error instanceof ApiError && [401, 403, 404, 501].includes(error.status)) && count < 2,
    refetchInterval: (query) => {
      const error = query.state.error
      return enabled && !(error instanceof ApiError && [401, 403, 404, 501].includes(error.status)) ? taskPollMs : false
    },
  })

  const now = Date.now()
  const candidates = (tasks.data?.items ?? [])
    .filter((task) => watchable(task, now, finishedGraceMs))
    .sort((a, b) => (b.metadata.creationTimestamp ?? '').localeCompare(a.metadata.creationTimestamp ?? ''))
  const watched = candidates.slice(0, maxTasks)
  const watchedKey = watched.map((t) => t.metadata.name).join('\u0000')

  const decided = new Set<string>()
  for (const d of decisions) if (d.action.toolCallID) decided.add(pendingKey(d.taskName, d.action.toolCallID))
  const decidedRef = useRef(decided)
  useEffect(() => {
    decidedRef.current = decided
  })

  const callsRef = useRef(new Map<string, PendingToolCall>())
  const expiredRef = useRef(new Set<string>())
  const cursorsRef = useRef(new Map<string, TaskCursor>())
  const watchedRef = useRef(watched)
  useEffect(() => {
    watchedRef.current = watched
  })
  const [snapshot, setSnapshot] = useState<{ calls: PendingToolCall[]; expired: number }>({ calls: [], expired: 0 })
  const publish = () => setSnapshot({ calls: [...callsRef.current.values()], expired: expiredRef.current.size })

  // A namespace switch starts over.
  useEffect(() => {
    callsRef.current = new Map()
    expiredRef.current = new Set()
    cursorsRef.current = new Map()
    setSnapshot({ calls: [], expired: 0 })
  }, [namespace])

  useEffect(() => {
    if (!enabled || !watchedKey) return
    let cancelled = false
    const tick = async () => {
      const at = Date.now()
      let changed = false
      await Promise.all(watchedRef.current.map(async (task) => {
        const name = task.metadata.name
        const cursor = cursorsRef.current.get(name) ?? { after: 0, stopped: false, failures: 0, nextAt: 0 }
        cursorsRef.current.set(name, cursor)
        if (cursor.stopped || at < cursor.nextAt) return
        const done = task.status?.completionTime ? Date.parse(task.status.completionTime) : NaN
        if (!ACTIVE_PHASES.has(task.status?.phase ?? '') && Number.isFinite(done)) {
          cursor.finishedAt = done
          const open = [...callsRef.current.values()].some((c) => c.taskName === name && !decidedRef.current.has(c.key))
          // Stop a finished Task after the grace period, or once it has no pending call left.
          if (at - done > finishedGraceMs || (cursor.after > 0 && !open)) {
            cursor.stopped = true
            return
          }
        }
        try {
          const page = await api.get<TaskEventsResponse>(executionEventApiPath.taskEvents(name), {
            namespace, after: String(cursor.after), limit: '200',
          })
          cursor.failures = 0
          cursor.nextAt = 0
          for (const event of page.events ?? []) {
            cursor.after = Math.max(cursor.after, event.seq)
            if (event.type !== 'ToolCallCompleted' || !event.toolCallID) continue
            const key = pendingKey(event.taskName || name, event.toolCallID)
            if (callsRef.current.has(key) || expiredRef.current.has(key)) continue
            callsRef.current.set(key, {
              key,
              taskName: event.taskName || name,
              agentName: event.agentName || task.spec?.agentRef?.name,
              toolName: event.toolName || 'a tool',
              toolCallID: event.toolCallID,
              createdAt: event.createdAt,
            })
            changed = true
          }
          if (typeof page.latestSeq === 'number' && page.latestSeq > cursor.after && (page.events?.length ?? 0) === 0) cursor.after = page.latestSeq
        } catch (error) {
          if (error instanceof ApiError && [401, 403, 404, 501].includes(error.status)) {
            // Permanent for this Task: never retried in a loop.
            cursor.stopped = true
            return
          }
          cursor.failures = Math.min(cursor.failures + 1, 5)
          cursor.nextAt = Date.now() + eventPollMs * 2 ** cursor.failures
        }
      }))
      // Expire calls that waited too long; matched ones are dropped quietly.
      for (const [key, call] of callsRef.current) {
        if (decidedRef.current.has(key)) {
          callsRef.current.delete(key)
          changed = true
        } else if (Date.now() - Date.parse(call.createdAt) > expiryMs) {
          callsRef.current.delete(key)
          expiredRef.current.add(key)
          changed = true
        }
      }
      if (changed && !cancelled) publish()
    }
    void tick()
    const timer = setInterval(() => void tick(), eventPollMs)
    return () => {
      cancelled = true
      clearInterval(timer)
    }
  }, [enabled, watchedKey, namespace, eventPollMs, expiryMs, finishedGraceMs])

  const pending = snapshot.calls
    .filter((call) => !decided.has(call.key))
    .sort((a, b) => b.createdAt.localeCompare(a.createdAt))
  return { pending, expired: snapshot.expired, watching: watched.length, candidates: candidates.length }
}
