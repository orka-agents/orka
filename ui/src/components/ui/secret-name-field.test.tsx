import { describe, it, expect, beforeEach, vi } from 'vitest'

const mockUseSecretNames = vi.fn()

vi.mock('@/hooks/use-secrets', () => ({
  useSecretNames: () => mockUseSecretNames(),
}))

import { useState } from 'react'
import userEvent from '@testing-library/user-event'
import { render, screen } from '@/test/test-utils'
import { ApiError } from '@/lib/api-client'
import { SecretNameField } from './secret-name-field'

function installDOMPolyfills() {
  if (typeof globalThis.ResizeObserver === 'undefined') {
    globalThis.ResizeObserver = class { observe() {}; unobserve() {}; disconnect() {} } as any
  }
  if (!HTMLElement.prototype.hasPointerCapture) HTMLElement.prototype.hasPointerCapture = () => false
  if (!HTMLElement.prototype.setPointerCapture) HTMLElement.prototype.setPointerCapture = () => {}
  if (!HTMLElement.prototype.releasePointerCapture) HTMLElement.prototype.releasePointerCapture = () => {}
  if (!HTMLElement.prototype.scrollIntoView) HTMLElement.prototype.scrollIntoView = () => {}
}

const changes: string[] = []

function Field({ initial = '' }: { initial?: string }) {
  const [value, setValue] = useState(initial)
  return (
    <>
      <label htmlFor="git-secret">Git Secret</label>
      <SecretNameField
        id="git-secret"
        value={value}
        onChange={(next) => {
          changes.push(next)
          setValue(next)
        }}
        placeholder="Optional Git credential secret"
      />
    </>
  )
}

describe('SecretNameField', () => {
  beforeEach(() => {
    installDOMPolyfills()
    changes.length = 0
    mockUseSecretNames.mockReset()
  })

  it('picks a listed Secret and can clear the choice', async () => {
    mockUseSecretNames.mockReturnValue({
      data: { items: [{ name: 'github-token', namespace: 'default', type: 'Opaque' }] },
      error: null,
      isLoading: false,
    })
    const user = userEvent.setup()
    render(<Field />)

    const trigger = screen.getByRole('combobox', { name: 'Git Secret' })
    expect(trigger).toHaveTextContent('Optional Git credential secret')
    expect(screen.queryByText(/type the Secret name/)).not.toBeInTheDocument()

    await user.click(trigger)
    await user.click(await screen.findByRole('option', { name: 'github-token' }))
    expect(trigger).toHaveTextContent('github-token')

    await user.click(trigger)
    await user.click(await screen.findByRole('option', { name: 'None' }))
    expect(changes).toEqual(['github-token', ''])
    expect(trigger).toHaveTextContent('Optional Git credential secret')
  })

  it('falls back to a typed name with a hint when listing Secrets is forbidden', async () => {
    mockUseSecretNames.mockReturnValue({
      data: undefined,
      error: new ApiError(403, '{"error":{"code":403,"message":"forbidden"}}'),
      isLoading: false,
    })
    const user = userEvent.setup()
    render(<Field />)

    expect(screen.queryByRole('combobox')).not.toBeInTheDocument()
    const input = screen.getByRole('textbox', { name: 'Git Secret' })
    expect(input).toHaveAccessibleDescription("You can't list Secrets; type the Secret name.")

    await user.type(input, 'repo-token')
    expect(input).toHaveValue('repo-token')
    await user.clear(input)
    expect(input).toHaveValue('')
    expect(changes.at(-1)).toBe('')
  })

  it('falls back to a typed name when the Secret list fails', () => {
    mockUseSecretNames.mockReturnValue({
      data: undefined,
      error: new ApiError(500, 'boom'),
      isLoading: false,
    })
    render(<Field initial="existing" />)

    const input = screen.getByRole('textbox', { name: 'Git Secret' })
    expect(input).toHaveValue('existing')
    expect(input).toHaveAccessibleDescription("Secrets couldn't be listed; type the Secret name.")
  })
})
