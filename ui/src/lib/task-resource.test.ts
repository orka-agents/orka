import { describe, expect, it } from 'vitest'
import { parse, stringify } from 'yaml'
import type { Task } from '@/schemas/task'
import { displayedTaskResource, HIDDEN_VALUE } from './task-resource'

const task = {
  metadata: {
    name: 'fix-bug',
    namespace: 'team-a',
    managedFields: [{ manager: 'kubectl' }],
    labels: { 'orka.ai/agent': 'fixer', 'auth-token': 'abc' },
    annotations: {
      'orka.ai/created-by': 'maya',
      'kubectl.kubernetes.io/last-applied-configuration': '{"spec":{"env":[{"name":"API_KEY","value":"sk-live"}]}}',
      'orka.ai/note': 'Bearer abc.def',
      'example.com/config': '{"password":"hunter2"}',
      'example.com/hook': 'https://hooks.slack.com/services/T0/B0/abc',
      'example.com/callback': 'https://cb.example.com/done?sig=abc',
      'example.com/blob': `{"settings":"${'x'.repeat(100)}"}`,
    },
  },
  spec: {
    type: 'container',
    image: 'busybox',
    env: [{ name: 'API_KEY', value: 'sk-live-123456' }, { name: 'FROM_SECRET' }],
    webhookURL: 'https://bot:pw@hooks.example.com/services/T0/B0/s3cr3t?sig=abc',
    agentRuntime: {
      workspace: {
        gitRepo: 'https://x-access-token:ghs_abc123@github.com/northwind/checkout.git',
        forkRepo: 'https://github.com/northwind/checkout-fork.git?token=abc123#frag',
      },
    },
  },
  status: { phase: 'Running', pullRequest: { url: 'https://github.com/northwind/checkout/pull/12' }, message: 'clone of https://alice:pa@ssword@example.com/repo failed; see https://github.com/northwind/checkout?tab=x', reason: 'notify https://hooks.slack.com/services/T0/B0/s3cr3tpath failed', detail: 'also https://discord.com/api/v10/webhooks/1/d1sc0rdtoken and https://hooks.slack.com:443/services/T0/B0/p0rtsecret', lastError: 'https://alice:pass@example.com:bad/repo?sig=x' },
  plan: { summary: 'not part of the resource' },
} as unknown as Task

describe('displayedTaskResource', () => {
  const shown = displayedTaskResource(task)

  it('names the resource and keeps its spec and status, but not the API-only plan', () => {
    expect(shown).toMatchObject({ apiVersion: 'core.orka.ai/v1alpha1', kind: 'Task', spec: { type: 'container', image: 'busybox' }, status: { phase: 'Running' } })
    expect(shown).not.toHaveProperty('plan')
    expect(shown.metadata).not.toHaveProperty('managedFields')
  })

  it('shows only the scheme and host of a webhook URL', () => {
    expect((shown.spec as Task['spec']).webhookURL).toBe(`https://hooks.example.com/${HIDDEN_VALUE}`)
    expect(JSON.stringify(shown)).not.toMatch(/s3cr3t|sig=|bot:pw/)
    const unparsable = displayedTaskResource({ ...task, spec: { ...task.spec, webhookURL: 'not a url s3cr3t' } } as Task)
    expect((unparsable.spec as Task['spec']).webhookURL).toBe(HIDDEN_VALUE)
  })

  it('hides the credentials in any URL value but keeps the rest of the URL', () => {
    expect(shown.spec).toMatchObject({
      agentRuntime: {
        workspace: {
          gitRepo: `https://${HIDDEN_VALUE}@github.com/northwind/checkout.git`,
          forkRepo: `https://github.com/northwind/checkout-fork.git?${HIDDEN_VALUE}#${HIDDEN_VALUE}`,
        },
      },
    })
    expect(shown.metadata).toMatchObject({ annotations: { 'example.com/callback': `https://cb.example.com/done?${HIDDEN_VALUE}` } })
    // A URL with nothing to hide stays as it is; in prose, only userinfo is hidden.
    expect(shown.status).toMatchObject({
      pullRequest: { url: 'https://github.com/northwind/checkout/pull/12' },
      message: `clone of https://${HIDDEN_VALUE}@example.com/repo failed; see https://github.com/northwind/checkout?tab=x`,
      reason: `notify https://hooks.slack.com/services/${HIDDEN_VALUE} failed`,
      detail: `also https://discord.com/api/v10/webhooks/${HIDDEN_VALUE} and https://hooks.slack.com:443/services/${HIDDEN_VALUE}`,
      // URL-shaped but unparsable: hidden whole.
      lastError: HIDDEN_VALUE,
    })
    expect(JSON.stringify(shown)).not.toMatch(/ghs_abc123|token=abc123|sig=abc|frag|alice|ssword|s3cr3tpath|d1sc0rdtoken|p0rtsecret/)
  })

  it('hides environment values and secret-looking labels and annotations', () => {
    expect((shown.spec as Task['spec']).env).toEqual([{ name: 'API_KEY', value: HIDDEN_VALUE }, { name: 'FROM_SECRET' }])
    expect(shown.metadata).toMatchObject({
      labels: { 'orka.ai/agent': 'fixer', 'auth-token': HIDDEN_VALUE },
      annotations: {
        'orka.ai/created-by': 'maya',
        'orka.ai/note': HIDDEN_VALUE,
        'example.com/config': HIDDEN_VALUE,
        'example.com/blob': HIDDEN_VALUE,
        // A Slack webhook carries its secret in the path.
        'example.com/hook': HIDDEN_VALUE,
      },
    })
    expect(JSON.stringify(shown)).not.toMatch(/sk-live|last-applied|Bearer|hunter2|xxxx/)
  })

  it('writes YAML that reads back as the same object', () => {
    expect(parse(stringify(shown))).toEqual(shown)
  })
})
