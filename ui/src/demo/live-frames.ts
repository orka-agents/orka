// Live view frames for the demo's browser Task: a recording of a real one.
// A browser Task ran CHECKOUT_SMOKE_SCRIPT's Playwright checkout against
// saucedemo.com, Sauce Labs' public demo store, on a kind cluster with
// --browser-live-view. These are the frames GET /api/v1/tasks/{id}/live
// returned while it ran, polled as the dashboard polls, at the controller's
// frame times. Back-to-back repeats are dropped and each picture is stored
// once. The demo loops the recording, holding the checkout overview still as
// the run did, so the dashboard gets 304 there.

/** The image the demo's browser Tasks run, so the live view applies to them. */
export const DEMO_BROWSER_IMAGE = 'ghcr.io/orka-agents/orka/browser-worker:0.4.0'

const FILES = import.meta.glob<string>('./live-recording/*.jpg', { eager: true, query: '?url', import: 'default' })

/** Each frame and when it arrived, in milliseconds from the first. */
const RECORDING: readonly (readonly [file: string, at: number])[] = [
  ['frame-00.jpg', 0], ['frame-01.jpg', 1500], ['frame-02.jpg', 2003], ['frame-03.jpg', 2500],
  ['frame-04.jpg', 3002], ['frame-05.jpg', 3514], ['frame-06.jpg', 4006], ['frame-07.jpg', 4502],
  ['frame-08.jpg', 5001], ['frame-09.jpg', 5502], ['frame-10.jpg', 7009], ['frame-11.jpg', 9003],
  ['frame-12.jpg', 9506], ['frame-13.jpg', 11501], ['frame-14.jpg', 13008], ['frame-15.jpg', 13502],
  ['frame-16.jpg', 14501], ['frame-17.jpg', 16004], ['frame-18.jpg', 18501], ['frame-19.jpg', 19499],
  ['frame-20.jpg', 20001], ['frame-21.jpg', 20504], ['frame-22.jpg', 21014], ['frame-23.jpg', 21501],
  ['frame-24.jpg', 22000], ['frame-25.jpg', 22501], ['frame-26.jpg', 23000], ['frame-27.jpg', 23501],
  ['frame-26.jpg', 24000], ['frame-28.jpg', 25004],
]

/** The run stood on the checkout overview this long before it closed the browser. */
const REVIEW_STILL_MS = 15_000
const LOOP_MS = RECORDING[RECORDING.length - 1][1] + REVIEW_STILL_MS

export interface LiveScene {
  /** The recorded frame to show. */
  file: string
  /** Names the picture: equal keys are the same frame. */
  key: string
  /** Milliseconds from the Task's start to when this picture first appeared. */
  since: number
}

/** What the demo browser shows elapsedMs after its Task started. */
export function liveScene(elapsedMs: number): LiveScene {
  const elapsed = Math.max(0, elapsedMs)
  const loop = Math.floor(elapsed / LOOP_MS)
  const t = elapsed - loop * LOOP_MS
  let index = 0
  while (index + 1 < RECORDING.length && RECORDING[index + 1][1] <= t) index++
  const [file, at] = RECORDING[index]
  return { file, key: `${loop}-${index}`, since: loop * LOOP_MS + at }
}

// Fetched frames, so repeated polls of the same picture are not fetched again.
const cache = new Map<string, Promise<Blob | null>>()

/** The scene's recorded JPEG, or null when it could not be loaded. */
export function liveFrame(scene: LiveScene): Promise<Blob | null> {
  const hit = cache.get(scene.file)
  if (hit) return hit
  const url = FILES[`./live-recording/${scene.file}`]
  const frame = url
    ? fetch(url).then((r) => (r.ok ? r.blob() : null)).catch(() => null)
    : Promise.resolve(null)
  // A failed load is tried again on the next poll.
  void frame.then((blob) => { if (!blob) cache.delete(scene.file) })
  cache.set(scene.file, frame)
  return frame
}
