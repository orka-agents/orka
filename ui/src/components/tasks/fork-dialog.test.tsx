import { describe, it, expect, beforeEach, vi } from 'vitest'
import { useState } from 'react'
import { http, HttpResponse, delay } from 'msw'
import { render, screen, waitFor } from '@/test/test-utils'
import userEvent from '@testing-library/user-event'
import { server } from '@/test/mocks/server'
import { useUIStore } from '@/stores/ui'
import { makeEvent } from '@/test/fixtures/events'

vi.mock('zustand/middleware', () => ({ persist: (fn: unknown) => fn }))
vi.mock('@tanstack/react-router', async () => {
  const actual = await vi.importActual('@tanstack/react-router')
  return {
    ...actual,
    Link: ({ children, to, params, ...props }: any) => {
      let href = to
      if (typeof to === 'string' && params) {
        for (const [k, v] of Object.entries(params)) href = href.replace(`$${k}`, v as string)
      }
      return <a href={href} {...props}>{children}</a>
    },
  }
})

import { ForkDialog } from './fork-dialog'

const API = '/api/v1'

describe('ForkDialog', () => {
  beforeEach(() => {
    useUIStore.setState({ namespace: 'default' })
  })

  it('seeds afterSeq from the selected event', () => {
    render(
      <ForkDialog taskId="tk" event={makeEvent({ seq: 5 })} open onOpenChange={() => {}} />,
    )
    expect(screen.getByText(/#5/)).toBeInTheDocument()
  })

  it('submits the fork with the selected afterSeq and shows the new task link', async () => {
    let capturedBody: unknown = null
    server.use(
      http.post(`${API}/tasks/:id/fork`, async ({ request }) => {
        capturedBody = await request.json()
        return HttpResponse.json(
          {
            namespace: 'default',
            sourceTaskName: 'tk',
            newTaskName: 'tk-fork-abcd',
            afterSeq: 5,
            forkContext: { sourceNamespace: 'default', sourceTask: 'tk', afterSeq: 5, events: [], truncated: false },
          },
          { status: 201 },
        )
      }),
    )
    const user = userEvent.setup()
    render(<ForkDialog taskId="tk" event={makeEvent({ seq: 5 })} open onOpenChange={() => {}} />)
    await user.click(screen.getByRole('button', { name: /create fork/i }))
    await waitFor(() => expect(screen.getByRole('link', { name: /tk-fork-abcd/ })).toBeInTheDocument())
    expect((capturedBody as Record<string, unknown>).afterSeq).toBe(5)
    expect(screen.getByRole('link', { name: /tk-fork-abcd/ })).toHaveAttribute('href', '/tasks/tk-fork-abcd')
  })

  it('passes optional name, agent, and prompt overrides', async () => {
    let capturedBody: any = null
    server.use(
      http.post(`${API}/tasks/:id/fork`, async ({ request }) => {
        capturedBody = await request.json()
        return HttpResponse.json(
          {
            namespace: 'default', sourceTaskName: 'tk', newTaskName: 'my-fork', afterSeq: 2,
            forkContext: { sourceNamespace: 'default', sourceTask: 'tk', afterSeq: 2, events: [], truncated: false },
          },
          { status: 201 },
        )
      }),
    )
    const user = userEvent.setup()
    render(<ForkDialog taskId="tk" event={makeEvent({ seq: 2 })} open onOpenChange={() => {}} />)
    await user.type(screen.getByLabelText(/new task name/i), 'my-fork')
    await user.type(screen.getByLabelText(/agent override/i), 'planner')
    await user.type(screen.getByLabelText(/prompt override/i), 'continue please')
    await user.click(screen.getByRole('button', { name: /create fork/i }))
    await waitFor(() => expect(capturedBody).not.toBeNull())
    expect(capturedBody).toEqual({
      afterSeq: 2,
      newTaskName: 'my-fork',
      agentRef: { name: 'planner' },
      prompt: 'continue please',
    })
  })

  it('shows a validation error from the backend', async () => {
    server.use(
      http.post(`${API}/tasks/:id/fork`, () =>
        new HttpResponse('afterSeq must be 0, latest, or an existing event sequence', { status: 400 }),
      ),
    )
    const user = userEvent.setup()
    render(<ForkDialog taskId="tk" event={makeEvent({ seq: 99 })} open onOpenChange={() => {}} />)
    await user.click(screen.getByRole('button', { name: /create fork/i }))
    await waitFor(() =>
      expect(screen.getByText(/afterSeq must be 0, latest, or an existing event sequence/i)).toBeInTheDocument(),
    )
  })

  it('resets state when closed, so reopening shows a fresh form (dialog stays mounted)', async () => {
    server.use(
      http.post(`${API}/tasks/:id/fork`, () =>
        HttpResponse.json(
          {
            namespace: 'default', sourceTaskName: 'tk', newTaskName: 'tk-fork-xyz', afterSeq: 4,
            forkContext: { sourceNamespace: 'default', sourceTask: 'tk', afterSeq: 4, events: [], truncated: false },
          },
          { status: 201 },
        ),
      ),
    )
    // Mirror how TaskEventTimeline keeps the dialog mounted across open/close.
    function Harness() {
      const [open, setOpen] = useState(true)
      return (
        <>
          <button onClick={() => setOpen(true)}>reopen</button>
          <ForkDialog taskId="tk" event={makeEvent({ seq: 4 })} open={open} onOpenChange={setOpen} />
        </>
      )
    }
    const user = userEvent.setup()
    render(<Harness />)
    // Fork succeeds -> success screen with the new task link.
    await user.click(screen.getByRole('button', { name: /create fork/i }))
    await waitFor(() => expect(screen.getByRole('link', { name: /tk-fork-xyz/ })).toBeInTheDocument())
    // Close via the footer Close button (distinct from the dialog's X, which is
    // also labelled "Close"); pick the one with visible text.
    const closeButtons = screen.getAllByRole('button', { name: 'Close' })
    const footerClose = closeButtons.find((b) => b.textContent === 'Close')!
    await user.click(footerClose)
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    // Reopen -> fresh form, not the stale success screen.
    await user.click(screen.getByRole('button', { name: 'reopen' }))
    await waitFor(() => expect(screen.getByRole('button', { name: /create fork/i })).toBeInTheDocument())
    expect(screen.queryByRole('link', { name: /tk-fork-xyz/ })).not.toBeInTheDocument()
  })

  it('sends an Idempotency-Key header and reuses it across retries of one submission', async () => {
    const keys: string[] = []
    let attempts = 0
    server.use(
      http.post(`${API}/tasks/:id/fork`, async ({ request }) => {
        keys.push(request.headers.get('Idempotency-Key') ?? '')
        attempts += 1
        // Fail the first attempt so the user retries from the error state.
        if (attempts === 1) return new HttpResponse('temporary failure', { status: 503 })
        return HttpResponse.json(
          {
            namespace: 'default', sourceTaskName: 'tk', newTaskName: 'tk-fork-1', afterSeq: 2,
            forkContext: { sourceNamespace: 'default', sourceTask: 'tk', afterSeq: 2, events: [], truncated: false },
          },
          { status: 201 },
        )
      }),
    )
    const user = userEvent.setup()
    render(<ForkDialog taskId="tk" event={makeEvent({ seq: 2 })} open onOpenChange={() => {}} />)
    await user.click(screen.getByRole('button', { name: /create fork/i }))
    await waitFor(() => expect(screen.getByText(/temporary failure/i)).toBeInTheDocument())
    // Retry the same submission.
    await user.click(screen.getByRole('button', { name: /create fork/i }))
    await waitFor(() => expect(screen.getByRole('link', { name: /tk-fork-1/ })).toBeInTheDocument())
    expect(keys).toHaveLength(2)
    expect(keys[0]).toBeTruthy()
    // The retry must carry the SAME key so the backend collapses the duplicate.
    expect(keys[1]).toBe(keys[0])
  })

  it('ignores an async fork result that resolves after the dialog was closed', async () => {
    server.use(
      http.post(`${API}/tasks/:id/fork`, async () => {
        // Resolve slowly so we can close the dialog mid-flight.
        await delay(80)
        return HttpResponse.json(
          {
            namespace: 'default', sourceTaskName: 'tk', newTaskName: 'tk-fork-late', afterSeq: 3,
            forkContext: { sourceNamespace: 'default', sourceTask: 'tk', afterSeq: 3, events: [], truncated: false },
          },
          { status: 201 },
        )
      }),
    )
    function Harness() {
      const [open, setOpen] = useState(true)
      return (
        <>
          <button onClick={() => setOpen(true)}>reopen</button>
          <ForkDialog taskId="tk" event={makeEvent({ seq: 3 })} open={open} onOpenChange={setOpen} />
        </>
      )
    }
    const user = userEvent.setup()
    render(<Harness />)
    await user.click(screen.getByRole('button', { name: /create fork/i }))
    // Close the dialog while the request is still in flight (Escape, a Radix
    // affordance that isn't disabled during submission).
    await user.keyboard('{Escape}')
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    // Give the in-flight request time to resolve against the now-closed dialog.
    await new Promise((r) => setTimeout(r, 150))
    // Reopen — the stale success screen must NOT appear; a fresh form shows.
    await user.click(screen.getByRole('button', { name: 'reopen' }))
    await waitFor(() => expect(screen.getByRole('button', { name: /create fork/i })).toBeInTheDocument())
    expect(screen.queryByRole('link', { name: /tk-fork-late/ })).not.toBeInTheDocument()
  })
})
