import { useUIStore } from '@/stores/ui'
import { useMediaQuery } from './use-media-query'

const PREFERS_DARK = '(prefers-color-scheme: dark)'

/** The theme on screen: the user's pick, or the system's while they pick System. */
export function useResolvedTheme(): 'light' | 'dark' {
  const theme = useUIStore((s) => s.theme)
  const systemDark = useMediaQuery(PREFERS_DARK)
  if (theme === 'system') return systemDark ? 'dark' : 'light'
  return theme
}
