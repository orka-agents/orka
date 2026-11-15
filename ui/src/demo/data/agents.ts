import type { Agent, AgentSpec } from '@/schemas/agent'
import { ago, DEMO_NAMESPACE, uid } from '../shared'
import { AGENTS, TASKS, type AgentKey } from '../world'

// Agent CRs for the demo namespace. AI agents run Orka's own tool loop against
// a Provider; runtime agents run a coding-agent CLI over ACP in a workspace.

const tools = (...names: string[]) => names.map((name) => ({ name, enabled: true }))

/** ConfigMaps in the demo namespace that agents read their system prompt from, by name and key. */
export const promptConfigMaps: Record<string, Record<string, string>> = {
  'incident-responder-prompt': {
    'system.md': [
      '# Incident responder',
      '',
      'You investigate production alerts for Northwind Labs and keep the incident channel informed.',
      '',
      '1. Read the alert, then pull latency, error-rate and saturation metrics for the affected service from Datadog.',
      '2. Correlate the start of the problem with deploys and merged pull requests in GitHub.',
      '3. Post a short status to the Slack thread and the PagerDuty incident: what you see, what you suspect, what you will check next.',
      '4. Propose one mitigation. Rollbacks, restarts and config changes in production need approval: call request_approval and wait.',
      '',
      'Quote metric values with their time window. Say when you are unsure. Never page anyone yourself.',
    ].join('\n'),
  },
}

const specs: Record<AgentKey, AgentSpec> = {
  triage: {
    providerRef: { name: AGENTS.triage.provider },
    model: { name: AGENTS.triage.model, temperature: 0.2, maxTokens: 2048 },
    systemPrompt: {
      inline: [
        'You triage new GitHub issues for Northwind Labs.',
        'For each issue: pick one area label (area/checkout, area/inventory, area/storefront, area/infra),',
        'one kind label (kind/bug, kind/feature, kind/question), and a priority (p0-p3).',
        'Search for duplicates before labelling. Never close an issue; suggest it instead.',
      ].join('\n'),
    },
    tools: tools('github_search', 'web_search'),
    session: { maxMessages: 40 },
  },
  reviewer: {
    model: { name: AGENTS.reviewer.model },
    systemPrompt: {
      inline: 'Review the pull request like a senior engineer on the owning team. Prioritize correctness, data safety, and missing tests over style. Leave at most 8 comments.',
    },
    runtime: {
      type: 'claude',
      contractVersion: 'orka.harness.v2',
      defaultMaxTurns: 40,
      defaultAllowedTools: ['Read', 'Grep', 'Glob'],
      defaultAllowBash: false,
    },
  },
  coordinator: {
    providerRef: { name: AGENTS.coordinator.provider },
    model: { name: AGENTS.coordinator.model, temperature: 0.3, contextWindow: 200000, maxTokens: 8192 },
    systemPrompt: {
      inline: [
        'You coordinate releases for the Northwind web storefront.',
        'Break the release into small tasks and delegate them to the allowed agents.',
        'Ask for approval before posting to Slack. Report blockers early.',
      ].join('\n'),
    },
    tools: tools('github_search', 'web_fetch', 'slack_post', 'request_approval'),
    coordination: {
      enabled: true,
      allowedAgents: [{ name: AGENTS.docs.name }, { name: AGENTS.triage.name }, { name: AGENTS.reviewer.name }, { name: AGENTS.fixer.name }],
      maxConcurrentChildren: 3,
      maxDepth: 2,
      autonomous: true,
      maxIterations: 25,
      approvalRequiredTools: ['slack_post'],
    },
  },
  incident: {
    providerRef: { name: AGENTS.incident.provider },
    model: { name: AGENTS.incident.model, temperature: 0.1, contextWindow: 200000, maxTokens: 8192 },
    systemPrompt: { configMapRef: { name: 'incident-responder-prompt', key: 'system.md' } },
    tools: tools('datadog_query', 'pagerduty_incident', 'github_search', 'slack_post', 'request_approval', 'reply_in_conversation'),
    coordination: {
      enabled: true,
      allowedAgents: [{ name: AGENTS.fixer.name }],
      maxConcurrentChildren: 1,
      maxDepth: 1,
      autonomous: true,
      maxIterations: 5,
    },
  },
  fixer: {
    model: { name: AGENTS.fixer.model },
    runtime: {
      type: 'codex',
      contractVersion: 'orka.harness.v2',
      defaultMaxTurns: 60,
      defaultAllowBash: true,
      defaultReasoningEffort: 'high',
    },
  },
  deps: {
    // OpenCode takes provider-qualified model IDs and explicit token limits.
    model: { name: `openai/${AGENTS.deps.model}`, contextWindow: 1047576, maxTokens: 32768 },
    runtime: {
      type: 'opencode',
      contractVersion: 'orka.harness.v2',
      defaultMaxTurns: 50,
      defaultAllowedTools: ['Read', 'Write', 'Edit', 'Bash', 'Glob', 'Grep'],
      defaultAllowBash: true,
    },
  },
  docs: {
    model: { name: AGENTS.docs.model },
    systemPrompt: {
      inline: 'Write for on-call engineers: short sentences, exact commands, and a rollback step for every change.',
    },
    runtime: { type: 'copilot', contractVersion: 'orka.harness.v2', defaultMaxTurns: 30 },
  },
  security: {
    providerRef: { name: AGENTS.security.provider },
    model: { name: AGENTS.security.model, temperature: 0, contextWindow: 200000, maxTokens: 16384 },
    systemPrompt: {
      inline: 'You audit code slices for exploitable vulnerabilities. Report only findings you can trace from an untrusted input to a sink, with a reproduction.',
    },
    runtime: { type: 'claude', contractVersion: 'orka.harness.v2', defaultMaxTurns: 40 },
  },
}

const createdDaysAgo: Record<AgentKey, number> = {
  triage: 61, reviewer: 58, coordinator: 34, incident: 47, fixer: 40, deps: 52, docs: 29, security: 21,
}

function agentStatus(key: AgentKey): NonNullable<Agent['status']> {
  const tasks = TASKS.filter((task) => task.agent === key)
  const active = tasks.filter((task) => task.phase === 'Running' || task.phase === 'Pending').length
  const lastUsed = tasks.length > 0 ? Math.min(...tasks.map((task) => task.createdAgo)) : undefined
  return {
    activeTasks: active,
    ...(lastUsed !== undefined && { lastUsed: ago(lastUsed) }),
    conditions: [
      { type: 'Ready', status: 'True', reason: 'Validated', message: 'Agent configuration is valid', lastTransitionTime: ago(createdDaysAgo[key] * 24 * 60 - 1) },
    ],
  }
}

export function demoAgent(name: string, spec: AgentSpec, createdAt: string, status?: Agent['status']): Agent {
  return {
    apiVersion: 'core.orka.ai/v1alpha1',
    kind: 'Agent',
    metadata: {
      name,
      namespace: DEMO_NAMESPACE,
      uid: uid(`agent:${name}`),
      creationTimestamp: createdAt,
      labels: { 'app.kubernetes.io/part-of': 'northwind-platform', 'northwind.dev/team': 'platform' },
    },
    spec,
    status,
  }
}

/**
 * An autonomous coordinator: the controller re-runs its Task until the plan
 * marks the goal complete, so its stories must end that way.
 */
export function isAutonomousAgent(key: AgentKey): boolean {
  const coordination = specs[key].coordination
  return Boolean(coordination?.enabled && coordination.autonomous)
}

/** The seeded agents, one per world.AGENTS entry. */
export function seedAgents(): Agent[] {
  return (Object.keys(AGENTS) as AgentKey[]).map((key) =>
    demoAgent(AGENTS[key].name, specs[key], ago(createdDaysAgo[key] * 24 * 60), agentStatus(key)),
  )
}
