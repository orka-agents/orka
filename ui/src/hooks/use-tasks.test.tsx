import { renderHook, waitFor, act } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import type { ReactNode } from 'react'

vi.mock('zustand/middleware', () => ({
  persist: (fn: unknown) => fn,
}))

import { useUIStore } from '@/stores/ui'
import {
  useTaskList,
  useTask,
  useTaskResult,
  useCreateTask,
  useDeleteTask,
} from './use-tasks'

function createWrapper() {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, gcTime: 0 }, mutations: { retry: false } },
  })
  return ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
  )
}

beforeEach(() => {
  useUIStore.setState({ namespace: 'default', sidebarCollapsed: false, theme: 'light' })
})

describe('useTaskList', () => {
  it('returns task list from API', async () => {
    const { result } = renderHook(() => useTaskList(), { wrapper: createWrapper() })
    await waitFor(() => expect(result.current.isSuccess).toBe(true))
    expect(result.current.data).toEqual({ items: [], metadata: {} })
  })
})

describe('useTask', () => {
  it('returns a single task by id', async () => {
    const { result } = renderHook(() => useTask('my-task'), { wrapper: createWrapper() })
    await waitFor(() => expect(result.current.isSuccess).toBe(true))
    expect(result.current.data).toMatchObject({
      metadata: { name: 'my-task', namespace: 'default' },
      status: { phase: 'Succeeded' },
    })
  })
})

describe('useTaskResult', () => {
  it('starts disabled and returns result on refetch', async () => {
    const { result } = renderHook(() => useTaskResult('my-task'), { wrapper: createWrapper() })
    // Should not fetch automatically
    expect(result.current.isFetching).toBe(false)
    expect(result.current.data).toBeUndefined()

    // Trigger manual refetch
    await act(async () => {
      await result.current.refetch()
    })
    await waitFor(() => expect(result.current.isSuccess).toBe(true))
    expect(result.current.data).toEqual({ result: 'task output' })
  })
})

describe('useCreateTask', () => {
  it('creates a task via mutation', async () => {
    const { result } = renderHook(() => useCreateTask(), { wrapper: createWrapper() })
    act(() => {
      result.current.mutate({ type: 'container', image: 'alpine' })
    })
    await waitFor(() => expect(result.current.isSuccess).toBe(true))
    expect(result.current.data).toMatchObject({ metadata: { name: 'new-task' } })
  })
})

describe('useDeleteTask', () => {
  it('deletes a task via mutation', async () => {
    const { result } = renderHook(() => useDeleteTask(), { wrapper: createWrapper() })
    act(() => {
      result.current.mutate('task-to-delete')
    })
    await waitFor(() => expect(result.current.isSuccess).toBe(true))
  })
})
