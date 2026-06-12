import { useEffect, useState } from 'react'

/**
 * Returns a transient "just-updated" flag for a tracked value.
 *
 * The flag flips to `true` the moment `value` changes (compared via
 * `Object.is`), then decays back to `false` after `durationMs`. It's meant to
 * briefly draw the eye to a row whose data just changed in a busy polling view
 * (lists refetch every 5–10s), then get out of the way.
 *
 * The very first render does not count as a change, so freshly-mounted rows
 * don't all glow at once.
 *
 * Consumers should gate any motion/animation they attach to this behind
 * `motion-safe:` so reduced-motion users still get the (non-essential) signal
 * without movement.
 */
export function useFreshness(value: unknown, durationMs = 1200): boolean {
  // Track the previous value in state (React's documented "adjust state during
  // render" pattern) — no refs, no synchronous setState inside an effect, so it
  // stays clean under the React Compiler lint rules. `changeId` increments on
  // every real change and is what the decay effect keys off.
  const [prev, setPrev] = useState(value)
  const [changeId, setChangeId] = useState(0)
  const [fresh, setFresh] = useState(false)

  if (!Object.is(prev, value)) {
    setPrev(value)
    setChangeId((n) => n + 1)
    setFresh(true)
  }

  useEffect(() => {
    if (changeId === 0) return
    const id = setTimeout(() => setFresh(false), durationMs)
    return () => clearTimeout(id)
  }, [changeId, durationMs])

  return fresh
}
