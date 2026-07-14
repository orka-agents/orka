import { useAuthStore } from '@/stores/auth'
import { API_BASE_URL } from './constants'

interface RequestOptions extends Omit<RequestInit, 'headers'> {
  headers?: Record<string, string>
  params?: Record<string, string>
}

class ApiError extends Error {
  constructor(public status: number, message: string) {
    super(message)
    this.name = 'ApiError'
  }
}

async function request<T>(path: string, options: RequestOptions = {}): Promise<T> {
  const { params, ...fetchOptions } = options
  const token = useAuthStore.getState().token

  const headers: Record<string, string> = {
    'Content-Type': 'application/json',
    ...options.headers,
  }

  if (token) {
    headers['Authorization'] = `Bearer ${token}`
  }

  let url = `${API_BASE_URL}${path}`
  if (params) {
    const searchParams = new URLSearchParams()
    for (const [key, value] of Object.entries(params)) {
      if (value) searchParams.set(key, value)
    }
    const qs = searchParams.toString()
    if (qs) url += `?${qs}`
  }

  const response = await fetch(url, { ...fetchOptions, headers })

  if (!response.ok) {
    if (response.status === 401) {
      useAuthStore.getState().clearToken()
    }
    const text = await response.text().catch(() => 'Unknown error')
    throw new ApiError(response.status, text)
  }

  if (response.status === 204) {
    return undefined as T
  }

  const text = await response.text()
  if (!text) {
    return undefined as T
  }

  const contentType = response.headers.get('Content-Type')?.split(';', 1)[0].trim().toLowerCase()
  if (contentType === 'application/json' || contentType?.endsWith('+json')) {
    return JSON.parse(text) as T
  }

  return text as T
}

export const api = {
  get: <T>(path: string, params?: Record<string, string>) =>
    request<T>(path, { method: 'GET', params }),

  post: <T>(path: string, body?: unknown, params?: Record<string, string>, headers?: Record<string, string>) =>
    request<T>(path, { method: 'POST', body: body ? JSON.stringify(body) : undefined, params, headers }),

  put: <T>(path: string, body?: unknown, params?: Record<string, string>) =>
    request<T>(path, { method: 'PUT', body: body ? JSON.stringify(body) : undefined, params }),

  delete: <T>(path: string, params?: Record<string, string>) =>
    request<T>(path, { method: 'DELETE', params }),
}

export { ApiError }
