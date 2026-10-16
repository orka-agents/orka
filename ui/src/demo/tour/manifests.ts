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
    apiVersion: 'orka.ai/v1alpha1',
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
    apiVersion: 'orka.ai/v1alpha1',
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
    apiVersion: 'orka.ai/v1alpha1',
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

// ---- YAML ---------------------------------------------------------------------
// Enough YAML for these manifests: maps, lists, and scalars. Absent and empty
// values are left out, and long prose is folded to fit the tour panel when
// folding reads back as the same text: words separated by single spaces.

const FOLD_WIDTH = 44
const pad = (indent: number) => ' '.repeat(indent)
const isMap = (value: unknown): value is Record<string, unknown> =>
  typeof value === 'object' && value !== null && !Array.isArray(value)

function scalar(value: unknown): string {
  if (typeof value !== 'string') return String(value)
  const plain = /^[A-Za-z_/][\w./@:+-]*$/.test(value) && !value.endsWith(':') &&
    !/^(true|false|yes|no|on|off|null|y|n)$/i.test(value)
  return plain ? value : JSON.stringify(value)
}

function folded(text: string, indent: number): string[] {
  const lines: string[] = []
  let line = ''
  for (const word of text.split(' ')) {
    if (line && line.length + 1 + word.length > FOLD_WIDTH) {
      lines.push(pad(indent) + line)
      line = word
    } else {
      line = line ? `${line} ${word}` : word
    }
  }
  if (line) lines.push(pad(indent) + line)
  return lines
}

function block(value: unknown, indent: number): string[] {
  if (Array.isArray(value)) {
    return value.flatMap((item) => {
      if (!isMap(item)) return [`${pad(indent)}- ${scalar(item)}`]
      const [first, ...rest] = block(item, indent + 2)
      return first ? [`${pad(indent)}- ${first.trimStart()}`, ...rest] : []
    })
  }
  if (!isMap(value)) return []
  return Object.entries(value).flatMap(([key, item]) => {
    if (item === undefined || item === null) return []
    if (Array.isArray(item) || isMap(item)) {
      const children = block(item, indent + 2)
      return children.length ? [`${pad(indent)}${key}:`, ...children] : []
    }
    if (typeof item === 'string' && item.length > FOLD_WIDTH && /^\S+( \S+)+$/.test(item)) {
      return [`${pad(indent)}${key}: >-`, ...folded(item, indent + 2)]
    }
    return [`${pad(indent)}${key}: ${scalar(item)}`]
  })
}

export const toYaml = (value: object) => block(value, 0).join('\n')
