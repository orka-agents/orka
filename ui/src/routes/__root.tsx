import { createRootRoute, Outlet, useNavigate, useLocation } from '@tanstack/react-router'
import { useEffect } from 'react'
import { RootLayout } from '@/components/layout/root-layout'
import { useAuthStore } from '@/stores/auth'
import { useResolvedTheme } from '@/hooks/use-resolved-theme'
import { hasPendingHashToken, rememberLoginReturn } from '@/lib/login-return'

function RootComponent() {
  const location = useLocation()
  const token = useAuthStore((s) => s.token)
  const theme = useResolvedTheme()
  const navigate = useNavigate()

  useEffect(() => {
    document.documentElement.classList.toggle('dark', theme === 'dark')
  }, [theme])

  const signingIn = !token || hasPendingHashToken()

  useEffect(() => {
    if (signingIn && location.pathname !== '/login') {
      rememberLoginReturn(location.pathname + location.searchStr)
      const hash = location.hash
      navigate({ to: '/login', ...(hash && { hash }) })
    }
  }, [signingIn, location.pathname, location.searchStr, location.hash, navigate])

  if (location.pathname === '/login') {
    return <Outlet />
  }

  if (signingIn) {
    return null
  }

  return <RootLayout />
}

export const Route = createRootRoute({
  component: RootComponent,
})
