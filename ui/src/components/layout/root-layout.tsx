import { Outlet } from '@tanstack/react-router'
import { Sidebar } from './sidebar'
import { Header } from './header'
import { Toaster } from '@/components/ui/sonner'

export function RootLayout() {
  return (
    <div className="flex h-screen overflow-hidden">
      <Sidebar />
      <div className="flex flex-1 flex-col overflow-hidden">
        <Header />
        <main className="flex-1 overflow-auto p-6">
          <Outlet />
        </main>
      </div>
      <Toaster />
    </div>
  )
}
