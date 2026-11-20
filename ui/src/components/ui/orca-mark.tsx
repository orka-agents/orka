import { useId } from 'react'

import { cn } from '@/lib/utils'

// Geometry of the Orka mark, an orca fin in a ring above the waves, on a 64x64 grid. It is the
// mark the website and Orka Bridge use (see "Logo files" in website/README.md); change it there
// first and copy it here.
const FIN =
  'M2 37.5C8 32.5 14 30 20.5 28.2C25.5 26.8 27.5 23 30 18.5C33.5 12.5 37.5 9 41.6 8.1C43.2 7.8 43.9 9.2 43.1 10.4C40.8 13.8 39.4 17.5 39.3 22.5C39.3 27.5 40.5 30.6 43.6 32.2C48.5 34.5 53.5 37 58 42C52 40.2 47 40.2 41.5 42.4C37 44.3 33.5 45.6 30.5 45.6C25 45.6 21.5 42.4 16 40.9C12 39.9 7 40.2 2 40.8Z'

const VARIANTS = {
  full: {
    ringRadius: 28.75,
    ringWidth: 2.5,
    clipRadius: 27.6,
    cut: 'M12.3 36.9C13.5 34.6 18 33.9 21.5 34.3C26 34.8 30 37.2 31.2 39.8C31.9 41.5 30.6 43 28 42.9C25 42.8 22.5 41.5 20 40C17.5 38.6 14.5 38.6 12.3 36.9ZM2 46.8C9 46.2 14 46.4 19 48C25 50 30 52.3 35 52.9C37.5 53.2 39 53.1 40.5 53.1C37 53.9 33 54.1 29.5 53.5C25 52.8 21 50.6 17 49.3C12 47.8 7 48.4 2 49.2Z',
    waves:
      'M0 42.6C7 41.6 12 41.6 16 42.6C21 43.9 25 47.2 30 48.7C34 49.8 38 49.7 42 48.9C46 48.2 49 48.4 52 50.2L64 57V64H0ZM32.3 47.3C36 46.8 39 44.8 42.5 43.4C46 42.2 50 42.2 53 43.2L60 46V49C57 48.2 54 47.4 51 47C47.5 46.5 44 47.2 40.5 47.6C37.5 47.9 34.5 48 32.3 47.3Z',
  },
  // 32 px and smaller: a heavier ring and two waves, which stay sharp at small sizes.
  small: {
    ringRadius: 28,
    ringWidth: 4,
    clipRadius: 26.5,
    cut: 'M11.5 37C13 34 18 33 22 33.6C26.5 34.2 30.5 36.8 31.8 39.6C32.6 41.6 31 43.4 28 43.3C25 43.2 22 41.8 19.5 40.3C17 38.9 14 39 11.5 37Z',
    waves:
      'M0 44.5C7 43.5 12 43.5 16 44.5C21 45.8 25 49 30 50.5C34 51.6 38 51.5 42 50.7C46 50 49 50.2 52 52L64 58V64H0ZM30.5 49C35 48.6 39 46.6 42.5 45.2C46 44 50 44 53 45L60 47.5V51C57 50 54 49.2 51 48.8C47.5 48.3 44 49 40.5 49.4C37 49.7 33.5 49.8 30.5 49Z',
  },
} as const

/**
 * Single source of truth for the Orka mark in the app.
 *
 * The fin and ring are ink and the waves are the secondary text grey, never a status colour,
 * because colour marks a task's state in the dashboard. Both follow the app theme. Use `small`
 * at 32 px and smaller.
 */
export function OrcaMark({
  small = false,
  className,
  ...props
}: React.ComponentProps<'svg'> & { small?: boolean }) {
  const v = VARIANTS[small ? 'small' : 'full']
  // Several marks can share a page, so the clip and mask ids must be unique per instance.
  const id = `orka-mark-${useId().replace(/[^a-zA-Z0-9_-]/g, '')}`
  return (
    <svg
      viewBox="0 0 64 64"
      aria-hidden="true"
      focusable="false"
      className={cn('shrink-0 select-none', className)}
      {...props}
    >
      <defs>
        <clipPath id={`${id}-inner`}>
          <circle cx="32" cy="32" r={v.clipRadius} />
        </clipPath>
        <mask id={`${id}-cut`} maskUnits="userSpaceOnUse" x="0" y="0" width="64" height="64">
          <rect width="64" height="64" fill="#fff" />
          <path d={v.cut} fill="#000" />
        </mask>
      </defs>
      <circle
        cx="32"
        cy="32"
        r={v.ringRadius}
        fill="none"
        strokeWidth={v.ringWidth}
        className="stroke-[#0b0b0c] dark:stroke-[#f4f4f5]"
      />
      <g clipPath={`url(#${id}-inner)`} mask={`url(#${id}-cut)`}>
        <path d={FIN} className="fill-[#0b0b0c] dark:fill-[#f4f4f5]" />
        <path d={v.waves} className="fill-[#4b4c52] dark:fill-[#a1a1a9]" />
      </g>
    </svg>
  )
}
