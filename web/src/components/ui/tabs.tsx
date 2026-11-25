import * as React from 'react'
import * as T from '@radix-ui/react-tabs'
import { cn } from '@/lib/utils'

export const Tabs = T.Root
export function TabsList({ className, ...p }: React.ComponentProps<typeof T.List>) {
  return <T.List className={cn('flex items-center gap-0.5 overflow-x-auto border-b border-border/80 px-2', className)} {...p} />
}
export function TabsTrigger({ className, ...p }: React.ComponentProps<typeof T.Trigger>) {
  return (
    <T.Trigger
      className={cn(
        'relative cursor-pointer rounded-t-md px-3 py-2.5 text-sm font-medium text-muted-foreground transition-colors hover:text-foreground data-[state=active]:text-foreground',
        'after:absolute after:inset-x-2 after:-bottom-px after:h-0.5 after:rounded-full after:bg-primary after:opacity-0 after:transition-opacity data-[state=active]:after:opacity-100',
        className,
      )}
      {...p}
    />
  )
}
export function TabsContent({ className, ...p }: React.ComponentProps<typeof T.Content>) {
  return <T.Content className={cn('ax-enter focus-visible:outline-none', className)} {...p} />
}
