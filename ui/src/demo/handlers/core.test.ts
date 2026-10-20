import { describe, expect, it } from 'vitest'
import { handleDemoRequest } from '../server'
import { DEMO_TOKEN } from '../token'

const validate = (token: string) =>
  handleDemoRequest(new Request('http://localhost/api/v1/auth/validate', { headers: { Authorization: `Bearer ${token}` } }))

describe('demo sign-in', () => {
  it('accepts only the demo credential', async () => {
    expect((await validate(DEMO_TOKEN)).status).toBe(200)
    expect((await validate('a-real-service-account-token')).status).toBe(401)
  })
})
