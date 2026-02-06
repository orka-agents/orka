import { useEffect } from 'react'
import { useNavigate, useLocation } from '@tanstack/react-router'
import { useAuthStore } from '@/stores/auth'

export function useAuthGuard() {
  const token = useAuthStore((s) => s.token)
  const navigate = useNavigate()
  const location = useLocation()

  useEffect(() => {
    if (!token && location.pathname !== '/login') {
      navigate({ to: '/login' })
    }
  }, [token, location.pathname, navigate])

  return { isAuthenticated: !!token }
}
