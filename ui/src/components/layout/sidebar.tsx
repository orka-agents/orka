import { Link, useLocation } from '@tanstack/react-router'
import { LayoutDashboard, ListTodo, MessageSquare, Bot, Wrench } from 'lucide-react'
import { cn } from '@/lib/utils'
import { useUIStore } from '@/stores/ui'
import { Button } from '@/components/ui/button'

const navItems = [
  { to: '/', label: 'Dashboard', icon: LayoutDashboard },
  { to: '/tasks', label: 'Tasks', icon: ListTodo },
  { to: '/sessions', label: 'Sessions', icon: MessageSquare },
  { to: '/agents', label: 'Agents', icon: Bot },
  { to: '/tools', label: 'Tools', icon: Wrench },
] as const

export function Sidebar() {
  const location = useLocation()
  const { sidebarCollapsed, toggleSidebar } = useUIStore()

  return (
    <aside className={cn(
      'flex flex-col border-r border-border bg-card transition-all duration-200',
      sidebarCollapsed ? 'w-16' : 'w-64'
    )}>
      <div className="flex h-14 items-center border-b border-border px-4">
        {!sidebarCollapsed && (
          <Link to="/" className="flex items-center gap-2 font-semibold text-foreground">
            <span className="text-xl">🐟</span>
            <span>Mercan</span>
          </Link>
        )}
        <Button
          variant="ghost"
          size="icon"
          onClick={toggleSidebar}
          className={cn('ml-auto h-8 w-8', sidebarCollapsed && 'mx-auto')}
        >
          <span className="text-sm">{sidebarCollapsed ? '→' : '←'}</span>
        </Button>
      </div>
      <nav className="flex-1 space-y-1 p-2">
        {navItems.map(({ to, label, icon: Icon }) => {
          const isActive = to === '/' ? location.pathname === '/' : location.pathname.startsWith(to)
          return (
            <Link
              key={to}
              to={to}
              className={cn(
                'flex items-center gap-3 rounded-md px-3 py-2 text-sm font-medium transition-colors',
                isActive
                  ? 'bg-primary text-primary-foreground'
                  : 'text-muted-foreground hover:bg-accent hover:text-accent-foreground',
                sidebarCollapsed && 'justify-center px-2'
              )}
            >
              <Icon className="h-4 w-4 shrink-0" />
              {!sidebarCollapsed && <span>{label}</span>}
            </Link>
          )
        })}
      </nav>
    </aside>
  )
}
