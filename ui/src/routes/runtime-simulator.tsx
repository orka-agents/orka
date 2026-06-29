import { createFileRoute } from '@tanstack/react-router'
import { RuntimeSimulator } from '@/components/runtime/runtime-simulator'

// DEV-only demo route. In a production build (import.meta.env.DEV === false) this
// renders nothing operative, so the simulator can never reach real data.
export const Route = createFileRoute('/runtime-simulator')({
  component: import.meta.env.DEV
    ? RuntimeSimulator
    : () => <div className="text-muted-foreground">Not available.</div>,
})
