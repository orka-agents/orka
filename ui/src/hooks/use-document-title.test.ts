import { describe, it, expect, beforeEach } from 'vitest'
import { renderHook } from '@testing-library/react'
import { useDocumentTitle } from './use-document-title'

describe('useDocumentTitle', () => {
  beforeEach(() => {
    document.title = 'Orka'
  })

  it('names the page in the tab and follows title changes', () => {
    const { rerender } = renderHook(({ title }) => useDocumentTitle(title), { initialProps: { title: 'Tasks' } })
    expect(document.title).toBe('Tasks · Orka')
    rerender({ title: 'task-123' })
    expect(document.title).toBe('task-123 · Orka')
  })

  it('restores the app title when the page goes away', () => {
    const { unmount } = renderHook(() => useDocumentTitle('Security'))
    unmount()
    expect(document.title).toBe('Orka')
  })

  it('falls back to the app title for an empty title', () => {
    renderHook(() => useDocumentTitle(''))
    expect(document.title).toBe('Orka')
  })
})
