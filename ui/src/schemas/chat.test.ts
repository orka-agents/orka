import { describe, it, expect } from 'vitest'
import {
  chatRequestSchema,
  chatUsageSchema,
  chatToolCallSchema,
  chatResponseSchema,
  chatConfigSchema,
} from './chat'
import type { ChatRequest, ChatUsage, ChatToolCall, ChatResponse, ChatConfig } from './chat'

describe('chatRequestSchema', () => {
  it('parses valid data with all fields', () => {
    const data = {
      message: 'Hello',
      sessionId: 'sess-1',
      namespace: 'default',
      provider: 'openai',
      model: 'gpt-4',
      temperature: 0.7,
      maxTokens: 1000,
      systemPrompt: 'Be helpful',
      agentRef: 'my-agent',
    }
    expect(chatRequestSchema.parse(data)).toEqual(data)
  })

  it('parses with only required fields', () => {
    const data = { message: 'Hello' }
    expect(chatRequestSchema.parse(data)).toEqual(data)
  })

  it('rejects missing message', () => {
    expect(() => chatRequestSchema.parse({})).toThrow()
    expect(() => chatRequestSchema.parse({ sessionId: 's1' })).toThrow()
  })

  it('rejects wrong types', () => {
    expect(() => chatRequestSchema.parse({ message: 123 })).toThrow()
    expect(() => chatRequestSchema.parse({ message: 'Hi', temperature: 'warm' })).toThrow()
  })
})

describe('chatUsageSchema', () => {
  it('parses valid data with all fields', () => {
    const data = {
      inputTokens: 100,
      outputTokens: 200,
      llmCalls: 3,
      toolCalls: 2,
      tasksCreated: 1,
      duration: '5s',
    }
    expect(chatUsageSchema.parse(data)).toEqual(data)
  })

  it('parses empty object (all optional)', () => {
    expect(chatUsageSchema.parse({})).toEqual({})
  })

  it('rejects wrong types', () => {
    expect(() => chatUsageSchema.parse({ inputTokens: '100' })).toThrow()
    expect(() => chatUsageSchema.parse({ llmCalls: true })).toThrow()
  })
})

describe('chatToolCallSchema', () => {
  it('parses valid data', () => {
    const data = { name: 'search', args: { query: 'test' }, result: { items: [] } }
    expect(chatToolCallSchema.parse(data)).toEqual(data)
  })

  it('parses with only required fields', () => {
    const data = { name: 'search', args: null }
    expect(chatToolCallSchema.parse(data)).toEqual(data)
  })

  it('parses with various args types', () => {
    expect(chatToolCallSchema.parse({ name: 't', args: 'string' })).toBeDefined()
    expect(chatToolCallSchema.parse({ name: 't', args: 42 })).toBeDefined()
    expect(chatToolCallSchema.parse({ name: 't', args: [1, 2] })).toBeDefined()
  })

  it('rejects missing name', () => {
    expect(() => chatToolCallSchema.parse({ args: {} })).toThrow()
  })
})

describe('chatResponseSchema', () => {
  it('parses valid data with all fields', () => {
    const data = {
      sessionId: 'sess-1',
      message: 'Hello!',
      toolCalls: [{ name: 'search', args: { q: 'test' } }],
      usage: { inputTokens: 50, outputTokens: 100 },
    }
    expect(chatResponseSchema.parse(data)).toEqual(data)
  })

  it('parses with only required fields', () => {
    const data = { sessionId: 'sess-1', message: 'Hello!' }
    expect(chatResponseSchema.parse(data)).toEqual(data)
  })

  it('rejects missing required fields', () => {
    expect(() => chatResponseSchema.parse({ sessionId: 'sess-1' })).toThrow()
    expect(() => chatResponseSchema.parse({ message: 'Hello!' })).toThrow()
    expect(() => chatResponseSchema.parse({})).toThrow()
  })

  it('rejects invalid toolCalls', () => {
    expect(() =>
      chatResponseSchema.parse({ sessionId: 's', message: 'm', toolCalls: [{ invalid: true }] })
    ).toThrow()
  })
})

describe('chatConfigSchema', () => {
  it('parses valid data', () => {
    const data = {
      enabled: true,
      provider: 'openai',
      model: 'gpt-4',
      maxIterations: 10,
      maxDuration: '60s',
      maxTasksPerTurn: 5,
      maxConcurrent: 3,
      availableTools: ['search', 'calculate'],
    }
    expect(chatConfigSchema.parse(data)).toEqual(data)
  })

  it('rejects missing required fields', () => {
    expect(() => chatConfigSchema.parse({})).toThrow()
    expect(() => chatConfigSchema.parse({ enabled: true })).toThrow()
    expect(() =>
      chatConfigSchema.parse({
        enabled: true,
        provider: 'openai',
        model: 'gpt-4',
        maxIterations: 10,
        maxDuration: '60s',
        maxTasksPerTurn: 5,
        maxConcurrent: 3,
        // missing availableTools
      })
    ).toThrow()
  })

  it('rejects wrong types', () => {
    expect(() =>
      chatConfigSchema.parse({
        enabled: 'yes',
        provider: 'openai',
        model: 'gpt-4',
        maxIterations: 10,
        maxDuration: '60s',
        maxTasksPerTurn: 5,
        maxConcurrent: 3,
        availableTools: [],
      })
    ).toThrow()
    expect(() =>
      chatConfigSchema.parse({
        enabled: true,
        provider: 'openai',
        model: 'gpt-4',
        maxIterations: 'ten',
        maxDuration: '60s',
        maxTasksPerTurn: 5,
        maxConcurrent: 3,
        availableTools: [],
      })
    ).toThrow()
  })
})

describe('exported types', () => {
  it('ChatRequest type matches schema', () => {
    const req: ChatRequest = { message: 'Hello' }
    expect(chatRequestSchema.parse(req)).toBeDefined()
  })

  it('ChatUsage type matches schema', () => {
    const usage: ChatUsage = {}
    expect(chatUsageSchema.parse(usage)).toBeDefined()
  })

  it('ChatToolCall type matches schema', () => {
    const call: ChatToolCall = { name: 'tool', args: null }
    expect(chatToolCallSchema.parse(call)).toBeDefined()
  })

  it('ChatResponse type matches schema', () => {
    const resp: ChatResponse = { sessionId: 's', message: 'm' }
    expect(chatResponseSchema.parse(resp)).toBeDefined()
  })

  it('ChatConfig type matches schema', () => {
    const config: ChatConfig = {
      enabled: true,
      provider: 'p',
      model: 'm',
      maxIterations: 1,
      maxDuration: '1s',
      maxTasksPerTurn: 1,
      maxConcurrent: 1,
      availableTools: [],
    }
    expect(chatConfigSchema.parse(config)).toBeDefined()
  })
})
