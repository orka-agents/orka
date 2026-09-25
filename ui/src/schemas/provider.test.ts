import { describe, it, expect } from 'vitest'
import { providerListResponseSchema } from './provider'

describe('providerListResponseSchema', () => {
  it('normalizes full Provider CRDs', () => {
    const parsed = providerListResponseSchema.parse({
      items: [
        { metadata: { name: 'anthropic', namespace: 'default' }, spec: { type: 'anthropic', defaultModel: 'claude-sonnet-4-20250514', apiKeySecretRef: { name: 'k' } }, status: { ready: true } },
      ],
      metadata: {},
    })
    expect(parsed.items).toEqual([
      { name: 'anthropic', namespace: 'default', type: 'anthropic', defaultModel: 'claude-sonnet-4-20250514', ready: true },
    ])
  })

  it('treats a missing status.ready on a full CRD as not ready', () => {
    const parsed = providerListResponseSchema.parse({
      items: [
        { metadata: { name: 'no-status' }, spec: { type: 'openai' } },
        { metadata: { name: 'empty-status' }, spec: { type: 'openai' }, status: {} },
      ],
    })
    expect(parsed.items.map((item) => item.ready)).toEqual([false, false])
  })

  it('leaves ready unknown when the flat projection omits it', () => {
    const parsed = providerListResponseSchema.parse({ items: [{ name: 'flat', type: 'openai' }] })
    expect(parsed.items[0].ready).toBeUndefined()
  })

  it('normalizes the flat transaction-token projection', () => {
    const parsed = providerListResponseSchema.parse({
      items: [{ name: 'openai', namespace: 'default', type: 'openai', defaultModel: 'gpt-5', ready: false }],
    })
    expect(parsed.items[0]).toMatchObject({ name: 'openai', type: 'openai', defaultModel: 'gpt-5', ready: false })
  })

  it('rejects items without a name', () => {
    expect(() => providerListResponseSchema.parse({ items: [{ spec: { type: 'openai' } }] })).toThrow()
  })
})
