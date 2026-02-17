import { useEffect, useMemo, useState } from 'react'
import { Moon, Sun, LogOut } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { useUIStore } from '@/stores/ui'
import { useAuthStore } from '@/stores/auth'
import { useNavigate } from '@tanstack/react-router'

export function Header() {
  const { theme, toggleTheme, namespace, setNamespace } = useUIStore()
  const { clearToken } = useAuthStore()
  const navigate = useNavigate()
  const [namespaceInput, setNamespaceInput] = useState(namespace)
  const namespaceSuggestions = useMemo(
    () => Array.from(new Set([namespaceInput, namespace, 'default', 'orka-system'].filter(Boolean))),
    [namespace, namespaceInput],
  )

  useEffect(() => {
    setNamespaceInput(namespace)
  }, [namespace])

  const handleLogout = () => {
    clearToken()
    navigate({ to: '/login' })
  }

  const commitNamespace = (value: string) => {
    const nextNamespace = value.trim() || 'default'
    setNamespaceInput(nextNamespace)
    if (nextNamespace !== namespace) {
      setNamespace(nextNamespace)
    }
  }

  return (
    <header className="flex h-14 items-center justify-between border-b border-border bg-card px-6">
      <div className="flex items-center gap-4">
        <Input
          aria-label="Namespace"
          className="h-9 w-48"
          list="namespace-suggestions"
          placeholder="Namespace"
          value={namespaceInput}
          onBlur={() => commitNamespace(namespaceInput)}
          onChange={(e) => setNamespaceInput(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === 'Enter') {
              commitNamespace(namespaceInput)
              e.currentTarget.blur()
            }
          }}
        />
        <datalist id="namespace-suggestions">
          {namespaceSuggestions.map((value) => (
            <option key={value} value={value} />
          ))}
        </datalist>
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
