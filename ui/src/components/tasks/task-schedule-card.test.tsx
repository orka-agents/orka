import { describe, it, expect } from 'vitest'
import { render, screen } from '@/test/test-utils'

import { TaskScheduleCard } from './task-schedule-card'
import type { Task } from '@/schemas/task'

describe('TaskScheduleCard', () => {
  it('renders nothing for unscheduled tasks', () => {
    const task = { metadata: { name: 't' }, spec: { type: 'container' } } as Task
    const { container } = render(<TaskScheduleCard task={task} />)
    expect(container).toBeEmptyDOMElement()
  })

  it('shows cron facts and suspension', () => {
    const task = {
      metadata: { name: 't' },
      spec: {
        type: 'ai',
        schedule: '0 9 * * 1-5',
        timeZone: 'UTC',
        concurrencyPolicy: 'Forbid',
        suspend: true,
        successfulRunsHistoryLimit: 3,
      },
      status: {
        lastScheduleTime: '2026-08-11T09:00:00Z',
        nextScheduleTime: '2026-08-12T09:00:00Z',
      },
    } as Task
    render(<TaskScheduleCard task={task} />)
    expect(screen.getByText('Schedule')).toBeInTheDocument()
    expect(screen.getByText('0 9 * * 1-5')).toBeInTheDocument()
    expect(screen.getByText('Suspended')).toBeInTheDocument()
    expect(screen.getByText('3')).toBeInTheDocument()
    expect(screen.getByText(/Last run/)).toBeInTheDocument()
    expect(screen.getByText(/Next run/)).toBeInTheDocument()
  })
})
