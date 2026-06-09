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

  it('parses with only URL', () => {
    const data = { url: 'https://api.example.com' }
    expect(httpExecutionSchema.parse(data)).toEqual(data)
  })

  it('parses auth-only HTTP config for MCP transport auth', () => {
    const data = { authSecretRef: { name: 'mcp-auth', key: 'token' }, authInject: 'header' }
    expect(httpExecutionSchema.parse(data)).toEqual(data)
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

  it('parses MCP-only tools without HTTP configuration', () => {
    const data = {
      description: 'MCP actor tool',
      mcp: {
        path: '/mcp',
        substrateActor: {
          templateRef: { name: 'mcp-template', namespace: 'ate-demo' },
          poolRef: { name: 'mcp-pool', namespace: 'default' },
          boot: true,
        },
      },
    }
    expect(toolSpecSchema.parse(data)).toEqual(data)
  })

  it('parses MCP tools with HTTP transport auth but no URL', () => {
    const data = {
      description: 'MCP actor tool',
      http: { authSecretRef: { name: 'mcp-auth', key: 'token' } },
      mcp: {
        path: '/mcp',
        substrateActor: {
          templateRef: { name: 'mcp-template', namespace: 'ate-demo' },
        },
      },
    }
    expect(toolSpecSchema.parse(data)).toEqual(data)
  })

  it('rejects missing description', () => {
    expect(() => toolSpecSchema.parse({ http: { url: 'http://x' } })).toThrow()
  })

  it('rejects missing backend configuration', () => {
    expect(() => toolSpecSchema.parse({ description: 'A tool' })).toThrow()
  })

  it('rejects plain HTTP tools without URL', () => {
    expect(() => toolSpecSchema.parse({ description: 'A tool', http: { method: 'GET' } })).toThrow()
  })

  it('rejects MCP tools without substrate actor backing', () => {
    expect(() =>
      toolSpecSchema.parse({ description: 'MCP actor tool', mcp: { path: '/mcp' } })
    ).toThrow()
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

  it('parses MCP actor endpoint status', () => {
    const data = {
      available: true,
      endpoint: 'http://router/mcp',
      actor: {
        provider: 'substrate',
        actorID: 'orka-p-pool-00001',
        routeHost: 'orka-p-pool-00001.actors.example.com',
        templateRef: { name: 'mcp-template', namespace: 'ate-demo' },
        poolRef: { name: 'mcp-pool', namespace: 'default' },
      },
    }
    expect(toolStatusSchema.parse(data)).toEqual(data)
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

  it('parses MCP-only tool detail response', () => {
    const data = {
      metadata: { name: 'mcp-tool', namespace: 'default' },
      spec: {
        description: 'Durable MCP tool',
        mcp: {
          path: '/mcp',
          substrateActor: {
            templateRef: { name: 'mcp-template', namespace: 'ate-demo' },
            poolRef: { name: 'mcp-pool', namespace: 'default' },
          },
        },
      },
      status: {
        available: true,
        endpoint: 'http://router/mcp',
        actor: {
          provider: 'substrate',
          actorID: 'orka-p-pool-00001',
          routeHost: 'orka-p-pool-00001.actors.example.com',
        },
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

  it('Tool type accepts MCP-only specs', () => {
    const tool: Tool = {
      metadata: { name: 'mcp-test' },
      spec: {
        description: 'test',
        mcp: {
          substrateActor: {
            templateRef: { name: 'mcp-template' },
          },
        },
      },
    }
    expect(toolSchema.parse(tool)).toBeDefined()
  })

  it('ToolListItem type matches schema', () => {
    const item: ToolListItem = { name: 'test', builtin: false, description: 'test' }
    expect(toolListItemSchema.parse(item)).toBeDefined()
  })
})
