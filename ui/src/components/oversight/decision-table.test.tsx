import { describe, expect, it, vi } from 'vitest'
import userEvent from '@testing-library/user-event'
import { render, screen } from '@/test/test-utils'
vi.mock('zustand/middleware', () => ({ persist: (fn: unknown) => fn }))

import { useAuthStore } from '@/stores/auth'
import { DecisionTable } from './decision-table'
import { QuestionMatrix } from './question-matrix'
import { http, HttpResponse } from 'msw'
import { server } from '@/test/mocks/server'
import { charterConfig, charterDecision, makeDecision, riskyDecision, unavailableDecision } from '@/test/fixtures/oversight'

const rowOf = (signal: string) => document.querySelector<HTMLElement>(`tr[data-signal="${signal}"]`)!

describe('DecisionTable', () => {
  it('gives each row one badge, the most severe, in words', () => {
    const risky = makeDecision({ id: 'r@1', signal: 'risky', disposition: 'audit_flagged' })
    render(<DecisionTable decisions={[makeDecision(), riskyDecision, risky, charterDecision, unavailableDecision]} onOpen={() => {}} />)
    const badges = [...document.querySelectorAll('tbody tr')].map((row) => [...row.querySelectorAll('[data-badge]')].map((b) => b.textContent))
    expect(badges).toEqual([
      ['Consistent with the task'],
      ['Would pause (audit mode)'],
      ['Looks risky'],
      ['Against the charter'],
      ['Scorer unavailable → would hold'],
    ])
  })

  it('summarises each row from its fields, never from the narrative or the Task name', async () => {
    const grey = makeDecision({
      id: 'g@1',
      taskName: 'dep-auditor-211-take050024',
      agentName: 'dep-auditor',
      actionNarrative: 'Task dep-auditor-211-take050024 (agent dep-auditor) ran code_exec ("[synthetic] List outdated packages"); started /usr/local/bin/python3; taint: none.',
      action: { toolName: 'code_exec', requestSummary: '[synthetic] List outdated packages', effects: { binaries: [{ value: '/usr/local/bin/python3', count: 1 }] } },
    })
    const hold = makeDecision({
      ...unavailableDecision,
      id: 'h@1',
      agentName: 'summarizer',
      action: { toolName: 'code_exec', requestSummary: 'Summarize the incident log', effects: { binaries: [{ value: '/usr/bin/python3', count: 1 }] } },
    })
    render(<DecisionTable decisions={[riskyDecision, charterDecision, hold, grey, makeDecision({ id: 'nobin@1', agentName: undefined })]} onOpen={() => {}} />)
    expect(screen.getByRole('button', { name: 'sent 2 KB to cdn.registry.example:443 — would pause' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'agent-a ran code_exec — run the tests — breaks an undeclared rule' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'scorer unavailable — would hold · summarizer ran python3 — Summarize the incident log' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'dep-auditor ran python3 — List outdated packages' })).toBeInTheDocument()
    // Without an agent the line starts with the verb; the tool stands in for a binary.
    expect(screen.getByRole('button', { name: 'ran code_exec — run the tests' })).toBeInTheDocument()
    const table = screen.getByRole('table')
    expect(table).not.toHaveTextContent('[synthetic]')
    expect(table).not.toHaveTextContent('dep-auditor-211-take050024')
    // Raw tool text and IDs are on hover, not in the row.
    const row = screen.getByRole('button', { name: /sent 2 KB/ })
    expect(row).toHaveAttribute('title', expect.stringContaining('request: fetch weights'))
    expect(row).toHaveAttribute('title', expect.stringContaining('task build-1'))
    expect(rowOf('risky')).not.toHaveTextContent('build-1')
  })

  it('names the declared rule number when the charter is declared', async () => {
    useAuthStore.setState({ token: 'test-token' })
    server.use(http.get('/api/v1/oversight/config', () => HttpResponse.json(charterConfig)))
    render(<DecisionTable decisions={[charterDecision]} onOpen={() => {}} />)
    expect(await screen.findByRole('button', { name: 'agent-a ran code_exec — run the tests — breaks rule 2' })).toBeInTheDocument()
  })

  it('says what was pushed where when an exec argument names an image', () => {
    const push = (args: string) => makeDecision({
      id: `p-${args}`,
      signal: 'against_charter',
      charterFindings: [{ producerRef: 'rules-a', boundID: 'B9', sentence: 's' }],
      action: {
        toolName: 'code_exec',
        effects: {
          bytesSent: 2 * 1024 * 1024,
          destinations: [{ value: 'registry.demo.svc:443', count: 1 }],
          notableEvents: [{ id: 'e1', kind: 'exec', args }],
        },
      },
    })
    render(<DecisionTable decisions={[
      push('crane push ./out registry.demo.svc/prod/api:latest'),
      push('oras push oci://registry.demo.svc:5000/team/tool:v1.2'),
      push('curl -X PUT /v2/prod/api/blobs/uploads/ -d @layer'),
      push('cp src/app.ts:12 build/'),
    ]} onOpen={() => {}} />)
    expect(screen.getByRole('button', { name: 'pushed api:latest to registry.demo.svc/prod (2 MB) — breaks an undeclared rule' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'pushed tool:v1.2 to registry.demo.svc:5000/team (2 MB) — breaks an undeclared rule' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'uploaded to registry.demo.svc/v2/prod/api/blobs/uploads/ (2 MB) — breaks an undeclared rule' })).toBeInTheDocument()
    // A file:line argument is not an image: the bytes and destination wording stays.
    expect(screen.getByRole('button', { name: 'sent 2 MB to registry.demo.svc:443 — breaks an undeclared rule' })).toBeInTheDocument()
  })

  it('keeps grey rows quiet and opens a row on click', async () => {
    const onOpen = vi.fn()
    render(<DecisionTable decisions={[makeDecision(), riskyDecision]} onOpen={onOpen} />)
    expect(rowOf('consistent').className).toContain('text-muted-foreground')
    expect(rowOf('consistent').className).not.toContain('border-l-')
    await userEvent.click(screen.getByRole('button', { name: /sent 2 KB/ }))
    expect(onOpen).toHaveBeenCalledWith('build-1@2')
  })

  it('selects rows without opening them', async () => {
    const onOpen = vi.fn()
    const onToggle = vi.fn()
    render(<DecisionTable decisions={[riskyDecision]} onOpen={onOpen} selectedIds={new Set()} onToggleSelect={onToggle} />)
    await userEvent.click(screen.getByRole('checkbox', { name: 'Select build-1@2' }))
    expect(onToggle).toHaveBeenCalledWith('build-1@2')
    expect(onOpen).not.toHaveBeenCalled()
  })
})

describe('QuestionMatrix', () => {
  it('puts questions in columns and highlights only triggered cells', async () => {
    const onOpen = vi.fn()
    render(<QuestionMatrix decisions={[makeDecision(), riskyDecision]} onOpen={onOpen} />)
    const headers = screen.getAllByRole('columnheader').map((h) => h.textContent)
    expect(headers).toEqual(['Action', 'Signal', 'K1', 'exfiltration', 'K10'])
    const hot = document.querySelectorAll('td[data-triggered]')
    expect([...hot].map((cell) => cell.getAttribute('aria-label'))).toEqual([
      'exfiltration: 0.91 against threshold 0.82, triggered',
      'K10: named, triggered',
    ])
    await userEvent.click(screen.getAllByRole('row')[2])
    expect(onOpen).toHaveBeenCalledWith('build-1@2')
  })

  it('heads columns with declared concern names', async () => {
    useAuthStore.setState({ token: 'test-token' })
    server.use(http.get('/api/v1/oversight/config', () => HttpResponse.json(charterConfig)))
    render(<QuestionMatrix decisions={[riskyDecision]} onOpen={() => {}} />)
    expect(await screen.findByRole('columnheader', { name: 'Data leaving the workspace' })).toBeInTheDocument()
    expect(screen.getByRole('columnheader', { name: 'Unrequested download' })).toBeInTheDocument()
  })
})
