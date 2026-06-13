import type { LucideIcon } from 'lucide-react'
import { Container, Sparkles, Bot } from 'lucide-react'
import type { TaskPhase, TaskType } from '@/schemas/task'

/**
 * Single source of truth for status & type visual encoding.
 *
 * Color carries exactly three meanings in Orka's UI — phase, task type, and
 * liveness — and they all originate here, flowing from the Phase-0 design
 * tokens (status-* / type-*), never from ad-hoc `*-100` pastels.
 *
 * This is a plain `.ts` module (no JSX) on purpose: it exports style constants
 * and would otherwise trip the `react-refresh/only-export-components` lint rule
 * if it lived alongside a component.
 *
 * Class strings are written as complete literals so Tailwind's content scanner
 * can see them — never compose them dynamically (e.g. `bg-status-${phase}`).
 */

export interface PhaseStyle {
  /** Human-readable phase label. */
  label: string
  /** Background utility for the filled status dot. */
  dotClass: string
  /** Border-color utility for a left-rail accent (`border-l-*`). */
  railClass: string
  /** Foreground utility for phase text. */
  textClass: string
  /** Translucent tint for soft fills (chips, column headers). */
  bgClass: string
  /** Whether this phase represents an in-flight, "live" state. */
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
    // A queued/future run — waiting like Pending, so it shares the pending
    // (amber) family rather than the live cyan.
    label: 'Scheduled',
    dotClass: 'bg-status-pending',
    railClass: 'border-status-pending',
    textClass: 'text-status-pending',
    bgClass: 'bg-status-pending-bg',
    live: false,
  },
  Cancelled: {
    // A terminal, user-stopped state — neutral/muted, deliberately off the
    // success/failure hues.
    label: 'Cancelled',
    dotClass: 'bg-muted-foreground',
    railClass: 'border-muted-foreground',
    textClass: 'text-muted-foreground',
    bgClass: 'bg-muted',
    live: false,
  },
}

/** Resolve the visual style for a task phase, defaulting to Pending. */
export function phaseStyle(phase?: TaskPhase | string): PhaseStyle {
  return PHASE_STYLES[phase as TaskPhase] ?? PHASE_STYLES.Pending
}

export interface TypeStyle {
  /** Human-readable type label. */
  label: string
  /** Lucide icon representing the task type. */
  icon: LucideIcon
  /** Foreground utility for the type icon/text. */
  textClass: string
  /** Soft tint utility for a type chip background. */
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

/** Resolve the visual style for a task type, defaulting to container. */
export function typeStyle(type?: TaskType | string): TypeStyle {
  return TYPE_STYLES[type as TaskType] ?? TYPE_STYLES.container
}
