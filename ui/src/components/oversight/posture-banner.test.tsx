import { describe, expect, it, beforeEach, vi } from 'vitest'
import { render, screen, within } from '@/test/test-utils'
import { http, HttpResponse } from 'msw'
import { server } from '@/test/mocks/server'

vi.mock('zustand/middleware', () => ({ persist: (fn: unknown) => fn }))

import { useAuthStore } from '@/stores/auth'
import { useUIStore } from '@/stores/ui'
import { PostureBanner, PostureView } from './posture-banner'
import { makePosture } from '@/test/fixtures/oversight'

describe('PostureView', () => {
  it('shows the confidence state with its summary and reasons', () => {
    render(<PostureView posture={makePosture()} />)
    expect(screen.getByTestId('confidence-state')).toHaveTextContent('Degraded')
    expect(screen.getByText('Nothing went against the charter in 10 actions.')).toBeInTheDocument()
    expect(screen.getByText('No canary evidence is reported yet')).toBeInTheDocument()
    expect(screen.getByText('Audit mode only: nothing is held')).toBeInTheDocument()
  })

  it('lists what the monitor set aside, and nothing when it set nothing aside', () => {
    const posture = makePosture()
    const { rerender } = render(<PostureView posture={{ ...posture, basis: { ...posture.basis,
      coverage: { ...posture.basis.coverage, excluded: { platform_traffic: 1, container_setup: 12 } } } }} />)
    const list = within(screen.getByTestId('coverage-excluded')).getAllByRole('listitem')
    expect(list.map((li) => li.textContent)).toEqual(['12 container-setup events', '1 platform connection'])
    rerender(<PostureView posture={posture} />)
    expect(screen.queryByTestId('coverage-excluded')).not.toBeInTheDocument()
  })

  it.each([
    ['warranted', 'Warranted'],
    ['degraded', 'Degraded'],
    ['blind', 'Blind'],
    ['attention_required', 'Attention required'],
  ])('labels confidence %s', (state, label) => {
    render(<PostureView posture={makePosture({ confidence: { state, summary: 's' } })} />)
    expect(screen.getByTestId('confidence-state')).toHaveTextContent(label)
  })

  it('describes visibility as coverage and never as warranted', () => {
    for (const state of ['full', 'partial', 'blind']) {
      const { unmount } = render(<PostureView posture={makePosture({
        confidence: { state: 'warranted', summary: 'ok' },
        visibility: { state, summary: 'All 10 actions were seen and attributed.', reasons: ['visibility_not_warranted'] },
      })} />)
      const coverage = screen.getByTestId('posture-coverage')
      expect(coverage).not.toHaveTextContent(/warranted/i)
      const gaps = screen.queryByTestId('coverage-gaps')
      if (gaps) expect(gaps).not.toHaveTextContent(/warranted/i)
      unmount()
    }
  })

  it('makes coverage gaps hard to miss', () => {
    render(<PostureView posture={makePosture()} />)
    const coverage = screen.getByTestId('posture-coverage')
    expect(coverage).toHaveTextContent('Partial coverage')
    expect(coverage).toHaveTextContent('8 of 10 actions attributed')
    expect(coverage).toHaveTextContent('3 unattributed events')
    const gaps = screen.getByRole('alert')
    expect(within(gaps).getByText(/dns_over_https/)).toBeInTheDocument()
  })

  it('shows audit control and labels the charter watched, not enforced', () => {
    render(<PostureView posture={makePosture()} />)
    expect(screen.getByText('Audit mode: would pause, never holds')).toBeInTheDocument()
    const list = screen.getByRole('list', { name: 'Charter and concerns' })
    expect(list).toHaveTextContent('Charter charter-r1 (watched, not enforced)')
    expect(list).toHaveTextContent('Concerns concerns-r4 (watched, not enforced)')
    expect(list).not.toHaveTextContent(/demo/i)
  })

  it('labels a demo charter only when the config says so', () => {
    render(<PostureView posture={makePosture()} demoCharterRevisions={['charter-r1']} />)
    expect(screen.getByText(/Demo charter: preview rule check, audit only/)).toBeInTheDocument()
  })

  it('shows coverage, gaps and the canary basis on an idle namespace', () => {
    render(<PostureView posture={makePosture({
      confidence: { state: 'blind', summary: 'No decisions were recorded in this window, so nothing was seen.' },
      visibility: { state: 'blind', summary: 'No decisions were recorded in this window, so nothing was seen.', reasons: ['no_decisions_in_window'] },
      basis: { ...makePosture().basis, actions: 0, coverage: { streamOK: 0, sensorAttributed: 0, unattributedEvents: 0 } },
    })} />)
    expect(screen.getByTestId('posture-coverage')).toHaveTextContent('0% attributed')
    expect(screen.getByTestId('coverage-gaps')).toHaveTextContent('No blind spot was declared.')
    expect(screen.getByText(/No canary evidence is reported yet/)).toBeInTheDocument()
  })
})

describe('decision latency', () => {
  it('shows p50, p95 and max in the coverage section', () => {
    render(<PostureView posture={makePosture({ basis: { ...makePosture().basis, decisionLatency: { count: 12, p50Seconds: 3.2, p95Seconds: 45, maxSeconds: 281 } } })} />)
    expect(screen.getByTestId('decision-latency')).toHaveTextContent('Decision latency: p50 3.2s · p95 45s · max 4m 41s (n=12)')
  })

  it('says when there are no live records', () => {
    render(<PostureView posture={makePosture()} />)
    expect(screen.getByTestId('decision-latency')).toHaveTextContent('Decision latency: no live records in this window')
  })
})

describe('PostureBanner', () => {
  beforeEach(() => {
    useUIStore.setState({ namespace: 'default' })
    useAuthStore.setState({ token: 'test-token' })
  })

  it('shows a clear state and stops on 501', async () => {
    let hits = 0
    server.use(http.get('/api/v1/oversight/posture', () => { hits++; return HttpResponse.json({ error: 'oversight is not enabled' }, { status: 501 }) }))
    render(<PostureBanner />)
    expect(await screen.findByText('Oversight is not enabled')).toBeInTheDocument()
    expect(hits).toBe(1)
  })

  it('shows not authorized on 403', async () => {
    server.use(http.get('/api/v1/oversight/posture', () => HttpResponse.json({ error: 'nope' }, { status: 403 })))
    render(<PostureBanner />)
    expect(await screen.findByText('Not authorized to view oversight posture')).toBeInTheDocument()
  })
})
