import { describe, it, expect } from 'vitest'
import { API_BASE_URL } from './constants'

describe('constants', () => {
  it('API_BASE_URL is /api/v1', () => {
    expect(API_BASE_URL).toBe('/api/v1')
  })
})
