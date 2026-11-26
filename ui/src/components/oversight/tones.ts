import type { SignalPresentation } from '@/lib/oversight'

export const TONE_BADGE: Record<SignalPresentation['tone'], string> = {
  muted: 'border-border bg-transparent text-muted-foreground',
  amber: 'border-status-pending/40 bg-status-pending-bg text-status-pending',
  red: 'border-status-failed/40 bg-status-failed-bg text-status-failed',
  'red-outline':
    'border-dashed border-status-failed bg-[repeating-linear-gradient(135deg,transparent_0_5px,var(--color-status-failed-bg)_5px_10px)] text-status-failed',
}

/** Row accents: grey rows stay quiet, the rest carry a coloured edge. */
export const TONE_ROW: Record<SignalPresentation['tone'], string> = {
  muted: 'text-muted-foreground',
  amber: 'border-l-2 border-l-status-pending bg-status-pending-bg/40',
  red: 'border-l-2 border-l-status-failed bg-status-failed-bg/40',
  'red-outline':
    'border-l-2 border-dashed border-l-status-failed bg-[repeating-linear-gradient(135deg,transparent_0_8px,var(--color-status-failed-bg)_8px_16px)]',
}
