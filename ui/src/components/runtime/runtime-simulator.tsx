import { useState } from 'react'
import { Card, CardContent } from '@/components/ui/card'
import { Button } from '@/components/ui/button'
import { Badge } from '@/components/ui/badge'
import { PageHeader } from '@/components/layout/page-header'
import { ActivitySpotlight } from './activity-spotlight'
import { TaskFlowPanel } from './task-flow-panel'
import { RuntimeTimeline } from './runtime-timeline'
import { initialSimState, stepSim, injectFailure, resetSim } from '@/lib/runtime-simulator'

/**
 * DEV/demo-only simulator. Reuses presentation panels with fully fixture-backed
 * state and Step/Run/Inject/Reset controls that mutate ONLY local state — never
 * a production Task or API. Mounted only behind a DEV-gated route, so it cannot
 * be confused with real data and cannot call mutating endpoints.
 */
export function RuntimeSimulator() {
  const [sim, setSim] = useState(initialSimState)
  const active = sim.tasks[0]
  const events = sim.events
  const latest = events[events.length - 1]

  return (
    <div className="space-y-4">
      <PageHeader
        title="Runtime Simulator"
        description="Fixture-only demo — no real tasks are touched"
        action={<Badge variant="destructive">SIMULATOR</Badge>}
      />
      <Card>
        <CardContent className="flex flex-wrap gap-2 py-4">
          <Button size="sm" onClick={() => setSim(stepSim)}>Step</Button>
          <Button size="sm" variant="destructive" onClick={() => setSim(injectFailure)}>Inject failure</Button>
          <Button size="sm" variant="outline" onClick={() => setSim(resetSim)}>Reset</Button>
        </CardContent>
      </Card>
      <div className="grid gap-4 lg:grid-cols-3">
        <div className="space-y-4 lg:col-span-2">
          <ActivitySpotlight task={active} latestEvent={latest} following={false} />
          <TaskFlowPanel task={active} events={events} />
        </div>
        <RuntimeTimeline events={events} />
      </div>
    </div>
  )
}
