import { describe, expect, it, vi } from 'vitest'

// jsdom serves no files: stand in for loading a recorded frame so the
// handler's success path runs.
vi.mock('./live-frames', async (importOriginal) => {
  const actual = await importOriginal<typeof import('./live-frames')>()
  return { ...actual, liveFrame: vi.fn(async () => new Blob(['jpeg'], { type: 'image/jpeg' })) }
})

import { liveScene } from './live-frames'
import { handleDemoRequest } from './server'

const live = (name: string, headers?: HeadersInit) =>
  handleDemoRequest(new Request(`http://localhost/api/v1/tasks/${name}/live?namespace=default`, { headers }))

describe('demo live view scenes', () => {
  const recorded = Object.keys(import.meta.glob('./live-recording/*.jpg')).map((path) => path.replace('./live-recording/', ''))
  const played = (fromMs: number, toMs: number) => {
    const files = new Set<string>()
    for (let t = fromMs; t < toMs; t += 100) files.add(liveScene(t).file)
    return files
  }

  it('plays every recorded frame, and only recorded frames', () => {
    const files = played(0, 45_000)
    expect([...files].sort()).toEqual([...recorded].sort())
    expect(recorded).toHaveLength(29)
  })

  it('plays the frames in their recorded order', () => {
    expect(liveScene(0).file).toBe('frame-00.jpg')
    expect(liveScene(12_600).file).toBe('frame-13.jpg')
    expect(liveScene(25_600).file).toBe('frame-28.jpg')
  })

  it('holds the checkout overview still, so the same picture keeps its key and time', () => {
    const first = liveScene(26_000)
    const last = liveScene(40_000)
    expect(last.key).toBe(first.key)
    expect(last.since).toBe(first.since)
  })

  it('loops the recording', () => {
    const loop = 25_004 + 15_000
    expect(liveScene(loop + 100).file).toBe('frame-00.jpg')
    expect(liveScene(loop + 100).key).not.toBe(liveScene(100).key)
    expect(liveScene(loop + 100).since).toBe(loop)
  })
})

describe('demo live view API', () => {
  it('serves the seeded browser Task a frame, and 304 for the same picture', async () => {
    const response = await live('storefront-checkout-smoke-g5h2w')
    expect(response.status).toBe(200)
    expect(response.headers.get('Content-Type')).toBe('image/jpeg')
    const etag = response.headers.get('ETag')
    expect(etag).toBeTruthy()
    expect(response.headers.get('X-Orka-Frame-Time')).toBeTruthy()

    const again = await live('storefront-checkout-smoke-g5h2w', { 'If-None-Match': etag ?? '' })
    expect([200, 304]).toContain(again.status)
  })

  it('has no live view for other Tasks', async () => {
    expect((await live('terraform-drift-check-u5r1a')).status).toBe(404)
    expect((await live('checkout-latency-investigation-7k2qp')).status).toBe(404)
    expect((await live('no-such-task')).status).toBe(404)
  })

  it('has no live view for a running container Task on another image', async () => {
    vi.useFakeTimers({ toFake: ['Date'], now: Date.now() })
    try {
      const forked = await handleDemoRequest(new Request('http://localhost/api/v1/tasks/db-migrate-orders-v42-e2l9o/fork?namespace=default', {
        method: 'POST', body: '{}', headers: { 'Content-Type': 'application/json' },
      }))
      const { newTaskName } = (await forked.json()) as { newTaskName: string }
      vi.setSystemTime(Date.now() + 10_000)
      const task = (await (await handleDemoRequest(new Request(`http://localhost/api/v1/tasks/${newTaskName}?namespace=default`))).json()) as {
        status?: { phase?: string }
      }
      expect(task.status?.phase).toBe('Running')
      expect((await live(newTaskName)).status).toBe(404)
    } finally {
      vi.useRealTimers()
    }
  })
})
