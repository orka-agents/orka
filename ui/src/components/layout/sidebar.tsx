import { Link, useLocation } from '@tanstack/react-router'
import { LayoutDashboard, ListTodo, MessageSquare, Bot, Wrench, Sparkles, Columns3, Activity, Shield, Radar, PanelLeftClose, PanelLeftOpen, RadioTower } from 'lucide-react'
import { cn } from '@/lib/utils'
import { useUIStore } from '@/stores/ui'
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
  { to: '/agents', label: 'Agents', icon: Bot },
  { to: '/tools', label: 'Tools', icon: Wrench },
] as const

export function Sidebar() {
  const location = useLocation()
  const { sidebarCollapsed, toggleSidebar } = useUIStore()

  return (
    <aside className={cn(
      'flex flex-col border-r border-border bg-card/80 backdrop-blur-md transition-all duration-200',
      sidebarCollapsed ? 'w-16' : 'w-64'
    )}>
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
          onClick={toggleSidebar}
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
  )
}
