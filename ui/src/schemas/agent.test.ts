import { describe, it, expect } from 'vitest'
import {
  modelConfigSchema,
  toolRefSchema,
  agentCLIRuntimeSchema,
  agentSpecSchema,
  agentStatusSchema,
  agentSchema,
} from './agent'
import type { Agent, AgentSpec, AgentStatus } from './agent'

describe('modelConfigSchema', () => {
  it('parses valid data with all fields', () => {
    const data = { provider: 'openai', name: 'gpt-4', temperature: 0.7, maxTokens: 1000 }
    expect(modelConfigSchema.parse(data)).toEqual(data)
  })

  it('parses empty object (all optional)', () => {
    expect(modelConfigSchema.parse({})).toEqual({})
  })

  it('rejects wrong types', () => {
    expect(() => modelConfigSchema.parse({ temperature: 'warm' })).toThrow()
    expect(() => modelConfigSchema.parse({ maxTokens: '1000' })).toThrow()
  })
})

describe('toolRefSchema', () => {
  it('parses valid data', () => {
    const data = { name: 'search-tool', enabled: true }
    expect(toolRefSchema.parse(data)).toEqual(data)
  })

  it('parses with only required fields', () => {
    expect(toolRefSchema.parse({ name: 'search-tool' })).toEqual({ name: 'search-tool' })
  })

  it('rejects missing name', () => {
    expect(() => toolRefSchema.parse({})).toThrow()
    expect(() => toolRefSchema.parse({ enabled: true })).toThrow()
  })

  it('rejects wrong types', () => {
    expect(() => toolRefSchema.parse({ name: 123 })).toThrow()
    expect(() => toolRefSchema.parse({ name: 'x', enabled: 'yes' })).toThrow()
  })
})

describe('agentCLIRuntimeSchema', () => {
  it('parses valid copilot runtime', () => {
    const data = {
      type: 'copilot',
      defaultMaxTurns: 50,
      defaultAllowedTools: ['bash', 'read'],
      defaultAllowBash: true,
    }
    expect(agentCLIRuntimeSchema.parse(data)).toEqual(data)
  })

  it('parses valid claude runtime', () => {
    const data = { type: 'claude' }
    expect(agentCLIRuntimeSchema.parse(data)).toEqual(data)
  })

  it('rejects invalid type', () => {
    expect(() => agentCLIRuntimeSchema.parse({ type: 'invalid' })).toThrow()
    expect(() => agentCLIRuntimeSchema.parse({ type: '' })).toThrow()
  })

  it('rejects missing type', () => {
    expect(() => agentCLIRuntimeSchema.parse({})).toThrow()
  })

  it('rejects wrong types for optional fields', () => {
    expect(() => agentCLIRuntimeSchema.parse({ type: 'copilot', defaultMaxTurns: 'many' })).toThrow()
    expect(() => agentCLIRuntimeSchema.parse({ type: 'copilot', defaultAllowBash: 'yes' })).toThrow()
  })
})

describe('agentSpecSchema', () => {
  it('parses valid data with all fields', () => {
    const data = {
      providerRef: { name: 'openai', namespace: 'default' },
      model: { provider: 'openai', name: 'gpt-4', temperature: 0.7 },
      systemPrompt: {
        inline: 'You are helpful',
        configMapRef: { name: 'prompt-cm', key: 'prompt.txt' },
      },
      tools: [{ name: 'search', enabled: true }],
      skills: [{ configMapRef: { name: 'skill-cm', key: 'skill.md' } }],
      resources: { limits: { memory: '256Mi' } },
      secretRef: { name: 'api-keys' },
      session: { persistence: 'always', ttl: '1h', maxMessages: 100 },
      rateLimit: { requestsPerMinute: 60, tokensPerMinute: 10000 },
      coordination: {
        enabled: true,
        allowedAgents: [{ name: 'helper', namespace: 'default' }],
        maxConcurrentChildren: 5,
        maxDepth: 3,
      },
      runtime: { type: 'copilot', defaultMaxTurns: 50 },
    }
    expect(agentSpecSchema.parse(data)).toEqual(data)
  })

  it('parses empty object (all optional)', () => {
    expect(agentSpecSchema.parse({})).toEqual({})
  })

  it('parses with nested optional fields', () => {
    const data = {
      model: { provider: 'anthropic' },
      coordination: { enabled: false },
    }
    expect(agentSpecSchema.parse(data)).toEqual(data)
  })

  it('rejects invalid coordination (missing enabled)', () => {
    expect(() => agentSpecSchema.parse({ coordination: {} })).toThrow()
  })

  it('rejects invalid systemPrompt configMapRef', () => {
    expect(() =>
      agentSpecSchema.parse({ systemPrompt: { configMapRef: { name: 'x' } } })
    ).toThrow()
  })
})

describe('agentStatusSchema', () => {
  it('parses valid data with all fields', () => {
    const data = {
      activeTasks: 3,
      lastUsed: '2024-01-01T00:00:00Z',
      conditions: [{ type: 'Ready', status: 'True', reason: 'Configured' }],
    }
    expect(agentStatusSchema.parse(data)).toEqual(data)
  })

  it('parses with only required fields', () => {
    expect(agentStatusSchema.parse({ activeTasks: 0 })).toEqual({ activeTasks: 0 })
  })

  it('rejects missing activeTasks', () => {
    expect(() => agentStatusSchema.parse({})).toThrow()
  })

  it('rejects wrong types', () => {
    expect(() => agentStatusSchema.parse({ activeTasks: 'three' })).toThrow()
  })

  it('rejects invalid conditions', () => {
    expect(() =>
      agentStatusSchema.parse({ activeTasks: 0, conditions: [{ type: 'Ready' }] })
    ).toThrow()
  })
})

describe('agentSchema', () => {
  it('parses valid full agent', () => {
    const data = {
      apiVersion: 'core.mercan.ai/v1alpha1',
      kind: 'Agent',
      metadata: {
        name: 'my-agent',
        namespace: 'default',
        uid: 'abc-123',
        creationTimestamp: '2024-01-01T00:00:00Z',
        labels: { app: 'test' },
        annotations: { note: 'val' },
      },
      spec: {
        model: { provider: 'openai', name: 'gpt-4' },
        tools: [{ name: 'search' }],
      },
      status: { activeTasks: 1, lastUsed: '2024-01-01T00:00:00Z' },
    }
    expect(agentSchema.parse(data)).toEqual(data)
  })

  it('parses minimal agent', () => {
    const data = {
      metadata: { name: 'my-agent' },
      spec: {},
    }
    expect(agentSchema.parse(data)).toEqual(data)
  })

  it('rejects missing metadata', () => {
    expect(() => agentSchema.parse({ spec: {} })).toThrow()
  })

  it('rejects missing spec', () => {
    expect(() => agentSchema.parse({ metadata: { name: 'x' } })).toThrow()
  })

  it('rejects missing metadata name', () => {
    expect(() => agentSchema.parse({ metadata: {}, spec: {} })).toThrow()
  })
})

describe('exported types', () => {
  it('Agent type matches schema', () => {
    const agent: Agent = {
      metadata: { name: 'test' },
      spec: {},
    }
    expect(agentSchema.parse(agent)).toBeDefined()
  })

  it('AgentSpec type matches schema', () => {
    const spec: AgentSpec = {}
    expect(agentSpecSchema.parse(spec)).toBeDefined()
  })

  it('AgentStatus type matches schema', () => {
    const status: AgentStatus = { activeTasks: 0 }
    expect(agentStatusSchema.parse(status)).toBeDefined()
  })
})
