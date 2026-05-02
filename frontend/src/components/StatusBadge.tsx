import { Badge } from '@/components/ui/badge'
import { useTranslation } from 'react-i18next'

interface StatusBadgeProps {
  status?: string | null
}

const statusConfig: Record<string, { variant: 'default' | 'secondary' | 'destructive' | 'outline'; dotColor: string }> = {
  active: { variant: 'default', dotColor: 'bg-emerald-500' },
  ready: { variant: 'default', dotColor: 'bg-emerald-500' },
  cooldown: { variant: 'secondary', dotColor: 'bg-amber-500' },
  rate_limited: { variant: 'secondary', dotColor: 'bg-yellow-500' },
  full_usage: { variant: 'secondary', dotColor: 'bg-amber-600' },
  image_active: { variant: 'default', dotColor: 'bg-cyan-500' },
  image_full_usage: { variant: 'secondary', dotColor: 'bg-amber-600' },
  unauthorized: { variant: 'destructive', dotColor: 'bg-red-500' },
  error: { variant: 'destructive', dotColor: 'bg-red-400' },
  paused: { variant: 'outline', dotColor: 'bg-blue-500' },
}

export default function StatusBadge({ status }: StatusBadgeProps) {
  const { t } = useTranslation()
  const key = status ?? 'unknown'
  const config = statusConfig[key] ?? { variant: 'outline' as const, dotColor: 'bg-gray-400' }

  return (
    <Badge variant={config.variant} className="gap-1.5 text-[13px]">
      <span className={`size-1.5 rounded-full ${config.dotColor}`} />
      {t(`status.${key}`, { defaultValue: t('status.unknown', { defaultValue: key }) })}
    </Badge>
  )
}
