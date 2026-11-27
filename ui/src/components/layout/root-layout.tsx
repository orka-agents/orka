import { lazy, Suspense } from 'react'
import { Outlet, useLocation } from '@tanstack/react-router'
import { Sidebar } from './sidebar'
import { Header } from './header'
import { Toaster } from '@/components/ui/sonner'
import { useIsMobile } from '@/hooks/use-media-query'
import { useUIStore } from '@/stores/ui'

const DemoTour = import.meta.env.MODE === 'demo'
  ? lazy(() => import('@/demo/tour/tour').then((m) => ({ default: m.TourPanel })))
  : null

export function RootLayout() {
  const isMobile = useIsMobile()
  const mobileSidebarOpen = useUIStore((s) => s.mobileSidebarOpen)
  const pageInert = isMobile && mobileSidebarOpen
  const section = useLocation().pathname.split('/')[1] ?? ''
  return (
    <div className="flex h-screen overflow-hidden">
      <Sidebar />
      <div className="flex min-w-0 flex-1 flex-col overflow-hidden" inert={pageInert} data-testid="page-content">
        <Header />
        <main className="relative min-w-0 flex-1 overflow-y-auto overflow-x-hidden p-4 md:p-6">
          <div key={section} className="h-full animate-page-enter">
            <Outlet />
          </div>
        </main>
        {DemoTour && <Suspense fallback={null}><DemoTour slot="bottom" /></Suspense>}
      </div>
      {DemoTour && <Suspense fallback={null}><DemoTour slot="rail" /></Suspense>}
      <Toaster />
    </div>
  )
}
