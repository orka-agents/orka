import { useUIStore } from '@/stores/ui'
import { useMediaQuery } from './use-media-query'

const PREFERS_DARK = '(prefers-color-scheme: dark)'

export function useResolvedTheme(): 'light' | 'dark' {
  const theme = useUIStore((s) => s.theme)
  const systemDark = useMediaQuery(PREFERS_DARK)
  if (theme === 'system') return systemDark ? 'dark' : 'light'
  return theme
}
