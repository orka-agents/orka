import { describe, it, expect } from 'vitest'
import {
  sessionSchema,
  sessionListItemSchema,
  transcriptMessageSchema,
} from './session'
import type { Session, SessionListItem, TranscriptMessage } from './session'

describe('sessionSchema', () => {
  it('parses valid data with all fields', () => {
    const data = {
      name: 'sess-1',
      namespace: 'default',
      transcript: '{"role":"user","content":"hi"}',
      messageCount: '5',
      inputTokens: '100',
      outputTokens: '200',
      activeTask: 'task-1',
      createdAt: '2024-01-01T00:00:00Z',
      updatedAt: '2024-01-01T01:00:00Z',
    }
    expect(sessionSchema.parse(data)).toEqual(data)
  })

  it('parses with only required fields', () => {
    const data = { name: 'sess-1', namespace: 'default' }
    expect(sessionSchema.parse(data)).toEqual(data)
  })

  it('rejects missing name', () => {
    expect(() => sessionSchema.parse({ namespace: 'default' })).toThrow()
  })

  it('rejects missing namespace', () => {
    expect(() => sessionSchema.parse({ name: 'sess-1' })).toThrow()
  })

  it('rejects wrong types', () => {
    expect(() => sessionSchema.parse({ name: 123, namespace: 'default' })).toThrow()
    expect(() => sessionSchema.parse({ name: 'sess-1', namespace: 'default', messageCount: 5 })).toThrow()
  })
})

describe('sessionListItemSchema', () => {
  it('parses valid data with all fields', () => {
    const data = {
      name: 'sess-1',
      namespace: 'default',
      messageCount: '10',
      inputTokens: '500',
      outputTokens: '800',
      activeTask: 'task-2',
      createdAt: '2024-01-01T00:00:00Z',
      updatedAt: '2024-01-01T02:00:00Z',
    }
    expect(sessionListItemSchema.parse(data)).toEqual(data)
  })

  it('parses with only required fields', () => {
    const data = { name: 'sess-1', namespace: 'default' }
    expect(sessionListItemSchema.parse(data)).toEqual(data)
  })

  it('rejects missing required fields', () => {
    expect(() => sessionListItemSchema.parse({})).toThrow()
    expect(() => sessionListItemSchema.parse({ name: 'sess-1' })).toThrow()
  })

  it('rejects wrong types for optional fields', () => {
    expect(() =>
      sessionListItemSchema.parse({ name: 'sess-1', namespace: 'default', inputTokens: 100 })
    ).toThrow()
  })
})

describe('transcriptMessageSchema', () => {
  it('parses valid data with all fields', () => {
    const data = {
      role: 'assistant',
      content: 'Hello!',
      timestamp: '2024-01-01T00:00:00Z',
      model: 'gpt-4',
      inputTokens: 50,
      outputTokens: 100,
    }
    expect(transcriptMessageSchema.parse(data)).toEqual(data)
  })

  it('parses with only required fields', () => {
    const data = { role: 'user', content: 'Hi' }
    expect(transcriptMessageSchema.parse(data)).toEqual(data)
  })

  it('rejects missing required fields', () => {
    expect(() => transcriptMessageSchema.parse({ role: 'user' })).toThrow()
    expect(() => transcriptMessageSchema.parse({ content: 'Hi' })).toThrow()
    expect(() => transcriptMessageSchema.parse({})).toThrow()
  })

  it('rejects wrong types', () => {
    expect(() => transcriptMessageSchema.parse({ role: 'user', content: 'Hi', inputTokens: '50' })).toThrow()
    expect(() => transcriptMessageSchema.parse({ role: 123, content: 'Hi' })).toThrow()
  })
})

describe('exported types', () => {
  it('Session type matches schema', () => {
    const session: Session = { name: 'sess-1', namespace: 'default' }
    expect(sessionSchema.parse(session)).toBeDefined()
  })

  it('SessionListItem type matches schema', () => {
    const item: SessionListItem = { name: 'sess-1', namespace: 'default' }
    expect(sessionListItemSchema.parse(item)).toBeDefined()
  })

  it('TranscriptMessage type matches schema', () => {
    const msg: TranscriptMessage = { role: 'user', content: 'test' }
    expect(transcriptMessageSchema.parse(msg)).toBeDefined()
  })
})
