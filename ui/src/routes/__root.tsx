import { createRootRoute, Outlet, useNavigate, useLocation } from '@tanstack/react-router'
import { useEffect } from 'react'
import { RootLayout } from '@/components/layout/root-layout'
import { useAuthStore } from '@/stores/auth'
import { useUIStore } from '@/stores/ui'
import { rememberLoginReturn } from '@/lib/login-return'

function RootComponent() {
  const location = useLocation()
  const token = useAuthStore((s) => s.token)
  const theme = useUIStore((s) => s.theme)
  const navigate = useNavigate()

  useEffect(() => {
    document.documentElement.classList.toggle('dark', theme === 'dark')
  }, [theme])

  useEffect(() => {
    if (!token && location.pathname !== '/login') {
      // Come back here after signing in, and keep the fragment in the address
      // bar: a connector consent callback carries its completion token there.
      rememberLoginReturn(location.pathname + location.searchStr)
      const hash = window.location.hash.slice(1)
      navigate({ to: '/login', ...(hash && { hash }) })
    }
  }, [token, location.pathname, location.searchStr, navigate])

  if (location.pathname === '/login') {
    return <Outlet />
  }

  if (!token) {
    return null
  }

  return <RootLayout />
}

export const Route = createRootRoute({
  component: RootComponent,
})
