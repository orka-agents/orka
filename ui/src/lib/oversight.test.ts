import { describe, expect, it } from 'vitest'
import { parse } from 'yaml'
import {
  destinationDomain,
  draftConcernYaml,
  formatBytes,
  groupIncidents,
  rowSummary,
  stripSynthetic,
  strongestEvidence,
  featureReason,
  pathDirectoryGlob,
  queryTerm,
  removeTerm,
  scoreVsThreshold,
  topTriggeredScore,
} from './oversight'
import { parseSSEFrames } from '@/hooks/use-oversight-stream'
import { charterConfig, charterDecision, makeDecision, riskyDecision, unavailableDecision } from '@/test/fixtures/oversight'

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

  it('formats bytes and strips the synthetic marker', () => {
    expect(formatBytes(2.6 * 1024 * 1024)).toBe('2.6 MB')
    expect(formatBytes(2048)).toBe('2 KB')
    expect(formatBytes(12)).toBe('12 B')
    expect(stripSynthetic('[synthetic] pushed')).toBe('pushed')
    expect(stripSynthetic('real [synthetic]')).toBe('real [synthetic]')
  })

  it('summarises a row from its fields', () => {
    const push = makeDecision({
      action: { toolName: 'shell', effects: { bytesSent: 2.6 * 1024 * 1024, destinations: [{ value: 'registry.demo.svc:443', count: 1 }] } },
      charterFindings: [{ producerRef: 'rules-a', boundID: 'B0', sentence: 'x' }],
      signal: 'against_charter',
    })
    expect(rowSummary(push, charterConfig)).toBe('sent 2.6 MB to registry.demo.svc:443 — breaks charter rule 1')
    expect(strongestEvidence(push)).toBe('kernel saw 2.6 MB sent to registry.demo.svc:443')
    expect(strongestEvidence(makeDecision())).toBeUndefined()
  })

  it('groups only what needs a person into incidents, most severe first', () => {
    const pause = (id: string, seq: number) => makeDecision({ id, seq, wouldHavePaused: true, signal: 'risky' })
    const incidents = groupIncidents([
      pause('p1', 1), pause('p2', 2), charterDecision, unavailableDecision,
      makeDecision({ id: 'r', signal: 'risky' }), makeDecision({ id: 'c' }),
    ], charterConfig)
    expect(incidents.map((i) => [i.kind, i.decisions.map((d) => d.id)])).toEqual([
      ['B1', ['build-2@1']],
      ['would-hold', ['build-3@1']],
      ['would-pause', ['p2', 'p1']],
    ])
    expect(incidents[0].title).toBe('agent-a broke charter rule 2: Never write outside the workspace.')
  })
})
