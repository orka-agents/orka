import { useEffect } from 'react'
import { api } from '@/lib/api-client'
import { useAuthStore } from '@/stores/auth'
import { useUIStore } from '@/stores/ui'

interface WhoAmI {
  namespace?: string
}

export function useCallerNamespace() {
  const token = useAuthStore((s) => s.token)

  useEffect(() => {
    if (!token) return
    let current = true
    api.get<WhoAmI>('/auth/whoami')
      .then((identity) => {
        if (current && useAuthStore.getState().token === token) {
          useUIStore.getState().adoptCallerNamespace(identity?.namespace)
        }
      })
      .catch(() => {})
    return () => {
      current = false
    }
  }, [token])
}
