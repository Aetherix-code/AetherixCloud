import { cva, type VariantProps } from 'class-variance-authority'
import { cn } from '@/lib/utils'

const badgeVariants = cva(
  'inline-flex items-center gap-1 rounded-md border px-1.5 py-0.5 text-[11px] font-semibold tracking-tight whitespace-nowrap',
  {
    variants: {
      tone: {
        neutral: 'border-transparent bg-muted text-muted-foreground',
        success: 'border-success/25 bg-success/10 text-success',
        warning: 'border-warning/25 bg-warning/10 text-warning',
        danger: 'border-destructive/25 bg-destructive/10 text-destructive',
        info: 'border-primary/25 bg-primary/10 text-primary',
      },
    },
    defaultVariants: { tone: 'neutral' },
  },
)

export function Badge({ className, tone, ...p }: React.HTMLAttributes<HTMLSpanElement> & VariantProps<typeof badgeVariants>) {
  return <span className={cn(badgeVariants({ tone }), className)} {...p} />
}

export type Tone = NonNullable<VariantProps<typeof badgeVariants>['tone']>
