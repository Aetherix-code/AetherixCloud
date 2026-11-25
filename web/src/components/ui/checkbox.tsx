import * as React from 'react'
import * as CheckboxPrimitive from '@radix-ui/react-checkbox'
import { Check } from 'lucide-react'
import { cn } from '@/lib/utils'

export const Checkbox = React.forwardRef<
  React.ComponentRef<typeof CheckboxPrimitive.Root>,
  React.ComponentPropsWithoutRef<typeof CheckboxPrimitive.Root>
>(({ className, ...props }, ref) => (
  <CheckboxPrimitive.Root
    ref={ref}
    className={cn(
      'peer size-4 shrink-0 rounded-[5px] border border-border bg-card transition-colors',
      'hover:border-primary/70 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/40',
      'data-[state=checked]:border-primary data-[state=checked]:bg-primary data-[state=checked]:text-primary-foreground',
      'disabled:cursor-not-allowed disabled:opacity-50',
      className,
    )}
    {...props}
  >
    <CheckboxPrimitive.Indicator className="flex items-center justify-center text-current">
      <Check className="size-3" strokeWidth={3} />
    </CheckboxPrimitive.Indicator>
  </CheckboxPrimitive.Root>
))
Checkbox.displayName = 'Checkbox'

export function CheckboxField({
  checked,
  onCheckedChange,
  disabled,
  children,
  className,
}: {
  checked: boolean
  onCheckedChange: (checked: boolean) => void
  disabled?: boolean
  children: React.ReactNode
  className?: string
}) {
  return (
    <label className={cn('flex cursor-pointer items-start gap-2.5 text-sm leading-none', disabled && 'cursor-not-allowed opacity-50', className)}>
      <Checkbox className="mt-0.5" checked={checked} disabled={disabled} onCheckedChange={(v) => onCheckedChange(v === true)} />
      <span className="leading-snug">{children}</span>
    </label>
  )
}
