import { describe, expect, it } from 'vitest'
import { parse } from 'yaml'
import {
  destinationDomain,
  draftConcernYaml,
  featureReason,
  pathDirectoryGlob,
  queryTerm,
  removeTerm,
  scoreVsThreshold,
  topTriggeredScore,
} from './oversight'
import { parseSSEFrames } from '@/hooks/use-oversight-stream'
import { makeDecision, riskyDecision } from '@/test/fixtures/oversight'

describe('oversight helpers', () => {
  it('builds key:value terms and quotes values with spaces', () => {
    expect(queryTerm('bin', 'wget')).toBe('bin:wget')
    expect(queryTerm('path', '/tmp/a b')).toBe('path:"/tmp/a b"')
    expect(queryTerm('task', 'a"b')).toBe('task:ab')
  })

  it('removes one echoed term', () => {
    expect(removeTerm([{ key: 'bin', value: 'wget' }, { key: 'text', value: 'two words' }, { key: 'tool', value: 'x' }], 0)).toBe('"two words" tool:x')
  })

  it('derives a destination domain and a path directory glob', () => {
    expect(destinationDomain('cdn.registry.example:443')).toBe('registry.example')
    expect(destinationDomain('10.0.0.1:443')).toBeUndefined()
    expect(pathDirectoryGlob('/etc/passwd')).toBe('/etc/*')
    expect(pathDirectoryGlob('/passwd')).toBeUndefined()
  })

  it('reads the top triggered score as score vs threshold', () => {
    const top = topTriggeredScore(riskyDecision)
    expect(top?.questionID).toBe('K2')
    expect(scoreVsThreshold(top!)).toBe('K2 0.91 ≥ 0.82')
  })

  it('says why a similar action matched', () => {
    expect(featureReason({ kind: 'tool', value: 'code_exec' })).toBe('same tool code_exec')
    expect(featureReason({ kind: 'dest_domain', value: 'registry.example' })).toBe('dest_domain registry.example')
  })

  it('drafts a concern from selected narratives and false alarms', () => {
    const falseAlarm = makeDecision({ id: 'fa', actionNarrative: 'Benign fetch.', feedback: { label: 'false_alarm', recordedAt: '2026-11-25T00:00:00Z' } })
    const text = draftConcernYaml('bin:wget', [riskyDecision, falseAlarm], [riskyDecision, falseAlarm])
    const draft = parse(text)
    expect(draft.id).toBe('CONCERN-ID')
    expect(draft.description).toContain('bin:wget')
    expect(draft.examples).toEqual(['Downloaded a model archive.'])
    expect(draft.not_examples).toEqual(['Benign fetch.'])
  })

  it('parses SSE frames, skipping comments and keeping the tail', () => {
    const { frames, rest } = parseSSEFrames('event: ready\ndata: {}\n\n: heartbeat\n\nid: 7\nevent: decision\ndata: {"a":1}\n\nid: 8\nevent: dec')
    expect(frames).toEqual([
      { id: undefined, event: 'ready', data: '{}' },
      { id: '7', event: 'decision', data: '{"a":1}' },
    ])
    expect(rest).toBe('id: 8\nevent: dec')
  })
})
