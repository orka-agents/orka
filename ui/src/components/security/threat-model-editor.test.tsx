import { describe, it, expect, beforeEach, vi } from 'vitest'

vi.mock('zustand/middleware', () => ({
  persist: (fn: unknown) => fn,
}))

vi.mock('sonner', () => ({ toast: { success: vi.fn(), error: vi.fn() } }))

import userEvent from '@testing-library/user-event'
import { focusManager } from '@tanstack/react-query'
import { http, HttpResponse } from 'msw'
import { toast } from 'sonner'
import { server } from '@/test/mocks/server'
import { act, render, screen, waitFor } from '@/test/test-utils'
import { useUIStore } from '@/stores/ui'
import { ThreatModelEditor } from './threat-model-editor'

const threatModelPath = '/api/v1/security/repositories/payments/threat-model'

function threatModel(version: number, content: string) {
  return {
    namespace: 'default',
    repositoryScan: 'payments',
    version,
    content,
    source: 'generated',
    createdAt: '2026-10-01T00:00:00Z',
    updatedAt: '2026-10-01T00:00:00Z',
  }
}

describe('ThreatModelEditor', () => {
  // undefined: no threat model exists yet (GET answers 404).
  let stored: ReturnType<typeof threatModel> | undefined
  let puts: Array<{ content: string; expectedVersion?: number }>

  beforeEach(() => {
    useUIStore.setState({ namespace: 'default', sidebarCollapsed: false, theme: 'light' })
    vi.mocked(toast.success).mockClear()
    vi.mocked(toast.error).mockClear()
    stored = threatModel(3, 'Generated model v3')
    puts = []
    server.use(
      http.get(threatModelPath, () => stored
        ? HttpResponse.json(stored)
        : HttpResponse.json({ error: { code: 404, message: 'threat model not found' } }, { status: 404 })),
      http.put(threatModelPath, async ({ request }) => {
        const body = await request.json() as { content: string; expectedVersion?: number }
        puts.push(body)
        // expectedVersion 0 expects no model yet; a positive one expects that version.
        const current = stored?.version ?? 0
        if (body.expectedVersion !== undefined && body.expectedVersion !== current) {
          return HttpResponse.json({ error: { code: 409, message: 'threat model changed since it was loaded' } }, { status: 409 })
        }
        stored = { ...threatModel(current + 1, body.content), source: 'edited' }
        return HttpResponse.json(stored)
      }),
    )
  })

  it('saves against the loaded version and builds the next save on the saved one', async () => {
    const user = userEvent.setup()
    render(<ThreatModelEditor repositoryName="payments" />)

    const editor = await screen.findByDisplayValue('Generated model v3')
    expect(editor).toBe(screen.getByRole('textbox', { name: 'Threat model' }))
    await user.type(editor, ' + auth notes')
    await user.click(screen.getByRole('button', { name: 'Save' }))
    await waitFor(() => expect(toast.success).toHaveBeenCalledWith('Threat model saved'))

    await user.type(editor, ' + more')
    await user.click(screen.getByRole('button', { name: 'Save' }))
    await waitFor(() => expect(puts).toHaveLength(2))

    expect(puts[0]).toMatchObject({ content: 'Generated model v3 + auth notes', expectedVersion: 3 })
    expect(puts[1]).toMatchObject({ content: 'Generated model v3 + auth notes + more', expectedVersion: 4 })
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
  })

  it('keeps the edits on a conflict and reloads the latest version on request', async () => {
    const user = userEvent.setup()
    render(<ThreatModelEditor repositoryName="payments" />)

    const editor = await screen.findByDisplayValue('Generated model v3')
    // Someone else saves while this editor still holds v3.
    stored = threatModel(4, 'Teammate model v4')
    await user.clear(editor)
    await user.type(editor, 'My local edits')
    await user.click(screen.getByRole('button', { name: 'Save' }))

    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent('The threat model changed since you loaded it')
    expect(editor).toHaveValue('My local edits')
    expect(puts[0].expectedVersion).toBe(3)
    expect(toast.success).not.toHaveBeenCalled()
    expect(toast.error).not.toHaveBeenCalled()

    await user.click(screen.getByRole('button', { name: 'Reload latest' }))
    await waitFor(() => expect(editor).toHaveValue('Teammate model v4'))
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()

    await user.type(editor, ' + mine')
    await user.click(screen.getByRole('button', { name: 'Save' }))
    await waitFor(() => expect(toast.success).toHaveBeenCalledWith('Threat model saved'))
    expect(puts[1]).toMatchObject({ content: 'Teammate model v4 + mine', expectedVersion: 4 })
  })

  it('creates the first threat model expecting that none exists yet', async () => {
    stored = undefined
    const user = userEvent.setup()
    render(<ThreatModelEditor repositoryName="payments" />)

    await screen.findByText(/No threat model has been generated yet/)
    const editor = screen.getByRole('textbox', { name: 'Threat model' })
    await user.type(editor, 'Initial notes')
    await user.click(screen.getByRole('button', { name: 'Save' }))
    await waitFor(() => expect(toast.success).toHaveBeenCalledWith('Threat model saved'))

    // Sent explicitly, not omitted: 0 is the "no model yet" precondition.
    expect(puts[0]).toEqual({ content: 'Initial notes', source: 'edited', expectedVersion: 0 })
    await user.type(editor, ' + more')
    await user.click(screen.getByRole('button', { name: 'Save' }))
    await waitFor(() => expect(puts).toHaveLength(2))
    expect(puts[1]).toMatchObject({ content: 'Initial notes + more', expectedVersion: 1 })
  })

  it('keeps first-create edits when a model appeared meanwhile and reloads it on request', async () => {
    stored = undefined
    const user = userEvent.setup()
    render(<ThreatModelEditor repositoryName="payments" />)

    await screen.findByText(/No threat model has been generated yet/)
    const editor = screen.getByRole('textbox', { name: 'Threat model' })
    // A scan generates the first model while this editor still holds none.
    stored = threatModel(1, 'Generated model v1')
    await user.type(editor, 'My first notes')
    await user.click(screen.getByRole('button', { name: 'Save' }))

    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent('The threat model changed since you loaded it')
    // The conflict refetches, so the "none yet" note stops contradicting it
    // while the edits stay put.
    await waitFor(() => expect(screen.queryByText(/No threat model has been generated yet/)).not.toBeInTheDocument())
    expect(editor).toBe(screen.getByRole('textbox', { name: 'Threat model' }))
    expect(editor).toHaveValue('My first notes')
    expect(puts[0]).toEqual({ content: 'My first notes', source: 'edited', expectedVersion: 0 })
    expect(toast.success).not.toHaveBeenCalled()
    expect(toast.error).not.toHaveBeenCalled()

    await user.click(screen.getByRole('button', { name: 'Reload latest' }))
    await waitFor(() => expect(editor).toHaveValue('Generated model v1'))
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()

    await user.type(editor, ' + mine')
    await user.click(screen.getByRole('button', { name: 'Save' }))
    await waitFor(() => expect(toast.success).toHaveBeenCalledWith('Threat model saved'))
    expect(puts[1]).toMatchObject({ content: 'Generated model v1 + mine', expectedVersion: 1 })
  })

  it('keeps typed text when a model loads in the background before the first save', async () => {
    stored = undefined
    const user = userEvent.setup()
    render(<ThreatModelEditor repositoryName="payments" />)

    await screen.findByText(/No threat model has been generated yet/)
    const editor = screen.getByRole('textbox', { name: 'Threat model' })
    await user.type(editor, 'Draft notes')
    // A scan generates the first model and a window-focus refetch brings it in.
    stored = threatModel(1, 'Generated model v1')
    await act(async () => {
      focusManager.setFocused(false)
      focusManager.setFocused(true)
    })
    await waitFor(() => expect(screen.queryByText(/No threat model has been generated yet/)).not.toBeInTheDocument())

    expect(screen.getByRole('textbox', { name: 'Threat model' })).toHaveValue('Draft notes')
    focusManager.setFocused(undefined)
  })

  it.each([
    [500, 'Could not load the threat model'],
    [403, 'Not authorized to view the threat model'],
  ])('shows a %i load failure and keeps the editor locked until the model loads', async (status, headline) => {
    let failing = true
    server.use(http.get(threatModelPath, () => failing
      ? HttpResponse.json({ error: { code: status, message: 'threat model store unavailable' } }, { status })
      : HttpResponse.json(stored)))
    const user = userEvent.setup()
    render(<ThreatModelEditor repositoryName="payments" />)

    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent(headline)
    expect(alert).toHaveTextContent('threat model store unavailable')
    expect(screen.queryByText(/No threat model has been generated yet/)).not.toBeInTheDocument()
    expect(screen.getByRole('textbox', { name: 'Threat model' })).toBeDisabled()
    expect(screen.getByRole('button', { name: 'Save' })).toBeDisabled()

    failing = false
    await user.click(screen.getByRole('button', { name: 'Retry' }))
    expect(await screen.findByDisplayValue('Generated model v3')).toBeEnabled()
    expect(screen.getByRole('button', { name: 'Save' })).toBeEnabled()
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
    expect(puts).toHaveLength(0)
  })

  it('reports other save failures with the server message', async () => {
    server.use(http.put(threatModelPath, () => HttpResponse.json({ error: { code: 403, message: 'not authorized' } }, { status: 403 })))
    const user = userEvent.setup()
    render(<ThreatModelEditor repositoryName="payments" />)

    await screen.findByDisplayValue('Generated model v3')
    await user.click(screen.getByRole('button', { name: 'Save' }))

    await waitFor(() => expect(toast.error).toHaveBeenCalledWith('Failed to save threat model: not authorized'))
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
  })
})
