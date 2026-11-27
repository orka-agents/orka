import { useEffect, useState } from 'react'

export function useFreshness(value: unknown, durationMs = 1200): boolean {
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
