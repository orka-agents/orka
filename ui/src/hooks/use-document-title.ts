import { useEffect } from 'react'

const appTitle = 'Orka'

/**
 * Shows "<title> · Orka" in the browser tab while the calling page is
 * mounted. Unmounting restores the bare app title, so the login page and
 * pages without a title of their own read "Orka" instead of a stale title.
 */
export function useDocumentTitle(title: string) {
  useEffect(() => {
    document.title = title ? `${title} · ${appTitle}` : appTitle
    return () => {
      document.title = appTitle
    }
  }, [title])
}
