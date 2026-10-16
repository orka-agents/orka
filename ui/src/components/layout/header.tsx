import { Monitor, Moon, Sun, LogOut } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuRadioGroup,
  DropdownMenuRadioItem,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu'
import { useUIStore, type ThemePreference } from '@/stores/ui'
import { useAuthStore } from '@/stores/auth'
import { useNavigate } from '@tanstack/react-router'

const THEMES = [
  { value: 'system', label: 'System', Icon: Monitor },
  { value: 'light', label: 'Light', Icon: Sun },
  { value: 'dark', label: 'Dark', Icon: Moon },
] as const

export function Header() {
  const { theme, setTheme, namespace, setNamespace } = useUIStore()
  const current = THEMES.find((option) => option.value === theme) ?? THEMES[0]
  const { clearToken } = useAuthStore()
  const navigate = useNavigate()

  const handleLogout = () => {
    clearToken()
    navigate({ to: '/login' })
  }

  return (
    <header className="z-20 flex h-14 shrink-0 items-center justify-between border-b border-border bg-card/80 px-6 backdrop-blur-md">
      <div className="flex items-center gap-4">
        <Select value={namespace} onValueChange={setNamespace}>
          <SelectTrigger className="w-48" aria-label="Namespace">
            <SelectValue placeholder="Namespace" />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="default">default</SelectItem>
            <SelectItem value="orka-system">orka-system</SelectItem>
          </SelectContent>
        </Select>
        {import.meta.env.MODE === 'demo' && (
          <span
            title="This dashboard runs on sample data in your browser. Nothing is sent to a cluster or a model."
            className="hidden shrink-0 rounded-full border border-border px-2.5 py-0.5 text-xs font-medium text-muted-foreground sm:inline"
          >
            Demo<span className="hidden lg:inline"> · sample data, no live cluster or model</span>
          </span>
        )}
      </div>
      <div className="flex items-center gap-2">
        <DropdownMenu>
          <DropdownMenuTrigger asChild>
            <Button variant="ghost" size="icon" aria-label={`Theme: ${current.label}`}>
              <current.Icon className="h-4 w-4" />
            </Button>
          </DropdownMenuTrigger>
          <DropdownMenuContent align="end">
            <DropdownMenuRadioGroup value={theme} onValueChange={(value) => setTheme(value as ThemePreference)}>
              {THEMES.map(({ value, label, Icon }) => (
                <DropdownMenuRadioItem key={value} value={value}>
                  <Icon className="h-4 w-4" aria-hidden="true" />
                  {label}
                </DropdownMenuRadioItem>
              ))}
            </DropdownMenuRadioGroup>
          </DropdownMenuContent>
        </DropdownMenu>
        <Button
          variant="ghost"
          size="icon"
          onClick={handleLogout}
          aria-label="Log out"
        >
          <LogOut className="h-4 w-4" />
        </Button>
      </div>
    </header>
  )
}
