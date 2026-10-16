import type { Tool } from '@/schemas/tool'
import { ago, DEMO_NAMESPACE, uid } from '../shared'

// Built-in tools are listed in every namespace with the flat shape the API
// server returns for them; custom tools are Tool CRs in the demo namespace.
// Built-in descriptions and schemas mirror internal/tools.

export interface BuiltinTool {
  name: string
  builtin: true
  description: string
  parameters: Record<string, unknown>
}

const object = (properties: Record<string, unknown>, required: string[], extra: Record<string, unknown> = {}) => ({
  type: 'object',
  properties,
  required,
  ...extra,
})

export const builtinTools: BuiltinTool[] = [
  {
    name: 'web_search',
    description: 'Search the web for information. Use this when you need to find current information or facts.',
    parameters: object(
      {
        query: { type: 'string', description: 'The search query', maxLength: 400 },
        limit: { type: 'integer', description: 'Maximum number of results to return (default: 5)', default: 5, maximum: 10 },
      },
      ['query'],
    ),
  },
  {
    name: 'code_exec',
    description: 'Execute code in a sandboxed environment. Supports Python, JavaScript (Node.js), and Bash.',
    parameters: object(
      {
        language: { type: 'string', description: 'Programming language to execute the code with', enum: ['python', 'python3', 'javascript', 'node', 'bash', 'sh'] },
        code: { type: 'string', description: 'The code to execute' },
        timeout: { type: 'integer', description: 'Execution timeout in seconds', default: 30, minimum: 1, maximum: 60 },
      },
      ['language', 'code'],
    ),
  },
  {
    name: 'file_read',
    description: 'Read the contents of a file from the workspace. Use this to examine code, configuration, or data files.',
    parameters: object(
      {
        path: { type: 'string', description: 'Path to the file to read (relative to workspace or absolute)' },
        offset: { type: 'integer', description: 'Byte offset to start reading from (default: 0)' },
        limit: { type: 'integer', description: 'Maximum number of bytes to read (default: 65536)' },
      },
      ['path'],
    ),
  },
  {
    name: 'web_fetch',
    description: 'Fetch and extract content from a URL. Returns extracted text from HTML pages, pretty-printed JSON, or raw content.',
    parameters: object(
      {
        url: { type: 'string', description: 'The URL to fetch (http or https only)', maxLength: 2048 },
        max_chars: { type: 'integer', description: 'Maximum characters to return (default: 50000)', default: 50000, maximum: 200000 },
        raw: { type: 'boolean', description: 'Return raw HTML instead of extracted text (default: false)', default: false },
      },
      ['url'],
    ),
  },
  {
    name: 'file_write',
    description: 'Write content to a file in the workspace. Use this to create or modify files.',
    parameters: object(
      {
        path: { type: 'string', description: 'Path to the file to write (relative to workspace or absolute)' },
        content: { type: 'string', description: 'Content to write to the file' },
        mode: { type: 'string', description: "Write mode: 'write' (overwrite) or 'append' (default: write)", enum: ['write', 'append'], default: 'write' },
        create_dirs: { type: 'boolean', description: "Create parent directories if they don't exist (default: true)", default: true },
      },
      ['path', 'content'],
    ),
  },
  {
    name: 'request_approval',
    description: 'Request explicit human approval before a consequential action. The worker enforces configured approval-required tools independently of this helper.',
    parameters: object(
      {
        action: { type: 'string', description: 'Human-readable action requiring approval' },
        riskSummary: { type: 'string', description: 'Why this action needs approval' },
        severity: { type: 'string', enum: ['warning', 'critical'], description: 'Approval severity' },
        targetTool: { type: 'string', description: 'Exact custom side-effect tool name the approval covers' },
        targetArguments: { type: 'object', description: 'Exact side-effect arguments the approval covers' },
      },
      ['action', 'targetTool', 'targetArguments'],
    ),
  },
  {
    name: 'reply_in_conversation',
    description: "Send a nonterminal message to this Task's originating gateway conversation and continue working. Content must be nonempty UTF-8, at most 16 KiB (16384 bytes); the controller enforces a configurable lifetime message limit (default 10). The receipt confirms durable acceptance, not delivery. No destination can be selected.",
    parameters: object(
      { content: { type: 'string', minLength: 1, maxLength: 16384, description: 'Message text, at most 16384 UTF-8 bytes; never truncated.' } },
      ['content'],
      { additionalProperties: false },
    ),
  },
].map((tool) => ({ ...tool, builtin: true as const }))

function customTool(name: string, createdDaysAgo: number, spec: Tool['spec'], status: Tool['status']): Tool {
  return {
    apiVersion: 'core.orka.ai/v1alpha1',
    kind: 'Tool',
    metadata: { name, namespace: DEMO_NAMESPACE, uid: uid(`tool:${name}`), creationTimestamp: ago(createdDaysAgo * 24 * 60) },
    spec,
    status,
  }
}

export const customTools: Tool[] = [
  customTool(
    'github_search',
    58,
    {
      description: 'Search issues, pull requests, and code across the northwind-labs GitHub organization.',
      parameters: object(
        {
          query: { type: 'string', description: 'GitHub search syntax, e.g. "repo:northwind-labs/checkout-service is:pr is:open label:release-blocker"' },
          type: { type: 'string', enum: ['issues', 'pulls', 'code'], default: 'issues' },
          limit: { type: 'integer', minimum: 1, maximum: 50, default: 10 },
        },
        ['query'],
      ),
      http: {
        url: 'http://github-proxy.platform-tools.svc.cluster.local:8080/search',
        method: 'POST',
        timeout: '20s',
        authSecretRef: { name: 'github-read-token', key: 'token' },
        authInject: 'header',
        headers: { Accept: 'application/vnd.github+json' },
      },
    },
    { available: true, lastCheck: ago(2), endpoint: 'http://github-proxy.platform-tools.svc.cluster.local:8080/search' },
  ),
  customTool(
    'pagerduty_incident',
    46,
    {
      description: 'Read a PagerDuty incident timeline, or add a note to an open incident. Adding notes requires approval.',
      parameters: object(
        {
          incidentId: { type: 'string', description: 'PagerDuty incident ID, e.g. Q2W8E4R6T0' },
          action: { type: 'string', enum: ['get_timeline', 'add_note'], default: 'get_timeline' },
          note: { type: 'string', description: 'Note text for add_note' },
        },
        ['incidentId'],
      ),
      http: {
        url: 'https://api.pagerduty.com/incidents',
        method: 'POST',
        timeout: '15s',
        authSecretRef: { name: 'pagerduty-api-key', key: 'api-key' },
        authInject: 'header',
        outboundAccessPolicyRef: { name: 'pagerduty-egress' },
      },
    },
    { available: true, lastCheck: ago(5), endpoint: 'https://api.pagerduty.com/incidents' },
  ),
  customTool(
    'datadog_query',
    46,
    {
      description: 'Run a Datadog metrics query over a time window and return the series as JSON.',
      parameters: object(
        {
          query: { type: 'string', description: 'Metrics query, e.g. "p99:trace.http.request.duration{service:checkout-service}"' },
          from: { type: 'string', description: 'Start of the window, RFC 3339 or relative (e.g. "-1h")', default: '-1h' },
          to: { type: 'string', description: 'End of the window', default: 'now' },
        },
        ['query'],
      ),
      http: {
        url: 'http://datadog-bridge.observability.svc.cluster.local:9000/v1/query',
        method: 'POST',
        timeout: '30s',
        authSecretRef: { name: 'datadog-api-keys', key: 'app-key' },
        authInject: 'header',
      },
    },
    { available: true, lastCheck: ago(1), endpoint: 'http://datadog-bridge.observability.svc.cluster.local:9000/v1/query' },
  ),
  customTool(
    'slack_post',
    55,
    {
      description: 'Post a message to an allowed Slack channel (#releases, #incidents, #platform-bots).',
      parameters: object(
        {
          channel: { type: 'string', enum: ['#releases', '#incidents', '#platform-bots'] },
          text: { type: 'string', description: 'Message text (Slack mrkdwn)' },
          threadTs: { type: 'string', description: 'Reply in this thread instead of the channel' },
        },
        ['channel', 'text'],
      ),
      http: {
        url: 'https://slack.com/api/chat.postMessage',
        method: 'POST',
        timeout: '10s',
        authSecretRef: { name: 'slack-bot-token', key: 'token' },
        authInject: 'header',
        outboundAccessPolicyRef: { name: 'slack-egress' },
      },
    },
    { available: true, lastCheck: ago(3), endpoint: 'https://slack.com/api/chat.postMessage' },
  ),
]

export const toolListItem = (tool: Tool) => ({
  name: tool.metadata.name,
  namespace: tool.metadata.namespace,
  builtin: false,
  description: tool.spec.description,
  available: tool.status?.available ?? false,
  url: tool.spec.http?.url ?? '',
})
