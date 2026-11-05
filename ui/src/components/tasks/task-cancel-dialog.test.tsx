import { describe, it, expect, beforeEach, vi } from 'vitest'
import { http, HttpResponse } from 'msw'
import { render, screen, waitFor } from '@/test/test-utils'
import userEvent from '@testing-library/user-event'
import { server } from '@/test/mocks/server'
import { useUIStore } from '@/stores/ui'
import { TaskCancelDialog } from './task-cancel-dialog'
import { isTaskCancellable, isTaskCancelling } from './task-state'
import type { Task } from '@/schemas/task'

vi.mock('zustand/middleware', () => ({ persist: (fn: unknown) => fn }))

const API = '/api/v1'

describe('TaskCancelDialog', () => {
  beforeEach(() => {
    useUIStore.setState({ namespace: 'team-a' })
  })

  it('sends the reason to the cancel endpoint and closes', async () => {
    let captured: { body: unknown; namespace: string | null } | null = null
    server.use(
      http.post(`${API}/tasks/:id/cancel`, async ({ request, params }) => {
        captured = { body: await request.json(), namespace: new URL(request.url).searchParams.get('namespace') }
        return HttpResponse.json({ namespace: 'team-a', name: params.id, phase: 'Running', requested: true, message: 'cancelled' })
      }),
    )
    const onOpenChange = vi.fn()
    const user = userEvent.setup()
    render(<TaskCancelDialog taskName="build" taskUID="build-uid" open onOpenChange={onOpenChange} />)
    await user.type(screen.getByLabelText(/Reason/), 'wrong repo')
    await user.click(screen.getByRole('button', { name: 'Cancel task' }))
    await waitFor(() => expect(onOpenChange).toHaveBeenCalledWith(false))
    expect(captured).toEqual({ body: { uid: 'build-uid', reason: 'wrong repo' }, namespace: 'team-a' })
  })

  it('shows a refusal and stays open', async () => {
    server.use(
      http.post(`${API}/tasks/:id/cancel`, () =>
        HttpResponse.json({ error: { code: 409, message: 'task cannot be cancelled: the task is Succeeded' } }, { status: 409 }),
      ),
    )
    const onOpenChange = vi.fn()
    const user = userEvent.setup()
    render(<TaskCancelDialog taskName="build" open onOpenChange={onOpenChange} />)
    await user.click(screen.getByRole('button', { name: 'Cancel task' }))
    expect(await screen.findByRole('alert')).toBeInTheDocument()
    expect(onOpenChange).not.toHaveBeenCalledWith(false)
  })
})

describe('task cancel state', () => {
  const task = (phase: string, annotations?: Record<string, string>, deletionTimestamp?: string) =>
    ({ metadata: { name: 't', annotations, deletionTimestamp }, spec: {}, status: { phase } }) as unknown as Task

  it('offers cancel only for Pending and Running tasks that are not being deleted', () => {
    expect(isTaskCancellable(task('Running'))).toBe(true)
    expect(isTaskCancellable(task('Pending'))).toBe(true)
    expect(isTaskCancellable(task('Succeeded'))).toBe(false)
    expect(isTaskCancellable(task('Running', undefined, '2026-11-05T00:00:00Z'))).toBe(false)
  })

  it('reports a requested cancellation until the task ends', () => {
    expect(isTaskCancelling(task('Running', { 'orka.ai/cancel-requested': 'cancelled by alice' }))).toBe(true)
    expect(isTaskCancelling(task('Cancelled', { 'orka.ai/cancel-requested': 'cancelled by alice' }))).toBe(false)
    expect(isTaskCancelling(task('Running'))).toBe(false)
  })
})
