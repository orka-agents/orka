import { describe, it, expect } from 'vitest'
import {
  httpExecutionSchema,
  toolSpecSchema,
  toolStatusSchema,
  toolSchema,
  toolListItemSchema,
} from './tool'
import type { Tool, ToolListItem } from './tool'

describe('httpExecutionSchema', () => {
  it('parses valid data with all fields', () => {
    const data = {
      url: 'https://api.example.com/search',
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      timeout: '30s',
      authSecretRef: { name: 'api-key-secret', key: 'api-key' },
      authInject: 'header',
      authBodyKey: 'apiKey',
    }
    expect(httpExecutionSchema.parse(data)).toEqual(data)
  })

  it('parses with only required fields', () => {
    const data = { url: 'https://api.example.com' }
    expect(httpExecutionSchema.parse(data)).toEqual(data)
  })

  it('rejects missing url', () => {
    expect(() => httpExecutionSchema.parse({})).toThrow()
    expect(() => httpExecutionSchema.parse({ method: 'GET' })).toThrow()
  })

  it('rejects wrong types', () => {
    expect(() => httpExecutionSchema.parse({ url: 123 })).toThrow()
    expect(() => httpExecutionSchema.parse({ url: 'http://x', headers: 'invalid' })).toThrow()
  })

  it('rejects invalid authSecretRef', () => {
    expect(() =>
      httpExecutionSchema.parse({ url: 'http://x', authSecretRef: { name: 'x' } })
    ).toThrow()
  })
})

describe('toolSpecSchema', () => {
  it('parses valid data with all fields', () => {
    const data = {
      description: 'Search the web',
      parameters: { type: 'object', properties: { query: { type: 'string' } } },
      http: { url: 'https://api.example.com/search', method: 'POST' },
    }
    expect(toolSpecSchema.parse(data)).toEqual(data)
  })

  it('parses with only required fields', () => {
    const data = {
      description: 'A tool',
      http: { url: 'https://api.example.com' },
    }
    expect(toolSpecSchema.parse(data)).toEqual(data)
  })

  it('rejects missing description', () => {
    expect(() => toolSpecSchema.parse({ http: { url: 'http://x' } })).toThrow()
  })

  it('rejects missing http', () => {
    expect(() => toolSpecSchema.parse({ description: 'A tool' })).toThrow()
  })

  it('rejects empty object', () => {
    expect(() => toolSpecSchema.parse({})).toThrow()
  })
})

describe('toolStatusSchema', () => {
  it('parses valid data with all fields', () => {
    const data = {
      available: true,
      lastCheck: '2024-01-01T00:00:00Z',
      error: '',
      conditions: [{ type: 'Ready', status: 'True' }],
    }
    expect(toolStatusSchema.parse(data)).toEqual(data)
  })

  it('parses with only required fields', () => {
    expect(toolStatusSchema.parse({ available: true })).toEqual({ available: true })
    expect(toolStatusSchema.parse({ available: false })).toEqual({ available: false })
  })

  it('rejects missing available', () => {
    expect(() => toolStatusSchema.parse({})).toThrow()
  })

  it('rejects wrong types', () => {
    expect(() => toolStatusSchema.parse({ available: 'yes' })).toThrow()
  })

  it('rejects invalid conditions', () => {
    expect(() =>
      toolStatusSchema.parse({ available: true, conditions: [{ type: 'Ready' }] })
    ).toThrow()
  })
})

describe('toolSchema', () => {
  it('parses valid full tool', () => {
    const data = {
      apiVersion: 'core.orka.ai/v1alpha1',
      kind: 'Tool',
      metadata: {
        name: 'search-tool',
        namespace: 'default',
        uid: 'abc-123',
        creationTimestamp: '2024-01-01T00:00:00Z',
      },
      spec: {
        description: 'Search the web',
        http: { url: 'https://api.example.com/search' },
      },
      status: { available: true },
    }
    expect(toolSchema.parse(data)).toEqual(data)
  })

  it('parses minimal tool', () => {
    const data = {
      metadata: { name: 'my-tool' },
      spec: {
        description: 'A tool',
        http: { url: 'https://example.com' },
      },
    }
    expect(toolSchema.parse(data)).toEqual(data)
  })

  it('rejects missing metadata', () => {
    expect(() =>
      toolSchema.parse({ spec: { description: 'x', http: { url: 'http://x' } } })
    ).toThrow()
  })

  it('rejects missing spec', () => {
    expect(() => toolSchema.parse({ metadata: { name: 'x' } })).toThrow()
  })

  it('rejects missing metadata name', () => {
    expect(() =>
      toolSchema.parse({ metadata: {}, spec: { description: 'x', http: { url: 'http://x' } } })
    ).toThrow()
  })
})

describe('toolListItemSchema', () => {
  it('parses valid data with all fields', () => {
    const data = {
      name: 'search-tool',
      namespace: 'default',
      builtin: false,
      description: 'Search the web',
      available: true,
      url: 'https://api.example.com/search',
    }
    expect(toolListItemSchema.parse(data)).toEqual(data)
  })

  it('parses with only required fields', () => {
    const data = { name: 'search', builtin: true, description: 'Built-in search' }
    expect(toolListItemSchema.parse(data)).toEqual(data)
  })

  it('rejects missing required fields', () => {
    expect(() => toolListItemSchema.parse({})).toThrow()
    expect(() => toolListItemSchema.parse({ name: 'x' })).toThrow()
    expect(() => toolListItemSchema.parse({ name: 'x', builtin: true })).toThrow()
    expect(() => toolListItemSchema.parse({ name: 'x', description: 'd' })).toThrow()
  })

  it('rejects wrong types', () => {
    expect(() =>
      toolListItemSchema.parse({ name: 'x', builtin: 'yes', description: 'd' })
    ).toThrow()
  })
})

describe('exported types', () => {
  it('Tool type matches schema', () => {
    const tool: Tool = {
      metadata: { name: 'test' },
      spec: { description: 'test', http: { url: 'http://x' } },
    }
    expect(toolSchema.parse(tool)).toBeDefined()
  })

  it('ToolListItem type matches schema', () => {
    const item: ToolListItem = { name: 'test', builtin: false, description: 'test' }
    expect(toolListItemSchema.parse(item)).toBeDefined()
  })
})
