import { cn } from '@/lib/utils'

/**
 * Single source of truth for the Orka brand mark in the app.
 *
 * The mark is the orca's head cropped from the existing Orka logo (no new art).
 * Light and dark variants follow the app theme: the dark one carries a light
 * outline so the black orca reads on a near-black background. Paths follow
 * Vite's base, so the static demo build (served from a subpath) finds them too.
 */
export function OrcaMark({
  className,
  ...props
}: React.ComponentProps<'img'>) {
  const base = import.meta.env.BASE_URL
  return (
    <>
      <img
        src={`${base}orka-mark.png`}
        alt=""
        aria-hidden="true"
        className={cn('shrink-0 select-none dark:hidden', className)}
        {...props}
      />
      <img
        src={`${base}orka-mark-dark.png`}
        alt=""
        aria-hidden="true"
        className={cn('hidden shrink-0 select-none dark:block', className)}
        {...props}
      />
    </>
  )
}
