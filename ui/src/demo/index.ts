import { useAuthStore } from '@/stores/auth'
import { useUIStore } from '@/stores/ui'
import { installDemoFetch } from './server'
import { DEMO_NAMESPACE } from './shared'
import { DEMO_TOKEN } from './token'

/**
 * Turns the dashboard into a self-contained demo: API calls are answered in
 * the browser from sample data and the visitor starts signed in with the demo
 * credential, which replaces any other stored token. Loaded only by the `demo`
 * build mode; the header marks the page as a demo.
 */
export function installDemo() {
  installDemoFetch()
  if (useAuthStore.getState().token !== DEMO_TOKEN) useAuthStore.getState().setToken(DEMO_TOKEN)
  useUIStore.getState().setNamespace(DEMO_NAMESPACE)
}
