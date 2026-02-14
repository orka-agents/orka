import { createFileRoute, useNavigate } from '@tanstack/react-router'
import { useEffect, useState } from 'react'
import { useAuthStore } from '@/stores/auth'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'

export const Route = createFileRoute('/login')({
  component: LoginPage,
})

function LoginPage() {
  const [tokenInput, setTokenInput] = useState('')
  const [error, setError] = useState('')
  const [isValidating, setIsValidating] = useState(false)
  const { setToken, token } = useAuthStore()
  const navigate = useNavigate()

  // Handle #token=... hash fragment from CLI login
  useEffect(() => {
    const hash = window.location.hash
    if (hash.startsWith('#token=')) {
      const t = hash.slice(7)
      if (t) {
        window.location.hash = ''
        setIsValidating(true)
        fetch('/api/v1/auth/validate', {
          headers: { 'Authorization': `Bearer ${t}` },
        }).then(res => {
          if (res.ok) {
            setToken(t)
          } else {
            setError('Token from CLI is invalid or expired. Please generate a new one.')
          }
        }).catch(() => {
          setError('Could not validate token. Is the server running?')
        }).finally(() => {
          setIsValidating(false)
        })
      }
    }
  }, [setToken])

  // Already authenticated
  useEffect(() => {
    if (token) {
      navigate({ to: '/' })
    }
  }, [token, navigate])

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault()
    const trimmed = tokenInput.trim()
    if (!trimmed) return

    setError('')
    setIsValidating(true)
    try {
      const res = await fetch('/api/v1/auth/validate', {
        headers: { 'Authorization': `Bearer ${trimmed}` },
      })
      if (res.ok) {
        setToken(trimmed)
      } else {
        setError('Invalid token. Please check your token and try again.')
      }
    } catch {
      setError('Invalid token. Please check your token and try again.')
    } finally {
      setIsValidating(false)
    }
  }

  return (
    <div className="flex min-h-screen items-center justify-center bg-background">
      <Card className="w-full max-w-md">
        <CardHeader className="text-center">
          <div className="mb-2 text-4xl">🪸</div>
          <CardTitle className="text-2xl">Orka</CardTitle>
          <CardDescription>
            Enter your Kubernetes service account token or use <code className="text-xs">orka login</code> from the CLI.
          </CardDescription>
        </CardHeader>
        <CardContent>
          <form onSubmit={handleSubmit} className="space-y-4">
            <Input
              type="password"
              placeholder="Paste your token here..."
              value={tokenInput}
              onChange={(e) => { setTokenInput(e.target.value); setError('') }}
              autoFocus
            />
            {error && (
              <p className="text-sm text-destructive">{error}</p>
            )}
            <Button type="submit" className="w-full" disabled={!tokenInput.trim() || isValidating}>
              {isValidating ? 'Validating…' : 'Sign In'}
            </Button>
          </form>
          <div className="mt-6 space-y-3 border-t pt-4">
            <p className="text-sm text-muted-foreground font-medium">How to get a token:</p>
            <div className="rounded-md bg-muted p-3">
              <code className="text-xs break-all select-all">
                kubectl create token default -n default
              </code>
            </div>
            <p className="text-xs text-muted-foreground">
              Or use the CLI: <code className="text-xs">orka login --server http://localhost:8080</code>
            </p>
          </div>
        </CardContent>
      </Card>
    </div>
  )
}
