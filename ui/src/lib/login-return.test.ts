import { describe, it, expect, beforeEach } from 'vitest'
import { rememberLoginReturn, takeLoginReturn } from './login-return'

describe('login return', () => {
  beforeEach(() => {
    sessionStorage.clear()
    window.history.replaceState(null, '', '/login')
  })

  it('returns home when nothing was remembered', () => {
    expect(takeLoginReturn()).toBe('/')
  })

  it('returns to the remembered path once', () => {
    rememberLoginReturn('/tasks?status=running')
    expect(takeLoginReturn()).toBe('/tasks?status=running')
    expect(takeLoginReturn()).toBe('/')
  })

  it('carries the fragment the address bar still holds', () => {
    rememberLoginReturn('/settings/connectors?status=pending')
    window.history.replaceState(null, '', '/login#completion=one-time')
    expect(takeLoginReturn()).toBe('/settings/connectors?status=pending#completion=one-time')
  })

  it.each(['//evil.example/x', '/\\evil.example', 'https://evil.example/', '/login?x=1'])('refuses %s', (path) => {
    rememberLoginReturn(path)
    expect(takeLoginReturn()).toBe('/')
  })
})
