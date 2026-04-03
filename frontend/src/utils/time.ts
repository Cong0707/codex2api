import i18n from '../i18n'

export interface RelativeTimeOptions {
  variant?: 'long' | 'compact'
  includeSeconds?: boolean
  fallback?: string
}

export function formatRelativeTime(dateStr?: string | null, options: RelativeTimeOptions = {}): string {
  const {
    variant = 'long',
    includeSeconds = false,
    fallback = '-',
  } = options

  if (!dateStr) {
    return fallback
  }

  const timestamp = new Date(dateStr).getTime()
  if (Number.isNaN(timestamp)) {
    return fallback
  }

  const diff = Math.max(0, Date.now() - timestamp)
  const seconds = Math.floor(diff / 1000)

  if (includeSeconds && seconds < 60) {
    return variant === 'compact'
      ? i18n.t('common.secondsAgoCompact', { count: seconds })
      : i18n.t('common.secondsAgoLong', { count: seconds })
  }

  const minutes = Math.floor(seconds / 60)
  if (minutes < 1) {
    return i18n.t('common.justNow')
  }

  if (minutes < 60) {
    return variant === 'compact'
      ? i18n.t('common.minutesAgoCompact', { count: minutes })
      : i18n.t('common.minutesAgoLong', { count: minutes })
  }

  const hours = Math.floor(minutes / 60)
  if (hours < 24) {
    return variant === 'compact'
      ? i18n.t('common.hoursAgoCompact', { count: hours })
      : i18n.t('common.hoursAgoLong', { count: hours })
  }

  const days = Math.floor(hours / 24)
  return variant === 'compact'
    ? i18n.t('common.daysAgoCompact', { count: days })
    : i18n.t('common.daysAgoLong', { count: days })
}

const TIMEZONE_STORAGE_KEY = 'codex2api_timezone'
const DEFAULT_TIMEZONE = 'Asia/Shanghai'

/** 获取用户选择的时区，默认 Asia/Shanghai */
export function getTimezone(): string {
  try {
    return localStorage.getItem(TIMEZONE_STORAGE_KEY) || DEFAULT_TIMEZONE
  } catch {
    return DEFAULT_TIMEZONE
  }
}

/** 设置时区并持久化到 localStorage */
export function setTimezone(tz: string): void {
  try {
    localStorage.setItem(TIMEZONE_STORAGE_KEY, tz)
  } catch { /* 忽略 */ }
}

/**
 * Format a date string as Beijing time (UTC+8)
 * Output format: YYYY-MM-DD HH:mm:ss
 *
 * 使用 Intl.DateTimeFormat 以用户选择的时区格式化，
 * 无论后端返回的是 UTC（带 Z）还是带时区偏移（+08:00），都能正确显示，
 * 避免手动加减偏移导致的重复转换问题。
 */
export function formatBeijingTime(dateStr?: string | null, fallback = '-'): string {
  if (!dateStr) return fallback

  const date = new Date(dateStr)
  if (Number.isNaN(date.getTime())) return fallback

  const fmt = new Intl.DateTimeFormat('sv-SE', {
    timeZone: getTimezone(),
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
    hour12: false,
  })

  // sv-SE locale 输出格式为 "YYYY-MM-DD HH:mm:ss"，正好是目标格式
  return fmt.format(date)
}
