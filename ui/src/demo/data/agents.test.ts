import { describe, expect, it } from 'vitest'
import { AGENTS, TASKS } from '../world'
import { isAutonomousAgent, seedAgents } from './agents'
import { liveStory, STORIES, type Beat } from './tasks'
import { builtinTools } from './tools'

// The controller refuses to run a Task whose agent breaks these rules, so the
// demo must not show one.
describe('demo agents', () => {
  const agents = seedAgents()

  it('use approvals only with autonomous coordination', () => {
    for (const agent of agents) {
      const coordination = agent.spec.coordination
      const asks = agent.spec.tools?.some((tool) => tool.name === 'request_approval' && tool.enabled !== false)
      const gates = (coordination?.approvalRequiredTools?.length ?? 0) > 0
      if (asks || gates) expect(Boolean(coordination?.enabled && coordination.autonomous), agent.metadata.name).toBe(true)
    }
  })

  it('gate only custom tools, and their Tasks never call one without an approval', () => {
    const builtins = new Set(builtinTools.map((tool) => tool.name))
    for (const agent of agents) {
      const gated = new Set(agent.spec.coordination?.approvalRequiredTools ?? [])
      for (const tool of gated) expect(builtins.has(tool), `${agent.metadata.name}: ${tool}`).toBe(false)

      const stories = TASKS.filter((seed) => seed.agent && AGENTS[seed.agent].name === agent.metadata.name)
        .flatMap((seed) => STORIES[seed.name] ?? [])
      for (const story of stories) {
        const events = [...story.beats, ...(story.declined?.beats ?? [])].flatMap((beat) => beat.events ?? [])
        const approved = new Set(events.filter((event) => event.type === 'ApprovalRequested').map((event) => event.toolCallID))
        for (const event of events) {
          if (event.type === 'ToolCallStarted' && gated.has(event.toolName ?? '')) {
            expect(approved.has(event.toolCallID), `${agent.metadata.name}: ${event.summary}`).toBe(true)
          }
        }
      }
    }
  })

  it('end every autonomous coordinator story by marking its goal complete', () => {
    // Otherwise the controller would start another iteration, up to maxIterations.
    const goalComplete = (beats: Beat[]) => {
      const plans = beats.flatMap((beat) => beat.events ?? []).filter((event) => event.type === 'PlanUpdated')
      return (plans.at(-1)?.content as { goalComplete?: boolean } | undefined)?.goalComplete === true
    }
    const autonomous = TASKS.filter((seed) => seed.agent && seed.type === 'ai' && isAutonomousAgent(seed.agent) && STORIES[seed.name])
    expect(autonomous.length).toBeGreaterThan(0)
    for (const seed of autonomous) {
      const story = STORIES[seed.name]
      expect(goalComplete(story.beats), seed.name).toBe(true)
      if (story.declined) expect(goalComplete(story.declined.beats), `${seed.name} declined`).toBe(true)
    }
    expect(goalComplete(liveStory('ai', { agent: 'coordinator', prompt: 'hi' }).beats)).toBe(true)
    expect(goalComplete(liveStory('ai', { agent: 'triage', prompt: 'hi' }).beats)).toBe(false)
  })
})
