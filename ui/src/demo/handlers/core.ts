import { http, HttpResponse } from 'msw'
import { api, ago, apiError, DEMO_NAMESPACE, inDemoNamespace, list, uid } from '../shared'
import { DEMO_TOKEN } from '../token'
import { PROVIDERS } from '../world'

const providers = Object.values(PROVIDERS).map((provider, index) => ({
  apiVersion: 'core.orka.ai/v1alpha1',
  kind: 'Provider',
  metadata: { name: provider.name, namespace: DEMO_NAMESPACE, uid: uid(`provider:${provider.name}`), creationTimestamp: ago(60 * 24 * (40 - index * 7)) },
  spec: { type: provider.type, defaultModel: provider.models[0], models: provider.models },
  status: { ready: true },
}))

const secrets = [
  { name: 'anthropic-api-key', type: 'Opaque' },
  { name: 'openai-api-key', type: 'Opaque' },
  { name: 'azure-openai-credentials', type: 'Opaque' },
  { name: 'github-app-checkout', type: 'Opaque' },
  { name: 'github-read-token', type: 'Opaque' },
  { name: 'slack-bot-token', type: 'Opaque' },
  { name: 'pagerduty-api-key', type: 'Opaque' },
  { name: 'datadog-api-keys', type: 'Opaque' },
].map((secret) => ({ ...secret, namespace: DEMO_NAMESPACE }))

/** Sign-in, chat configuration, providers, and secrets. */
export const coreHandlers = [
  // Only the demo credential signs in, so the demo never stores a real token
  // a visitor pastes or links in.
  http.get(api('/auth/validate'), ({ request }) =>
    request.headers.get('Authorization') === `Bearer ${DEMO_TOKEN}`
      ? HttpResponse.json({ valid: true, subject: 'demo-user' })
      : apiError(401, 'the demo signs in only with its own sample credential'),
  ),

  http.get(api('/chat/config'), () =>
    HttpResponse.json({
      enabled: true,
      provider: PROVIDERS.anthropic.name,
      model: PROVIDERS.anthropic.models[0],
      maxIterations: 20,
      maxDuration: '10m',
      maxTasksPerTurn: 5,
      maxConcurrent: 4,
      availableTools: [
        'create_ai_task', 'create_agent_task', 'create_container_task', 'create_pr_monitor', 'check_task_progress',
        'fetch_task_output', 'wait_for_task', 'cancel_task', 'list_agents', 'list_tools', 'list_tasks',
        'list_connections', 'create_agent', 'update_agent', 'delete_agent', 'create_tool', 'delete_tool', 'delete_session',
      ],
      connectorsEnabled: true,
    }),
  ),

  http.get(api('/providers'), ({ request }) => list(inDemoNamespace(request) ? providers : [])),

  http.get(api('/secrets'), ({ request }) => list(inDemoNamespace(request) ? secrets : [])),
]
