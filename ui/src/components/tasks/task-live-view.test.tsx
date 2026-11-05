import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor } from '@/test/test-utils'

vi.mock('zustand/middleware', () => ({ persist: (fn: unknown) => fn }))

import { TaskLiveView } from './task-live-view'
import { LIVE_VIEW_POLL_MS } from '@/hooks/use-task-live-view'
import { useAuthStore } from '@/stores/auth'

const fetchMock = vi.fn()

beforeEach(() => {
  vi.stubGlobal('fetch', fetchMock)
  useAuthStore.setState({ token: 'viewer-token' })
})

afterEach(() => {
  fetchMock.mockReset()
  vi.unstubAllGlobals()
  vi.useRealTimers()
})

const frame = () =>
  new Response(new Blob(['jpeg'], { type: 'image/jpeg' }), {
    status: 200,
    headers: { 'Content-Type': 'image/jpeg', ETag: '"7"', 'X-Orka-Frame-Time': '2026-10-28T23:30:00Z' },
  })

describe('TaskLiveView', () => {
  // The dashboard's Content Security Policy allows data: images, not blob: URLs.
  it('shows the latest frame, fetched with the bearer token', async () => {
    fetchMock.mockResolvedValue(frame())
    render(<TaskLiveView taskId="shop" running />)

    const img = await screen.findByRole('img', { name: "What the Task's browser shows now" })
    expect(img).toHaveAttribute('src', expect.stringMatching(/^data:image\/jpeg;base64,/))
    expect(screen.getByText('View only')).toBeInTheDocument()
    expect(screen.getByText(/Not recorded/)).toBeInTheDocument()
    const [url, init] = fetchMock.mock.calls[0]
    expect(String(url)).toContain('/tasks/shop/live')
    expect(init.headers.Authorization).toBe('Bearer viewer-token')
  })

  it('names the Task it shows by UID', async () => {
    fetchMock.mockResolvedValue(frame())
    render(<TaskLiveView taskId="shop" uid="uid-shop" running />)
    await screen.findByRole('img')
    expect(new URL(String(fetchMock.mock.calls[0][0]), 'http://localhost').searchParams.get('uid')).toBe('uid-shop')
  })

  it('waits for the first frame', async () => {
    fetchMock.mockResolvedValue(new Response(null, { status: 204 }))
    render(<TaskLiveView taskId="shop" running />)
    expect(await screen.findByText('Waiting for the browser to show a page.')).toBeInTheDocument()
  })

  it('renders nothing and stops polling when the Task has no live view', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    fetchMock.mockResolvedValue(new Response(null, { status: 404 }))
    const { container } = render(<TaskLiveView taskId="shop" running />)

    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1))
    await waitFor(() => expect(container).toBeEmptyDOMElement())
    await vi.advanceTimersByTimeAsync(LIVE_VIEW_POLL_MS * 3)
    expect(fetchMock).toHaveBeenCalledTimes(1)
  })

  it('does not poll a Task that is not running', async () => {
    const { container } = render(<TaskLiveView taskId="shop" running={false} />)
    expect(container).toBeEmptyDOMElement()
    expect(fetchMock).not.toHaveBeenCalled()
  })

  it('sends the frame tag back, so an unchanged frame costs no download', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    fetchMock.mockResolvedValueOnce(frame()).mockResolvedValue(new Response(null, { status: 304 }))
    render(<TaskLiveView taskId="shop" running />)

    await screen.findByRole('img')
    await vi.advanceTimersByTimeAsync(LIVE_VIEW_POLL_MS)
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2))
    expect(fetchMock.mock.calls[1][1].headers['If-None-Match']).toBe('"7"')
    expect(screen.getByRole('img')).toHaveAttribute('src', expect.stringMatching(/^data:image\/jpeg;base64,/))
  })

  it("never shows another Task's frame while the new one waits", async () => {
    fetchMock.mockImplementation(async (url: string) =>
      String(url).includes('/tasks/shop/') ? frame() : new Response(null, { status: 204 }),
    )
    const { rerender } = render(<TaskLiveView taskId="shop" running />)
    await screen.findByRole('img')

    rerender(<TaskLiveView taskId="checkout" running />)
    expect(await screen.findByText('Waiting for the browser to show a page.')).toBeInTheDocument()
    expect(screen.queryByRole('img')).not.toBeInTheDocument()
  })

  it('drops a frame the controller no longer has', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    fetchMock.mockResolvedValueOnce(frame()).mockResolvedValue(new Response(null, { status: 204 }))
    render(<TaskLiveView taskId="shop" running />)
    await screen.findByRole('img')

    await vi.advanceTimersByTimeAsync(LIVE_VIEW_POLL_MS)
    expect(await screen.findByText('Waiting for the browser to show a page.')).toBeInTheDocument()
    expect(screen.queryByRole('img')).not.toBeInTheDocument()
  })

  it('keeps the frame and stops polling while paused', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    fetchMock.mockResolvedValue(frame())
    const { rerender } = render(<TaskLiveView taskId="shop" running />)
    await screen.findByRole('img')

    rerender(<TaskLiveView taskId="shop" running paused />)
    const calls = fetchMock.mock.calls.length
    await vi.advanceTimersByTimeAsync(LIVE_VIEW_POLL_MS * 3)
    expect(fetchMock.mock.calls.length).toBe(calls)
    expect(screen.getByRole('img')).toBeInTheDocument()
    expect(screen.getByText(/Paused: turn Following back on/)).toBeInTheDocument()
  })
})
