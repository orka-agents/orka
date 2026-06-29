import { describe, it, expect, beforeEach, vi } from 'vitest'
import { http, HttpResponse } from 'msw'
import { render, screen, waitFor } from '@/test/test-utils'
import userEvent from '@testing-library/user-event'
import { server } from '@/test/mocks/server'
import { useUIStore } from '@/stores/ui'
import { makeTrace } from '@/test/fixtures/trace'

vi.mock('zustand/middleware', () => ({ persist: (fn: unknown) => fn }))
vi.mock('@tanstack/react-router', async () => {
  const actual = await vi.importActual('@tanstack/react-router')
  return { ...actual, Link: ({ children, ...props }: any) => <a {...props}>{children}</a> }
})

import { TaskTracePanel } from './task-trace-panel'

const API = '/api/v1'

describe('TaskTracePanel', () => {
  beforeEach(() => {
    useUIStore.setState({ namespace: 'default' })
  })

  it('renders the trace once loaded', async () => {
    server.use(
      http.get(`${API}/tasks/:id/trace`, () =>
        HttpResponse.json(
          makeTrace({
            task: { namespace: 'default', name: 'tk', phase: 'Succeeded', resultAvailable: false },
            latestSeq: 2,
            toolCalls: [{ id: 't1', name: 'web_fetch', status: 'completed', startSeq: 1, endSeq: 2 }],
          }),
        ),
      ),
    )
    render(<TaskTracePanel taskId="tk" />)
    await waitFor(() => expect(screen.getByText('Execution trace')).toBeInTheDocument())
    expect(screen.getByText('Tool calls')).toBeInTheDocument()
    expect(screen.getByText('web_fetch')).toBeInTheDocument()
  })

  it('shows an error state with retry', async () => {
    server.use(
      http.get(`${API}/tasks/:id/trace`, () => new HttpResponse('boom', { status: 500 })),
    )
    render(<TaskTracePanel taskId="tk" />)
    await waitFor(() => expect(screen.getByText('Failed to load the task trace.')).toBeInTheDocument())
    expect(screen.getByRole('button', { name: /retry/i })).toBeInTheDocument()
  })

  it('shows a clear message when execution events are not enabled (501)', async () => {
    server.use(
      http.get(`${API}/tasks/:id/trace`, () => new HttpResponse('not enabled', { status: 501 })),
    )
    render(<TaskTracePanel taskId="tk" />)
    await waitFor(() =>
      expect(screen.getByText(/execution event storage is not enabled/i)).toBeInTheDocument(),
    )
  })

  it('retry refetches the trace', async () => {
    let calls = 0
    server.use(
      http.get(`${API}/tasks/:id/trace`, () => {
        calls += 1
        if (calls === 1) return new HttpResponse('boom', { status: 500 })
        return HttpResponse.json(makeTrace({ latestSeq: 1, timeline: [] }))
      }),
    )
    const user = userEvent.setup()
    render(<TaskTracePanel taskId="tk" />)
    await waitFor(() => expect(screen.getByRole('button', { name: /retry/i })).toBeInTheDocument())
    await user.click(screen.getByRole('button', { name: /retry/i }))
    await waitFor(() => expect(screen.getByText('Execution trace')).toBeInTheDocument())
    expect(calls).toBeGreaterThanOrEqual(2)
  })
})
