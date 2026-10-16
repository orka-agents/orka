import { describe, expect, it } from 'vitest'
import { AGENTS, TASKS } from '../world'
import { seedAgents } from './agents'
import { STORIES } from './tasks'
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
})
