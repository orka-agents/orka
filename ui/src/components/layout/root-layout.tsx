import { Outlet } from '@tanstack/react-router'
import { Sidebar } from './sidebar'
import { Header } from './header'
import { Toaster } from '@/components/ui/sonner'
import { useIsMobile } from '@/hooks/use-media-query'
import { useUIStore } from '@/stores/ui'

export function RootLayout() {
  const isMobile = useIsMobile()
  const mobileSidebarOpen = useUIStore((s) => s.mobileSidebarOpen)
  // While the mobile navigation drawer overlays the page, the covered content
  // is inert so keyboard and assistive-technology focus cannot reach it.
  const pageInert = isMobile && mobileSidebarOpen
  return (
    <div className="flex h-screen overflow-hidden">
      <Sidebar />
      <div className="flex min-w-0 flex-1 flex-col overflow-hidden" inert={pageInert} data-testid="page-content">
        <Header />
        <main className="min-w-0 flex-1 overflow-y-auto overflow-x-hidden p-4 md:p-6">
          <Outlet />
        </main>
      </div>
      <Toaster />
    </div>
  )
}
