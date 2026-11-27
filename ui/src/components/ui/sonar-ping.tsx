import { cn } from '@/lib/utils'

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
