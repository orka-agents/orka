import { useEffect, useRef } from 'react'
import { Link, useLocation } from '@tanstack/react-router'
import { LayoutDashboard, ListTodo, MessageSquare, Bot, Wrench, Sparkles, Columns3, Activity, Shield, Radar, Boxes, RadioTower, PanelLeftClose, PanelLeftOpen } from 'lucide-react'

import { cn } from '@/lib/utils'
import { useUIStore } from '@/stores/ui'
import { useIsMobile } from '@/hooks/use-media-query'
import { Button } from '@/components/ui/button'
import { OrcaMark } from '@/components/ui/orca-mark'

const navItems = [
  { to: '/', label: 'Dashboard', icon: LayoutDashboard },
  { to: '/chat', label: 'Chat', icon: Sparkles },
  { to: '/monitors', label: 'Monitors', icon: Radar },
  { to: '/security', label: 'Security', icon: Shield },
  { to: '/tasks', label: 'Tasks', icon: ListTodo },
  { to: '/kanban', label: 'Board', icon: Columns3 },
  { to: '/live', label: 'Live', icon: Activity },
  { to: '/gateways', label: 'Gateways', icon: RadioTower },
  { to: '/sessions', label: 'Sessions', icon: MessageSquare },
  { to: '/runtimes', label: 'Runtimes', icon: Boxes },
  { to: '/agents', label: 'Agents', icon: Bot },
  { to: '/tools', label: 'Tools', icon: Wrench },
] as const

export function Sidebar() {
  const location = useLocation()
  const { sidebarCollapsed: desktopCollapsed, toggleSidebar, mobileSidebarOpen, setMobileSidebarOpen } = useUIStore()
  const isMobile = useIsMobile()

  // Below the md breakpoint the expanded sidebar would leave ~135px for content,
  // so it starts as the icon rail on entry; expanding it there overlays the
  // page (see the max-md: classes) instead of squeezing it. The mobile overlay
  // state is separate from (and never written to) the persisted desktop
  // preference, so leaving the breakpoint restores the desktop layout.
  useEffect(() => {
    if (isMobile) setMobileSidebarOpen(false)
  }, [isMobile, setMobileSidebarOpen])

  const sidebarCollapsed = isMobile ? !mobileSidebarOpen : desktopCollapsed
  const overlayOpen = isMobile && mobileSidebarOpen
  const closeOverlay = () => setMobileSidebarOpen(false)
  const handleToggle = () => {
    if (isMobile) setMobileSidebarOpen(!mobileSidebarOpen)
    else toggleSidebar()
  }

  // The mobile overlay is modal: it takes initial focus, keeps Tab inside the
  // drawer, closes on Escape, and hands focus back to whatever opened it. The
  // covered page is made inert by RootLayout while it is open.
  const asideRef = useRef<HTMLElement>(null)
  useEffect(() => {
    if (!overlayOpen) return
    const aside = asideRef.current
    if (!aside) return
    const previouslyFocused = document.activeElement instanceof HTMLElement ? document.activeElement : null
    const focusables = () =>
      Array.from(aside.querySelectorAll<HTMLElement>('a[href], button:not([disabled]), [tabindex]:not([tabindex="-1"])'))
    if (!aside.contains(document.activeElement)) focusables()[0]?.focus()
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === 'Escape') {
        event.preventDefault()
        setMobileSidebarOpen(false)
        return
      }
      if (event.key !== 'Tab') return
      const items = focusables()
      if (items.length === 0) return
      const first = items[0]
      const last = items[items.length - 1]
      if (event.shiftKey && document.activeElement === first) {
        event.preventDefault()
        last.focus()
      } else if (!event.shiftKey && document.activeElement === last) {
        event.preventDefault()
        first.focus()
      }
    }
    aside.addEventListener('keydown', onKeyDown)
    return () => {
      aside.removeEventListener('keydown', onKeyDown)
      if (previouslyFocused && previouslyFocused.isConnected) previouslyFocused.focus()
    }
  }, [overlayOpen, setMobileSidebarOpen])

  return (
    <>
    {overlayOpen && (
      <div
        className="fixed inset-0 z-30 bg-black/40 md:hidden"
        aria-hidden="true"
        data-testid="sidebar-backdrop"
        onClick={closeOverlay}
      />
    )}
    <aside
      ref={asideRef}
      role={overlayOpen ? 'dialog' : undefined}
      aria-modal={overlayOpen ? true : undefined}
      aria-label={overlayOpen ? 'Navigation' : undefined}
      className={cn(
        'flex shrink-0 flex-col border-r border-border bg-card/80 backdrop-blur-md transition-all duration-200',
        sidebarCollapsed ? 'w-16' : 'w-64 max-md:fixed max-md:inset-y-0 max-md:left-0 max-md:z-40 max-md:bg-card max-md:shadow-xl',
      )}
    >
      <div className="flex h-14 items-center border-b border-border px-4">
        {!sidebarCollapsed && (
          <Link to="/" className="flex items-center gap-2 font-semibold text-foreground">
            <OrcaMark className="h-6 w-6" />
            <span>Orka</span>
          </Link>
        )}
        <Button
          variant="ghost"
          size="icon"
          onClick={handleToggle}
          aria-label={sidebarCollapsed ? 'Expand sidebar' : 'Collapse sidebar'}
          className={cn('ml-auto h-8 w-8', sidebarCollapsed && 'mx-auto')}
        >
          {sidebarCollapsed ? (
            <PanelLeftOpen className="h-4 w-4" />
          ) : (
            <PanelLeftClose className="h-4 w-4" />
          )}
        </Button>
      </div>
      <nav className="flex-1 space-y-1 p-2">
        {navItems.map(({ to, label, icon: Icon }) => {
          const isActive = to === '/' ? location.pathname === '/' : location.pathname.startsWith(to)
          return (
            <Link
              key={to}
              to={to}
              aria-current={isActive ? 'page' : undefined}
              // The collapsed rail hides the visible text, so the link needs
              // an explicit accessible name.
              aria-label={sidebarCollapsed ? label : undefined}
              title={sidebarCollapsed ? label : undefined}
              onClick={() => { if (overlayOpen) closeOverlay() }}
              className={cn(
                'relative flex items-center gap-3 rounded-md px-3 py-2 text-sm font-medium transition-colors',
                // Refined active state: left accent bar + subtle tint + colored
                // icon/text, replacing the old heavy solid fill.
                isActive
                  ? 'bg-primary/10 text-primary before:absolute before:inset-y-1.5 before:left-0 before:w-0.5 before:rounded-full before:bg-primary'
                  : 'text-muted-foreground hover:bg-accent hover:text-accent-foreground',
                sidebarCollapsed && 'justify-center px-2'
              )}
            >
              <Icon className={cn('h-4 w-4 shrink-0', isActive && 'text-primary')} />
              {!sidebarCollapsed && <span>{label}</span>}
            </Link>
          )
        })}
      </nav>
    </aside>
    </>
  )
}
