import { createFileRoute, useLocation, useNavigate } from '@tanstack/react-router'
import { useEffect, useState } from 'react'
import { useAuthStore } from '@/stores/auth'
import { hasPendingHashToken, takeHashToken, takeLoginReturn } from '@/lib/login-return'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { OrcaMark } from '@/components/ui/orca-mark'
import { DEMO_TOKEN } from '@/demo/token'

export const Route = createFileRoute('/login')({
  component: LoginPage,
})

function LoginPage() {
  const [tokenInput, setTokenInput] = useState('')
  const [error, setError] = useState('')
  const [isValidating, setIsValidating] = useState(hasPendingHashToken)
  const { setToken, token } = useAuthStore()
  const navigate = useNavigate()
  const location = useLocation()

  // Sign in with the #token=... fragment from `orka dashboard` or a deep link.
  // captureHashToken took it out of the address bar at app start.
  useEffect(() => {
    const t = takeHashToken()
    if (!t) return
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
  }, [setToken])

  // Authenticated and no token still being validated: return to the page that
  // sent us here. Only while the address is still /login: the token swaps the
  // root layout before the route match changes, so this page mounts again for
  // a moment at the destination, where the return has already been taken.
  // A rejected sign-in stays here with its error, even when an earlier token
  // is still stored, instead of silently continuing as the old identity.
  useEffect(() => {
    if (token && !isValidating && !error && location.pathname === '/login') {
      navigate({ href: takeLoginReturn(location.hash) })
    }
  }, [token, isValidating, error, location.pathname, location.hash, navigate])

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
          <OrcaMark className="mx-auto mb-2 h-12 w-12" />
          <CardTitle className="text-2xl">Orka</CardTitle>
          <CardDescription>
            {import.meta.env.MODE === 'demo'
              ? "A demo of Orka's optional web dashboard on mock data in your browser. It needs no token, and everything you see or do in it can be done with the orka CLI or kubectl."
              : <>Enter your Kubernetes service account token or use <code className="text-xs">orka dashboard</code> from the CLI.</>}
          </CardDescription>
        </CardHeader>
        <CardContent>
          {/* The public demo never asks for a real cluster token. */}
          {import.meta.env.MODE === 'demo' ? (
            <div className="space-y-4">
              {error && (
                <p className="text-sm text-destructive">{error}</p>
              )}
              <Button className="w-full" disabled={isValidating} onClick={() => { setError(''); setToken(DEMO_TOKEN) }}>
                Sign in to the demo
              </Button>
            </div>
          ) : (
            <>
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
                  Or use the CLI: <code className="text-xs">orka dashboard --server http://localhost:8080</code>
                </p>
              </div>
            </>
          )}
        </CardContent>
      </Card>
    </div>
  )
}
