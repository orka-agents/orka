import { describe, it, expect, beforeEach } from 'vitest'
import { captureHashToken, hasPendingHashToken, rememberLoginReturn, takeHashToken, takeLoginReturn } from './login-return'

describe('login return', () => {
  beforeEach(() => {
    sessionStorage.clear()
    takeHashToken()
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
    expect(takeLoginReturn('completion=one-time')).toBe('/settings/connectors?status=pending#completion=one-time')
  })

  it('ignores the address bar fragment, which is the route under hash routing', () => {
    rememberLoginReturn('/security')
    window.history.replaceState(null, '', '/#/login#/security')
    expect(takeLoginReturn()).toBe('/security')
  })

  it.each(['//evil.example/x', '/\\evil.example', 'https://evil.example/', '/login?x=1'])('refuses %s', (path) => {
    rememberLoginReturn(path)
    expect(takeLoginReturn()).toBe('/')
  })

  it('takes a #token= fragment out of the address bar and hands it over once', () => {
    window.history.replaceState({ key: 'k' }, '', '/monitors/example?tab=runs#token=abc.def')
    captureHashToken()
    expect(window.location.pathname + window.location.search + window.location.hash).toBe('/monitors/example?tab=runs')
    expect(window.history.state).toEqual({ key: 'k' })
    expect(hasPendingHashToken()).toBe(true)
    expect(takeHashToken()).toBe('abc.def')
    expect(hasPendingHashToken()).toBe(false)
    expect(takeHashToken()).toBeNull()
  })

  it('leaves other fragments in place', () => {
    window.history.replaceState(null, '', '/settings/connectors#completion=one-time')
    captureHashToken()
    expect(window.location.hash).toBe('#completion=one-time')
    expect(hasPendingHashToken()).toBe(false)
  })

  it('strips an empty #token= without a pending token', () => {
    window.history.replaceState(null, '', '/login#token=')
    captureHashToken()
    expect(window.location.hash).toBe('')
    expect(hasPendingHashToken()).toBe(false)
  })
})
