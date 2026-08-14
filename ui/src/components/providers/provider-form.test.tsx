import { describe, it, expect, beforeEach, vi } from 'vitest'
import { act, fireEvent, render, screen, waitFor } from '@/test/test-utils'
import userEvent from '@testing-library/user-event'

vi.mock('zustand/middleware', () => ({
  persist: (fn: unknown) => fn,
}))

const navigate = vi.fn()
vi.mock('@tanstack/react-router', async () => {
  const actual = await vi.importActual('@tanstack/react-router')
  return {
    ...actual,
    Link: ({ children, to, ...props }: any) => <a href={to} {...props}>{children}</a>,
    useNavigate: () => navigate,
    useLocation: () => ({ pathname: '/providers/new' }),
  }
})

import { useUIStore } from '@/stores/ui'
import { http, HttpResponse } from 'msw'
import { server } from '@/test/mocks/server'
import { ProviderForm } from './provider-form'
import type { Provider } from '@/schemas/provider'

describe('ProviderForm', () => {
  beforeEach(() => {
    // Radix Select needs pointer-capture + scrollIntoView APIs jsdom lacks.
    if (!HTMLElement.prototype.hasPointerCapture) HTMLElement.prototype.hasPointerCapture = () => false
    if (!HTMLElement.prototype.scrollIntoView) HTMLElement.prototype.scrollIntoView = () => {}
    navigate.mockReset()
    useUIStore.setState({ namespace: 'default', sidebarCollapsed: false, theme: 'light' })
    server.use(
      http.get('/api/v1/secrets', () =>
        HttpResponse.json({ items: [{ name: 'anthropic-key', namespace: 'default', type: 'Opaque' }] }),
      ),
    )
  })

  it('requires name and Secret before submitting', async () => {
    const user = userEvent.setup()
    render(<ProviderForm />)
    await user.click(screen.getByRole('button', { name: /create provider/i }))
    // Blocked client-side: nothing posted, no navigation.
    expect(navigate).not.toHaveBeenCalled()
  })

  it('creates a provider with the selected Secret', async () => {
    let posted: any
    server.use(
      http.post('/api/v1/providers', async ({ request }) => {
        posted = await request.json()
        return HttpResponse.json({ metadata: { name: 'anthropic', namespace: 'default' }, spec: posted.spec }, { status: 201 })
      }),
    )
    const user = userEvent.setup()
    render(<ProviderForm />)
    await user.type(screen.getByLabelText(/^name$/i), 'anthropic')
    const secretTrigger = screen.getByRole('combobox', { name: /credentials secret/i })
    await act(async () => {
      fireEvent.pointerDown(secretTrigger, { button: 0, pointerId: 1, pointerType: 'mouse' })
    })
    const option = await screen.findByRole('option', { name: 'anthropic-key' })
    await act(async () => {
      fireEvent.click(option)
    })
    await user.type(screen.getByLabelText(/default model/i), 'claude-sonnet-4')
    await user.click(screen.getByRole('button', { name: /create provider/i }))
    await waitFor(() => expect(navigate).toHaveBeenCalledWith({ to: '/providers' }))
    expect(posted.name).toBe('anthropic')
    expect(posted.spec.secretRef.name).toBe('anthropic-key')
    expect(posted.spec.defaultModel).toBe('claude-sonnet-4')
  })

  it('edits an existing provider and preserves identity', async () => {
    let putBody: any
    server.use(
      http.put('/api/v1/providers/:name', async ({ request, params }) => {
        putBody = await request.json()
        return HttpResponse.json({ metadata: { name: params.name, namespace: 'default' }, spec: putBody.spec })
      }),
    )
    const initial: Provider = {
      metadata: { name: 'anthropic', namespace: 'default' },
      spec: { type: 'anthropic', secretRef: { name: 'anthropic-key' }, defaultModel: 'old-model' },
      status: { ready: true },
    }
    const user = userEvent.setup()
    render(<ProviderForm initial={initial} />)
    // Base URL is server-preserved on update, so the field is locked.
    expect(screen.getByLabelText(/base url/i)).toBeDisabled()
    const model = screen.getByLabelText(/default model/i)
    await user.clear(model)
    await user.type(model, 'new-model')
    await user.click(screen.getByRole('button', { name: /save changes/i }))
    await waitFor(() =>
      expect(navigate).toHaveBeenCalledWith({ to: '/providers/$providerName', params: { providerName: 'anthropic' } }),
    )
    expect(putBody.spec.defaultModel).toBe('new-model')
  })
})
