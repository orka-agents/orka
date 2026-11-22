import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import '@fontsource-variable/geist'
import '@fontsource-variable/geist-mono'
import './index.css'
import { App } from './app'

async function start() {
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
