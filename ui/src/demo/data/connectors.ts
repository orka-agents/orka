import type { Connection, ConnectorProvider } from '@/lib/connectors'
import { ago, DEMO_NAMESPACE, digest } from '../shared'

// ConnectorProviders an operator has configured, and the demo visitor's own
// linked accounts. GitHub uses Orka's built-in tool catalog; the others are
// custom MCP-backed tools.

const read = (...names: string[]) => names.map((name) => ({ name, class: 'read' as const }))
const write = (...names: string[]) => names.map((name) => ({ name, class: 'write' as const }))

export const connectorProviders: ConnectorProvider[] = [
  {
    name: 'github',
    namespace: DEMO_NAMESPACE,
    displayName: 'GitHub',
    ready: true,
    scopes: { read: ['metadata:read', 'issues:read', 'pull_requests:read', 'checks:read'], write: ['issues:write', 'pull_requests:write'] },
    tools: [
      ...read('get_issue', 'list_issues', 'list_pull_requests', 'review_pull_request', 'check_pull_request_ci'),
      ...write('comment_on_issue', 'create_pull_request', 'post_review_comment'),
    ],
  },
  {
    name: 'slack',
    namespace: DEMO_NAMESPACE,
    displayName: 'Slack',
    ready: true,
    scopes: { read: ['channels:history', 'search:read', 'users:read'], write: ['chat:write'] },
    tools: [...read('search_messages', 'get_thread'), ...write('post_message')],
  },
  {
    name: 'linear',
    namespace: DEMO_NAMESPACE,
    displayName: 'Linear',
    ready: true,
    scopes: { read: ['read'], write: ['issues:create', 'comments:create'] },
    tools: [...read('search_issues', 'get_project'), ...write('create_issue', 'add_comment')],
  },
  {
    name: 'pagerduty',
    namespace: DEMO_NAMESPACE,
    displayName: 'PagerDuty',
    ready: true,
    scopes: { read: ['incidents.read', 'services.read'], write: ['incidents.write'] },
    tools: [...read('list_incidents', 'get_incident'), ...write('acknowledge_incident', 'add_incident_note')],
  },
  {
    name: 'jira',
    namespace: DEMO_NAMESPACE,
    displayName: 'Jira Cloud',
    ready: false,
    scopes: { read: ['read:jira-work'], write: ['write:jira-work'] },
    tools: [...read('search_issues', 'get_issue'), ...write('create_issue', 'transition_issue')],
  },
]

/** The visitor's link name for a provider, as the controller derives it from provider and subject. */
export const connectionName = (provider: string) => `${provider}-${digest(`demo-user:${provider}`, 8)}`

export function grantedScopes(provider: string, mode: string): string[] {
  const scopes = connectorProviders.find((p) => p.name === provider)?.scopes
  return [...(scopes?.read ?? []), ...(mode === 'readWrite' ? scopes?.write ?? [] : [])]
}

export function seedConnections(): Connection[] {
  return [
    {
      name: connectionName('github'),
      namespace: DEMO_NAMESPACE,
      provider: 'github',
      mode: 'readWrite',
      state: 'Ready',
      grantedScopes: grantedScopes('github', 'readWrite'),
      linkedAt: ago(9 * 24 * 60),
      lastRefreshTime: ago(22),
      expiresAt: ago(-7 * 60),
      ready: true,
    },
    {
      name: connectionName('slack'),
      namespace: DEMO_NAMESPACE,
      provider: 'slack',
      mode: 'readOnly',
      state: 'Ready',
      grantedScopes: grantedScopes('slack', 'readOnly'),
      linkedAt: ago(16 * 24 * 60),
      lastRefreshTime: ago(3 * 60),
      ready: true,
    },
    {
      name: connectionName('pagerduty'),
      namespace: DEMO_NAMESPACE,
      provider: 'pagerduty',
      mode: 'readOnly',
      state: 'Ready',
      grantedScopes: grantedScopes('pagerduty', 'readOnly'),
      linkedAt: ago(41 * 24 * 60),
      lastRefreshTime: ago(2 * 24 * 60),
      ready: false,
      message: 'PagerDuty rejected the refresh token (invalid_grant). Reconnect to keep using its tools.',
    },
  ]
}
