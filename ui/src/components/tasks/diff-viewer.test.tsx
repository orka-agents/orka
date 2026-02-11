import { describe, it, expect, vi } from 'vitest'
import { render, screen } from '@/test/test-utils'

vi.mock('zustand/middleware', () => ({
  persist: (fn: unknown) => fn,
}))

vi.mock('@tanstack/react-router', async () => {
  const actual = await vi.importActual('@tanstack/react-router')
  return { ...actual, Link: ({ children, to, ...props }: any) => <a href={to} {...props}>{children}</a>, useNavigate: () => vi.fn(), useLocation: () => ({ pathname: '/tasks' }) }
})

import { DiffViewer } from './diff-viewer'

const sampleDiff = `diff --git a/file.go b/file.go
--- a/file.go
+++ b/file.go
@@ -1,3 +1,4 @@
 package main
-import "fmt"
+import "log"
+import "os"
 func main() {}`

describe('DiffViewer', () => {
  it('renders a unified diff with correct line styles', () => {
    render(<DiffViewer diff={sampleDiff} />)
    const viewer = screen.getByTestId('diff-viewer')
    expect(viewer).toBeInTheDocument()
    // Check that addition lines have green background
    const additionLines = viewer.querySelectorAll('.bg-green-100')
    expect(additionLines.length).toBe(2)
    // Check that deletion lines have red background
    const deletionLines = viewer.querySelectorAll('.bg-red-100')
    expect(deletionLines.length).toBe(1)
  })

  it('renders file headers', () => {
    render(<DiffViewer diff={sampleDiff} />)
    expect(screen.getByText('--- a/file.go')).toBeInTheDocument()
    expect(screen.getByText('+++ b/file.go')).toBeInTheDocument()
  })

  it('renders hunk headers with blue styling', () => {
    render(<DiffViewer diff={sampleDiff} />)
    const viewer = screen.getByTestId('diff-viewer')
    const hunkHeaders = viewer.querySelectorAll('.bg-blue-50')
    expect(hunkHeaders.length).toBe(1)
  })

  it('renders empty diff message', () => {
    render(<DiffViewer diff="" />)
    expect(screen.getByText('No diff available')).toBeInTheDocument()
  })
})
