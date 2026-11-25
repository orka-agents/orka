import { describe, expect, it } from 'vitest'
import { parse } from 'yaml'
import {
  actionPhrase,
  confidenceReasons,
  credentialStory,
  destinationDomain,
  draftConcernYaml,
  formatBytes,
  distributionRepo,
  groupIncidents,
  parseImageRef,
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
import { charterConfig, charterDecision, makeDecision, makePosture, riskyDecision, unavailableDecision } from '@/test/fixtures/oversight'
import type { PostureResponse } from '@/schemas/oversight'

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
    expect(rowSummary(push, charterConfig)).toBe('sent 2.6 MB to registry.demo.svc:443 — breaks rule 1')
    expect(strongestEvidence(push)).toBe('kernel saw 2.6 MB sent to registry.demo.svc:443')
    expect(strongestEvidence(makeDecision())).toBeUndefined()
  })

  it('groups what needs a person into one incident per agent run, most severe first', () => {
    const pause = (id: string, seq: number) => makeDecision({ id, seq, wouldHavePaused: true, signal: 'risky' })
    const incidents = groupIncidents([
      pause('p1', 1), pause('p2', 2), { ...charterDecision, agentName: 'agent-b' }, { ...unavailableDecision, agentName: 'agent-c' },
      makeDecision({ id: 'r', signal: 'risky' }), makeDecision({ id: 'c' }),
    ], charterConfig)
    expect(incidents.map((i) => [i.actor, i.badge, i.decisions.map((d) => d.id)])).toEqual([
      ['agent-b', 'against_charter', ['build-2@1']],
      ['agent-c', 'would_hold', ['build-3@1']],
      ['agent-a', 'would_pause', ['p2', 'p1']],
    ])
    expect(incidents[2].title).toBe('agent-a ran code_exec — run the tests — would pause')
    expect(incidents[1].title).toBe('scorer unavailable — would hold · agent-c ran code_exec — run the tests')
  })

  it('makes one card of an agent run that broke two rules, in the order first broken', () => {
    const run = (id: string, seq: number, bound: string, effects: object, taintFlags: string[] = []) => makeDecision({
      id, seq, taskName: 'report-7', agentName: 'report-agent', signal: 'against_charter',
      charterFindings: [{ producerRef: 'rules-a', boundID: bound, sentence: `sentence ${bound}` }],
      action: { toolName: 'code_exec', effects, taintFlags },
    })
    const incidents = groupIncidents([
      run('r1', 1, 'B2', { counts: { credential_read: 1 }, paths: [{ value: '/var/run/secrets/token', count: 1 }] }, ['credential_read']),
      run('r2', 2, 'B1', { counts: { net_connect: 1 }, destinations: [{ value: 'sink.demo.svc:443', count: 1 }] }),
      makeDecision({ id: 'other-run', seq: 3, taskName: 'report-8', agentName: 'report-agent', wouldHavePaused: true, signal: 'risky' }),
    ], charterConfig)
    expect(incidents).toHaveLength(2)
    expect(incidents[0].decisions.map((d) => d.id)).toEqual(['r2', 'r1'])
    expect(incidents[0].title).toBe('report-agent read a token, then sent it to sink.demo.svc — breaks rules 3 and 2')
    expect(incidents[0].detail).toBe('Rule 3: Never send workspace data to undeclared hosts.\nRule 2: Never write outside the workspace.')
  })

  it('reads image references generically', () => {
    expect(parseImageRef('push registry.demo.svc/prod/api:latest')).toEqual({ host: 'registry.demo.svc', path: 'prod', name: 'api', tag: 'latest' })
    expect(parseImageRef('push oci://reg:5000/a/b/c:v1')).toEqual({ host: 'reg:5000', path: 'a/b', name: 'c', tag: 'v1' })
    expect(parseImageRef('push localhost/app:dev')).toEqual({ host: 'localhost', path: '', name: 'app', tag: 'dev' })
    expect(parseImageRef('vim src/app.ts:12')).toBeUndefined()
    expect(parseImageRef('team/app:1')).toBeUndefined()
  })

  it('reads an OCI distribution path as its repository', () => {
    expect(distributionRepo('/v2/prod/api/blobs/uploads/')).toBe('prod/api')
    expect(distributionRepo('/v2/prod/api/blobs/uploads/7a1c-uuid')).toBe('prod/api')
    expect(distributionRepo('/v2/prod/api/manifests/latest')).toBe('prod/api')
    expect(distributionRepo('/upload/here/')).toBe('upload/here')
  })

  it('says a token was read on a row with a credential read', () => {
    const read = makeDecision({
      agentName: 'report-agent',
      signal: 'against_charter',
      charterFindings: [{ producerRef: 'rules-a', boundID: 'B2', sentence: 's' }],
      action: {
        toolName: 'code_exec',
        requestSummary: 'Write the weekly status report',
        effects: { counts: { credential_read: 1 }, binaries: [{ value: '/usr/bin/python3', count: 1 }], paths: [{ value: '/var/run/secrets/token', count: 1 }] },
      },
    })
    expect(rowSummary(read, charterConfig)).toBe('report-agent read a token — breaks rule 3')
  })

  it('titles an incident from its most descriptive decision and sums bytes only when all have them', () => {
    const base = { taskName: 'push-1', agentName: 'registry-pusher', signal: 'against_charter' as const, charterFindings: [{ producerRef: 'r', boundID: 'B0', sentence: 's' }] }
    const upload = (seq: number, bytes?: number) => makeDecision({
      ...base, id: `u${seq}`, seq,
      action: { toolName: 'code_exec', effects: { bytesSent: bytes, destinations: [{ value: 'registry.demo.svc:443', count: 1 }], notableEvents: [{ id: 'e', kind: 'exec', args: 'curl -X PUT /v2/prod/api/blobs/uploads/' }] } },
    })
    const push = (seq: number, bytes?: number) => makeDecision({
      ...base, id: `p${seq}`, seq,
      action: { toolName: 'code_exec', effects: { bytesSent: bytes, destinations: [{ value: 'registry.demo.svc:443', count: 1 }], notableEvents: [{ id: 'e', kind: 'exec', args: 'crane push registry.demo.svc/prod/api:latest' }] } },
    })
    const all = groupIncidents([push(1, 1024 * 1024), upload(2, 1024 * 1024)], charterConfig)
    expect(all).toHaveLength(1)
    expect(all[0].title).toBe('registry-pusher pushed api:latest to registry.demo.svc/prod (2 MB) — breaks rule 1')
    const partial = groupIncidents([push(1, 1024 * 1024), upload(2)], charterConfig)
    expect(partial[0].title).toBe('registry-pusher pushed api:latest to registry.demo.svc/prod — breaks rule 1')
  })
})

describe('confidenceReasons', () => {
  const posture = (reasons: string[], signals: Record<string, number> = {}, extra: Partial<PostureResponse['basis']> = {}) =>
    ({ ...makePosture(), confidence: { state: 'degraded', summary: '', reasons },
      basis: { ...makePosture().basis, signals, ...extra } }) as PostureResponse

  it('never leaves the state word bare and leaves audit mode to its own item', () => {
    expect(confidenceReasons(posture(['audit_mode_only', 'canaries_not_reported']))).toEqual(['no canaries reported'])
  })

  it('puts a scorer outage before unreviewed risk, with counts', () => {
    const p = posture(['risky_unreviewed', 'held_or_fail_closed', 'canaries_not_reported'],
      { monitor_unavailable: 1, risky: 3 }, { unreviewedFlagged: 3 })
    expect(confidenceReasons(p)).toEqual(['scorer unavailable on 1 action', '3 risky unreviewed'])
  })
})

describe('blocked attempts', () => {
  const blockedPush = makeDecision({
    agentName: 'registry-pusher',
    action: {
      toolName: 'code_exec',
      effects: {
        counts: { exec: 1, connect_blocked: 1 },
        blockedCount: 1,
        destinations: [{ value: 'registry.demo.svc:80', count: 1 }],
        notableEvents: [
          { id: 'e1', kind: 'exec', binary: '/usr/local/bin/python3', args: '/tmp/push.py push /tmp/image.tar registry.demo.svc/prod/api:latest' },
          { id: 'e2', kind: 'connect_blocked', dest: 'registry.demo.svc:80', outcome: 'denied by policy' },
        ],
      },
    },
  })

  it('never reads as a completed push', () => {
    expect(actionPhrase(blockedPush)).toBe('tried to push api:latest to registry.demo.svc/prod (blocked by policy)')
    expect(strongestEvidence(blockedPush)).toBe('kernel saw a blocked connection to registry.demo.svc:80; no data seen leaving')
  })

  it('says a blocked send after a token read was only attempted', () => {
    const read = makeDecision({
      id: 'r@0', seq: 1,
      action: { toolName: 'code_exec', taintFlags: ['credential_read'],
        effects: { counts: { credential_read: 1 }, paths: [{ value: '/var/run/secrets/kubernetes.io/serviceaccount/token', count: 1 }] } },
    })
    const send = makeDecision({
      id: 'r@1', seq: 2,
      action: { toolName: 'code_exec', taintFlags: ['credential_read'],
        effects: { counts: { connect_blocked: 1 }, blockedCount: 1, destinations: [{ value: 'paste.demo.svc:80', count: 1 }] } },
    })
    expect(credentialStory([send, read])).toBe('read a token, then tried to send it to paste.demo.svc (blocked by policy)')
  })
})
