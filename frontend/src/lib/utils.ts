import { clsx, type ClassValue } from "clsx"
import { twMerge } from "tailwind-merge"

export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs))
}

export function formatCompactEmail(email?: string | null, fallback = '-'): string {
  const value = email?.trim() ?? ''
  if (!value) return fallback

  const parts = value.split('@')
  if (parts.length !== 2 || !parts[0] || !parts[1]) return value

  const [localPart, domainPart] = parts
  const labels = domainPart.split('.').filter(Boolean)
  if (labels.length < 2) return value
  if (labels.length < 3) return value

  const hiddenLevels = labels.length - 2
  return `${localPart}@${'*'.repeat(hiddenLevels)}.${labels.slice(-2).join('.')}`
}
