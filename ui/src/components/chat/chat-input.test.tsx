import { describe, it, expect, vi, beforeEach } from 'vitest'

vi.mock('zustand/middleware', async () => {
  const actual = await vi.importActual('zustand/middleware')
  return { ...actual, persist: (fn: any) => fn }
})

import { render, screen, fireEvent } from '@/test/test-utils'
import { ChatInput } from './chat-input'
import { useChatStore } from '@/stores/chat'
import { useUIStore } from '@/stores/ui'
import { useAuthStore } from '@/stores/auth'

beforeEach(() => {
  useUIStore.setState({ namespace: 'default', sidebarCollapsed: false, theme: 'light' })
  useAuthStore.setState({ token: 'test-token' })
  useChatStore.setState({ messages: [], currentSessionId: null, isStreaming: false })
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

  it('send button is disabled when streaming', () => {
    useChatStore.setState({ isStreaming: true })
    render(<ChatInput onSend={vi.fn()} />)
    fireEvent.change(screen.getByPlaceholderText('Message the meta agent...'), {
      target: { value: 'hello' },
    })
    expect(screen.getByRole('button')).toBeDisabled()
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
