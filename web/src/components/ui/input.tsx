import * as React from 'react'
import { cn } from '@/lib/utils'

const base =
  'w-full rounded-md border bg-card/60 px-3 text-sm text-foreground shadow-[inset_0_1px_0_rgb(255_255_255/0.03)] placeholder:text-muted-foreground/80 transition-[border-color,box-shadow] focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/40 focus-visible:border-primary/50 disabled:opacity-50'

export const Input = React.forwardRef<HTMLInputElement, React.InputHTMLAttributes<HTMLInputElement>>(({ className, ...p }, ref) => (
  <input ref={ref} className={cn(base, 'h-9', className)} {...p} />
))
Input.displayName = 'Input'

export const Textarea = React.forwardRef<HTMLTextAreaElement, React.TextareaHTMLAttributes<HTMLTextAreaElement>>(({ className, ...p }, ref) => (
  <textarea ref={ref} className={cn(base, 'py-2.5 font-mono text-[13px] leading-relaxed', className)} {...p} />
))
Textarea.displayName = 'Textarea'

export const Select = React.forwardRef<HTMLSelectElement, React.SelectHTMLAttributes<HTMLSelectElement>>(({ className, ...p }, ref) => (
  <select ref={ref} className={cn(base, 'h-9 bg-card', className)} {...p} />
))
Select.displayName = 'Select'

export function Label({ className, ...p }: React.LabelHTMLAttributes<HTMLLabelElement>) {
  return <label className={cn('text-[11px] font-semibold uppercase tracking-[0.04em] text-muted-foreground', className)} {...p} />
}

export function Field({ label, hint, children }: { label: string; hint?: string; children: React.ReactNode }) {
  return (
    <div className="space-y-1.5">
      <Label>{label}</Label>
      {children}
      {hint && <p className="text-xs leading-relaxed text-muted-foreground">{hint}</p>}
    </div>
  )
}
