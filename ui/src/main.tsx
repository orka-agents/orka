import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
// Offline-safe fonts — bundled by Vite from @fontsource (no runtime CDN fetch).
import '@fontsource-variable/geist'
import '@fontsource-variable/geist-mono'
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
