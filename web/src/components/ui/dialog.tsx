import * as React from 'react'
import * as D from '@radix-ui/react-dialog'
import { X } from 'lucide-react'
import { cn } from '@/lib/utils'

export const Dialog = D.Root
export const DialogTrigger = D.Trigger
export const DialogClose = D.Close

export function DialogContent({ className, children, ...p }: React.ComponentProps<typeof D.Content>) {
  return (
    <D.Portal>
      <D.Overlay className="fixed inset-0 z-50 bg-black/55 backdrop-blur-[3px] data-[state=open]:animate-in" />
      <D.Content
        className={cn(
          'ax-enter fixed left-1/2 top-1/2 z-50 w-[calc(100%-2rem)] max-w-lg -translate-x-1/2 -translate-y-1/2 rounded-xl border bg-card p-5 ax-panel focus:outline-none',
          className,
        )}
        {...p}
      >
        {children}
        <D.Close className="absolute right-3 top-3 cursor-pointer rounded-md p-1.5 text-muted-foreground transition-colors hover:bg-muted hover:text-foreground" aria-label="Close">
          <X className="size-4" />
        </D.Close>
      </D.Content>
    </D.Portal>
  )
}
export function DialogHeader({ title, description }: { title: string; description?: string }) {
  return (
    <div className="mb-4 space-y-1.5 pr-8">
      <D.Title className="text-base font-semibold tracking-tight">{title}</D.Title>
      <D.Description className="text-sm leading-relaxed text-muted-foreground">{description ?? ' '}</D.Description>
    </div>
  )
}
