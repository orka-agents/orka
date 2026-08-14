import { Link, useLocation } from '@tanstack/react-router'
import {
  Activity, Boxes, Bot, Brain, Cable, Columns3, LayoutDashboard, ListTodo,
  MessageSquare, PanelLeftClose, PanelLeftOpen, Radar, RadioTower, Settings2,
  Shield, Sparkles, SquareLibrary, Wrench,
} from 'lucide-react'

import { cn } from '@/lib/utils'
import { useUIStore } from '@/stores/ui'
import { Button } from '@/components/ui/button'
import { OrcaMark } from '@/components/ui/orca-mark'

// The nav reads as a water column: what you do at the surface, descending to
// what runs it. Group labels are depth markers, not decoration.
const navGroups = [
  {
    label: 'Operate',
    items: [
      { to: '/', label: 'Dashboard', icon: LayoutDashboard },
      { to: '/chat', label: 'Chat', icon: Sparkles },
      { to: '/tasks', label: 'Tasks', icon: ListTodo },
      { to: '/kanban', label: 'Board', icon: Columns3 },
      { to: '/live', label: 'Live', icon: Activity },
      { to: '/sessions', label: 'Sessions', icon: MessageSquare },
    ],
  },
  {
    label: 'Automation',
    items: [
      { to: '/monitors', label: 'Monitors', icon: Radar },
      { to: '/security', label: 'Security', icon: Shield },
    ],
  },
  {
    label: 'Registry',
    items: [
      { to: '/agents', label: 'Agents', icon: Bot },
      { to: '/providers', label: 'Providers', icon: Cable },
      { to: '/tools', label: 'Tools', icon: Wrench },
      { to: '/skills', label: 'Skills', icon: SquareLibrary },
      { to: '/memory', label: 'Memory', icon: Brain },
    ],
  },
  {
    label: 'Fabric',
    items: [
      { to: '/runtimes', label: 'Runtimes', icon: Boxes },
      { to: '/gateways', label: 'Gateways', icon: RadioTower },
    ],
  },
  {
    label: 'Platform',
    items: [
      { to: '/system', label: 'System', icon: Settings2 },
    ],
  },
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
      <nav className="flex-1 space-y-3 overflow-y-auto p-2">
        {navGroups.map((group, groupIndex) => (
          <div key={group.label}>
            {sidebarCollapsed ? (
              groupIndex > 0 && <div className="mx-3 mb-2 border-t border-border" aria-hidden="true" />
            ) : (
              <p className="px-3 pb-1 pt-1.5 text-[10px] font-semibold uppercase tracking-[0.14em] text-muted-foreground/80">
                {group.label}
              </p>
            )}
            <div className="space-y-0.5">
              {group.items.map(({ to, label, icon: Icon }) => {
                const isActive = to === '/' ? location.pathname === '/' : location.pathname.startsWith(to)
                return (
                  <Link
                    key={to}
                    to={to}
                    aria-current={isActive ? 'page' : undefined}
                    title={sidebarCollapsed ? label : undefined}
                    className={cn(
                      'relative flex items-center gap-3 rounded-md px-3 py-2 text-sm font-medium transition-colors',
                      // Left accent bar + subtle tint + colored icon/text.
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
            </div>
          </div>
        ))}
      </nav>
    </aside>
  )
}
