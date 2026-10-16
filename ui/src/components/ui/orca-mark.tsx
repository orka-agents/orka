import { cn } from '@/lib/utils'

/**
 * Single source of truth for the Orka brand mark.
 *
 * Renders the same `public/favicon.svg` asset used by the browser tab, so the
 * wordmark and favicon can never drift (replaces the old emoji-brand mismatch).
 * The path follows Vite's base, so the static demo build (served from a
 * subpath) finds it too.
 */
export function OrcaMark({
  className,
  ...props
}: React.ComponentProps<'img'>) {
  return (
    <img
      src={`${import.meta.env.BASE_URL}favicon.svg`}
      alt=""
      aria-hidden="true"
      className={cn('shrink-0 select-none', className)}
      {...props}
    />
  )
}
