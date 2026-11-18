import { describe, it, expect, vi, beforeEach } from 'vitest'

vi.mock('zustand/middleware', async () => {
  const actual = await vi.importActual('zustand/middleware')
  return { ...actual, persist: (fn: any) => fn }
})

const chatConfig = { current: { model: 'claude-sonnet-4-20250514', provider: 'anthropic', enabled: true } as Record<string, unknown> }
const providerList = {
  current: {
    data: {
      items: [
        { name: 'anthropic', type: 'anthropic', defaultModel: 'claude-sonnet-4-20250514', ready: true },
        { name: 'openai-proxy', type: 'openai', defaultModel: 'gpt-5', ready: true },
      ],
    },
    error: null,
  } as { data?: { items: Array<Record<string, unknown>> }; error: unknown },
}

vi.mock('@/hooks/use-chat', () => ({
  useSendMessage: () => vi.fn(),
  useChatConfig: () => ({ data: chatConfig.current }),
}))

vi.mock('@/hooks/use-providers', () => ({
  useProviderList: () => providerList.current,
}))

import { fireEvent, waitFor } from '@testing-library/react'

import { render, screen } from '@/test/test-utils'
import { ChatPage } from './chat-page'
import { ApiError } from '@/lib/api-client'
import { useUIStore } from '@/stores/ui'
import { useAuthStore } from '@/stores/auth'
import { useChatStore } from '@/stores/chat'
import type { ChatMessage } from '@/schemas/chat'

beforeEach(() => {
  chatConfig.current = { model: 'claude-sonnet-4-20250514', provider: 'anthropic', enabled: true }
  providerList.current = {
    data: {
      items: [
        { name: 'anthropic', type: 'anthropic', defaultModel: 'claude-sonnet-4-20250514', ready: true },
        { name: 'openai-proxy', type: 'openai', defaultModel: 'gpt-5', ready: true },
      ],
    },
    error: null,
  }
  useUIStore.setState({ namespace: 'default', sidebarCollapsed: false, theme: 'light' })
  useAuthStore.setState({ token: 'test-token' })
  useChatStore.setState({ messages: [], currentSessionId: null, isStreaming: false, provider: '', model: '', activeNamespace: 'default', selections: {} })
  Element.prototype.scrollIntoView = vi.fn()
  if (!Element.prototype.hasPointerCapture) Element.prototype.hasPointerCapture = () => false
  if (!Element.prototype.setPointerCapture) Element.prototype.setPointerCapture = () => {}
  if (!Element.prototype.releasePointerCapture) Element.prototype.releasePointerCapture = () => {}
})

describe('ChatPage', () => {
  it('renders "Chat" heading', () => {
    render(<ChatPage />)
    expect(screen.getByText('Chat')).toBeInTheDocument()
  })

  it('names the page in the browser tab', () => {
    const { unmount } = render(<ChatPage />)
    expect(document.title).toBe('Chat · Orka')
    unmount()
    expect(document.title).toBe('Orka')
  })

  it('renders without crashing', () => {
    const { container } = render(<ChatPage />)
    expect(container).toBeTruthy()
  })

  it('New Chat button appears when messages exist', () => {
    const msgs: ChatMessage[] = [
      { id: 'msg-1', role: 'user', content: 'Hello', timestamp: new Date().toISOString() },
    ]
    useChatStore.setState({ messages: msgs })
    render(<ChatPage />)
    expect(screen.getByText('New Chat')).toBeInTheDocument()
  })

  it('New Chat resets an idle chat at once, without asking', () => {
    const confirmSpy = vi.spyOn(window, 'confirm')
    try {
      const epoch = useChatStore.getState().turnEpoch
      useChatStore.setState({
        messages: [{ id: 'msg-1', role: 'user', content: 'Hello', timestamp: new Date().toISOString() }],
        currentSessionId: 'chat-idle',
      })
      render(<ChatPage />)
      const newChat = screen.getByRole('button', { name: /New Chat/ })
      newChat.focus()
      fireEvent.click(newChat)
      expect(confirmSpy).not.toHaveBeenCalled()
      expect(useChatStore.getState().messages).toEqual([])
      expect(useChatStore.getState().currentSessionId).toBeNull()
      expect(useChatStore.getState().turnEpoch).toBe(epoch + 1)
      // The button leaves with the transcript; focus moves to the composer
      // instead of dropping to <body>.
      expect(screen.queryByRole('button', { name: /New Chat/ })).not.toBeInTheDocument()
      expect(screen.getByPlaceholderText('Message the meta agent...')).toHaveFocus()
    } finally {
      confirmSpy.mockRestore()
    }
  })

  it('New Chat during a reply asks first and keeps the running chat when declined', () => {
    const confirmSpy = vi.spyOn(window, 'confirm').mockReturnValue(false)
    try {
      const epoch = useChatStore.getState().turnEpoch
      useChatStore.setState({
        messages: [{ id: 'msg-1', role: 'user', content: 'Hello', timestamp: new Date().toISOString() }],
        currentSessionId: 'chat-running',
        isStreaming: true,
      })
      render(<ChatPage />)
      const newChat = screen.getByRole('button', { name: /New Chat/ })
      newChat.focus()
      fireEvent.click(newChat)
      expect(confirmSpy).toHaveBeenCalledWith('Stop the current reply and delete this chat?')
      expect(newChat).toHaveFocus()
      expect(useChatStore.getState().messages).toHaveLength(1)
      expect(useChatStore.getState().currentSessionId).toBe('chat-running')
      expect(useChatStore.getState().isStreaming).toBe(true)
      // The epoch is what orphans (and so cancels and deletes) the turn.
      expect(useChatStore.getState().turnEpoch).toBe(epoch)
    } finally {
      confirmSpy.mockRestore()
    }
  })

  it('New Chat during a reply resets once confirmed', () => {
    const confirmSpy = vi.spyOn(window, 'confirm').mockReturnValue(true)
    try {
      const epoch = useChatStore.getState().turnEpoch
      useChatStore.setState({
        messages: [{ id: 'msg-1', role: 'user', content: 'Hello', timestamp: new Date().toISOString() }],
        currentSessionId: 'chat-running',
        isStreaming: true,
      })
      render(<ChatPage />)
      const newChat = screen.getByRole('button', { name: /New Chat/ })
      newChat.focus()
      fireEvent.click(newChat)
      expect(confirmSpy).toHaveBeenCalledTimes(1)
      expect(useChatStore.getState().messages).toEqual([])
      expect(useChatStore.getState().currentSessionId).toBeNull()
      expect(useChatStore.getState().isStreaming).toBe(false)
      expect(useChatStore.getState().turnEpoch).toBe(epoch + 1)
      expect(screen.getByPlaceholderText('Message the meta agent...')).toHaveFocus()
    } finally {
      confirmSpy.mockRestore()
    }
  })

  it('offers the server default plus Provider CRDs and prefills the model on pick', async () => {
    render(<ChatPage />)
    expect(screen.getByRole('combobox', { name: 'Chat provider' })).toHaveTextContent('Server default (anthropic / claude-sonnet-4-20250514)')

    fireEvent.pointerDown(screen.getByRole('combobox', { name: 'Chat provider' }), { button: 0, pointerId: 1, pointerType: 'mouse' })
    fireEvent.click(await screen.findByRole('option', { name: /openai-proxy/ }))

    await waitFor(() => expect(useChatStore.getState().provider).toBe('openai-proxy'))
    expect(useChatStore.getState().model).toBe('gpt-5')
    expect(screen.getByRole('textbox', { name: 'Chat model' })).toHaveValue('gpt-5')
    expect(screen.getByText('gpt-5')).toBeInTheDocument()
  })

  it('model input edits the persisted model override', () => {
    useChatStore.setState({ provider: 'anthropic', model: 'claude-sonnet-4-20250514' })
    render(<ChatPage />)
    fireEvent.change(screen.getByRole('textbox', { name: 'Chat model' }), { target: { value: 'claude-opus-4-1' } })
    expect(useChatStore.getState().model).toBe('claude-opus-4-1')
  })

  it('uses symmetric negative margins so the surface is not clipped on mobile', () => {
    const { container } = render(<ChatPage />)
    const surface = container.firstElementChild as HTMLElement
    expect(surface.className).toContain('-m-4')
    expect(surface.className).toContain('md:-m-6')
    expect(surface.className).not.toMatch(/(^|\s)-m-6(\s|$)/)
    // The height adds the cancelled padding back so no band is left under
    // the input and only the transcript scrolls.
    expect(surface.className).toContain('h-[calc(100%+2rem)]')
    expect(surface.className).toContain('md:h-[calc(100%+3rem)]')
    expect(surface.className).not.toMatch(/(^|\s)h-full(\s|$)/)
  })

  it('lets the header controls wrap onto their own row on narrow screens', () => {
    useChatStore.setState({ messages: [{ id: 'msg-1', role: 'user', content: 'Hello', timestamp: new Date().toISOString() }] })
    render(<ChatPage />)
    const newChat = screen.getByRole('button', { name: /New Chat/ })
    const controls = newChat.parentElement as HTMLElement
    expect(controls.className).toContain('w-full')
    expect(controls.className).toContain('sm:w-auto')
    expect(controls.parentElement!.className).toContain('flex-wrap')
    expect(screen.getByRole('textbox', { name: 'Chat model' }).className).not.toMatch(/(^|\s)w-48(\s|$)/)
    expect(screen.getByRole('combobox', { name: 'Chat provider' }).className).not.toMatch(/(^|\s)min-w-40(\s|$)/)
  })

  it('surfaces a not-authorized state instead of an empty provider list on 403', async () => {
    providerList.current = { data: undefined, error: new ApiError(403, JSON.stringify({ error: { code: 403, message: 'not authorized' } })) }
    render(<ChatPage />)
    expect(screen.getByRole('alert')).toHaveTextContent('Not authorized to list Providers in default')

    fireEvent.pointerDown(screen.getByRole('combobox', { name: 'Chat provider' }), { button: 0, pointerId: 1, pointerType: 'mouse' })
    expect(await screen.findByRole('option', { name: /Server default/ })).toBeInTheDocument()
    expect(screen.queryByRole('option', { name: /anthropic \(/ })).not.toBeInTheDocument()
  })

  it('ignores cached Provider items once listing them is forbidden', async () => {
    providerList.current = {
      data: { items: [{ name: 'anthropic', type: 'anthropic', defaultModel: 'claude-sonnet-4-20250514', ready: true }] },
      error: new ApiError(403, JSON.stringify({ error: { code: 403, message: 'not authorized' } })),
    }
    render(<ChatPage />)
    expect(screen.getByRole('alert')).toHaveTextContent('Not authorized to list Providers in default')
    fireEvent.pointerDown(screen.getByRole('combobox', { name: 'Chat provider' }), { button: 0, pointerId: 1, pointerType: 'mouse' })
    expect(await screen.findByRole('option', { name: /Server default/ })).toBeInTheDocument()
    expect(screen.queryByRole('option', { name: /anthropic \(/ })).not.toBeInTheDocument()
  })

  it('shows other provider list failures with their message', () => {
    providerList.current = { data: undefined, error: new ApiError(500, 'boom') }
    render(<ChatPage />)
    expect(screen.getByRole('alert')).toHaveTextContent('Providers unavailable: boom')
  })

  it('preselects the sole ready provider when the server has no default provider', async () => {
    chatConfig.current = { model: '', provider: '', enabled: true }
    providerList.current = {
      data: { items: [{ name: 'openai-proxy', type: 'openai', ready: true, defaultModel: 'gpt-5' }], metadata: {} },
      error: undefined,
    }
    render(<ChatPage />)
    await waitFor(() => expect(useChatStore.getState().provider).toBe('openai-proxy'))
    expect(useChatStore.getState().model).toBe('gpt-5')
  })

  it('leaves the picker empty and blocks sending when several providers exist and there is no server default', async () => {
    chatConfig.current = { model: '', provider: '', enabled: true }
    render(<ChatPage />)
    expect(useChatStore.getState().provider).toBe('')
    expect(screen.queryByRole('option', { name: /Server default/ })).not.toBeInTheDocument()
    expect(await screen.findByText('Choose a provider before sending.')).toBeInTheDocument()
  })

  it('disables the model override when neither a provider nor a server default provider exists', async () => {
    chatConfig.current = { model: '', provider: '', enabled: true }
    render(<ChatPage />)
    const modelInput = screen.getByRole('textbox', { name: 'Chat model' })
    expect(modelInput).toBeDisabled()
    expect(modelInput).toHaveAccessibleDescription('Choose a provider to override the model.')

    // Picking a Provider CRD re-enables it.
    useChatStore.setState({ selections: { default: { provider: 'openai-proxy', model: 'gpt-5' } } })
    render(<ChatPage />)
    await waitFor(() => expect(screen.getAllByRole('textbox', { name: 'Chat model' })[1]).not.toBeDisabled())
  })

  it('keeps the model override editable when the server has a default provider to pin it to', () => {
    render(<ChatPage />)
    expect(screen.getByRole('textbox', { name: 'Chat model' })).not.toBeDisabled()
    expect(screen.queryByText('Choose a provider to override the model.')).not.toBeInTheDocument()
  })

  it('clears a persisted provider that is no longer listed', async () => {
    useChatStore.setState({ provider: 'retired-provider', model: 'old-model', selections: { default: { provider: 'retired-provider', model: 'old-model' } } })
    render(<ChatPage />)
    await waitFor(() => expect(useChatStore.getState().provider).toBe(''))
    expect(useChatStore.getState().model).toBe('')
    expect(useChatStore.getState().selections.default).toEqual({ provider: '', model: '' })
    expect(screen.getByRole('combobox', { name: 'Chat provider' })).toHaveTextContent('Server default')
  })

  it('clears a persisted provider when listing Providers is forbidden', async () => {
    providerList.current = { data: undefined, error: new ApiError(403, JSON.stringify({ error: { code: 403, message: 'not authorized' } })) }
    useChatStore.setState({ provider: 'openai-proxy', model: 'gpt-5', selections: { default: { provider: 'openai-proxy', model: 'gpt-5' } } })
    render(<ChatPage />)
    await waitFor(() => expect(useChatStore.getState().provider).toBe(''))
    expect(useChatStore.getState().model).toBe('')
  })

  it('does not clear the new namespace selection while switching namespaces', async () => {
    useChatStore.setState({
      activeNamespace: 'default', provider: 'openai-proxy', model: 'gpt-5',
      selections: { default: { provider: 'openai-proxy', model: 'gpt-5' }, team: { provider: 'anthropic', model: 'claude-sonnet-4-20250514' } },
    })
    render(<ChatPage />)
    await waitFor(() => expect(useChatStore.getState().provider).toBe('openai-proxy'))

    // Namespace "team" only lists anthropic; the previous namespace's pick is
    // not in it, but that must not clear team's own valid selection.
    providerList.current = { data: { items: [{ name: 'anthropic', type: 'anthropic', defaultModel: 'claude-sonnet-4-20250514', ready: true }] }, error: null }
    useUIStore.setState({ namespace: 'team' })
    await waitFor(() => expect(useChatStore.getState().activeNamespace).toBe('team'))
    await waitFor(() => expect(useChatStore.getState().provider).toBe('anthropic'))
    expect(useChatStore.getState().selections.team).toEqual({ provider: 'anthropic', model: 'claude-sonnet-4-20250514' })
    expect(useChatStore.getState().selections.default).toEqual({ provider: 'openai-proxy', model: 'gpt-5' })
  })

  it('keeps a persisted provider while the Provider list is still loading', () => {
    providerList.current = { data: undefined, error: null }
    useChatStore.setState({ provider: 'openai-proxy', model: 'gpt-5', selections: { default: { provider: 'openai-proxy', model: 'gpt-5' } } })
    render(<ChatPage />)
    expect(useChatStore.getState().provider).toBe('openai-proxy')
  })

  it('scopes the persisted provider/model to the active namespace', async () => {
    useChatStore.setState({
      activeNamespace: '',
      selections: { default: { provider: 'openai-proxy', model: 'gpt-5' }, team: { provider: '', model: '' } },
    })
    render(<ChatPage />)
    await waitFor(() => expect(useChatStore.getState().provider).toBe('openai-proxy'))
    expect(screen.getByRole('textbox', { name: 'Chat model' })).toHaveValue('gpt-5')

    useUIStore.setState({ namespace: 'team' })
    await waitFor(() => expect(useChatStore.getState().provider).toBe(''))
    expect(screen.getByRole('textbox', { name: 'Chat model' })).toHaveValue('')
    expect(useChatStore.getState().selections.default).toEqual({ provider: 'openai-proxy', model: 'gpt-5' })
  })

  it('session ID badge shown when currentSessionId is set', () => {
    useChatStore.setState({ currentSessionId: 'session-abc-123' })
    render(<ChatPage />)
    expect(screen.getByText('session-abc-123')).toBeInTheDocument()
  })

  it('ellipsizes a long model name from its end, with the full name in the title', () => {
    const longModel = 'us.anthropic.claude-sonnet-4-5-20250929-v1:0-with-a-very-long-deployment-suffix'
    chatConfig.current = { ...chatConfig.current, model: longModel }
    useChatStore.setState({ currentSessionId: 'chat-0123456789abcdef0123456789abcdef' })
    render(<ChatPage />)
    for (const text of [longModel, 'chat-0123456789abcdef0123456789abcdef']) {
      const label = screen.getByText(text)
      const badge = label.parentElement as HTMLElement
      // A centered flex box would show only the middle of overflowing text;
      // the inner span truncates from the end inside a left-aligned badge.
      expect(label.tagName).toBe('SPAN')
      expect(label).toHaveClass('truncate')
      expect(badge).toHaveAttribute('data-slot', 'badge')
      expect(badge).toHaveAttribute('title', text)
      expect(badge).toHaveClass('min-w-0', 'max-w-full', 'justify-start')
      expect(badge).not.toHaveClass('justify-center')
    }
  })
})
