import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
// Offline-safe fonts — bundled by Vite from @fontsource (no runtime CDN fetch).
import '@fontsource/inter/400.css'
import '@fontsource/inter/500.css'
import '@fontsource/inter/600.css'
import '@fontsource/inter/700.css'
import '@fontsource/jetbrains-mono/400.css'
import '@fontsource/jetbrains-mono/500.css'
import '@fontsource/jetbrains-mono/600.css'
import './index.css'
import { App } from './app'

async function start() {
  // The demo build answers API calls in the browser from sample data. The
  // mode check is static, so other builds drop the demo code entirely.
  if (import.meta.env.MODE === 'demo') {
    const { installDemo } = await import('./demo')
    installDemo()
  }
  createRoot(document.getElementById('root')!).render(
    <StrictMode>
      <App />
    </StrictMode>,
  )
}

void start()
