import { useAuthStore } from '@/stores/auth'
import { useUIStore } from '@/stores/ui'
import { installDemoFetch } from './server'
import { DEMO_NAMESPACE } from './shared'
import { DEMO_TOKEN } from './token'

export function installDemo() {
  installDemoFetch()
  if (useAuthStore.getState().token !== DEMO_TOKEN) useAuthStore.getState().setToken(DEMO_TOKEN)
  useUIStore.getState().setNamespace(DEMO_NAMESPACE)
}
