import { TriangleAlert } from 'lucide-react'
import type { StatusProblem } from './status-problem'

/** Detail-page alert naming the Ready=False reason and its message. */
export function StatusProblemAlert({ problem }: { problem: StatusProblem }) {
  return (
    <div role="alert" className="flex items-start gap-3 rounded-md border border-destructive/40 bg-destructive/10 p-3 text-sm">
      <TriangleAlert className="mt-0.5 size-4 shrink-0 text-destructive" aria-hidden="true" />
      <div className="min-w-0 space-y-1">
        <p className="font-medium">{problem.reason}</p>
        {problem.message && <p className="break-words">{problem.message}</p>}
      </div>
    </div>
  )
}
