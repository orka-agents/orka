import { describe, it, expect } from 'vitest'
import { fireEvent, render, screen } from '@/test/test-utils'
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

  it('renders a persisted tool exchange with expandable arguments and results', () => {
    const jsonl = [
      { role: 'user', content: 'List my tasks' },
      { role: 'assistant', content: '', toolCalls: [
        { id: 'call-1', name: 'list_tasks', arguments: { namespace: 'default' } },
      ] },
      { role: 'tool', content: '{"success":true,"data":[]}', toolCallID: 'call-1' },
      { role: 'assistant', content: 'No tasks found' },
    ].map((message) => JSON.stringify(message)).join('\n')

    const { container } = render(<TranscriptViewer transcript={jsonl} />)
    const call = screen.getByText('Tool call:')
    const result = screen.getByText('Tool result:')
    expect(call).toHaveTextContent('list_tasks')
    expect(result).toHaveTextContent('list_tasks')
    expect(call.closest('details')).not.toHaveAttribute('open')
    expect(result.closest('details')).not.toHaveAttribute('open')

    fireEvent.click(call)
    fireEvent.click(result)
    expect(call.closest('details')).toHaveAttribute('open')
    expect(result.closest('details')).toHaveAttribute('open')
    expect(screen.getByText(/"namespace": "default"/)).toBeVisible()
    expect(screen.getByText('{"success":true,"data":[]}')).toBeVisible()
    expect(screen.getAllByText('Call ID: call-1')).toHaveLength(2)
    expect(screen.getByText('No tasks found')).toBeVisible()
    expect([...container.querySelectorAll('pre')].every((element) => element.textContent)).toBe(true)
  })

  it('labels reordered tool results by ID and preserves assistant text', () => {
    const jsonl = [
      { role: 'assistant', content: 'Checking both resources', toolCalls: [
        { id: 'call-1', name: 'list_tasks', arguments: {} },
        { id: 'call-2', name: 'list_agents', arguments: {} },
      ] },
      { role: 'tool', content: 'Agents found', toolCallID: 'call-2' },
      { role: 'tool', content: 'Tasks found', toolCallID: 'call-1' },
      { role: 'assistant', content: '', toolCalls: [
        { id: 'call-1', name: 'get_task', arguments: { name: 'task-1' } },
      ] },
      { role: 'tool', content: 'Task details', toolCallID: 'call-1' },
    ].map((message) => JSON.stringify(message)).join('\n')

    render(<TranscriptViewer transcript={jsonl} />)
    expect(screen.getByText('Checking both resources')).toBeVisible()
    const results = screen.getAllByText('Tool result:')
    expect(results.map((result) => result.textContent)).toEqual([
      'Tool result: list_agents',
      'Tool result: list_tasks',
      'Tool result: get_task',
    ])
  })

  it('preserves serialized tool arguments and results verbatim', () => {
    const argumentsText = '{"id":9223372036854775807}'
    const resultText = '{\n  "id": 9223372036854775807,\n  "value": 0.1234567890123456789,\n  "zero": -0\n}\n'
    const transcript = [
      { role: 'assistant', toolCalls: [{ id: 'precise-call', name: 'read_value', arguments: argumentsText }] },
      { role: 'tool', toolCallID: 'precise-call', content: resultText },
    ].map((message) => JSON.stringify(message)).join('\n')

    const { container } = render(<TranscriptViewer transcript={transcript} />)
    fireEvent.click(screen.getByText('Tool call:'))
    fireEvent.click(screen.getByText('Tool result:'))
    const content = container.querySelectorAll('details pre')
    expect(content).toHaveLength(2)
    expect(content[0].textContent).toBe(argumentsText)
    expect(content[1].textContent).toBe(resultText)
    expect(content[0]).toBeVisible()
    expect(content[1]).toBeVisible()
  })

  it('uses exact argument text from the Session API when JSON numbers exceed browser precision', () => {
    const argumentsText = '{\n  "big": 9223372036854775807,\n  "decimal": 0.1234567890123456789,\n  "nested": [\n    9007199254740993,\n    -0,\n    1.2300e+42\n  ],\n  "zero": -0\n}'
    const transcript = `{"role":"assistant","content":"","toolCalls":[{"id":"precise-call","name":"read_value","arguments":{"big":9223372036854775807,"decimal":0.1234567890123456789,"nested":[9007199254740993,-0,1.2300e+42],"zero":-0},"argumentsText":${JSON.stringify(argumentsText)}}]}`

    const { container } = render(<TranscriptViewer transcript={transcript} />)
    fireEvent.click(screen.getByText('Tool call:'))
    const content = container.querySelector('details pre')
    expect(content?.textContent).toBe(argumentsText)
    expect(content).toBeVisible()
  })

  it('shows standalone named and unknown tool results without inventing a status', () => {
    const jsonl = [
      { role: 'tool', name: 'file_read', content: 'plain text result', toolCallID: 'missing-call' },
      { role: 'tool', content: 'another result', toolCallID: 'call-unknown' },
    ].map((message) => JSON.stringify(message)).join('\n')

    render(<TranscriptViewer transcript={jsonl} />)
    const results = screen.getAllByText('Tool result:')
    expect(results[0]).toHaveTextContent('file_read')
    expect(results[1]).toHaveTextContent('call-unknown')
    fireEvent.click(results[0])
    expect(screen.getByText('plain text result')).toBeVisible()
  })
})
