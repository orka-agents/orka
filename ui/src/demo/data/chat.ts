import { getResponse } from 'msw'
import type { Agent } from '@/schemas/agent'
import type { Task } from '@/schemas/task'
import { createDemoTask, taskHandlers } from '../handlers/tasks'
import { DEMO_NAMESPACE } from '../shared'
import { AGENTS, REPOS, TASKS, type AgentKey, type RepoKey } from '../world'
import { builtinTools, customTools } from './tools'

// The demo chat is scripted, not a model: each user message is matched to a
// scenario by keywords, and the scenario plays back tool calls against the
// demo data followed by an assistant message. Tool names and the
// {success, data} result envelope match the real meta agent. Task state is
// read live through the task domain's API, so answers track the simulation.

/** One step of a scripted turn. Tool `run` and message `content` are evaluated when the step plays. */
export type ChatStep =
  | { kind: 'tool'; name: string; args: Record<string, unknown>; run: () => ToolResult | Promise<ToolResult> }
  | { kind: 'message'; content: () => string }

export interface ToolResult {
  success: boolean
  data?: unknown
  error?: string
}

export interface ChatTurnContext {
  sessionId: string
  namespace: string
  /** Current agents, including any created or deleted during the demo. */
  agents: () => Agent[]
}

const ok = (data: unknown): ToolResult => ({ success: true, data })

const AGENT_BY_NAME = Object.fromEntries((Object.keys(AGENTS) as AgentKey[]).map((key) => [AGENTS[key].name, key]))

/** Reads the demo task API (the same handlers the dashboard calls). */
async function taskApi<T>(path: string): Promise<T | undefined> {
  const response = await getResponse(taskHandlers, new Request(`http://demo.local/api/v1${path}`))
  return response?.ok ? ((await response.json()) as T) : undefined
}

const getTask = (name: string) => taskApi<Task>(`/tasks/${encodeURIComponent(name)}?namespace=${DEMO_NAMESPACE}`)
const listTasks = async () => (await taskApi<{ items: Task[] }>(`/tasks?namespace=${DEMO_NAMESPACE}&limit=200`))?.items ?? []

function taskSummary(task: Task) {
  return {
    name: task.metadata.name,
    agent: task.spec.agentRef?.name,
    type: task.spec.type,
    phase: task.status?.phase ?? 'Pending',
    createdAt: task.metadata.creationTimestamp,
    ...(task.plan?.progressPct !== undefined && { progressPct: task.plan.progressPct }),
  }
}

const phaseOf = (task: Task | undefined) => task?.status?.phase ?? 'Pending'
const isActive = (task: Task) => ['Pending', 'Running', 'Finalizing'].includes(phaseOf(task))
const isTerminal = (task: Task | undefined) => ['Succeeded', 'Failed', 'Cancelled'].includes(phaseOf(task))

const bullet = (lines: string[]) => lines.map((line) => `• ${line}`).join('\n')

function formatTokens(tokens: number) {
  if (tokens >= 1_000_000) return `${(tokens / 1_000_000).toFixed(1)}M`
  if (tokens >= 1000) return `${Math.round(tokens / 1000)}k`
  return String(tokens)
}

export const SUGGESTED_PROMPTS = [
  'Why did the latest tasks fail?',
  'How is the v2.14.0 release going?',
  "What's the status of the checkout latency investigation?",
  'Review PR #1486 in checkout-service',
  'Fix the flaky test in inventory-api',
  'Which agents can I use?',
  "What's running right now?",
]

function helpMessage(prefix: string) {
  return `${prefix}\n\nThis demo chat is scripted — it answers from the sample cluster instead of calling a model. Try one of these:\n${bullet(SUGGESTED_PROMPTS)}`
}

function detectRepo(text: string): RepoKey | undefined {
  if (/checkout|payment|cart|idempot/.test(text)) return 'checkout'
  if (/inventory|stock|sku|reservation/.test(text)) return 'inventory'
  if (/storefront|web|frontend|carousel|a11y|accessib/.test(text)) return 'storefront'
  if (/infra|terraform|runbook|postgres|platform/.test(text)) return 'infra'
  return undefined
}

function failedTasksTurn(): ChatStep[] {
  let failed: Task[] = []
  const deps = 'deps-weekly-bump-20261015'
  return [
    {
      kind: 'tool',
      name: 'list_tasks',
      args: { phase: 'Failed', limit: 10 },
      run: async () => {
        failed = (await listTasks()).filter((task) => phaseOf(task) === 'Failed')
        return ok({ tasks: failed.map(taskSummary), count: failed.length })
      },
    },
    {
      kind: 'tool',
      name: 'fetch_task_output',
      args: { name: deps },
      run: async () => {
        const task = await getTask(deps)
        return ok({
          name: deps,
          phase: phaseOf(task),
          message: task?.status?.message,
          output:
            '17 packages upgraded (pydantic 2.9.2 → 3.0.1, …). uv run pytest -q: 41 failed, 612 passed — pydantic v3 removed Config classes. ' +
            'Migrated inventory/models/sku.py to model_config and replaced 11 validators: 18 failed, 635 passed. ' +
            'The prompt then exceeded the 15m task timeout; no changes were published.',
        })
      },
    },
    {
      kind: 'message',
      content: () =>
        [
          `${failed.length === 1 ? 'One task has' : `${failed.length} tasks have`} failed recently. The two that matter:`,
          '',
          bullet([
            `${deps} (dependency-bot, inventory-api) — the weekly bump pulled in pydantic 3.0, which removed Config classes, and 41 tests broke. The bot migrated the models and validators and got it down to 18 failures, then ran into the 15-minute task timeout. Nothing was published, so main is untouched.`,
            'db-migrate-orders-v42-e2l9o (container) — migration 042 failed on staging because the idempotency_key column already exists on orders (SQLSTATE 42701); the transaction rolled back cleanly.',
          ]),
          '',
          'Suggested next steps:',
          bullet([
            'Pin pydantic<3 for this week\'s bump, and give bug-fixer the v3 migration as its own task with a longer timeout ("Fix the pydantic v3 migration in inventory-api").',
            'Make migration 042 idempotent (ADD COLUMN IF NOT EXISTS) before re-running it.',
          ]),
        ].join('\n'),
    },
  ]
}

function releaseTurn(): ChatStep[] {
  const name = 'release-v2-14-0-plan-r2w8z'
  let plan: Task | undefined
  let children: Task[] = []
  return [
    {
      kind: 'tool',
      name: 'check_task_progress',
      args: { name },
      run: async () => {
        plan = await getTask(name)
        children = (await taskApi<{ items: Task[] }>(`/tasks/${name}/children?namespace=${DEMO_NAMESPACE}`))?.items ?? []
        if (!plan) return { success: false, error: `task ${name} not found` }
        return ok({ ...taskSummary(plan), plan: plan.plan, children: children.map(taskSummary) })
      },
    },
    {
      kind: 'tool',
      name: 'fetch_task_output',
      args: { name: 'release-v2-14-0-blockers-p3c5v' },
      run: () =>
        ok({
          name: 'release-v2-14-0-blockers-p3c5v',
          phase: 'Succeeded',
          output: 'No open release blockers for milestone v2.14.0. #866 Coupon field accepts whitespace — closed by #871. #874 Apple Pay button missing on Safari 18 — closed by #879.',
        }),
    },
    {
      kind: 'message',
      content: () => {
        if (!plan) return "I couldn't find the v2.14.0 release task in this namespace."
        const pct = plan.plan?.progressPct
        const done = phaseOf(plan) === 'Succeeded'
        const childLines = children.map((child) => `${child.metadata.name} (${child.spec.agentRef?.name ?? child.spec.type}) — ${phaseOf(child).toLowerCase()}`)
        return [
          done
            ? 'Release v2.14.0 of web-storefront is ready to tag. release-captain finished the plan:'
            : `Release v2.14.0 of web-storefront is ${pct !== undefined ? `${pct}% done` : 'in progress'}${plan.plan?.summary ? ` — "${plan.plan.summary}"` : ''}. release-captain is coordinating it:`,
          '',
          bullet([
            '23 PRs merged since v2.13.2 (8 features, 11 fixes, 4 chores).',
            'Blockers: none open — #866 was closed by #871 and #874 (Safari Apple Pay button) by #879.',
            ...childLines.map((line) => `Delegated: ${line}`),
          ]),
          '',
          done
            ? 'Next step for maya.chen: merge the release-notes PR, then tag v2.14.0 from main.'
            : 'Remaining: review the notes, open the release PR, then hand off to maya.chen for the tag.',
        ].join('\n')
      },
    },
  ]
}

function incidentTurn(): ChatStep[] {
  const name = 'checkout-latency-investigation-7k2qp'
  let task: Task | undefined
  let pendingApprovals = 0
  return [
    {
      kind: 'tool',
      name: 'check_task_progress',
      args: { name },
      run: async () => {
        task = await getTask(name)
        const approvals = await taskApi<{ approvals: Array<{ status: string }> }>(`/tasks/${name}/approvals?namespace=${DEMO_NAMESPACE}`)
        pendingApprovals = approvals?.approvals.filter((approval) => approval.status === 'pending').length ?? 0
        if (!task) return { success: false, error: `task ${name} not found` }
        return ok({ ...taskSummary(task), pendingApprovals, message: task.status?.message })
      },
    },
    {
      kind: 'message',
      content: () => {
        if (!task) return "I couldn't find the checkout latency investigation in this namespace."
        const findings = bullet([
          'p99 on checkout-service rose from 182 ms to 1,412 ms at 14:04 UTC; errors went from 0.4% to 2.1%, mostly 504s on POST /v1/orders.',
          'Waiting Postgres connections jumped from 0 to 37 at 14:02 — right as #1477 rolled out, which lowered DB_MAX_CONNS from 50 to 20.',
          'CPU and query latency are flat, so requests are queueing for a database connection, not slow queries.',
        ])
        const state = isTerminal(task)
          ? `incident-responder has finished the checkout latency investigation (${phaseOf(task).toLowerCase()}). What it found:`
          : 'incident-responder is still on the checkout latency spike (started from the #incidents Slack thread). What it has so far:'
        const next = pendingApprovals > 0
          ? 'It proposes rolling back to v3.18.2 and is waiting for your approval — approve or decline it on the task page.'
          : isTerminal(task)
            ? 'Its recommendation: roll back to v3.18.2, then restore DB_MAX_CONNS=50 before re-shipping #1477.'
            : 'It is drafting a mitigation (roll back to v3.18.2) and will ask for approval before acting.'
        return [state, '', findings, '', next].join('\n')
      },
    },
  ]
}

interface CreatePlan {
  agent: AgentKey
  repo?: RepoKey
  prompt: string
}

function planTask(message: string): CreatePlan {
  const text = message.toLowerCase()
  const request = message.trim().replace(/[.?!]+$/, '')
  const repo = detectRepo(text)
  const number = text.match(/#\s?(\d{2,5})|\b(?:pr|issue)\s+(\d{2,5})/)
  const ref = number?.[1] ?? number?.[2]
  if (/review/.test(text)) {
    const target = repo ?? 'checkout'
    return { agent: 'reviewer', repo: target, prompt: `Review PR #${ref ?? '1486'} in ${REPOS[target].slug}. Focus on correctness, data safety, and missing tests.` }
  }
  if (/triage/.test(text)) {
    const target = repo ?? 'inventory'
    return { agent: 'triage', repo: target, prompt: `Triage issue #${ref ?? '3394'} in ${REPOS[target].slug}: label it, check for duplicates, and route it.` }
  }
  if (/dependenc|deps|bump|upgrade/.test(text)) {
    const target = repo ?? 'inventory'
    return { agent: 'deps', repo: target, prompt: `Bump dependencies in ${REPOS[target].slug} to their latest compatible versions, run the tests, and open one PR.` }
  }
  if (/doc|readme|runbook|release note/.test(text)) {
    const target = repo ?? 'infra'
    return { agent: 'docs', repo: target, prompt: `${request}. Work in ${REPOS[target].slug} and open a PR with the changes.` }
  }
  if (/investigat|incident|alert|latency/.test(text)) {
    return { agent: 'incident', prompt: `Investigate: ${request}. Correlate with recent deploys and metrics, and propose a mitigation.` }
  }
  const target = repo ?? 'inventory'
  return { agent: 'fixer', repo: target, prompt: `${request}. Work in ${REPOS[target].slug}: reproduce with a failing test, fix it, and open a PR.` }
}

function createTaskTurn(message: string, context: ChatTurnContext): ChatStep[] {
  const plan = planTask(message)
  const agent = AGENTS[plan.agent]
  const toolName = agent.kind === 'ai' ? 'create_ai_task' : 'create_agent_task'
  let created: string | undefined
  let failure: string | undefined
  return [
    {
      kind: 'tool',
      name: toolName,
      args: {
        agent: agent.name,
        prompt: plan.prompt,
        ...(plan.repo && agent.kind === 'agent' && { workspace: { gitRepo: REPOS[plan.repo].url, intent: plan.agent === 'reviewer' ? 'read' : 'write' } }),
      },
      run: () => {
        try {
          created = createDemoTask({ agent: plan.agent, prompt: plan.prompt, repo: plan.repo, session: context.sessionId, createdBy: 'chat' })
          return ok({ name: created, namespace: context.namespace, agent: agent.name, phase: 'Pending' })
        } catch (err) {
          failure = err instanceof Error ? err.message : 'task creation failed'
          return { success: false, error: failure }
        }
      },
    },
    {
      kind: 'message',
      content: () =>
        created
          ? [
              `Started ${created} on ${agent.name} (${'runtime' in agent ? `${agent.runtime} runtime` : agent.model}).`,
              '',
              plan.repo ? `It works in ${REPOS[plan.repo].slug}${plan.agent === 'reviewer' ? ' with a read-only checkout' : ' and will open a pull request when it is done'}.` : 'It will post its findings back here and in the task result.',
              'Follow it live from the chip below or on the Tasks page — events, logs, and the result stream in as it runs.',
            ].join('\n')
          : `I couldn't create the task: ${failure}.`,
    },
  ]
}

function agentsTurn(context: ChatTurnContext): ChatStep[] {
  const describe = (agent: Agent) => {
    const key = AGENT_BY_NAME[agent.metadata.name] as AgentKey | undefined
    const runtime = agent.spec.runtime && 'type' in agent.spec.runtime ? agent.spec.runtime.type : undefined
    return {
      name: agent.metadata.name,
      type: runtime ? 'runtime' : 'ai',
      ...(runtime && { runtime }),
      model: agent.spec.model?.name,
      activeTasks: agent.status?.activeTasks ?? 0,
      description: key ? AGENTS[key].description : undefined,
    }
  }
  let agents: ReturnType<typeof describe>[] = []
  return [
    {
      kind: 'tool',
      name: 'list_agents',
      args: {},
      run: () => {
        agents = context.agents().map(describe)
        return ok({ agents, count: agents.length })
      },
    },
    {
      kind: 'message',
      content: () =>
        [
          `There are ${agents.length} agents in ${context.namespace}:`,
          '',
          bullet(
            agents.map(
              (agent) =>
                `${agent.name} — ${agent.description ?? 'custom agent'} (${agent.runtime ? `${agent.runtime} runtime` : agent.model ?? 'provider default'}${agent.activeTasks ? `, ${agent.activeTasks} active` : ''})`,
            ),
          ),
          '',
          'Runtime agents work in a repository checkout over ACP; AI agents run Orka’s tool loop directly. Ask me to "review", "fix", "triage", or "bump dependencies" and I’ll start the right one.',
        ].join('\n'),
    },
  ]
}

function toolsTurn(): ChatStep[] {
  const custom = customTools.map((tool) => ({ name: tool.metadata.name, description: tool.spec.description, available: tool.status?.available }))
  return [
    {
      kind: 'tool',
      name: 'list_tools',
      args: {},
      run: () => ok({ builtin: builtinTools.map((tool) => tool.name), custom, count: builtinTools.length + custom.length }),
    },
    {
      kind: 'message',
      content: () =>
        [
          `Agents here can use ${builtinTools.length} built-in tools (${builtinTools.map((tool) => tool.name).join(', ')}) and ${custom.length} custom HTTP tools:`,
          '',
          bullet(custom.map((tool) => `${tool.name} — ${tool.description}`)),
          '',
          'pagerduty_incident notes and slack_post messages go through human approval before they run.',
        ].join('\n'),
    },
  ]
}

function statusTurn(): ChatStep[] {
  let tasks: Task[] = []
  const dayAgo = () => Date.now() - 24 * 60 * 60_000
  return [
    {
      kind: 'tool',
      name: 'list_tasks',
      args: { limit: 50 },
      run: async () => {
        tasks = await listTasks()
        return ok({ tasks: tasks.slice(0, 50).map(taskSummary), count: tasks.length })
      },
    },
    {
      kind: 'message',
      content: () => {
        const active = tasks.filter(isActive)
        const today = tasks.filter((task) => Date.parse(task.metadata.creationTimestamp ?? '') >= dayAgo())
        const counts = today.reduce<Record<string, number>>((acc, task) => ({ ...acc, [phaseOf(task)]: (acc[phaseOf(task)] ?? 0) + 1 }), {})
        // Token totals come from the seeded catalog; the Usage page has the exact numbers.
        const tokens = TASKS.filter((seed) => seed.createdAgo < 24 * 60).reduce((sum, seed) => sum + (seed.inputTokens ?? 0) + (seed.outputTokens ?? 0), 0)
        return [
          active.length === 0 ? 'Nothing is running right now.' : `${active.length} ${active.length === 1 ? 'task is' : 'tasks are'} active right now:`,
          ...(active.length > 0 ? ['', bullet(active.map((task) => `${task.metadata.name} — ${task.spec.agentRef?.name ?? task.spec.type}, ${phaseOf(task).toLowerCase()}`))] : []),
          '',
          `In the last 24 hours: ${today.length} tasks (${Object.entries(counts).map(([phase, count]) => `${count} ${phase.toLowerCase()}`).join(', ')}), roughly ${formatTokens(tokens)} tokens. The Usage page breaks that down by agent and pull-request outcome.`,
        ].join('\n')
      },
    },
  ]
}

/** Picks and builds the scripted turn for a user message. */
export function scriptChatTurn(message: string, context: ChatTurnContext): ChatStep[] {
  const text = message.toLowerCase()
  if (context.namespace !== DEMO_NAMESPACE) {
    return [{ kind: 'message', content: () => `The ${context.namespace} namespace has no agents or tasks in this demo. Switch the namespace picker to "${DEMO_NAMESPACE}" to explore the sample cluster.` }]
  }
  if (/\b(fail|failed|failing|broke|broken|error)\b|why did/.test(text)) return failedTasksTurn()
  if (/release|v2\.14/.test(text)) return releaseTurn()
  if (/latency|incident|outage|p99/.test(text) && !/\b(start|create|kick off|open)\b/.test(text)) return incidentTurn()
  if (/\b(review|fix|triage|bump|upgrade|create|start|kick off|investigate|write|document|update)\b/.test(text)) return createTaskTurn(message, context)
  if (/\bagents?\b|who can/.test(text)) return agentsTurn(context)
  if (/\btools?\b/.test(text)) return toolsTurn()
  if (/running|status|busy|usage|tokens?|cost|summary|today/.test(text)) return statusTurn()
  if (/^\s*(hi|hello|hey|help)\b/.test(text)) return [{ kind: 'message', content: () => helpMessage('Hi! I’m the Orka meta agent for the Northwind Labs platform namespace.') }]
  return [{ kind: 'message', content: () => helpMessage('I don’t have a scripted answer for that one.') }]
}
