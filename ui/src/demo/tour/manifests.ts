import { stringify } from 'yaml'
import { seedAgents } from '../data/agents'
import { gatewayBindings } from '../data/gateways'
import { monitors } from '../data/monitors'
import { AGENTS, TASKS } from '../world'

/**
 * A resource trimmed to the fields a tour stop talks about. Each is cut from
 * the same sample object the page shows, so the two cannot disagree.
 */
export interface Manifest {
  apiVersion: string
  kind: string
  metadata: { name: string }
  spec: Record<string, unknown>
}

function named<T extends { metadata: { name: string } }>(items: T[], kind: string, name: string): T {
  const item = items.find((candidate) => candidate.metadata.name === name)
  if (!item) throw new Error(`demo tour: no sample ${kind} named ${name}`)
  return item
}

export function taskManifest(name: string): Manifest {
  const seed = TASKS.find((task) => task.name === name)
  if (!seed?.agent) throw new Error(`demo tour: no sample agent Task named ${name}`)
  return {
    apiVersion: 'core.orka.ai/v1alpha1',
    kind: 'Task',
    metadata: { name },
    spec: {
      type: seed.type,
      agentRef: { name: AGENTS[seed.agent].name },
      ...(seed.type === 'ai' ? { ai: { prompt: seed.prompt } } : { prompt: seed.prompt }),
    },
  }
}

export function agentManifest(name: string): Manifest {
  const { spec } = named(seedAgents(), 'Agent', name)
  const coordination = spec.coordination
  return {
    apiVersion: 'core.orka.ai/v1alpha1',
    kind: 'Agent',
    metadata: { name },
    spec: {
      providerRef: spec.providerRef,
      model: spec.model && { name: spec.model.name },
      systemPrompt: spec.systemPrompt,
      tools: spec.tools?.map((tool) => ({ name: tool.name })),
      coordination: coordination && { enabled: coordination.enabled, autonomous: coordination.autonomous, maxIterations: coordination.maxIterations },
    },
  }
}

export function monitorManifest(name: string): Manifest {
  const { spec } = named(monitors, 'RepositoryMonitor', name)
  const labels = spec.triggers?.github?.labels
  return {
    apiVersion: 'core.orka.ai/v1alpha1',
    kind: 'RepositoryMonitor',
    metadata: { name },
    spec: {
      repoURL: spec.repoURL,
      schedule: spec.schedule,
      agents: spec.agents && { reviewer: spec.agents.reviewer, implementer: spec.agents.implementer, repairer: spec.agents.repairer },
      // Issue labels default to orka:implement and the other orka:* labels.
      triggers: labels && { github: { labels: { enabled: labels.enabled } } },
      repair: spec.repair && { enabled: spec.repair.enabled },
    },
  }
}

export function bindingManifest(name: string): Manifest {
  const { spec } = named(gatewayBindings, 'GatewayBinding', name)
  return {
    apiVersion: 'gateway.orka.ai/v1alpha1',
    kind: 'GatewayBinding',
    metadata: { name },
    spec: { gatewayRef: spec.gatewayRef, agentRef: spec.agentRef, match: spec.match },
  }
}

// The tour panel fits about 40 characters, so long values fold onto
// continuation lines. The writer can run a first line past lineWidth, hence 38.
export const toYaml = (value: object) => stringify(value, { lineWidth: 38 }).trimEnd()
