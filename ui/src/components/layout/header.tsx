import { Moon, Sun, LogOut } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { useUIStore } from '@/stores/ui'
import { useAuthStore } from '@/stores/auth'
import { useNavigate } from '@tanstack/react-router'

export function Header() {
  const { theme, toggleTheme, namespace, setNamespace } = useUIStore()
  const { clearToken } = useAuthStore()
  const navigate = useNavigate()

  const handleLogout = () => {
    clearToken()
    navigate({ to: '/login' })
  }

  return (
    <header className="flex h-14 items-center justify-between border-b border-border bg-card px-6">
      <div className="flex items-center gap-4">
        <Select value={namespace} onValueChange={setNamespace}>
          <SelectTrigger className="w-48">
            <SelectValue placeholder="Namespace" />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="default">default</SelectItem>
            <SelectItem value="orka-system">orka-system</SelectItem>
          </SelectContent>
        </Select>
      </div>
      <div className="flex items-center gap-2">
        <Button variant="ghost" size="icon" onClick={toggleTheme}>
          {theme === 'light' ? <Moon className="h-4 w-4" /> : <Sun className="h-4 w-4" />}
        </Button>
        <Button variant="ghost" size="icon" onClick={handleLogout}>
          <LogOut className="h-4 w-4" />
        </Button>
      </div>
    </header>
  )
}
