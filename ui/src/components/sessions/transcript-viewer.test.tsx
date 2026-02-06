import { describe, it, expect } from 'vitest'
import { render, screen } from '@/test/test-utils'
import { TranscriptViewer } from './transcript-viewer'

describe('TranscriptViewer', () => {
  it('shows empty message when transcript is undefined', () => {
    render(<TranscriptViewer />)
    expect(screen.getByText('No messages in this session.')).toBeInTheDocument()
  })

  it('shows empty message when transcript is empty string', () => {
    render(<TranscriptViewer transcript="" />)
    expect(screen.getByText('No messages in this session.')).toBeInTheDocument()
  })

  it('renders user and assistant messages from valid JSONL', () => {
    const jsonl = [
      JSON.stringify({ role: 'user', content: 'Hello' }),
      JSON.stringify({ role: 'assistant', content: 'Hi there!' }),
    ].join('\n')

    render(<TranscriptViewer transcript={jsonl} />)
    expect(screen.getByText('Hello')).toBeInTheDocument()
    expect(screen.getByText('Hi there!')).toBeInTheDocument()
  })

  it('skips invalid JSON lines gracefully', () => {
    const jsonl = [
      JSON.stringify({ role: 'user', content: 'Good line' }),
      'not valid json {{{',
      JSON.stringify({ role: 'assistant', content: 'Also good' }),
    ].join('\n')

    render(<TranscriptViewer transcript={jsonl} />)
    expect(screen.getByText('Good line')).toBeInTheDocument()
    expect(screen.getByText('Also good')).toBeInTheDocument()
  })

  it('applies different alignment for user vs assistant messages', () => {
    const jsonl = [
      JSON.stringify({ role: 'user', content: 'User msg' }),
      JSON.stringify({ role: 'assistant', content: 'Bot msg' }),
    ].join('\n')

    const { container } = render(<TranscriptViewer transcript={jsonl} />)
    const rows = container.querySelectorAll('.flex.gap-3')
    // user message: justify-end
    expect(rows[0].className).toContain('justify-end')
    // assistant message: justify-start
    expect(rows[1].className).toContain('justify-start')
  })

  it('shows metadata (model, tokens) when present', () => {
    const jsonl = JSON.stringify({
      role: 'assistant',
      content: 'Reply',
      model: 'claude',
      inputTokens: 10,
      outputTokens: 20,
    })

    render(<TranscriptViewer transcript={jsonl} />)
    expect(screen.getByText('claude')).toBeInTheDocument()
    expect(screen.getByText('↑10')).toBeInTheDocument()
    expect(screen.getByText('↓20')).toBeInTheDocument()
  })

  it('does not show metadata section when none present', () => {
    const jsonl = JSON.stringify({ role: 'user', content: 'Plain message' })
    const { container } = render(<TranscriptViewer transcript={jsonl} />)
    expect(screen.getByText('Plain message')).toBeInTheDocument()
    // No metadata spans should exist
    expect(container.querySelector('.opacity-70')).toBeNull()
  })
})
