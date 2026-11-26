import { describe, expect, it, beforeEach, vi } from 'vitest'
import { act, renderHook, waitFor } from '@testing-library/react'
import { http, HttpResponse } from 'msw'
import { server } from '@/test/mocks/server'

vi.mock('zustand/middleware', () => ({ persist: (fn: unknown) => fn }))

import { useAuthStore } from '@/stores/auth'
import { useUIStore } from '@/stores/ui'
import { useOversightStream } from './use-oversight-stream'
import { makeDecision } from '@/test/fixtures/oversight'

const frame = (seq: number) => `id: ${seq}\nevent: decision\ndata: ${JSON.stringify(makeDecision({ id: `t@${seq}`, seq }))}\n\n`

describe('useOversightStream', () => {
  beforeEach(() => {
    useUIStore.setState({ namespace: 'default' })
    useAuthStore.setState({ token: 'test-token' })
  })

  it('reconnects after a failure and resumes from the last seq without duplicates', async () => {
    const afters: string[] = []
    let calls = 0
    server.use(http.get('/api/v1/oversight/decisions/stream', ({ request }) => {
      afters.push(new URL(request.url).searchParams.get('after') ?? '')
      calls++
      if (calls === 1) return new HttpResponse(`event: ready\ndata: {}\n\n${frame(5)}${frame(5)}`, { headers: { 'Content-Type': 'text/event-stream' } })
      if (calls === 2) return new HttpResponse('boom', { status: 500 })
      return new HttpResponse(`event: ready\ndata: {}\n\n${frame(6)}`, { headers: { 'Content-Type': 'text/event-stream' } })
    }))
    const { result, unmount } = renderHook(() => useOversightStream({ q: '', enabled: true, after: 2, reconnectDelayMs: 5 }))
    await waitFor(() => expect(result.current.decisions.map((d) => d.seq)).toEqual([6, 5]), { timeout: 3000 })
    expect(afters.slice(0, 3)).toEqual(['2', '5', '5'])
    expect(result.current.lastSeq).toBe(6)
    unmount()
  })

  it('drops the previous namespace\'s decisions on a switch while paused', async () => {
    server.use(http.get('/api/v1/oversight/decisions/stream', () =>
      new HttpResponse(`event: ready\ndata: {}\n\n${frame(7)}`, { headers: { 'Content-Type': 'text/event-stream' } })))
    const { result, rerender, unmount } = renderHook(({ enabled }) => useOversightStream({ q: '', enabled, after: 0, reconnectDelayMs: 5 }), { initialProps: { enabled: true } })
    await waitFor(() => expect(result.current.decisions.map((d) => d.seq)).toEqual([7]))
    rerender({ enabled: false })
    expect(result.current.decisions.map((d) => d.seq)).toEqual([7])
    act(() => useUIStore.setState({ namespace: 'other' }))
    await waitFor(() => expect(result.current.decisions).toEqual([]))
    expect(result.current.lastSeq).toBe(0)
    unmount()
  })

  it('stops for good on 403', async () => {
    let calls = 0
    server.use(http.get('/api/v1/oversight/decisions/stream', () => { calls++; return new HttpResponse('', { status: 403 }) }))
    const { result, unmount } = renderHook(() => useOversightStream({ q: '', enabled: true, after: 0, reconnectDelayMs: 5 }))
    await waitFor(() => expect(result.current.status).toBe('forbidden'))
    await new Promise((r) => setTimeout(r, 50))
    expect(calls).toBe(1)
    unmount()
  })
})
