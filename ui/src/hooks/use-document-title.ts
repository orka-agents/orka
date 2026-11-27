import { useEffect } from 'react'

const appTitle = 'Orka'

export function useDocumentTitle(title: string) {
  useEffect(() => {
    document.title = title ? `${title} · ${appTitle}` : appTitle
    return () => {
      document.title = appTitle
    }
  }, [title])
}
