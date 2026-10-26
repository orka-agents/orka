import { describe, it, expect, vi, beforeEach } from 'vitest'

vi.mock('zustand/middleware', async () => {
  const actual = await vi.importActual('zustand/middleware')
  return { ...actual, persist: (fn: any) => fn }
})

import { act } from 'react'
import { render, screen, fireEvent } from '@/test/test-utils'
import userEvent from '@testing-library/user-event'
import { ChatInput } from './chat-input'
import { useChatStore } from '@/stores/chat'
import { useUIStore } from '@/stores/ui'
import { useAuthStore } from '@/stores/auth'

beforeEach(() => {
  useUIStore.setState({ namespace: 'default', sidebarCollapsed: false, theme: 'light' })
  useAuthStore.setState({ token: 'test-token' })
  useChatStore.setState({ messages: [], currentSessionId: null, isStreaming: false, stopRequested: false })
})

describe('ChatInput', () => {
  it('renders textarea and send button', () => {
    render(<ChatInput onSend={vi.fn()} />)
    expect(screen.getByPlaceholderText('Message the meta agent...')).toBeInTheDocument()
    expect(screen.getByRole('button')).toBeInTheDocument()
  })

  it('send button is disabled when input is empty', () => {
    render(<ChatInput onSend={vi.fn()} />)
    expect(screen.getByRole('button')).toBeDisabled()
  })

  it('names the send button for assistive technology', () => {
    render(<ChatInput onSend={vi.fn()} />)
    expect(screen.getByRole('button', { name: 'Send message' })).toBeInTheDocument()
  })

  it('swaps Send for Stop while streaming and keeps the composer usable', () => {
    useChatStore.setState({ isStreaming: true })
    const onSend = vi.fn()
    render(<ChatInput onSend={onSend} />)
    const textarea = screen.getByPlaceholderText('Message the meta agent...')
    expect(textarea).not.toBeDisabled()
    expect(screen.queryByRole('button', { name: 'Send message' })).not.toBeInTheDocument()
    fireEvent.change(textarea, { target: { value: 'next question' } })
    fireEvent.keyDown(textarea, { key: 'Enter', shiftKey: false })
    // Drafting continues while the reply streams; only sending is blocked.
    expect(onSend).not.toHaveBeenCalled()
    expect(textarea).toHaveValue('next question')
  })

  it('Stop requests the turn to stop and then shows it is stopping', () => {
    useChatStore.setState({ isStreaming: true })
    render(<ChatInput onSend={vi.fn()} />)
    fireEvent.click(screen.getByRole('button', { name: 'Stop and end this chat' }))
    expect(useChatStore.getState().stopRequested).toBe(true)
    expect(screen.getByRole('button', { name: 'Stopping and ending chat' })).toBeDisabled()
  })

  it('keeps focus in the composer when a turn starts and ends', () => {
    render(<ChatInput onSend={() => useChatStore.setState({ isStreaming: true })} />)
    const textarea = screen.getByPlaceholderText('Message the meta agent...')
    textarea.focus()
    fireEvent.change(textarea, { target: { value: 'hello' } })
    fireEvent.keyDown(textarea, { key: 'Enter', shiftKey: false })
    expect(document.activeElement).toBe(textarea)
    act(() => useChatStore.setState({ isStreaming: false }))
    expect(document.activeElement).toBe(textarea)
  })

  it('moves focus to the composer when Stop is pressed, so it is not lost when Send replaces Stop', async () => {
    const user = userEvent.setup()
    useChatStore.setState({ isStreaming: true })
    render(<ChatInput onSend={vi.fn()} />)
    const textarea = screen.getByPlaceholderText('Message the meta agent...')
    await user.click(screen.getByRole('button', { name: 'Stop and end this chat' }))
    expect(useChatStore.getState().stopRequested).toBe(true)
    expect(document.activeElement).toBe(textarea)
    act(() => useChatStore.getState().setStreaming(false))
    expect(screen.getByRole('button', { name: 'Send message' })).toBeInTheDocument()
    expect(document.activeElement).toBe(textarea)
  })

  it('sends once when Enter is pressed twice in the same tick', () => {
    // The real onSend starts the turn synchronously by setting isStreaming.
    const onSend = vi.fn(() => useChatStore.setState({ isStreaming: true }))
    render(<ChatInput onSend={onSend} />)
    const textarea = screen.getByPlaceholderText('Message the meta agent...')
    fireEvent.change(textarea, { target: { value: 'hello' } })
    act(() => {
      fireEvent.keyDown(textarea, { key: 'Enter', shiftKey: false })
      fireEvent.keyDown(textarea, { key: 'Enter', shiftKey: false })
    })
    expect(onSend).toHaveBeenCalledTimes(1)
  })

  it('typing in textarea updates value', () => {
    render(<ChatInput onSend={vi.fn()} />)
    const textarea = screen.getByPlaceholderText('Message the meta agent...')
    fireEvent.change(textarea, { target: { value: 'hello world' } })
    expect(textarea).toHaveValue('hello world')
  })

  it('clicking send calls onSend with trimmed text and clears input', () => {
    const onSend = vi.fn()
    render(<ChatInput onSend={onSend} />)
    const textarea = screen.getByPlaceholderText('Message the meta agent...')
    fireEvent.change(textarea, { target: { value: '  hello  ' } })
    fireEvent.click(screen.getByRole('button'))
    expect(onSend).toHaveBeenCalledWith('hello')
    expect(textarea).toHaveValue('')
  })

  it('Enter key sends message', () => {
    const onSend = vi.fn()
    render(<ChatInput onSend={onSend} />)
    const textarea = screen.getByPlaceholderText('Message the meta agent...')
    fireEvent.change(textarea, { target: { value: 'test msg' } })
    fireEvent.keyDown(textarea, { key: 'Enter', shiftKey: false })
    expect(onSend).toHaveBeenCalledWith('test msg')
  })

  it('Shift+Enter does NOT send (allows newline)', () => {
    const onSend = vi.fn()
    render(<ChatInput onSend={onSend} />)
    const textarea = screen.getByPlaceholderText('Message the meta agent...')
    fireEvent.change(textarea, { target: { value: 'test msg' } })
    fireEvent.keyDown(textarea, { key: 'Enter', shiftKey: true })
    expect(onSend).not.toHaveBeenCalled()
  })

  it('auto-resizes textarea on input change', () => {
    render(<ChatInput onSend={vi.fn()} />)
    const textarea = screen.getByPlaceholderText('Message the meta agent...') as HTMLTextAreaElement
    // Mock scrollHeight for the auto-resize useEffect
    Object.defineProperty(textarea, 'scrollHeight', { value: 120, configurable: true })
    fireEvent.change(textarea, { target: { value: 'line1\nline2\nline3' } })
    // The useEffect sets height to 'auto' then to Math.min(scrollHeight, 200) + 'px'
    expect(textarea.style.height).toBe('120px')
  })

  it('auto-resize caps at 200px', () => {
    render(<ChatInput onSend={vi.fn()} />)
    const textarea = screen.getByPlaceholderText('Message the meta agent...') as HTMLTextAreaElement
    Object.defineProperty(textarea, 'scrollHeight', { value: 500, configurable: true })
    fireEvent.change(textarea, { target: { value: 'lots of text' } })
    expect(textarea.style.height).toBe('200px')
  })
})
