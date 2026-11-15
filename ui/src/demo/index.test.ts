import { afterEach, describe, expect, it, vi } from 'vitest'

vi.mock('zustand/middleware', async () => {
  const actual = await vi.importActual('zustand/middleware')
  return { ...actual, persist: (fn: unknown) => fn }
})

import { useAuthStore } from '@/stores/auth'
import { installDemo } from '.'
import { DEMO_TOKEN } from './token'

const realFetch = window.fetch

describe('installDemo', () => {
  afterEach(() => {
    window.fetch = realFetch
  })

  it('replaces any other stored token with the demo credential', () => {
    useAuthStore.setState({ token: 'a-real-service-account-token' })
    installDemo()
    expect(useAuthStore.getState().token).toBe(DEMO_TOKEN)
  })
})
