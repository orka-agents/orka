import type { LucideIcon } from 'lucide-react'
import { Container, Sparkles, Bot } from 'lucide-react'
import type { TaskPhase, TaskType } from '@/schemas/task'

export interface PhaseStyle {
  label: string
  dotClass: string
  railClass: string
  textClass: string
  bgClass: string
  live: boolean
}

const PHASE_STYLES: Record<TaskPhase, PhaseStyle> = {
  Pending: {
    label: 'Pending',
    dotClass: 'bg-status-pending',
    railClass: 'border-status-pending',
    textClass: 'text-status-pending',
    bgClass: 'bg-status-pending-bg',
    live: false,
  },
  Running: {
    label: 'Running',
    dotClass: 'bg-status-running',
    railClass: 'border-status-running',
    textClass: 'text-status-running',
    bgClass: 'bg-status-running-bg',
    live: true,
  },
  Finalizing: {
    label: 'Finalizing',
    dotClass: 'bg-status-finalizing',
    railClass: 'border-status-finalizing',
    textClass: 'text-status-finalizing',
    bgClass: 'bg-status-finalizing-bg',
    live: false,
  },
  Succeeded: {
    label: 'Succeeded',
    dotClass: 'bg-status-succeeded',
    railClass: 'border-status-succeeded',
    textClass: 'text-status-succeeded',
    bgClass: 'bg-status-succeeded-bg',
    live: false,
  },
  Failed: {
    label: 'Failed',
    dotClass: 'bg-status-failed',
    railClass: 'border-status-failed',
    textClass: 'text-status-failed',
    bgClass: 'bg-status-failed-bg',
    live: false,
  },
  Scheduled: {
    label: 'Scheduled',
    dotClass: 'bg-status-pending',
    railClass: 'border-status-pending',
    textClass: 'text-status-pending',
    bgClass: 'bg-status-pending-bg',
    live: false,
  },
  Cancelled: {
    label: 'Cancelled',
    dotClass: 'bg-muted-foreground',
    railClass: 'border-muted-foreground',
    textClass: 'text-muted-foreground',
    bgClass: 'bg-muted',
    live: false,
  },
}

export function phaseStyle(phase?: TaskPhase | string): PhaseStyle {
  return PHASE_STYLES[phase as TaskPhase] ?? PHASE_STYLES.Pending
}

export interface TypeStyle {
  label: string
  icon: LucideIcon
  textClass: string
  tintClass: string
}

const TYPE_STYLES: Record<TaskType, TypeStyle> = {
  container: {
    label: 'container',
    icon: Container,
    textClass: 'text-type-container',
    tintClass: 'bg-type-container/10 text-type-container',
  },
  ai: {
    label: 'ai',
    icon: Sparkles,
    textClass: 'text-type-ai',
    tintClass: 'bg-type-ai/10 text-type-ai',
  },
  agent: {
    label: 'agent',
    icon: Bot,
    textClass: 'text-type-agent',
    tintClass: 'bg-type-agent/10 text-type-agent',
  },
}

export function typeStyle(type?: TaskType | string): TypeStyle {
  return TYPE_STYLES[type as TaskType] ?? TYPE_STYLES.container
}

export function taskTypeLabel(type?: string): string {
  if (!type) return '-'
  if (type === 'ai') return 'AI'
  return type.charAt(0).toUpperCase() + type.slice(1)
}
