import { describe, expect, it, vi } from 'vitest'
import { fireEvent, render, screen, waitFor } from '@/test/test-utils'
import type { Task } from '@/schemas/task'
import { TaskYaml } from './task-yaml'

const task = {
  metadata: { name: 'triage', namespace: 'default' },
  spec: {
    type: 'ai',
    priority: 500,
    agentRef: { name: 'triager' },
    ai: { prompt: 'Label the issue.\nThen look for duplicates.' },
    env: [{ name: 'API_KEY', value: 'sk-live-123456' }],
  },
  status: { phase: 'Succeeded', attempts: 1 },
} as unknown as Task

const tokenOf = (region: HTMLElement, text: string) =>
  [...region.querySelectorAll('span')].find((span) => span.textContent === text)?.className

describe('TaskYaml', () => {
  it('shows the resource as colored YAML without environment values', async () => {
    render(<TaskYaml task={task} />)
    const region = await screen.findByRole('region', { name: 'Task YAML' })
    expect(region).toHaveTextContent('kind: Task')
    expect(region.textContent).not.toContain('sk-live')
    expect(tokenOf(region, 'kind')).toBe('text-primary')
    expect(tokenOf(region, 'Task')).toBe('text-status-succeeded')
    expect(tokenOf(region, '500')).toBe('text-status-pending')
    // The prompt is a block scalar: its lines are text, and the key after it is a key again.
    expect(tokenOf(region, '      Then look for duplicates.')).toBe('text-status-succeeded')
    expect(tokenOf(region, 'env')).toBe('text-primary')
  })

  it('colors an indented multi-line value as text, not as fields', async () => {
    const indented = { ...task, spec: { ...task.spec, ai: { prompt: '  enabled: false\n  retries: 3' } } } as Task
    render(<TaskYaml task={indented} />)
    const region = await screen.findByRole('region', { name: 'Task YAML' })
    expect(region.textContent).toMatch(/prompt: \|2-/)
    expect(tokenOf(region, '        enabled: false')).toBe('text-status-succeeded')
    expect(tokenOf(region, 'enabled')).toBeUndefined()
  })

  it('offers a retry when the YAML library fails to load', async () => {
    let calls = 0
    const load = () => (++calls === 1 ? Promise.reject(new Error('chunk failed')) : import('yaml').then((yaml) => yaml.stringify))
    render(<TaskYaml task={task} load={load} />)
    expect(await screen.findByRole('alert')).toHaveTextContent('The YAML view could not load')
    fireEvent.click(screen.getByRole('button', { name: 'Retry' }))
    expect(await screen.findByRole('region', { name: 'Task YAML' })).toHaveTextContent('kind: Task')
    expect(calls).toBe(2)
  })

  it('copies the YAML it shows', async () => {
    const writeText = vi.fn().mockResolvedValue(undefined)
    Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText } })
    render(<TaskYaml task={task} />)
    await screen.findByRole('region', { name: 'Task YAML' })
    fireEvent.click(screen.getByRole('button', { name: 'Copy' }))
    await waitFor(() => expect(writeText).toHaveBeenCalled())
    expect(writeText.mock.calls[0][0]).toContain('apiVersion: core.orka.ai/v1alpha1')
    expect(writeText.mock.calls[0][0]).not.toContain('sk-live')
  })
})
