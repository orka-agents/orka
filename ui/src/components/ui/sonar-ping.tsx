import { cn } from '@/lib/utils'

/**
 * Decorative "sonar" idle illustration — concentric rings pinging outward from
 * a center dot, evoking listening for activity. Used in idle/empty states (e.g.
 * the Live view when nothing is running). Purely ornamental (aria-hidden);
 * motion is gated behind motion-safe.
 */
export function SonarPing({ className }: { className?: string }) {
  return (
    <div
      className={cn('relative grid size-20 place-items-center', className)}
      aria-hidden="true"
    >
      <span className="absolute size-20 rounded-full border border-live/20 motion-safe:animate-ping" />
      <span className="absolute size-14 rounded-full border border-live/30 motion-safe:animate-pulse-live" />
      <span className="absolute size-8 rounded-full bg-live/10" />
      <span className="relative size-2.5 rounded-full bg-live motion-safe:animate-pulse-live" />
    </div>
  )
}
