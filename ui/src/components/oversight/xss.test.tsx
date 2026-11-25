import { describe, expect, it, beforeEach, vi } from 'vitest'
import { render, screen } from '@/test/test-utils'
import { http, HttpResponse } from 'msw'
import { server } from '@/test/mocks/server'

vi.mock('zustand/middleware', () => ({ persist: (fn: unknown) => fn }))

import { useAuthStore } from '@/stores/auth'
import { useUIStore } from '@/stores/ui'
import { DecisionInspector } from './decision-inspector'
import { DecisionTable } from './decision-table'
import { CharterPanel } from './charter-panel'
import { makeDecision } from '@/test/fixtures/oversight'

const IMG = '<img src=x onerror="window.__pwned=1">'
const SCRIPT = '<script>window.__pwned=1</script>'

const hostile = makeDecision({
  id: 'x@1',
  signal: 'against_charter',
  actionNarrative: `narrative ${IMG}`,
  action: {
    toolName: `tool${SCRIPT}`,
    requestSummary: `summary ${IMG}`,
    effects: { notableEvents: [{ id: 'ev-1', kind: 'exec', args: `args ${SCRIPT}`, binary: `/bin/${IMG}` }] },
  },
  charterFindings: [{ producerRef: `rules${IMG}`, boundID: 'B1', sentence: `sentence ${SCRIPT}` }],
  advisors: [{ providerRef: 'advisor-a', modelID: 'm', status: 'ok', summary: `advisor ${IMG}`, verdict: SCRIPT }],
})

describe('oversight renders API strings as text', () => {
  beforeEach(() => {
    useUIStore.setState({ namespace: 'default' })
    useAuthStore.setState({ token: 'test-token' })
    ;(window as unknown as { __pwned?: number }).__pwned = undefined
    server.use(
      http.get('/api/v1/oversight/decisions/:id', () => HttpResponse.json(hostile)),
      http.get('/api/v1/oversight/config', () => HttpResponse.json({
        queryKeys: [], maxBulkFeedback: 200, demoCharterRevisions: [],
        charters: [{ revision: 'charter-r1', title: `title ${SCRIPT}`, bounds: [{ id: 'B1', sentence: `bound ${IMG}` }] }],
      })),
    )
  })

  it('never turns markup in a decision into elements', async () => {
    render(<DecisionTable decisions={[hostile]} onOpen={() => {}} />)
    render(<DecisionInspector decisionId="x@1" onClose={() => {}} onPivot={() => {}} onOpenDecision={() => {}} />)
    render(<CharterPanel onPivot={() => {}} />)
    expect(await screen.findByText(`bound ${IMG}`)).toBeInTheDocument()
    expect(screen.getByText(`title ${SCRIPT}`)).toBeInTheDocument()
    expect(await screen.findByTestId('charter-rule')).toHaveTextContent(`Rule 1 of title ${SCRIPT}`)
    expect(await screen.findByTestId('request-summary')).toHaveTextContent(`summary ${IMG}`)
    expect(screen.getByTestId('advisor-summary')).toHaveTextContent(`advisor ${IMG}`)
    expect(screen.getAllByText(`sentence ${SCRIPT}`).length).toBeGreaterThan(0)
    expect(screen.getByText(`args ${SCRIPT}`)).toBeInTheDocument()
    expect(document.querySelector('img')).toBeNull()
    expect(document.querySelector('script')).toBeNull()
    expect(document.querySelector('[onerror]')).toBeNull()
    expect((window as unknown as { __pwned?: number }).__pwned).toBeUndefined()
  })
})
