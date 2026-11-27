import type { Task } from '@/schemas/task'

export const UNASSIGNED_AGENT = 'unassigned'

export function isLiveTask(task: Task): boolean {
  return task.status?.phase === 'Running'
}

function epochMs(ts?: string): number | null {
  if (!ts) return null
  const ms = new Date(ts).getTime()
  return Number.isNaN(ms) ? null : ms
}

export function elapsedSeconds(task: Task, nowMs: number): number | null {
  const start = epochMs(task.status?.startTime)
  if (start === null) return null
  return Math.max(0, Math.floor((nowMs - start) / 1000))
}

export function formatElapsed(seconds: number | null): string {
  if (seconds === null) return '—'
  if (seconds < 60) return `${seconds}s`
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m ${seconds % 60}s`
  return `${Math.floor(seconds / 3600)}h ${Math.floor((seconds % 3600) / 60)}m`
}
