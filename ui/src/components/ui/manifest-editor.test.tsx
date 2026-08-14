import { describe, it, expect, vi } from 'vitest'
import { fireEvent, render, screen, waitFor } from '@/test/test-utils'
import userEvent from '@testing-library/user-event'

import { ManifestEditor } from './manifest-editor'

describe('ManifestEditor', () => {
  it('seeds the textarea with YAML from the initial value', () => {
    render(
      <ManifestEditor
        open
        onOpenChange={() => {}}
        title="Edit spec"
        initialValue={{ spec: { replicas: 2 } }}
        submitLabel="Save"
        onSubmit={() => {}}
      />,
    )
    expect(screen.getByLabelText('Manifest YAML')).toHaveValue('spec:\n  replicas: 2\n')
  })

  it('submits the parsed YAML document', async () => {
    const onSubmit = vi.fn()
    const user = userEvent.setup()
    render(
      <ManifestEditor
        open
        onOpenChange={() => {}}
        title="Edit spec"
        initialValue={{}}
        submitLabel="Save"
        onSubmit={onSubmit}
      />,
    )
    const textarea = screen.getByLabelText('Manifest YAML')
    await user.clear(textarea)
    await user.type(textarea, 'spec:{enter}  targetActors: 3')
    await user.click(screen.getByRole('button', { name: 'Save' }))
    await waitFor(() => expect(onSubmit).toHaveBeenCalledWith({ spec: { targetActors: 3 } }))
  })

  it('shows a parse error and keeps the dialog open', async () => {
    const onSubmit = vi.fn()
    const user = userEvent.setup()
    render(
      <ManifestEditor
        open
        onOpenChange={() => {}}
        title="Edit spec"
        initialValue={{}}
        submitLabel="Save"
        onSubmit={onSubmit}
      />,
    )
    const textarea = screen.getByLabelText('Manifest YAML')
    fireEvent.change(textarea, { target: { value: 'not: valid: yaml: [[[' } })
    await user.click(screen.getByRole('button', { name: 'Save' }))
    expect(await screen.findByRole('alert')).toBeInTheDocument()
    expect(onSubmit).not.toHaveBeenCalled()
  })

  it('rejects non-mapping documents', async () => {
    const onSubmit = vi.fn()
    const user = userEvent.setup()
    render(
      <ManifestEditor
        open
        onOpenChange={() => {}}
        title="Edit spec"
        initialValue={{}}
        submitLabel="Save"
        onSubmit={onSubmit}
      />,
    )
    const textarea = screen.getByLabelText('Manifest YAML')
    await user.clear(textarea)
    await user.type(textarea, '- just')
    await user.click(screen.getByRole('button', { name: 'Save' }))
    expect(await screen.findByRole('alert')).toHaveTextContent(/mapping/i)
    expect(onSubmit).not.toHaveBeenCalled()
  })

  it('surfaces submit rejections inline', async () => {
    const onSubmit = vi.fn().mockRejectedValue(new Error('server said no'))
    const user = userEvent.setup()
    render(
      <ManifestEditor
        open
        onOpenChange={() => {}}
        title="Edit spec"
        initialValue={{ spec: {} }}
        submitLabel="Save"
        onSubmit={onSubmit}
      />,
    )
    await user.click(screen.getByRole('button', { name: 'Save' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('server said no')
  })
})
