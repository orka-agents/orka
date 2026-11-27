import { useState } from 'react'
import { Monitor, Moon, Sun, LogOut } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuRadioGroup,
  DropdownMenuRadioItem,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu'
import { namespaceOptions, useUIStore, type ThemePreference } from '@/stores/ui'
import { isKubernetesNamespace } from '@/lib/connectors'
import { useAuthStore } from '@/stores/auth'
import { useNavigate } from '@tanstack/react-router'

const THEMES = [
  { value: 'system', label: 'System', Icon: Monitor },
  { value: 'light', label: 'Light', Icon: Sun },
  { value: 'dark', label: 'Dark', Icon: Moon },
] as const

const OTHER_NAMESPACE = '__other__'

function NamespacePicker() {
  const namespace = useUIStore((s) => s.namespace)
  const recentNamespaces = useUIStore((s) => s.recentNamespaces)
  const callerNamespace = useUIStore((s) => s.callerNamespace)
  const setNamespace = useUIStore((s) => s.setNamespace)
  const [typing, setTyping] = useState(false)
  const [draft, setDraft] = useState('')
  const typed = draft.trim()
  const invalid = typed !== '' && !isKubernetesNamespace(typed)

  const stopTyping = () => {
    setTyping(false)
    setDraft('')
  }

  if (typing) {
    return (
      <form
        className="flex items-center gap-2"
        onSubmit={(event) => {
          event.preventDefault()
          if (!isKubernetesNamespace(typed)) return
          setNamespace(typed)
          stopTyping()
        }}
      >
        <Input
          autoFocus
          className="w-48"
          aria-label="Namespace name"
          aria-invalid={invalid}
          aria-describedby={invalid ? 'namespace-name-error' : undefined}
          placeholder="Type a namespace"
          autoCapitalize="none"
          autoComplete="off"
          spellCheck={false}
          value={draft}
          onChange={(event) => setDraft(event.target.value)}
          onKeyDown={(event) => {
            if (event.key === 'Escape') stopTyping()
          }}
          onBlur={() => {
            if (typed === '') stopTyping()
          }}
        />
        {invalid && (
          <span id="namespace-name-error" role="alert" className="hidden text-xs text-destructive sm:inline">
            Use a lowercase DNS-1123 label
          </span>
        )}
      </form>
    )
  }

  return (
    <Select
      value={namespace}
      onValueChange={(value) => (value === OTHER_NAMESPACE ? setTyping(true) : setNamespace(value))}
    >
      <SelectTrigger className="w-48" aria-label="Namespace">
        <SelectValue placeholder="Namespace" />
      </SelectTrigger>
      <SelectContent>
        {namespaceOptions({ namespace, recentNamespaces, callerNamespace }).map((option) => (
          <SelectItem key={option} value={option}>{option}</SelectItem>
        ))}
        <SelectItem value={OTHER_NAMESPACE}>Other namespace…</SelectItem>
      </SelectContent>
    </Select>
  )
}

export function Header() {
  const theme = useUIStore((s) => s.theme)
  const setTheme = useUIStore((s) => s.setTheme)
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
        <NamespacePicker />
        {import.meta.env.MODE === 'demo' && (
          <span
            title="Orka's optional web dashboard, on mock data in your browser. Nothing is sent to a cluster or a model, and everything you see or do in it can be done with the orka CLI or kubectl."
            className="hidden shrink-0 rounded-full border border-border px-2.5 py-0.5 text-xs font-medium text-muted-foreground sm:inline"
          >
            Demo<span className="hidden lg:inline"> · optional dashboard, mock data</span>
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
