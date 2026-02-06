import { createRootRoute, Outlet, useNavigate, useLocation } from '@tanstack/react-router'
import { useEffect } from 'react'
import { RootLayout } from '@/components/layout/root-layout'
import { useAuthStore } from '@/stores/auth'
import { useUIStore } from '@/stores/ui'

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
      navigate({ to: '/login' })
    }
  }, [token, location.pathname, navigate])

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
