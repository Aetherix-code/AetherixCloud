import * as React from 'react'
import * as S from '@radix-ui/react-switch'
import { cn } from '@/lib/utils'

export function Switch({ className, ...p }: React.ComponentProps<typeof S.Root>) {
  return (
    <S.Root
      className={cn(
        'inline-flex h-5 w-9 shrink-0 cursor-pointer items-center rounded-full border border-transparent bg-muted-foreground/25 transition-colors',
        'data-[state=checked]:bg-primary focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/40 disabled:opacity-50',
        className,
      )}
      {...p}
    >
      <S.Thumb className="block size-4 translate-x-0.5 rounded-full bg-white shadow-sm transition-transform data-[state=checked]:translate-x-[18px]" />
    </S.Root>
  )
}
