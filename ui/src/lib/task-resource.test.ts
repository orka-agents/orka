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
    ai: { prompt: 'Build a basic HTTP server with Basic Authentication; Bearer tokens expire, so set max_tokens: 100 and see https://github.com/northwind/checkout?tab=files.' },
    env: [{ name: 'API_KEY', value: 'sk-live-123456' }, { name: 'FROM_SECRET' }],
    webhookURL: 'https://bot:pw@hooks.example.com/services/T0/B0/s3cr3t?sig=abc',
    agentRuntime: {
      workspace: {
        gitRepo: 'https://x-access-token:ghs_abc123@github.com/northwind/checkout.git',
        forkRepo: 'https://github.com/northwind/checkout-fork.git?token=abc123#frag',
      },
    },
  },
  status: { phase: 'Running', pullRequest: { url: 'https://github.com/northwind/checkout/pull/12' } },
  plan: { summary: 'not part of the resource' },
} as unknown as Task

const messageShown = (message: string) =>
  (displayedTaskResource({ ...task, status: { phase: 'Running', message } } as Task).status as { message: string }).message

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

  it('keeps a URL value but hides its credentials, or the whole value when it will not parse', () => {
    expect(shown.spec).toMatchObject({
      agentRuntime: {
        workspace: {
          gitRepo: `https://${HIDDEN_VALUE}@github.com/northwind/checkout.git`,
          forkRepo: `https://github.com/northwind/checkout-fork.git?${HIDDEN_VALUE}#${HIDDEN_VALUE}`,
        },
      },
    })
    expect(shown.metadata).toMatchObject({ annotations: { 'example.com/callback': `https://cb.example.com/done?${HIDDEN_VALUE}` } })
    expect(shown.status).toMatchObject({ pullRequest: { url: 'https://github.com/northwind/checkout/pull/12' } })
    expect(messageShown('https://hooks.slack.com:443/services/T0/B0/p0rtsecret')).toBe(`https://hooks.slack.com/${HIDDEN_VALUE}`)
    expect(messageShown('https://user:pass@hooks.slack.com/services/T0/B0/s3cr3t')).toBe(`https://hooks.slack.com/${HIDDEN_VALUE}`)
    expect(messageShown('https://user:pass@discord.com/api/v10/webhooks/1/d1sc0rd')).toBe(`https://discord.com/${HIDDEN_VALUE}`)
    expect(messageShown('https://files.example.com/download/ghp_ExampleToken123456')).toBe(`https://files.example.com/${HIDDEN_VALUE}`)
    expect(messageShown('https://files.example.com/download/ghp_ExampleToken123456?x=1')).toBe(`https://files.example.com/${HIDDEN_VALUE}`)
    expect(messageShown('https://files.example.com/download/%67hp_ExampleToken123456')).toBe(`https://files.example.com/${HIDDEN_VALUE}`)
    expect(messageShown('https://files.example.com/download/%67hp_ExampleToken123456?x=1')).toBe(`https://files.example.com/${HIDDEN_VALUE}`)
    expect(messageShown('https://hooks.slack.com/%73ervices/T0/B0/s3cr3t')).toBe(`https://hooks.slack.com/${HIDDEN_VALUE}`)
    expect(messageShown('https://files.example.com/100%/report')).toBe(`https://files.example.com/${HIDDEN_VALUE}`)
    expect(messageShown('https://github.com/northwind/check%20out/pull/12')).toBe('https://github.com/northwind/check%20out/pull/12')
    expect(messageShown('https://alice:pass@example.com:bad/repo?sig=x')).toBe(HIDDEN_VALUE)
    expect(JSON.stringify(shown)).not.toMatch(/ghs_abc123|token=abc123|sig=abc|frag/)
  })

  // Finding where a secret ends inside prose is not reliable, so text that
  // shows any credential sign is hidden whole rather than cut.
  it.each([
    ['userinfo in a URL', 'clone of https://alice:pa@ssword@example.com/repo failed'],
    ['an apostrophe in URL userinfo', "clone of https://alice:pa'ss@example.test/repo failed"],
    ['a lowercase bearer token', 'use bearer abcdefghijklmnopqrstuvwxyz012345 here'],
    ['a Google signature parameter', 'see https://storage.example.com/o?X-Goog-Signature=abc'],
    ['a signed URL', 'download failed for https://bucket.example.com/a?X-Amz-Signature=sig (retrying)'],
    ['a Slack webhook', 'notify https://hooks.slack.com/services/T0/B0/s3cr3t failed'],
    ['a versioned Discord webhook', 'also https://discord.com/api/v10/webhooks/1/token'],
    ['a GitHub token', 'use ghp_abcdefgh123456 for the clone'],
    ['a GitLab token', 'Clone using glpat-AbCdEf123456789xyz'],
    ['an npm token', 'publish with npm_AbCdEfGhIjKlMnOpQrStUvWx'],
    ['a Stripe key', 'charge with sk_live_AbCdEf1234567890'],
    ['a Hugging Face token', 'pull with hf_AbCdEfGhIjKlMnOpQrStUv'],
    ['a long Bearer token', 'then Bearer eyJhbGciOiJIUzI1NiJ9.e30.sig works'],
    ['Basic credentials without the header', 'request used Basic dXNlcjpwYXNzd29yZA== and was rejected'],
    ['an Authorization header', 'send Authorization: lin_api_bareKey123'],
    ['an uppercase Authorization header', 'AUTHORIZATION: Basic dXNlcjpwYXNz'],
    ['a quoted Authorization header', 'Authorization: "Bearer abc" then go'],
    ['a parameterized Authorization header', 'Authorization: AWS4-HMAC-SHA256 Credential=AKID/x, SignedHeaders=host, Signature=abc'],
    ['a quoted password with spaces', 'password="a pass \\" phrase" done'],
    ['a JSON password', '{"url":"https://example.test/?mode=x","password":"two word passphrase"}'],
    ['an escaped JSON password', 'response: {\\"password\\":\\"two word passphrase\\"}'],
    ['an escaped JSON Authorization header', 'headers: {\\"Authorization\\":\\"opaque\\"}'],
    ['an API key label with a space', 'Use API key: q8Zr2opaque to deploy'],
    ['a YAML password with a doubled quote', "password: 'it''s a tail' done"],
    ['a connection string', 'Server=db;User Id=sa;Password="start""rest";'],
    ['OAuth fields', 'got access_token=at123 refresh_token="rt 456" clientSecret: cs789'],
    ['AWS keys', 'AWS_SECRET_ACCESS_KEY=wJalrXUtnFEMI'],
    ['a private key', '-----BEGIN RSA PRIVATE KEY-----\nMIIE'],
  ])('hides text with %s whole', (_, text) => {
    expect(messageShown(text)).toBe(HIDDEN_VALUE)
  })

  it('scrubs long adversarial text in linear time', () => {
    const started = performance.now()
    for (const text of ['1-'.repeat(100_000) + ': x', 'a.'.repeat(100_000) + 'x', '?' + 'a-'.repeat(100_000) + '=', '?' + 'token-'.repeat(30_000) + 'x', 'x'.repeat(200_000)]) {
      messageShown(text)
    }
    // Quadratic matching takes minutes here; linear takes milliseconds.
    expect(performance.now() - started).toBeLessThan(2000)
  })

  it('leaves ordinary text alone', () => {
    expect((shown.spec as Task['spec']).ai?.prompt).toBe(
      'Build a basic HTTP server with Basic Authentication; Bearer tokens expire, so set max_tokens: 100 and see https://github.com/northwind/checkout?tab=files.',
    )
    for (const text of ['the aws_access_key_id rotated; maxTokens=5', 'Build the layout at https://example.test/?design=compact&assignees=2']) {
      expect(messageShown(text)).toBe(text)
    }
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
    expect(JSON.stringify(shown)).not.toMatch(/sk-live|last-applied|Bearer abc|hunter2|xxxx/)
  })

  it('writes YAML that reads back as the same object', () => {
    expect(parse(stringify(shown))).toEqual(shown)
  })
})
