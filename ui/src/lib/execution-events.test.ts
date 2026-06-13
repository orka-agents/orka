import { describe, it, expect } from 'vitest'
import {
  parseSSEBlock,
  parseExecutionEventFrame,
  SSEFrameBuffer,
  executionEventCategory,
  normalizeSeverity,
  isTruncated,
  executionEventApi,
  executionEventApiPath,
  mergeEventsBySeq,
  maxSeq,
  EXECUTION_EVENT_CATEGORY_ORDER,
} from './execution-events'

function eventData(overrides: Record<string, unknown> = {}) {
  return JSON.stringify({
    id: 'evt-1',
    namespace: 'default',
    streamType: 'task',
    streamID: 'my-task',
    seq: 1,
    type: 'TaskStarted',
    severity: 'info',
    createdAt: '2026-06-13T00:00:00Z',
    ...overrides,
  })
}

describe('parseSSEBlock', () => {
  it('parses id/event/data fields with single-space stripping', () => {
    const raw = parseSSEBlock('id: 5\nevent: execution_event\ndata: {"a":1}')
    expect(raw).not.toBeNull()
    expect(raw?.id).toBe('5')
    expect(raw?.event).toBe('execution_event')
    expect(raw?.data).toBe('{"a":1}')
    expect(raw?.isComment).toBe(false)
  })

  it('joins multi-line data with newlines', () => {
    const raw = parseSSEBlock('event: execution_event\ndata: line1\ndata: line2')
    expect(raw?.data).toBe('line1\nline2')
  })

  it('classifies comment-only blocks as heartbeat comments', () => {
    const raw = parseSSEBlock(': heartbeat')
    expect(raw?.isComment).toBe(true)
  })

  it('returns null for an empty block', () => {
    expect(parseSSEBlock('')).toBeNull()
    expect(parseSSEBlock('\n')).toBeNull()
  })

  it('tolerates CRLF line endings', () => {
    const raw = parseSSEBlock('id: 3\r\nevent: execution_event\r\ndata: {"a":1}')
    expect(raw?.id).toBe('3')
    expect(raw?.event).toBe('execution_event')
    expect(raw?.data).toBe('{"a":1}')
  })
})

describe('parseExecutionEventFrame', () => {
  it('parses execution_event frames', () => {
    const frame = parseExecutionEventFrame(`id: 1\nevent: execution_event\ndata: ${eventData()}`)
    expect(frame?.kind).toBe('event')
    if (frame?.kind === 'event') {
      expect(frame.seq).toBe(1)
      expect(frame.event.type).toBe('TaskStarted')
    }
  })

  it('parses stream_complete frames', () => {
    const frame = parseExecutionEventFrame(
      'id: 9\nevent: stream_complete\ndata: {"lastSeq":9,"type":"TaskSucceeded"}',
    )
    expect(frame?.kind).toBe('complete')
    if (frame?.kind === 'complete') {
      expect(frame.seq).toBe(9)
      expect(frame.complete.type).toBe('TaskSucceeded')
    }
  })

  it('classifies heartbeat comment frames', () => {
    const frame = parseExecutionEventFrame(': heartbeat')
    expect(frame?.kind).toBe('heartbeat')
  })

  it('classifies invalid JSON as unknown without throwing', () => {
    const frame = parseExecutionEventFrame('event: execution_event\ndata: {not json')
    expect(frame?.kind).toBe('unknown')
  })

  it('classifies schema-mismatched payloads as unknown', () => {
    const frame = parseExecutionEventFrame('event: execution_event\ndata: {"foo":"bar"}')
    expect(frame?.kind).toBe('unknown')
  })
})

describe('SSEFrameBuffer', () => {
  it('emits frames as blank-line-delimited blocks arrive', () => {
    const buf = new SSEFrameBuffer()
    const frames = buf.push(
      `id: 1\nevent: execution_event\ndata: ${eventData({ seq: 1 })}\n\n` +
        `id: 2\nevent: execution_event\ndata: ${eventData({ seq: 2 })}\n\n`,
    )
    expect(frames).toHaveLength(2)
    expect(frames[0].kind).toBe('event')
    expect(frames[1].kind).toBe('event')
  })

  it('buffers partial frames across chunks', () => {
    const buf = new SSEFrameBuffer()
    const first = buf.push('id: 1\nevent: execution_event\ndata: ')
    expect(first).toHaveLength(0)
    const second = buf.push(`${eventData({ seq: 1 })}\n\n`)
    expect(second).toHaveLength(1)
    expect(second[0].kind).toBe('event')
  })

  it('handles heartbeat frames interleaved with events', () => {
    const buf = new SSEFrameBuffer()
    const frames = buf.push(
      `: heartbeat\n\nid: 1\nevent: execution_event\ndata: ${eventData({ seq: 1 })}\n\n`,
    )
    expect(frames.map((f) => f.kind)).toEqual(['heartbeat', 'event'])
  })

  it('flush returns a trailing block without a final separator', () => {
    const buf = new SSEFrameBuffer()
    buf.push(`event: stream_complete\ndata: {"lastSeq":4,"type":"TaskFailed"}`)
    const flushed = buf.flush()
    expect(flushed).toHaveLength(1)
    expect(flushed[0].kind).toBe('complete')
  })
})

describe('executionEventCategory', () => {
  it('maps known types to functional categories', () => {
    expect(executionEventCategory('TaskStarted')).toBe('lifecycle')
    expect(executionEventCategory('ModelRequestStarted')).toBe('model')
    expect(executionEventCategory('ToolCallFailed')).toBe('tools')
    expect(executionEventCategory('WorkspacePreparationStarted')).toBe('workspace')
    expect(executionEventCategory('ArtifactUploadCompleted')).toBe('artifacts')
    expect(executionEventCategory('ApprovalRequested')).toBe('approvals')
    expect(executionEventCategory('TaskForkCreated')).toBe('fork')
    expect(executionEventCategory('WorkerStarted')).toBe('worker')
  })

  it('maps unknown types to other', () => {
    expect(executionEventCategory('SomethingNew')).toBe('other')
  })

  it('every mapped category is present in the display order', () => {
    expect(new Set(EXECUTION_EVENT_CATEGORY_ORDER).size).toBe(EXECUTION_EVENT_CATEGORY_ORDER.length)
  })
})

describe('normalizeSeverity', () => {
  it('coerces known severities', () => {
    expect(normalizeSeverity('error')).toBe('error')
    expect(normalizeSeverity('WARNING')).toBe('warning')
    expect(normalizeSeverity('debug')).toBe('debug')
  })

  it('defaults unknown/empty to info', () => {
    expect(normalizeSeverity()).toBe('info')
    expect(normalizeSeverity('weird')).toBe('info')
  })
})

describe('isTruncated', () => {
  it('detects any truncation flag', () => {
    expect(isTruncated({ truncation: { summaryTruncated: true } })).toBe(true)
    expect(isTruncated({ truncation: { contentJsonTruncated: true } })).toBe(true)
  })
  it('returns false without truncation', () => {
    expect(isTruncated({})).toBe(false)
    expect(isTruncated({ truncation: { summaryOriginalChars: 0 } })).toBe(false)
  })
})

describe('path builders', () => {
  it('produce stable, encoded paths', () => {
    expect(executionEventApi.taskEvents('a b')).toBe('/api/v1/tasks/a%20b/events')
    expect(executionEventApi.taskStream('t')).toBe('/api/v1/tasks/t/stream')
    expect(executionEventApi.sessionStream('s')).toBe('/api/v1/sessions/s/stream')
    expect(executionEventApiPath.taskApprovalDecision('t', 'ap 1')).toBe(
      '/tasks/t/approvals/ap%201/decision',
    )
    expect(executionEventApiPath.taskFork('t')).toBe('/tasks/t/fork')
  })
})

describe('mergeEventsBySeq', () => {
  it('dedupes by seq and sorts ascending, later source wins', () => {
    const a = [{ seq: 1, v: 'a1' }, { seq: 2, v: 'a2' }]
    const b = [{ seq: 2, v: 'b2' }, { seq: 3, v: 'b3' }]
    const merged = mergeEventsBySeq(a, b)
    expect(merged.map((e) => e.seq)).toEqual([1, 2, 3])
    expect(merged.find((e) => e.seq === 2)?.v).toBe('b2')
  })

  it('handles empty sources', () => {
    expect(mergeEventsBySeq<{ seq: number }>([], [])).toEqual([])
  })
})

describe('maxSeq', () => {
  it('returns the highest seq or the fallback', () => {
    expect(maxSeq([{ seq: 3 }, { seq: 7 }, { seq: 1 }])).toBe(7)
    expect(maxSeq([], 5)).toBe(5)
  })
})
