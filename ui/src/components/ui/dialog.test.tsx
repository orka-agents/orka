import { describe, it, expect } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { Dialog as DialogPrimitive } from 'radix-ui'
import { Dialog, DialogContent, DialogDescription, DialogTitle } from './dialog'

describe('DialogContent', () => {
  it('leaves focus restoration to Radix when nothing had focus at open', async () => {
    render(
      <Dialog defaultOpen>
        <DialogPrimitive.Trigger>Open</DialogPrimitive.Trigger>
        <DialogContent>
          <DialogTitle>Title</DialogTitle>
          <DialogDescription>Body</DialogDescription>
        </DialogContent>
      </Dialog>,
    )
    await screen.findByRole('dialog')
    await userEvent.keyboard('{Escape}')
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    await waitFor(() => expect(screen.getByRole('button', { name: 'Open' })).toHaveFocus())
  })
})
