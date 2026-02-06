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
  const { setToken, token } = useAuthStore()
  const navigate = useNavigate()

  // Handle #token=... hash fragment from CLI login
  useEffect(() => {
    const hash = window.location.hash
    if (hash.startsWith('#token=')) {
      const t = hash.slice(7)
      if (t) {
        setToken(t)
        window.location.hash = ''
      }
    }
  }, [setToken])

  // Already authenticated
  useEffect(() => {
    if (token) {
      navigate({ to: '/' })
    }
  }, [token, navigate])

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault()
    if (tokenInput.trim()) {
      setToken(tokenInput.trim())
    }
  }

  return (
    <div className="flex min-h-screen items-center justify-center bg-background">
      <Card className="w-full max-w-md">
        <CardHeader className="text-center">
          <div className="mb-2 text-4xl">🪸</div>
          <CardTitle className="text-2xl">Mercan</CardTitle>
          <CardDescription>
            Enter your Kubernetes service account token or use <code className="text-xs">mercan login</code> from the CLI.
          </CardDescription>
        </CardHeader>
        <CardContent>
          <form onSubmit={handleSubmit} className="space-y-4">
            <Input
              type="password"
              placeholder="Paste your token here..."
              value={tokenInput}
              onChange={(e) => setTokenInput(e.target.value)}
              autoFocus
            />
            <Button type="submit" className="w-full" disabled={!tokenInput.trim()}>
              Sign In
            </Button>
          </form>
        </CardContent>
      </Card>
    </div>
  )
}
