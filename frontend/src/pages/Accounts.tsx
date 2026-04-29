import type { ChangeEvent, DragEvent, ReactNode } from 'react'
import { useCallback, useEffect, useRef, useState } from 'react'
import { api, getAdminKey } from '../api'
import Modal from '../components/Modal'
import PageHeader from '../components/PageHeader'
import Pagination from '../components/Pagination'
import StateShell from '../components/StateShell'
import StatusBadge from '../components/StatusBadge'
import ToastNotice from '../components/ToastNotice'
import { useDataLoader } from '../hooks/useDataLoader'
import { useConfirmDialog } from '../hooks/useConfirmDialog'
import { useToast } from '../hooks/useToast'
import type { AccountRow, AddAccountRequest, AddATAccountRequest } from '../types'
import { getErrorMessage } from '../utils/error'
import { formatCompactEmail } from '../lib/utils'
import { formatRelativeTime, formatBeijingTime } from '../utils/time'
import { Card, CardContent } from '@/components/ui/card'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'
import { Plus, RefreshCw, Trash2, Zap, FlaskConical, Ban, Timer, AlertTriangle, Upload, Download, ArrowDownToLine, KeyRound, ExternalLink, FileText, FileJson, BarChart3, Search, Fingerprint, FolderOpen, Lock, Unlock, RotateCcw } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import type { TFunction } from 'i18next'
import AccountUsageModal from '../components/AccountUsageModal'

function formatWaitRemaining(seconds: number, t: TFunction): string {
  const safeSeconds = Number.isFinite(seconds) ? Math.max(0, Math.floor(seconds)) : 0
  if (safeSeconds <= 0) {
    return `0${t('accounts.waitUnitSecond')}`
  }

  const days = Math.floor(safeSeconds / 86400)
  const hours = Math.floor((safeSeconds % 86400) / 3600)
  const minutes = Math.floor((safeSeconds % 3600) / 60)
  const secs = safeSeconds % 60
  const parts: string[] = []
  if (days > 0) parts.push(`${days}${t('accounts.waitUnitDay')}`)
  if (hours > 0) parts.push(`${hours}${t('accounts.waitUnitHour')}`)
  if (minutes > 0) parts.push(`${minutes}${t('accounts.waitUnitMinute')}`)
  if (parts.length === 0) parts.push(`${secs}${t('accounts.waitUnitSecond')}`)
  return parts.join(' ')
}

function formatWaitPoint(dateStr?: string): string {
  if (!dateStr) return '--'
  const date = new Date(dateStr)
  if (Number.isNaN(date.getTime())) return '--'
  const fmt = new Intl.DateTimeFormat('sv-SE', {
    timeZone: 'Asia/Shanghai',
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    hour12: false,
  })
  const parts = fmt.formatToParts(date)
  const month = parts.find((part) => part.type === 'month')?.value ?? '00'
  const day = parts.find((part) => part.type === 'day')?.value ?? '00'
  const hour = parts.find((part) => part.type === 'hour')?.value ?? '00'
  const minute = parts.find((part) => part.type === 'minute')?.value ?? '00'
  return `${month}-${day} ${hour}:${minute}`
}

function clampUsagePercent(value?: number | null): number {
  if (typeof value !== 'number' || !Number.isFinite(value)) {
    return 0
  }
  return Math.min(100, Math.max(0, value))
}

function roundTo2(value: number): number {
  return Math.round(value * 100) / 100
}

type QuotaRateConfig = {
  plus: number
  pro: number
  team: number
}

type QuotaStatsFree = {
  accountCount: number
  quotaTotal: number
  quotaUsed: number
  quotaRemaining: number
  quotaUsedPercent: number
  quotaRemainingPercent: number
  quotaUsedAccounts: number
  quotaRemainingAccounts: number
}

type QuotaStatsWindowed = {
  accountCount: number
  quotaTotal: number
  usage5hUsed: number
  usage5hRemaining: number
  usage5hUsedPercent: number
  usage5hRemainingPercent: number
  usage5hUsedAccounts: number
  usage5hRemainingAccounts: number
  usage7dUsed: number
  usage7dRemaining: number
  usage7dUsedPercent: number
  usage7dRemainingPercent: number
  usage7dUsedAccounts: number
  usage7dRemainingAccounts: number
  waiting5hCount: number
  waiting7dCount: number
}

function normalizeRate(value: number, fallback: number): number {
  if (!Number.isFinite(value) || value <= 0) {
    return fallback
  }
  return value
}

function resolvePlanWeight(planType: string | undefined, rates: QuotaRateConfig): number {
  const plan = (planType || '').toLowerCase()
  if (plan === 'pro') return rates.pro
  if (plan === 'plus') return rates.plus
  if (plan === 'team') return rates.team
  if (plan === 'free') return 1
  return rates.team
}

function calcWeightedTotal(accounts: AccountRow[], rates: QuotaRateConfig): number {
  return accounts.reduce((sum, account) => sum + resolvePlanWeight(account.plan_type, rates) * 100, 0)
}

function calcWeightedUsed(accounts: AccountRow[], rates: QuotaRateConfig, usageKey: 'usage_percent_5h' | 'usage_percent_7d'): number {
  return accounts.reduce((sum, account) => {
    const weight = resolvePlanWeight(account.plan_type, rates)
    const usedPercent = clampUsagePercent(account[usageKey])
    return sum + weight * usedPercent
  }, 0)
}

// 5h 可用额度需扣除 7d 窗口影响：按账号维度取 max(5h_used, 7d_used)
function calcWeightedUsed5hEffective(accounts: AccountRow[], rates: QuotaRateConfig): number {
  return accounts.reduce((sum, account) => {
    const weight = resolvePlanWeight(account.plan_type, rates)
    const used5h = clampUsagePercent(account.usage_percent_5h)
    const used7d = clampUsagePercent(account.usage_percent_7d)
    return sum + weight * Math.max(used5h, used7d)
  }, 0)
}

function isUsageFull(value?: number | null): boolean {
  if (value === null || value === undefined) return false
  if (!Number.isFinite(value)) return false
  return value >= 100
}

function calcWaitCount(accounts: AccountRow[], waitType: '5h' | '7d'): number {
  return accounts.filter((account) => {
    if (!account.wait_mode) return false
    const waitReason = (account.wait_reason || '').toLowerCase()
    const status = (account.status || '').toLowerCase()
    const isFullUsageWait = waitReason === 'full_usage' || status === 'full_usage'
    const isRateLimitedWait = waitReason === 'rate_limited' || status === 'rate_limited'
    const isFull5h = isUsageFull(account.usage_percent_5h)
    const isFull7d = isUsageFull(account.usage_percent_7d)
    if (waitType === '7d') {
      // 7d 满时优先计入 7d 等待（即使当前 wait_reason 不是 full_usage）
      return isFull7d
    }

    // 5h 统计排除所有 7d 已满账号：5h 到时后也不会可用
    if (isFull7d) return false
    if (isRateLimitedWait) return true
    if (isFullUsageWait) {
      return isFull5h
    }
    return false
  }).length
}

function calcFreeQuotaStats(accounts: AccountRow[], rates: QuotaRateConfig): QuotaStatsFree {
  const quotaTotal = calcWeightedTotal(accounts, rates)
  const quotaUsed = calcWeightedUsed(accounts, rates, 'usage_percent_7d')
  const quotaRemaining = Math.max(0, quotaTotal - quotaUsed)
  const quotaUsedPercent = quotaTotal > 0 ? roundTo2((quotaUsed / quotaTotal) * 100) : 0
  const quotaRemainingPercent = quotaTotal > 0 ? roundTo2((quotaRemaining / quotaTotal) * 100) : 0
  return {
    accountCount: accounts.length,
    quotaTotal: roundTo2(quotaTotal),
    quotaUsed: roundTo2(quotaUsed),
    quotaRemaining: roundTo2(quotaRemaining),
    quotaUsedPercent,
    quotaRemainingPercent,
    quotaUsedAccounts: roundTo2(quotaUsed / 100),
    quotaRemainingAccounts: roundTo2(quotaRemaining / 100),
  }
}

function calcWindowedQuotaStats(accounts: AccountRow[], rates: QuotaRateConfig): QuotaStatsWindowed {
  const quotaTotal = calcWeightedTotal(accounts, rates)
  const usage5hUsed = calcWeightedUsed5hEffective(accounts, rates)
  const usage5hRemaining = Math.max(0, quotaTotal - usage5hUsed)
  const usage7dUsed = calcWeightedUsed(accounts, rates, 'usage_percent_7d')
  const usage7dRemaining = Math.max(0, quotaTotal - usage7dUsed)
  return {
    accountCount: accounts.length,
    quotaTotal: roundTo2(quotaTotal),
    usage5hUsed: roundTo2(usage5hUsed),
    usage5hRemaining: roundTo2(usage5hRemaining),
    usage5hUsedPercent: quotaTotal > 0 ? roundTo2((usage5hUsed / quotaTotal) * 100) : 0,
    usage5hRemainingPercent: quotaTotal > 0 ? roundTo2((usage5hRemaining / quotaTotal) * 100) : 0,
    usage5hUsedAccounts: roundTo2(usage5hUsed / 100),
    usage5hRemainingAccounts: roundTo2(usage5hRemaining / 100),
    usage7dUsed: roundTo2(usage7dUsed),
    usage7dRemaining: roundTo2(usage7dRemaining),
    usage7dUsedPercent: quotaTotal > 0 ? roundTo2((usage7dUsed / quotaTotal) * 100) : 0,
    usage7dRemainingPercent: quotaTotal > 0 ? roundTo2((usage7dRemaining / quotaTotal) * 100) : 0,
    usage7dUsedAccounts: roundTo2(usage7dUsed / 100),
    usage7dRemainingAccounts: roundTo2(usage7dRemaining / 100),
    waiting5hCount: calcWaitCount(accounts, '5h'),
    waiting7dCount: calcWaitCount(accounts, '7d'),
  }
}

function formatMetric(value: number, fractionDigits = 2): string {
  if (!Number.isFinite(value)) return '0'
  return value.toLocaleString(undefined, {
    minimumFractionDigits: 0,
    maximumFractionDigits: fractionDigits,
  })
}

function formatUSD(value?: number | null): string {
  const amount = typeof value === 'number' && Number.isFinite(value) ? value : 0
  return amount.toFixed(4).replace(/\.?0+$/, '')
}

const PAGE_SIZE_MIN = 20
const PAGE_SIZE_MAX = 200
const PAGE_SIZE_OPTIONS = Array.from({ length: ((PAGE_SIZE_MAX - PAGE_SIZE_MIN) / 10) + 1 }, (_, index) => PAGE_SIZE_MIN + index * 10)

export default function Accounts() {
  const { t } = useTranslation()
  const [showAdd, setShowAdd] = useState(false)
  const [page, setPage] = useState(1)
  const [pageSize, setPageSize] = useState(PAGE_SIZE_MIN)
  const [statusFilter, setStatusFilter] = useState<'all' | 'normal' | 'rate_limited' | 'full_usage' | 'banned' | 'locked'>('all')
  const [searchQuery, setSearchQuery] = useState('')
  const [planFilter, setPlanFilter] = useState<'all' | 'plus' | 'pro' | 'team' | 'free'>('all')
  const [sortKey, setSortKey] = useState<'requests' | 'usage' | 'importTime' | null>(null)
  const [sortDir, setSortDir] = useState<'asc' | 'desc'>('desc')
  const [addForm, setAddForm] = useState<AddAccountRequest>({
    refresh_token: '',
    proxy_url: '',
  })
  const [submitting, setSubmitting] = useState(false)
  const [selected, setSelected] = useState<Set<number>>(new Set())
  const [refreshingIds, setRefreshingIds] = useState<Set<number>>(new Set())
  const [batchLoading, setBatchLoading] = useState(false)
  const [batchTesting, setBatchTesting] = useState(false)
  const [batchRefreshingAll, setBatchRefreshingAll] = useState(false)
  const [cleaningBanned, setCleaningBanned] = useState(false)
  const [cleaningRateLimited, setCleaningRateLimited] = useState(false)
  const [cleaningError, setCleaningError] = useState(false)
  const [testingAccount, setTestingAccount] = useState<AccountRow | null>(null)
  const [authInfoLoadingIds, setAuthInfoLoadingIds] = useState<Set<number>>(new Set())
  const [quotaInfoLoadingIds, setQuotaInfoLoadingIds] = useState<Set<number>>(new Set())
  const [rawInfoDialog, setRawInfoDialog] = useState<{
    kind: 'auth' | 'quota'
    account: AccountRow
    fetchedAt: string
    refreshedFields: Record<string, string>
    rawEndpoint?: string
    planSource?: string
    rawText: string
  } | null>(null)
  const [usageAccount, setUsageAccount] = useState<AccountRow | null>(null)
  const [importing, setImporting] = useState(false)
  const [nowMs, setNowMs] = useState(() => Date.now())
  const [showImportPicker, setShowImportPicker] = useState(false)
  const [dragging, setDragging] = useState(false)
  const dragCounter = useRef(0)
  const [showExportPicker, setShowExportPicker] = useState(false)
  const [exporting, setExporting] = useState(false)
  const [showMigrate, setShowMigrate] = useState(false)
  const [migrateUrl, setMigrateUrl] = useState('')
  const [migrateKey, setMigrateKey] = useState('')
  const [migrating, setMigrating] = useState(false)
  const [importProgress, setImportProgress] = useState<{ show: boolean; current: number; total: number; success: number; duplicate: number; failed: number; done: boolean }>({ show: false, current: 0, total: 0, success: 0, duplicate: 0, failed: 0, done: false })
  const [addMethod, setAddMethod] = useState<'rt' | 'at' | 'oauth'>('rt')
  const [atForm, setAtForm] = useState<AddATAccountRequest>({
    access_token: '',
    proxy_url: '',
  })
  const [oauthStep, setOauthStep] = useState<'generate' | 'exchange'>('generate')
  const [oauthSession, setOauthSession] = useState<{ session_id: string; auth_url: string } | null>(null)
  const [oauthProxyUrl, setOauthProxyUrl] = useState('')
  const [oauthCallbackUrl, setOauthCallbackUrl] = useState('')
  const [oauthName, setOauthName] = useState('')
  const [oauthGenerating, setOauthGenerating] = useState(false)
  const [oauthCompleting, setOauthCompleting] = useState(false)
  const [quotaRates, setQuotaRates] = useState<QuotaRateConfig>({
    plus: 10,
    pro: 100,
    team: 10,
  })
  const fileInputRef = useRef<HTMLInputElement>(null)
  const jsonInputRef = useRef<HTMLInputElement>(null)
  const atFileInputRef = useRef<HTMLInputElement>(null)
  const folderInputRef = useRef<HTMLInputElement>(null)
  const { toast, showToast } = useToast()
  const { confirm, confirmDialog } = useConfirmDialog()

  const loadAccounts = useCallback(async () => {
    const data = await api.getAccounts()
    return data.accounts ?? []
  }, [])

  const { data: accounts, loading, error, reload, reloadSilently } = useDataLoader<AccountRow[]>({
    initialData: [],
    load: loadAccounts,
  })
  const usageBootstrapReloadedRef = useRef(false)

  const loadQuotaRates = useCallback(async () => {
    try {
      const settings = await api.getSettings()
      setQuotaRates({
        plus: normalizeRate(settings.quota_rate_plus ?? 10, 10),
        pro: normalizeRate(settings.quota_rate_pro ?? 100, 100),
        team: normalizeRate(settings.quota_rate_team ?? 10, 10),
      })
    } catch {
      setQuotaRates({
        plus: 10,
        pro: 100,
        team: 10,
      })
    }
  }, [])

  useEffect(() => {
    void loadQuotaRates()
  }, [loadQuotaRates])

  useEffect(() => {
    const hasMissingUsage = accounts.some(
      (account) => account.plan_type?.toLowerCase() === 'free' && (account.usage_percent_7d === null || account.usage_percent_7d === undefined)
    )
    if (!hasMissingUsage || usageBootstrapReloadedRef.current) {
      return
    }

    usageBootstrapReloadedRef.current = true
    const timer = window.setTimeout(() => {
      void reloadSilently()
    }, 4000)

    return () => window.clearTimeout(timer)
  }, [accounts, reloadSilently])

  useEffect(() => {
    const timer = window.setInterval(() => setNowMs(Date.now()), 1000)
    return () => window.clearInterval(timer)
  }, [])

  const totalAccounts = accounts.length
  const normalAccounts = accounts.filter((account) => account.status === 'active' || account.status === 'ready').length
  const rateLimitedAccounts = accounts.filter((account) => account.status === 'rate_limited').length
  const fullUsageAccounts = accounts.filter((account) => account.status === 'full_usage').length
  const bannedAccounts = accounts.filter((account) => account.status === 'unauthorized').length
  const lockedAccounts = accounts.filter((account) => account.locked).length
  const healthyAccounts = accounts.filter((account) => account.health_tier === 'healthy').length
  const warmAccounts = accounts.filter((account) => account.health_tier === 'warm').length
  const riskyAccounts = accounts.filter((account) => account.health_tier === 'risky').length
  const quotaSourceAccounts = accounts.filter((account) => (account.status || '').toLowerCase() !== 'unauthorized')
  const freeQuotaAccounts = quotaSourceAccounts.filter((account) => (account.plan_type || '').toLowerCase() === 'free')
  const paidQuotaAccounts = quotaSourceAccounts.filter((account) => (account.plan_type || '').toLowerCase() !== 'free')
  const freeQuotaStats = calcFreeQuotaStats(freeQuotaAccounts, quotaRates)
  const paidQuotaStats = calcWindowedQuotaStats(paidQuotaAccounts, quotaRates)
  const totalQuota = calcWeightedTotal(quotaSourceAccounts, quotaRates)
  const totalUsed5h = calcWeightedUsed5hEffective(quotaSourceAccounts, quotaRates)
  const totalUsed7d = calcWeightedUsed(quotaSourceAccounts, quotaRates, 'usage_percent_7d')
  const totalRemaining5h = Math.max(0, totalQuota - totalUsed5h)
  const totalRemaining7d = Math.max(0, totalQuota - totalUsed7d)
  const totalQuotaStats: QuotaStatsWindowed = {
    accountCount: quotaSourceAccounts.length,
    quotaTotal: roundTo2(totalQuota),
    usage5hUsed: roundTo2(totalUsed5h),
    usage5hRemaining: roundTo2(totalRemaining5h),
    usage5hUsedPercent: totalQuota > 0 ? roundTo2((totalUsed5h / totalQuota) * 100) : 0,
    usage5hRemainingPercent: totalQuota > 0 ? roundTo2((totalRemaining5h / totalQuota) * 100) : 0,
    usage5hUsedAccounts: roundTo2(totalUsed5h / 100),
    usage5hRemainingAccounts: roundTo2(totalRemaining5h / 100),
    usage7dUsed: roundTo2(totalUsed7d),
    usage7dRemaining: roundTo2(totalRemaining7d),
    usage7dUsedPercent: totalQuota > 0 ? roundTo2((totalUsed7d / totalQuota) * 100) : 0,
    usage7dRemainingPercent: totalQuota > 0 ? roundTo2((totalRemaining7d / totalQuota) * 100) : 0,
    usage7dUsedAccounts: roundTo2(totalUsed7d / 100),
    usage7dRemainingAccounts: roundTo2(totalRemaining7d / 100),
    waiting5hCount: calcWaitCount(quotaSourceAccounts, '5h'),
    waiting7dCount: calcWaitCount(quotaSourceAccounts, '7d'),
  }

  const filteredAccounts = accounts.filter((account) => {
    // 状态过滤
    switch (statusFilter) {
      case 'normal':
        if (account.status !== 'active' && account.status !== 'ready') return false
        break
      case 'rate_limited':
        if (account.status !== 'rate_limited') return false
        break
      case 'full_usage':
        if (account.status !== 'full_usage') return false
        break
      case 'banned':
        if (account.status !== 'unauthorized') return false
        break
      case 'locked':
        if (!account.locked) return false
        break
    }
    // 套餐过滤
    if (planFilter !== 'all') {
      const plan = (account.plan_type || '').toLowerCase()
      if (plan !== planFilter) return false
    }
    // 搜索过滤
    if (searchQuery) {
      const q = searchQuery.toLowerCase()
      const email = (account.email || '').toLowerCase()
      const name = (account.name || '').toLowerCase()
      if (!email.includes(q) && !name.includes(q)) return false
    }
    return true
  })

  const sortedAccounts = [...filteredAccounts].sort((a, b) => {
    if (!sortKey) return 0
    let diff = 0
    if (sortKey === 'requests') {
      diff = ((a.success_requests ?? 0) + (a.error_requests ?? 0)) - ((b.success_requests ?? 0) + (b.error_requests ?? 0))
    } else if (sortKey === 'usage') {
      diff = (a.usage_percent_7d ?? -1) - (b.usage_percent_7d ?? -1)
    } else if (sortKey === 'importTime') {
      diff = new Date(a.created_at || 0).getTime() - new Date(b.created_at || 0).getTime()
    }
    return sortDir === 'asc' ? diff : -diff
  })

  const totalPages = Math.max(1, Math.ceil(sortedAccounts.length / pageSize))
  const pagedAccounts = sortedAccounts.slice((page - 1) * pageSize, page * pageSize)
  const allPageSelected = pagedAccounts.length > 0 && pagedAccounts.every((a) => selected.has(a.id))

  const toggleSelect = (id: number) => {
    setSelected((prev) => {
      const next = new Set(prev)
      if (next.has(id)) next.delete(id)
      else next.add(id)
      return next
    })
  }

  const toggleSelectAll = () => {
    if (allPageSelected) {
      setSelected((prev) => {
        const next = new Set(prev)
        for (const a of pagedAccounts) next.delete(a.id)
        return next
      })
    } else {
      setSelected((prev) => {
        const next = new Set(prev)
        for (const a of pagedAccounts) next.add(a.id)
        return next
      })
    }
  }

  const handleAdd = async () => {
    if (!addForm.refresh_token.trim()) return
    setSubmitting(true)
    try {
      await api.addAccount(addForm)
      showToast(t('accounts.addSuccess'))
      setShowAdd(false)
      setAddForm({ refresh_token: '', proxy_url: '' })
      void reload()
    } catch (error) {
      showToast(t('accounts.addFailed', { error: getErrorMessage(error) }), 'error')
    } finally {
      setSubmitting(false)
    }
  }

  const handleAddAT = async () => {
    if (!atForm.access_token.trim()) return
    setSubmitting(true)
    try {
      await api.addATAccount(atForm)
      showToast(t('accounts.addSuccess'))
      setShowAdd(false)
      setAtForm({ access_token: '', proxy_url: '' })
      void reload()
    } catch (error) {
      showToast(t('accounts.addFailed', { error: getErrorMessage(error) }), 'error')
    } finally {
      setSubmitting(false)
    }
  }

  const handleOAuthGenerate = async () => {
    setOauthGenerating(true)
    try {
      const result = await api.generateOAuthURL({ proxy_url: oauthProxyUrl })
      setOauthSession(result)
      setOauthStep('exchange')
    } catch (error) {
      showToast(t('accounts.oauthFailed', { error: getErrorMessage(error) }), 'error')
    } finally {
      setOauthGenerating(false)
    }
  }

  const handleOAuthComplete = async () => {
    if (!oauthSession) return
    let code = ''
    let state = ''
    const raw = oauthCallbackUrl.trim()
    try {
      const url = new URL(raw)
      code = url.searchParams.get('code') ?? ''
      state = url.searchParams.get('state') ?? ''
    } catch {
      const qs = raw.includes('?') ? raw.split('?')[1] : raw
      const params = new URLSearchParams(qs)
      code = params.get('code') ?? ''
      state = params.get('state') ?? ''
    }
    if (!code || !state) {
      showToast(t('accounts.oauthParseError'), 'error')
      return
    }
    setOauthCompleting(true)
    try {
      const result = await api.exchangeOAuthCode({
        session_id: oauthSession.session_id,
        code,
        state,
        name: oauthName.trim() || undefined,
        proxy_url: oauthProxyUrl.trim() || undefined,
      })
      showToast(result.email ? t('accounts.oauthSuccess', { email: result.email }) : t('accounts.oauthSuccessNoEmail'))
      setShowAdd(false)
      setAddMethod('rt')
      setOauthStep('generate')
      setOauthSession(null)
      setOauthCallbackUrl('')
      setOauthName('')
      void reload()
    } catch (error) {
      showToast(t('accounts.oauthFailed', { error: getErrorMessage(error) }), 'error')
    } finally {
      setOauthCompleting(false)
    }
  }

  const readImportSSE = async (res: Response) => {
    setImportProgress({ show: true, current: 0, total: 0, success: 0, duplicate: 0, failed: 0, done: false })
    const reader = res.body?.getReader()
    if (!reader) return
    const decoder = new TextDecoder()
    let buffer = ''
    for (;;) {
      const { done, value } = await reader.read()
      if (done) break
      buffer += decoder.decode(value, { stream: true })
      const lines = buffer.split('\n')
      buffer = lines.pop() ?? ''
      for (const line of lines) {
        if (!line.startsWith('data: ')) continue
        try {
          const event = JSON.parse(line.slice(6)) as { type: string; current: number; total: number; success: number; duplicate: number; failed: number }
          setImportProgress(p => ({ ...p, current: event.current, total: event.total, success: event.success, duplicate: event.duplicate, failed: event.failed, done: event.type === 'complete' }))
          if (event.type === 'complete') void reload()
        } catch { /* 忽略解析异常 */ }
      }
    }
  }

  const importFiles = async (files: File[], format: 'txt' | 'json' | 'at_txt') => {
    setImporting(true)
    try {
      const formData = new FormData()
      if (format !== 'txt') formData.append('format', format)
      for (const f of files) formData.append('file', f)
      const res = await fetch('/api/admin/accounts/import', { method: 'POST', body: formData, headers: getAdminKey() ? { 'X-Admin-Key': getAdminKey() } : {} })
      if (res.headers.get('content-type')?.includes('text/event-stream')) {
        await readImportSSE(res)
      } else {
        const data = await res.json()
        if (!res.ok) {
          showToast(data.error ? t('accounts.importFailedWithReason', { error: data.error }) : t('accounts.importFailed'), 'error')
        } else {
          showToast(t('accounts.importCompleted'))
          void reload()
        }
      }
    } catch (error) {
      showToast(t('accounts.importFailedWithReason', { error: getErrorMessage(error) }), 'error')
    } finally {
      setImporting(false)
    }
  }

  const handleDragEnter = (e: DragEvent) => {
    e.preventDefault()
    e.stopPropagation()
    dragCounter.current++
    if (dragCounter.current === 1) setDragging(true)
  }

  const handleDragOver = (e: DragEvent) => {
    e.preventDefault()
    e.stopPropagation()
  }

  const handleDragLeave = (e: DragEvent) => {
    e.preventDefault()
    e.stopPropagation()
    dragCounter.current--
    if (dragCounter.current === 0) setDragging(false)
  }

  const readAllEntriesFromDirectory = (dirEntry: FileSystemDirectoryEntry): Promise<File[]> => {
    return new Promise((resolve) => {
      const files: File[] = []
      const readEntries = (reader: FileSystemDirectoryReader) => {
        reader.readEntries(async (entries) => {
          if (entries.length === 0) { resolve(files); return }
          for (const entry of entries) {
            if (entry.isFile) {
              const file = await new Promise<File>((res) => (entry as FileSystemFileEntry).file(res))
              files.push(file)
            } else if (entry.isDirectory) {
              const subFiles = await readAllEntriesFromDirectory(entry as FileSystemDirectoryEntry)
              files.push(...subFiles)
            }
          }
          readEntries(reader)
        })
      }
      readEntries(dirEntry.createReader())
    })
  }

  const handleDrop = async (e: DragEvent) => {
    e.preventDefault()
    e.stopPropagation()
    dragCounter.current = 0
    setDragging(false)
    if (importing) return

    // 检测是否拖入了文件夹
    const items = e.dataTransfer.items
    const hasDirectories = items && Array.from(items).some(
      item => item.webkitGetAsEntry?.()?.isDirectory
    )

    if (hasDirectories) {
      const allFiles: File[] = []
      for (const item of Array.from(items)) {
        const entry = item.webkitGetAsEntry?.()
        if (!entry) continue
        if (entry.isDirectory) {
          const dirFiles = await readAllEntriesFromDirectory(entry as FileSystemDirectoryEntry)
          allFiles.push(...dirFiles)
        } else if (entry.isFile) {
          const file = await new Promise<File>((res) => (entry as FileSystemFileEntry).file(res))
          allFiles.push(file)
        }
      }

      const validFiles = allFiles.filter(f => {
        const ext = f.name.split('.').pop()?.toLowerCase()
        return (ext === 'txt' || ext === 'json') && f.size > 0
      })

      if (validFiles.length === 0) {
        showToast(t('accounts.folderNoValidFiles'), 'error')
        return
      }

      const txtFiles = validFiles.filter(f => f.name.split('.').pop()?.toLowerCase() === 'txt')
      const jsonFiles = validFiles.filter(f => f.name.split('.').pop()?.toLowerCase() === 'json')

      if (jsonFiles.length > 0) {
        await importFiles([...jsonFiles, ...txtFiles], 'json')
      } else if (txtFiles.length > 0) {
        await importFiles(txtFiles, 'txt')
      }
      return
    }

    // 原有的文件拖放逻辑
    const files = Array.from(e.dataTransfer.files).filter(f => f.size > 0)
    if (files.length === 0) return

    const txtFiles: File[] = []
    const jsonFiles: File[] = []
    for (const f of files) {
      const ext = f.name.split('.').pop()?.toLowerCase()
      if (ext === 'txt') txtFiles.push(f)
      else if (ext === 'json') jsonFiles.push(f)
      else {
        showToast(t('accounts.unsupportedFileType', { name: f.name }), 'error')
        return
      }
    }

    if (jsonFiles.length > 0) {
      await importFiles([...jsonFiles, ...txtFiles], 'json')
    } else if (txtFiles.length > 0) {
      await importFiles(txtFiles, 'txt')
    }
  }

  const handleFileImport = async (event: ChangeEvent<HTMLInputElement>) => {
    const file = event.target.files?.[0]
    if (!file) return
    if (!file.name.endsWith('.txt')) {
      showToast(t('accounts.selectTxtFile'), 'error')
      return
    }
    setShowImportPicker(false)
    await importFiles([file], 'txt')
    if (fileInputRef.current) fileInputRef.current.value = ''
  }

  const handleJsonImport = async (event: ChangeEvent<HTMLInputElement>) => {
    const files = event.target.files
    if (!files || files.length === 0) return
    setShowImportPicker(false)
    await importFiles(Array.from(files), 'json')
    if (jsonInputRef.current) jsonInputRef.current.value = ''
  }

  const handleAtFileImport = async (event: ChangeEvent<HTMLInputElement>) => {
    const file = event.target.files?.[0]
    if (!file) return
    if (!file.name.endsWith('.txt')) {
      showToast(t('accounts.selectTxtFile'), 'error')
      return
    }
    setShowImportPicker(false)
    await importFiles([file], 'at_txt')
    if (atFileInputRef.current) atFileInputRef.current.value = ''
  }

  const handleFolderImport = async (event: ChangeEvent<HTMLInputElement>) => {
    const files = event.target.files
    if (!files || files.length === 0) return
    setShowImportPicker(false)

    const validFiles = Array.from(files).filter(f => {
      const ext = f.name.split('.').pop()?.toLowerCase()
      return (ext === 'txt' || ext === 'json') && f.size > 0
    })

    if (validFiles.length === 0) {
      showToast(t('accounts.folderNoValidFiles'), 'error')
      if (folderInputRef.current) folderInputRef.current.value = ''
      return
    }

    const txtFiles = validFiles.filter(f => f.name.split('.').pop()?.toLowerCase() === 'txt')
    const jsonFiles = validFiles.filter(f => f.name.split('.').pop()?.toLowerCase() === 'json')

    if (jsonFiles.length > 0) {
      await importFiles([...jsonFiles, ...txtFiles], 'json')
    } else if (txtFiles.length > 0) {
      await importFiles(txtFiles, 'txt')
    }

    if (folderInputRef.current) folderInputRef.current.value = ''
  }

  const handleExport = async (format: 'json' | 'txt', scope: 'healthy' | 'selected') => {
    setExporting(true)
    setShowExportPicker(false)
    try {
      const params: { filter: 'healthy' | 'all'; ids?: number[] } = {
        filter: scope === 'healthy' ? 'healthy' : 'all',
      }
      if (scope === 'selected') {
        params.ids = Array.from(selected)
        params.filter = 'all'
      }
      const data = await api.exportAccounts(params)
      if (data.length === 0) {
        showToast(t('accounts.exportNoAccounts'), 'error')
        return
      }
      const ts = new Date().toISOString().replace(/[:.]/g, '-').slice(0, 19)
      if (format === 'json') {
        const blob = new Blob([JSON.stringify(data, null, 2)], { type: 'application/json' })
        downloadBlob(blob, `cpa-${ts}-${data.length}.json`)
      } else {
        const text = data
          .map(e => e.refresh_token || e.access_token || '')
          .filter(Boolean)
          .join('\n')
        const blob = new Blob([text], { type: 'text/plain' })
        downloadBlob(blob, `token-${ts}-${data.length}.txt`)
      }
      showToast(t('accounts.exportSuccess', { count: data.length }))
    } catch (error) {
      showToast(`${t('accounts.exportFailed')}: ${getErrorMessage(error)}`, 'error')
    } finally {
      setExporting(false)
    }
  }

  const handleMigrate = async () => {
    setMigrating(true)
    setShowMigrate(false)
    try {
      const res = await fetch('/api/admin/accounts/migrate', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', ...(getAdminKey() ? { 'X-Admin-Key': getAdminKey() } : {}) },
        body: JSON.stringify({ url: migrateUrl.trim(), admin_key: migrateKey.trim() }),
      })
      if (res.headers.get('content-type')?.includes('text/event-stream')) {
        await readImportSSE(res)
      } else {
        const data = await res.json()
        if (!res.ok) {
          showToast(data.error ? `${t('accounts.migrateFailed')}: ${data.error}` : t('accounts.migrateFailed'), 'error')
        } else {
          showToast(t('accounts.migrateSuccess', { imported: data.imported ?? 0, duplicate: data.duplicate ?? 0, failed: data.failed ?? 0 }))
          void reload()
        }
      }
    } catch (error) {
      showToast(`${t('accounts.migrateFailed')}: ${getErrorMessage(error)}`, 'error')
    } finally {
      setMigrating(false)
      setMigrateUrl('')
      setMigrateKey('')
    }
  }

  const handleDelete = async (account: AccountRow) => {
    const confirmed = await confirm({
      title: t('accounts.deleteTitle'),
      description: t('accounts.deleteDesc', { account: account.email || `ID ${account.id}` }),
      confirmText: t('accounts.deleteConfirm'),
      tone: 'destructive',
      confirmVariant: 'destructive',
    })
    if (!confirmed) return
    try {
      await api.deleteAccount(account.id)
      showToast(t('accounts.deleted'))
      void reload()
    } catch (error) {
      showToast(t('accounts.deleteFailed', { error: getErrorMessage(error) }), 'error')
    }
  }

  const handleRefresh = async (account: AccountRow) => {
    setRefreshingIds((prev) => new Set(prev).add(account.id))
    try {
      const result = await api.refreshAccount(account.id)
      showToast(result.message || t('accounts.refreshRequested'))
      void reloadSilently()
    } catch (error) {
      showToast(t('accounts.refreshFailed', { error: getErrorMessage(error) }), 'error')
    } finally {
      setRefreshingIds((prev) => {
        const next = new Set(prev)
        next.delete(account.id)
        return next
      })
    }
  }

  const openRawInfoDialog = (
    kind: 'auth' | 'quota',
    account: AccountRow,
    result: Awaited<ReturnType<typeof api.getAccountAuthInfo>>,
  ) => {
    const rawText = JSON.stringify(result.raw, null, 2)
    setRawInfoDialog({
      kind,
      account,
      fetchedAt: result.fetched_at,
      refreshedFields: result.refreshed_fields ?? {},
      rawEndpoint: result.raw_endpoint,
      planSource: result.plan_source,
      rawText: rawText || '{}',
    })
  }

  const handleViewAuthInfo = async (account: AccountRow) => {
    setAuthInfoLoadingIds((prev) => new Set(prev).add(account.id))
    try {
      const result = await api.getAccountAuthInfo(account.id)
      openRawInfoDialog('auth', account, result)
      showToast(result.message || t('accounts.rawInfoFetchSuccess'))
      void reloadSilently()
    } catch (error) {
      showToast(t('accounts.rawInfoFetchFailed', { error: getErrorMessage(error) }), 'error')
    } finally {
      setAuthInfoLoadingIds((prev) => {
        const next = new Set(prev)
        next.delete(account.id)
        return next
      })
    }
  }

  const handleViewQuotaInfo = async (account: AccountRow) => {
    setQuotaInfoLoadingIds((prev) => new Set(prev).add(account.id))
    try {
      const result = await api.getAccountQuotaInfo(account.id)
      openRawInfoDialog('quota', account, result)
      showToast(result.message || t('accounts.rawInfoFetchSuccess'))
      void reloadSilently()
    } catch (error) {
      showToast(t('accounts.rawInfoFetchFailed', { error: getErrorMessage(error) }), 'error')
    } finally {
      setQuotaInfoLoadingIds((prev) => {
        const next = new Set(prev)
        next.delete(account.id)
        return next
      })
    }
  }

  const handleToggleLock = async (account: AccountRow) => {
    const newLocked = !account.locked
    try {
      await api.toggleAccountLock(account.id, newLocked)
      showToast(newLocked ? t('accounts.lockSuccess') : t('accounts.unlockSuccess'))
      void reload()
    } catch (error) {
      showToast(t('accounts.lockFailed', { error: getErrorMessage(error) }), 'error')
    }
  }

  const handleBatchDelete = async () => {
    if (selected.size === 0) return
    const confirmed = await confirm({
      title: t('accounts.batchDeleteTitle'),
      description: t('accounts.batchDeleteDesc', { count: selected.size }),
      confirmText: t('accounts.deleteConfirm'),
      tone: 'destructive',
      confirmVariant: 'destructive',
    })
    if (!confirmed) return
    setBatchLoading(true)
    let success = 0
    let fail = 0
    for (const id of selected) {
      try {
        await api.deleteAccount(id)
        success++
      } catch {
        fail++
      }
    }
    showToast(t('accounts.batchDeleteDone', { success, fail }))
    setSelected(new Set())
    setBatchLoading(false)
    void reload()
  }

  const handleBatchRefresh = async () => {
    if (selected.size === 0) return
    setBatchLoading(true)
    let success = 0
    let fail = 0
    for (const id of selected) {
      try {
        await api.refreshAccount(id)
        success++
      } catch {
        fail++
      }
    }
    showToast(t('accounts.batchRefreshDone', { success, fail }))
    setBatchLoading(false)
    void reload()
  }

  const handleBatchLock = async (locked: boolean) => {
    if (selected.size === 0) return
    setBatchLoading(true)
    let success = 0
    let fail = 0
    for (const id of selected) {
      try {
        await api.toggleAccountLock(id, locked)
        success++
      } catch {
        fail++
      }
    }
    showToast(t(locked ? 'accounts.batchLockDone' : 'accounts.batchUnlockDone', { success, fail }))
    setBatchLoading(false)
    setSelected(new Set())
    void reload()
  }

  const handleBatchTest = async () => {
    setBatchTesting(true)
    try {
      const result = await api.batchTestAccounts()
      showToast(t('accounts.batchTestDone', {
        success: result.success,
        banned: result.banned,
        rateLimited: result.rate_limited,
        failed: result.failed,
      }))
      void reload()
    } catch (error) {
      showToast(t('accounts.batchTestFailed', { error: getErrorMessage(error) }), 'error')
    } finally {
      setBatchTesting(false)
    }
  }

  const handleBatchRefreshAll = async () => {
    setBatchRefreshingAll(true)
    try {
      const result = await api.batchRefreshAccounts()
      showToast(t('accounts.batchRefreshDone', { success: result.success, fail: result.failed }))
      void reload()
    } catch (error) {
      showToast(t('accounts.batchRefreshFailed', { error: getErrorMessage(error) }), 'error')
    } finally {
      setBatchRefreshingAll(false)
    }
  }

  const handleCleanBanned = async () => {
    const confirmed = await confirm({
      title: t('accounts.cleanBannedTitle'),
      description: t('accounts.cleanBannedDesc'),
      confirmText: t('accounts.cleanConfirm'),
      tone: 'warning',
    })
    if (!confirmed) return
    setCleaningBanned(true)
    try {
      await api.cleanBanned()
      showToast(t('accounts.cleanBannedSuccess'))
      void reload()
    } catch (error) {
      showToast(t('accounts.cleanBannedFailed', { error: getErrorMessage(error) }), 'error')
    } finally {
      setCleaningBanned(false)
    }
  }

  const handleCleanRateLimited = async () => {
    const confirmed = await confirm({
      title: t('accounts.cleanRateLimitedTitle'),
      description: t('accounts.cleanRateLimitedDesc'),
      confirmText: t('accounts.cleanConfirm'),
      tone: 'warning',
    })
    if (!confirmed) return
    setCleaningRateLimited(true)
    try {
      await api.cleanRateLimited()
      showToast(t('accounts.cleanRateLimitedSuccess'))
      void reload()
    } catch (error) {
      showToast(t('accounts.cleanRateLimitedFailed', { error: getErrorMessage(error) }), 'error')
    } finally {
      setCleaningRateLimited(false)
    }
  }

  const handleCleanError = async () => {
    const confirmed = await confirm({
      title: t('accounts.cleanErrorTitle'),
      description: t('accounts.cleanErrorDesc'),
      confirmText: t('accounts.cleanConfirm'),
      tone: 'warning',
    })
    if (!confirmed) return
    setCleaningError(true)
    try {
      await api.cleanError()
      showToast(t('accounts.cleanErrorSuccess'))
      void reload()
    } catch (error) {
      showToast(t('accounts.cleanErrorFailed', { error: getErrorMessage(error) }), 'error')
    } finally {
      setCleaningError(false)
    }
  }

  return (
    <div
      className="relative"
      onDragEnter={handleDragEnter}
      onDragOver={handleDragOver}
      onDragLeave={handleDragLeave}
      onDrop={(e) => void handleDrop(e)}
    >
      {dragging && (
        <div className="pointer-events-none absolute inset-0 z-50 flex items-center justify-center rounded-2xl border-2 border-dashed border-primary bg-primary/5 backdrop-blur-sm">
          <div className="flex flex-col items-center gap-2 text-primary">
            <Upload className="size-10" />
            <span className="text-lg font-semibold">{t('accounts.dropToImport')}</span>
            <span className="text-sm text-muted-foreground">{t('accounts.dropHint')}</span>
          </div>
        </div>
      )}
    <StateShell
      variant="page"
      loading={loading}
      error={error}
      onRetry={() => void reload()}
      loadingTitle={t('accounts.loadingTitle')}
      loadingDescription={t('accounts.loadingDesc')}
      errorTitle={t('accounts.errorTitle')}
    >
      <>
        <PageHeader
          title={t('accounts.title')}
          description={t('accounts.description')}
          onRefresh={() => void reload()}
          actions={(
            <div className="flex items-center gap-1.5">
              <Button variant="outline" size="sm" disabled={batchTesting} onClick={() => void handleBatchTest()}>
                <FlaskConical className="size-3" />
                {batchTesting ? t('accounts.batchTesting') : t('accounts.batchTest')}
              </Button>
              <Button variant="outline" size="sm" disabled={batchRefreshingAll} onClick={() => void handleBatchRefreshAll()}>
                <RefreshCw className="size-3" />
                {batchRefreshingAll ? t('accounts.refreshing') : t('accounts.batchRefresh')}
              </Button>
              <Button variant="outline" size="sm" disabled={cleaningBanned} onClick={() => void handleCleanBanned()}>
                <Ban className="size-3" />
                {cleaningBanned ? t('accounts.cleaning') : t('accounts.cleanBanned')}
              </Button>
              <Button variant="outline" size="sm" disabled={cleaningRateLimited} onClick={() => void handleCleanRateLimited()}>
                <Timer className="size-3" />
                {cleaningRateLimited ? t('accounts.cleaning') : t('accounts.cleanRateLimited')}
              </Button>
              <Button variant="outline" size="sm" disabled={cleaningError} onClick={() => void handleCleanError()}>
                <AlertTriangle className="size-3" />
                {cleaningError ? t('accounts.cleaning') : t('accounts.cleanError')}
              </Button>
              <Button onClick={() => setShowAdd(true)}>
                <Plus className="size-3.5" />
                {t('accounts.addAccount')}
              </Button>
              <Button variant="outline" disabled={importing} onClick={() => setShowImportPicker(true)}>
                <Upload className="size-3.5" />
                {importing ? t('accounts.importing') : t('accounts.importFile')}
              </Button>
              <Button variant="outline" disabled={exporting} onClick={() => setShowExportPicker(true)}>
                <Download className="size-3.5" />
                {exporting ? t('accounts.exporting') : t('accounts.export')}
              </Button>
              <Button variant="outline" disabled={migrating} onClick={() => setShowMigrate(true)}>
                <ArrowDownToLine className="size-3.5" />
                {migrating ? t('accounts.migrating') : t('accounts.migrateImport')}
              </Button>
              <input
                ref={fileInputRef}
                type="file"
                accept=".txt"
                className="hidden"
                onChange={(e) => void handleFileImport(e)}
              />
              <input
                ref={jsonInputRef}
                type="file"
                accept=".json"
                multiple
                className="hidden"
                onChange={(e) => void handleJsonImport(e)}
              />
              <input
                ref={atFileInputRef}
                type="file"
                accept=".txt"
                className="hidden"
                onChange={(e) => void handleAtFileImport(e)}
              />
              <input
                ref={folderInputRef}
                type="file"
                className="hidden"
                onChange={(e) => void handleFolderImport(e)}
                {...({ webkitdirectory: '', directory: '' } as React.InputHTMLAttributes<HTMLInputElement>)}
              />
            </div>
          )}
        />

        <div className="mb-4 grid grid-cols-2 gap-3 xl:grid-cols-5">
          <CompactStat label={t('accounts.totalAccounts')} chipLabel={t('accounts.filterAll')} value={totalAccounts} tone="neutral" />
          <CompactStat label={t('accounts.normalAccounts')} chipLabel={t('accounts.filterNormal')} value={normalAccounts} tone="success" />
          <CompactStat label={t('accounts.rateLimited')} chipLabel={t('accounts.filterRateLimited')} value={rateLimitedAccounts} tone="warning" />
          <CompactStat label={t('accounts.fullUsageAccounts')} chipLabel={t('accounts.filterFullUsage')} value={fullUsageAccounts} tone="warning" />
          <CompactStat label={t('accounts.bannedAccounts')} chipLabel={t('accounts.filterBanned')} value={bannedAccounts} tone="danger" />
        </div>

        <div className="mb-2 text-[12px] font-semibold text-muted-foreground">{t('accounts.quotaSectionFreeTitle')}</div>
        <div className="mb-4 grid grid-cols-2 gap-3 xl:grid-cols-5">
          <CompactStat label={t('accounts.quotaAccountCount')} value={freeQuotaStats.accountCount} tone="neutral" />
          <CompactStat label={t('accounts.quotaTotal')} value={formatMetric(freeQuotaStats.quotaTotal)} tone="neutral" />
          <CompactStat label={t('accounts.quotaRemaining')} value={formatMetric(freeQuotaStats.quotaRemaining)} tone="success" />
          <CompactStat label={t('accounts.quotaRemainingPercent')} value={`${freeQuotaStats.quotaRemainingPercent.toFixed(2)}%`} tone="cyan" />
          <CompactStat label={t('accounts.quotaRemainingAccounts')} value={formatMetric(freeQuotaStats.quotaRemainingAccounts)} tone="cyan" />
        </div>

        <div className="mb-2 text-[12px] font-semibold text-muted-foreground">{t('accounts.quotaSectionPaidTitle')}</div>
        <div className="mb-4 grid grid-cols-2 gap-3 xl:grid-cols-5">
          <CompactStat label={t('accounts.quotaAccountCount')} value={paidQuotaStats.accountCount} tone="neutral" />
          <CompactStat label={t('accounts.quotaTotal')} value={formatMetric(paidQuotaStats.quotaTotal)} tone="neutral" />
          <CompactStat label={t('accounts.quota5hRemaining')} value={formatMetric(paidQuotaStats.usage5hRemaining)} tone="success" />
          <CompactStat label={t('accounts.quota5hRemainingPercent')} value={`${paidQuotaStats.usage5hRemainingPercent.toFixed(2)}%`} tone="cyan" />
          <CompactStat label={t('accounts.quota5hRemainingAccounts')} value={formatMetric(paidQuotaStats.usage5hRemainingAccounts)} tone="cyan" />
          <CompactStat label={t('accounts.quota5hWaitingCount')} value={paidQuotaStats.waiting5hCount} tone="warning" />
          <CompactStat label={t('accounts.quota7dWaitingCount')} value={paidQuotaStats.waiting7dCount} tone="warning" />
          <CompactStat label={t('accounts.quota7dRemaining')} value={formatMetric(paidQuotaStats.usage7dRemaining)} tone="success" />
          <CompactStat label={t('accounts.quota7dRemainingPercent')} value={`${paidQuotaStats.usage7dRemainingPercent.toFixed(2)}%`} tone="cyan" />
          <CompactStat label={t('accounts.quota7dRemainingAccounts')} value={formatMetric(paidQuotaStats.usage7dRemainingAccounts)} tone="cyan" />
        </div>

        <div className="mb-2 text-[12px] font-semibold text-muted-foreground">{t('accounts.quotaSectionTotalTitle')}</div>
        <div className="mb-4 grid grid-cols-2 gap-3 xl:grid-cols-5">
          <CompactStat label={t('accounts.quotaAccountCount')} value={totalQuotaStats.accountCount} tone="neutral" />
          <CompactStat label={t('accounts.quotaTotal')} value={formatMetric(totalQuotaStats.quotaTotal)} tone="neutral" />
          <CompactStat label={t('accounts.quota5hRemaining')} value={formatMetric(totalQuotaStats.usage5hRemaining)} tone="success" />
          <CompactStat label={t('accounts.quota5hRemainingPercent')} value={`${totalQuotaStats.usage5hRemainingPercent.toFixed(2)}%`} tone="cyan" />
          <CompactStat label={t('accounts.quota5hRemainingAccounts')} value={formatMetric(totalQuotaStats.usage5hRemainingAccounts)} tone="cyan" />
          <CompactStat label={t('accounts.quota5hWaitingCount')} value={totalQuotaStats.waiting5hCount} tone="warning" />
          <CompactStat label={t('accounts.quota7dWaitingCount')} value={totalQuotaStats.waiting7dCount} tone="warning" />
          <CompactStat label={t('accounts.quota7dRemaining')} value={formatMetric(totalQuotaStats.usage7dRemaining)} tone="success" />
          <CompactStat label={t('accounts.quota7dRemainingPercent')} value={`${totalQuotaStats.usage7dRemainingPercent.toFixed(2)}%`} tone="cyan" />
          <CompactStat label={t('accounts.quota7dRemainingAccounts')} value={formatMetric(totalQuotaStats.usage7dRemainingAccounts)} tone="cyan" />
        </div>

        <div className="mb-4 flex flex-wrap items-center gap-2 rounded-2xl border border-border bg-white/55 px-4 py-3 text-[12px] text-muted-foreground shadow-[inset_0_1px_0_rgba(255,255,255,0.72)]">
          <span className="font-semibold text-foreground">{t('accounts.filter')}</span>
          {([['all', t('accounts.filterAll')], ['normal', t('accounts.filterNormal')], ['rate_limited', t('accounts.filterRateLimited')], ['full_usage', t('accounts.filterFullUsage')], ['banned', t('accounts.filterBanned')], ['locked', t('accounts.filterLocked')]] as const).map(([key, label]) => (
            <button
              key={key}
              onClick={() => { setStatusFilter(key); setPage(1) }}
              className={`rounded-full px-3 py-1 font-semibold transition-colors ${
                statusFilter === key
                  ? 'bg-primary text-primary-foreground'
                  : 'bg-muted/50 text-muted-foreground hover:bg-muted'
              }`}
            >
              {label} {key === 'all' ? totalAccounts : key === 'normal' ? normalAccounts : key === 'rate_limited' ? rateLimitedAccounts : key === 'full_usage' ? fullUsageAccounts : key === 'banned' ? bannedAccounts : lockedAccounts}
            </button>
          ))}
        </div>

        <div className="mb-4 flex flex-wrap items-center gap-2 rounded-2xl border border-border bg-white/55 px-4 py-3 text-[12px] text-muted-foreground shadow-[inset_0_1px_0_rgba(255,255,255,0.72)]">
          <span className="font-semibold text-foreground">{t('accounts.schedulerView')}</span>
          <SchedulerChip label={t('accounts.healthy')} value={healthyAccounts} tone="success" />
          <SchedulerChip label={t('accounts.warm')} value={warmAccounts} tone="warning" />
          <SchedulerChip label={t('accounts.risky')} value={riskyAccounts} tone="danger" />
          <SchedulerChip label={t('status.unauthorized')} value={bannedAccounts} tone="neutral" />
        </div>

        <div className="mb-4 flex items-center gap-2">
          <div className="relative w-64">
            <Search className="absolute left-3 top-1/2 -translate-y-1/2 size-4 text-muted-foreground pointer-events-none" />
            <Input
              className="pl-9 h-8 rounded-lg text-[13px]"
              placeholder={t('accounts.searchPlaceholder')}
              value={searchQuery}
              onChange={(e: ChangeEvent<HTMLInputElement>) => { setSearchQuery(e.target.value); setPage(1) }}
            />
          </div>
          <div className="flex items-center gap-1 rounded-lg border border-border bg-muted/30 p-0.5">
            {(['all', 'plus', 'pro', 'team', 'free'] as const).map((key) => (
              <button
                key={key}
                onClick={() => { setPlanFilter(key); setPage(1) }}
                className={`rounded-md px-2.5 py-1 text-[12px] font-medium transition-colors ${
                  planFilter === key
                    ? 'bg-background shadow-sm text-foreground'
                    : 'text-muted-foreground hover:text-foreground'
                }`}
              >
                {key === 'all' ? t('accounts.filterAll') : key.charAt(0).toUpperCase() + key.slice(1)}
              </button>
            ))}
          </div>
          <div className="ml-auto flex items-center gap-2 rounded-lg border border-border bg-muted/30 px-2 py-1">
            <span className="text-[12px] text-muted-foreground">{t('accounts.pageSizeLabel')}</span>
            <select
              value={pageSize}
              onChange={(event) => {
                const next = Number(event.target.value)
                if (!Number.isFinite(next)) return
                const clamped = Math.min(PAGE_SIZE_MAX, Math.max(PAGE_SIZE_MIN, Math.floor(next)))
                setPageSize(clamped)
                setPage(1)
              }}
              className="h-7 min-w-[86px] rounded-md border border-input bg-background px-2 text-[12px] text-foreground outline-none focus:ring-2 focus:ring-ring"
            >
              {PAGE_SIZE_OPTIONS.map((size) => (
                <option key={size} value={size}>
                  {size}
                </option>
              ))}
            </select>
          </div>
        </div>

        <Pagination
          page={page}
          totalPages={totalPages}
          onPageChange={setPage}
          totalItems={accounts.length}
          pageSize={pageSize}
        />

        {selected.size > 0 && (
          <div className="flex items-center justify-between gap-3 px-4 py-2.5 mb-4 rounded-2xl bg-primary/10 border border-primary/20 text-sm font-semibold text-primary">
            <span>{t('common.selected', { count: selected.size })}</span>
            <div className="flex items-center gap-1.5">
              <Button variant="outline" size="sm" disabled={batchLoading} onClick={() => void handleBatchRefresh()}>
                {t('accounts.batchRefresh')}
              </Button>
              <Button variant="outline" size="sm" disabled={batchLoading} onClick={() => void handleBatchLock(true)}>
                <Lock className="size-3 mr-1" />{t('accounts.lock')}
              </Button>
              <Button variant="outline" size="sm" disabled={batchLoading} onClick={() => void handleBatchLock(false)}>
                <Unlock className="size-3 mr-1" />{t('accounts.unlock')}
              </Button>
              <Button variant="destructive" size="sm" disabled={batchLoading} onClick={() => void handleBatchDelete()}>
                {t('accounts.batchDelete')}
              </Button>
              <Button variant="outline" size="sm" onClick={() => setSelected(new Set())}>
                {t('accounts.cancelSelection')}
              </Button>
            </div>
          </div>
        )}

        <Card>
          <CardContent className="p-6">
            <StateShell
              variant="section"
              isEmpty={accounts.length === 0}
              emptyTitle={t('accounts.noData')}
              emptyDescription={t('accounts.noDataDesc')}
              action={<Button onClick={() => setShowAdd(true)}>{t('accounts.addAccount')}</Button>}
            >
              <div className="overflow-auto border border-border rounded-xl">
                <Table>
                  <TableHeader>
                    <TableRow>
                      <TableHead className="w-10">
                        <input
                          type="checkbox"
                          className="size-4 cursor-pointer accent-[hsl(var(--primary))]"
                          checked={allPageSelected}
                          onChange={toggleSelectAll}
                        />
                      </TableHead>
                      <TableHead className="text-[13px] font-semibold">ID</TableHead>
                      <TableHead className="text-[13px] font-semibold">{t('accounts.email')}</TableHead>
                      <TableHead className="text-[13px] font-semibold">{t('accounts.plan')}</TableHead>
                      <TableHead className="text-[13px] font-semibold">{t('accounts.status')}</TableHead>
                      <TableHead
                        className="text-[13px] font-semibold cursor-pointer select-none hover:text-primary transition-colors"
                        onClick={() => { if (sortKey === 'requests') { setSortDir(d => d === 'asc' ? 'desc' : 'asc') } else { setSortKey('requests'); setSortDir('desc') }; setPage(1) }}
                      >
                        {t('accounts.requests')} {sortKey === 'requests' ? (sortDir === 'desc' ? '↓' : '↑') : ''}
                      </TableHead>
                      <TableHead
                        className="text-[13px] font-semibold cursor-pointer select-none hover:text-primary transition-colors"
                        onClick={() => { if (sortKey === 'usage') { setSortDir(d => d === 'asc' ? 'desc' : 'asc') } else { setSortKey('usage'); setSortDir('desc') }; setPage(1) }}
                      >
                        {t('accounts.usage')} {sortKey === 'usage' ? (sortDir === 'desc' ? '↓' : '↑') : ''}
                      </TableHead>
                      <TableHead
                        className="text-[13px] font-semibold cursor-pointer select-none hover:text-primary transition-colors"
                        onClick={() => { if (sortKey === 'importTime') { setSortDir(d => d === 'asc' ? 'desc' : 'asc') } else { setSortKey('importTime'); setSortDir('desc') }; setPage(1) }}
                      >
                        {t('accounts.importTime')} {sortKey === 'importTime' ? (sortDir === 'desc' ? '↓' : '↑') : ''}
                      </TableHead>
                      <TableHead className="text-[13px] font-semibold">{t('accounts.updatedAt')}</TableHead>
                      <TableHead className="text-[13px] font-semibold text-right">{t('accounts.actions')}</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {pagedAccounts.map((account) => {
                      const waitReasonKey = account.wait_reason || account.status || 'cooldown'
                      const waitRemainingSeconds = (() => {
                        if (account.wait_until) {
                          const untilMs = new Date(account.wait_until).getTime()
                          if (!Number.isNaN(untilMs)) {
                            return Math.max(0, Math.floor((untilMs - nowMs) / 1000))
                          }
                        }
                        return Math.max(0, Math.floor(account.wait_remaining_seconds ?? 0))
                      })()
                      const waitProbeAt = account.wait_probe_at || account.wait_until
                      const waitProbeRemainingSeconds = (() => {
                        if (waitProbeAt) {
                          const probeMs = new Date(waitProbeAt).getTime()
                          if (!Number.isNaN(probeMs)) {
                            return Math.max(0, Math.floor((probeMs - nowMs) / 1000))
                          }
                        }
                        return Math.max(0, Math.floor(account.wait_probe_remaining_seconds ?? 0))
                      })()

                      return (
                        <TableRow key={account.id} className={selected.has(account.id) ? 'bg-primary/5' : ''}>
                        <TableCell>
                          <input
                            type="checkbox"
                            className="size-4 cursor-pointer accent-[hsl(var(--primary))]"
                            checked={selected.has(account.id)}
                            onChange={() => toggleSelect(account.id)}
                          />
                        </TableCell>
                        <TableCell className="text-[14px] font-mono text-muted-foreground">{account.id}</TableCell>
                        <TableCell className="text-[14px] text-muted-foreground">
                          {formatCompactEmail(account.email)}
                          {account.at_only && (
                            <span className="ml-1.5 inline-flex items-center rounded-md bg-amber-50 px-1.5 py-0.5 text-[10px] font-medium text-amber-700 ring-1 ring-inset ring-amber-600/20 dark:bg-amber-950 dark:text-amber-400 dark:ring-amber-400/20">
                              AT
                            </span>
                          )}
                          {account.locked && (
                            <span className="ml-1.5 inline-flex items-center rounded-md bg-blue-50 px-1.5 py-0.5 text-[10px] font-medium text-blue-700 ring-1 ring-inset ring-blue-600/20 dark:bg-blue-950 dark:text-blue-400 dark:ring-blue-400/20">
                              <Lock className="size-2.5 mr-0.5" />{t('accounts.lock')}
                            </span>
                          )}
                        </TableCell>
                        <TableCell
                          className="text-[13px] font-medium"
                        >
                          {account.plan_type || '-'}
                        </TableCell>
                        <TableCell>
                          <div className="space-y-1">
                            <StatusBadge status={account.status} />
                            <div className="text-[11px] text-muted-foreground">
                              {t('accounts.healthSummary', {
                                health: formatHealthTier(account.health_tier, t),
                                score: Math.round(account.scheduler_score ?? 0),
                                concurrency: account.dynamic_concurrency_limit ?? '-',
                              })}
                            </div>
                            {account.wait_mode ? (
                              <div className="space-y-0.5 text-[11px] text-amber-700">
                                <div>
                                  {t('accounts.waitingModeDetail', {
                                    reason: t(`status.${waitReasonKey}`, { defaultValue: waitReasonKey }),
                                    left: formatWaitRemaining(waitRemainingSeconds, t),
                                  })}
                                </div>
                                <div>
                                  {t('accounts.waitProbeAt', {
                                    time: formatWaitPoint(waitProbeAt),
                                    left: formatWaitRemaining(waitProbeRemainingSeconds, t),
                                  })}
                                </div>
                                <div>
                                  {t('accounts.waitExitAt', {
                                    time: formatWaitPoint(account.wait_until),
                                  })}
                                </div>
                              </div>
                            ) : null}
                            {account.last_failure_message ? (
                              <div className="text-[11px] text-red-600 break-all">
                                {`${account.last_failure_status ?? ''} ${account.last_failure_message}`.trim()}
                              </div>
                            ) : null}
                          </div>
                        </TableCell>
                        <TableCell>
                          <div className="flex items-center gap-2 text-[13px]">
                            <span className="text-emerald-600 font-medium">{account.success_requests ?? 0}</span>
                            <span className="text-muted-foreground">/</span>
                            <span className="text-red-500 font-medium">{account.error_requests ?? 0}</span>
                          </div>
                        </TableCell>
                        <TableCell>
                          <UsageCell account={account} t={t} />
                        </TableCell>
                        <TableCell className="text-[13px] text-muted-foreground whitespace-nowrap">{formatBeijingTime(account.created_at)}</TableCell>
                        <TableCell className="text-[14px] text-muted-foreground">{formatRelativeTime(account.updated_at)}</TableCell>
                        <TableCell className="text-right">
                          <div className="flex items-center gap-1 justify-end">
                            <Button
                              variant="outline"
                              size="icon"
                              className="h-7 w-8 px-0"
                              disabled={authInfoLoadingIds.has(account.id)}
                              onClick={() => void handleViewAuthInfo(account)}
                              title={t('accounts.viewAuthInfo')}
                            >
                              <FileText className={`size-3.5 ${authInfoLoadingIds.has(account.id) ? 'animate-spin' : ''}`} />
                            </Button>
                            <Button
                              variant="outline"
                              size="icon"
                              className="h-7 w-8 px-0"
                              disabled={quotaInfoLoadingIds.has(account.id)}
                              onClick={() => void handleViewQuotaInfo(account)}
                              title={t('accounts.viewQuotaInfo')}
                            >
                              <FileJson className={`size-3.5 ${quotaInfoLoadingIds.has(account.id) ? 'animate-spin' : ''}`} />
                            </Button>
                            <Button
                              variant="outline"
                              size="icon"
                              className="h-7 w-8 px-0"
                              onClick={() => setUsageAccount(account)}
                              title={t('accounts.usageDetail')}
                            >
                              <BarChart3 className="size-3.5" />
                            </Button>
                            <Button
                              variant="outline"
                              size="icon"
                              className="h-7 w-8 px-0"
                              onClick={() => setTestingAccount(account)}
                              title={t('accounts.testConnection')}
                            >
                              <Zap className="size-3.5" />
                            </Button>
                            <Button
                              variant="outline"
                              size="icon"
                              className="h-7 w-8 px-0"
                              disabled={refreshingIds.has(account.id) || account.at_only}
                              onClick={() => void handleRefresh(account)}
                              title={account.at_only ? t('accounts.atRefreshDisabled') : t('accounts.refreshAccessToken')}
                            >
                              <RefreshCw className={`size-3.5 ${refreshingIds.has(account.id) ? 'animate-spin' : ''}`} />
                            </Button>
                            <Button
                              variant={account.locked ? 'default' : 'outline'}
                              size="icon"
                              className="h-7 w-8 px-0"
                              onClick={() => void handleToggleLock(account)}
                              title={account.locked ? t('accounts.unlockHint') : t('accounts.lockHint')}
                            >
                              {account.locked ? <Lock className="size-3.5" /> : <Unlock className="size-3.5" />}
                            </Button>
                            <Button
                              variant="destructive"
                              size="icon"
                              className="h-7 w-8 px-0"
                              onClick={() => void handleDelete(account)}
                              title={t('accounts.deleteAccount')}
                            >
                              <Trash2 className="size-3.5" />
                            </Button>
                          </div>
                        </TableCell>
                        </TableRow>
                      )
                    })}
                  </TableBody>
                </Table>
              </div>
              <Pagination
                page={page}
                totalPages={totalPages}
                onPageChange={setPage}
                totalItems={accounts.length}
                pageSize={pageSize}
              />
            </StateShell>
          </CardContent>
        </Card>

        <Modal
          show={showAdd}
          title={t('accounts.addTitle')}
          contentClassName="sm:max-w-[640px]"
          onClose={() => {
            setShowAdd(false)
            setAddMethod('rt')
            setOauthStep('generate')
            setOauthSession(null)
            setOauthCallbackUrl('')
            setOauthName('')
          }}
          footer={(
            <>
              <Button
                variant="outline"
                onClick={() => {
                  setShowAdd(false)
                  setAddMethod('rt')
                  setOauthStep('generate')
                  setOauthSession(null)
                  setOauthCallbackUrl('')
                  setOauthName('')
                }}
              >
                {t('common.cancel')}
              </Button>
              {addMethod === 'rt' ? (
                <Button onClick={() => void handleAdd()} disabled={submitting || !addForm.refresh_token.trim()}>
                  {submitting ? t('accounts.adding') : t('accounts.submit')}
                </Button>
              ) : addMethod === 'at' ? (
                <Button onClick={() => void handleAddAT()} disabled={submitting || !atForm.access_token.trim()}>
                  {submitting ? t('accounts.adding') : t('accounts.submit')}
                </Button>
              ) : oauthStep === 'generate' ? (
                <Button onClick={() => void handleOAuthGenerate()} disabled={oauthGenerating}>
                  {oauthGenerating ? t('accounts.oauthGenerating') : t('accounts.oauthGenerateBtn')}
                </Button>
              ) : (
                <Button
                  onClick={() => void handleOAuthComplete()}
                  disabled={oauthCompleting || !oauthCallbackUrl.trim()}
                >
                  {oauthCompleting ? t('accounts.oauthCompleting') : t('accounts.oauthCompleteBtn')}
                </Button>
              )}
            </>
          )}
        >
          {/* Tab switcher */}
          <div className="flex gap-1 p-1 mb-5 rounded-xl bg-muted/50 border border-border">
            <button
              onClick={() => setAddMethod('rt')}
              className={`flex-1 flex items-center justify-center gap-1.5 rounded-lg py-2 text-sm font-semibold transition-all ${
                addMethod === 'rt'
                  ? 'bg-background shadow-sm text-foreground'
                  : 'text-muted-foreground hover:text-foreground'
              }`}
            >
              <RefreshCw className="size-3.5" />
              {t('accounts.addMethodRT')}
            </button>
            <button
              onClick={() => setAddMethod('at')}
              className={`flex-1 flex items-center justify-center gap-1.5 rounded-lg py-2 text-sm font-semibold transition-all ${
                addMethod === 'at'
                  ? 'bg-background shadow-sm text-foreground'
                  : 'text-muted-foreground hover:text-foreground'
              }`}
            >
              <Fingerprint className="size-3.5" />
              {t('accounts.addMethodAT')}
            </button>
            <button
              onClick={() => { setAddMethod('oauth'); setOauthStep('generate'); setOauthSession(null); setOauthCallbackUrl('') }}
              className={`flex-1 flex items-center justify-center gap-1.5 rounded-lg py-2 text-sm font-semibold transition-all ${
                addMethod === 'oauth'
                  ? 'bg-background shadow-sm text-foreground'
                  : 'text-muted-foreground hover:text-foreground'
              }`}
            >
              <KeyRound className="size-3.5" />
              {t('accounts.addMethodOAuth')}
            </button>
          </div>

          {addMethod === 'rt' ? (
            <div className="space-y-4">
              <div>
                <label className="block mb-2 text-sm font-semibold text-muted-foreground">{t('accounts.refreshTokenLabel')} *</label>
                <textarea
                  className="w-full min-h-[160px] p-3 border border-input rounded-xl bg-background text-sm resize-y focus:outline-none focus:ring-2 focus:ring-ring"
                  placeholder={t('accounts.refreshTokenPlaceholder')}
                  value={addForm.refresh_token}
                  onChange={(event: ChangeEvent<HTMLTextAreaElement>) =>
                    setAddForm((form) => ({ ...form, refresh_token: event.target.value }))
                  }
                  rows={6}
                />
              </div>
              <div>
                <label className="block mb-2 text-sm font-semibold text-muted-foreground">{t('accounts.proxyUrl')}</label>
                <Input
                  placeholder={t('accounts.proxyUrlPlaceholder')}
                  value={addForm.proxy_url}
                  onChange={(event: ChangeEvent<HTMLInputElement>) =>
                    setAddForm((form) => ({ ...form, proxy_url: event.target.value }))
                  }
                />
              </div>
            </div>
          ) : addMethod === 'at' ? (
            <div className="space-y-4">
              <div className="rounded-xl border border-amber-200 bg-amber-50 px-4 py-3 text-sm text-amber-800 dark:border-amber-800 dark:bg-amber-950/50 dark:text-amber-300">
                {t('accounts.atWarning')}
              </div>
              <div>
                <label className="block mb-2 text-sm font-semibold text-muted-foreground">{t('accounts.accessTokenLabel')} *</label>
                <textarea
                  className="w-full min-h-[160px] p-3 border border-input rounded-xl bg-background text-sm resize-y focus:outline-none focus:ring-2 focus:ring-ring"
                  placeholder={t('accounts.accessTokenPlaceholder')}
                  value={atForm.access_token}
                  onChange={(event: ChangeEvent<HTMLTextAreaElement>) =>
                    setAtForm((form) => ({ ...form, access_token: event.target.value }))
                  }
                  rows={6}
                />
              </div>
              <div>
                <label className="block mb-2 text-sm font-semibold text-muted-foreground">{t('accounts.proxyUrl')}</label>
                <Input
                  placeholder={t('accounts.proxyUrlPlaceholder')}
                  value={atForm.proxy_url}
                  onChange={(event: ChangeEvent<HTMLInputElement>) =>
                    setAtForm((form) => ({ ...form, proxy_url: event.target.value }))
                  }
                />
              </div>
            </div>
          ) : (
            <div className="space-y-5">
              {oauthStep === 'generate' ? (
                <>
                  <div className="rounded-xl border border-border bg-muted/30 px-4 py-3 text-sm text-muted-foreground">
                    <p className="font-semibold text-foreground mb-1">{t('accounts.oauthStep1Title')}</p>
                    <p>{t('accounts.oauthStep1Desc')}</p>
                  </div>
                  <div>
                    <label className="block mb-2 text-sm font-semibold text-muted-foreground">{t('accounts.oauthNameLabel')}</label>
                    <Input
                      placeholder={t('accounts.oauthNamePlaceholder')}
                      value={oauthName}
                      onChange={(e: ChangeEvent<HTMLInputElement>) => setOauthName(e.target.value)}
                    />
                  </div>
                  <div>
                    <label className="block mb-2 text-sm font-semibold text-muted-foreground">{t('accounts.oauthProxyUrl')}</label>
                    <Input
                      placeholder={t('accounts.oauthProxyUrlPlaceholder')}
                      value={oauthProxyUrl}
                      onChange={(e: ChangeEvent<HTMLInputElement>) => setOauthProxyUrl(e.target.value)}
                    />
                  </div>
                </>
              ) : (
                <>
                  <div className="rounded-xl border border-border bg-muted/30 px-4 py-3 text-sm text-muted-foreground">
                    <p className="font-semibold text-foreground mb-1">{t('accounts.oauthStep2Title')}</p>
                    <p>{t('accounts.oauthStep2Desc')}</p>
                  </div>
                  {oauthSession && (
                    <div className="rounded-xl border border-primary/30 bg-primary/5 px-4 py-3">
                      <p className="text-xs font-semibold text-muted-foreground mb-2">{t('accounts.oauthOpenLink')}</p>
                      <a
                        href={oauthSession.auth_url}
                        target="_blank"
                        rel="noopener noreferrer"
                        className="inline-flex items-center gap-1.5 text-sm font-semibold text-primary hover:underline break-all"
                      >
                        <ExternalLink className="size-3.5 shrink-0" />
                        {t('accounts.oauthOpenLink')}
                      </a>
                    </div>
                  )}
                  <div>
                    <label className="block mb-2 text-sm font-semibold text-muted-foreground">{t('accounts.oauthCallbackUrlLabel')}</label>
                    <Input
                      placeholder={t('accounts.oauthCallbackUrlPlaceholder')}
                      value={oauthCallbackUrl}
                      onChange={(e: ChangeEvent<HTMLInputElement>) => setOauthCallbackUrl(e.target.value)}
                    />
                    <p className="mt-1.5 text-xs text-muted-foreground">{t('accounts.oauthCallbackUrlHint')}</p>
                  </div>
                  <button
                    onClick={() => { setOauthStep('generate'); setOauthSession(null); setOauthCallbackUrl('') }}
                    className="text-xs text-muted-foreground hover:text-foreground underline underline-offset-2"
                  >
                    {t('accounts.oauthRestart')}
                  </button>
                </>
              )}
            </div>
          )}
        </Modal>

        <Modal
          show={showImportPicker}
          title={t('accounts.importTitle')}
          contentClassName="sm:max-w-[640px]"
          onClose={() => setShowImportPicker(false)}
        >
          <div className="grid grid-cols-2 gap-3">
            <button
              className="flex items-center gap-3 rounded-xl border border-border px-4 py-3 text-left hover:bg-muted/50 transition-colors"
              onClick={() => {
                setShowImportPicker(false)
                fileInputRef.current?.click()
              }}
            >
              <FileText className="size-5 shrink-0 text-muted-foreground" />
              <div>
                <div className="text-sm font-medium">{t('accounts.importTxt')}</div>
                <div className="text-[11px] text-muted-foreground">{t('accounts.importTxtDesc')}</div>
              </div>
            </button>
            <button
              className="flex items-center gap-3 rounded-xl border border-border px-4 py-3 text-left hover:bg-muted/50 transition-colors"
              onClick={() => {
                setShowImportPicker(false)
                jsonInputRef.current?.click()
              }}
            >
              <FileJson className="size-5 shrink-0 text-muted-foreground" />
              <div>
                <div className="text-sm font-medium">{t('accounts.importJson')}</div>
                <div className="text-[11px] text-muted-foreground">{t('accounts.importJsonDesc')}</div>
              </div>
            </button>
            <button
              className="flex items-center gap-3 rounded-xl border border-border px-4 py-3 text-left hover:bg-muted/50 transition-colors"
              onClick={() => {
                setShowImportPicker(false)
                atFileInputRef.current?.click()
              }}
            >
              <Fingerprint className="size-5 shrink-0 text-muted-foreground" />
              <div>
                <div className="text-sm font-medium">{t('accounts.importAtTxt')}</div>
                <div className="text-[11px] text-muted-foreground">{t('accounts.importAtTxtDesc')}</div>
              </div>
            </button>
            <button
              className="flex items-center gap-3 rounded-xl border border-border px-4 py-3 text-left hover:bg-muted/50 transition-colors"
              onClick={() => {
                setShowImportPicker(false)
                folderInputRef.current?.click()
              }}
            >
              <FolderOpen className="size-5 shrink-0 text-muted-foreground" />
              <div>
                <div className="text-sm font-medium">{t('accounts.importFolder')}</div>
                <div className="text-[11px] text-muted-foreground">{t('accounts.importFolderDesc')}</div>
              </div>
            </button>
          </div>
        </Modal>

        <Modal
          show={showExportPicker}
          title={t('accounts.exportTitle')}
          contentClassName="sm:max-w-[580px]"
          onClose={() => setShowExportPicker(false)}
        >
          <div className="grid grid-cols-2 gap-3">
            <button
              className="flex items-center gap-3 rounded-xl border border-border px-4 py-3.5 text-left hover:bg-muted/50 transition-colors"
              onClick={() => void handleExport('json', 'healthy')}
            >
              <FileJson className="size-5 shrink-0 text-muted-foreground" />
              <div className="min-w-0">
                <div className="text-sm font-medium whitespace-nowrap">{t('accounts.exportHealthyJson')}</div>
                <div className="text-[11px] text-muted-foreground">{t('accounts.exportHealthyJsonDesc')}</div>
              </div>
            </button>
            <button
              className="flex items-center gap-3 rounded-xl border border-border px-4 py-3.5 text-left hover:bg-muted/50 transition-colors"
              onClick={() => void handleExport('txt', 'healthy')}
            >
              <FileText className="size-5 shrink-0 text-muted-foreground" />
              <div className="min-w-0">
                <div className="text-sm font-medium whitespace-nowrap">{t('accounts.exportHealthyTxt')}</div>
                <div className="text-[11px] text-muted-foreground">{t('accounts.exportHealthyTxtDesc')}</div>
              </div>
            </button>
            <button
              className="flex items-center gap-3 rounded-xl border border-border px-4 py-3.5 text-left hover:bg-muted/50 transition-colors disabled:opacity-40 disabled:pointer-events-none"
              disabled={selected.size === 0}
              onClick={() => void handleExport('json', 'selected')}
            >
              <FileJson className="size-5 shrink-0 text-muted-foreground" />
              <div className="min-w-0">
                <div className="text-sm font-medium whitespace-nowrap">{t('accounts.exportSelectedJson')}</div>
                <div className="text-[11px] text-muted-foreground">{t('accounts.exportSelectedJsonDesc')}</div>
              </div>
            </button>
            <button
              className="flex items-center gap-3 rounded-xl border border-border px-4 py-3.5 text-left hover:bg-muted/50 transition-colors disabled:opacity-40 disabled:pointer-events-none"
              disabled={selected.size === 0}
              onClick={() => void handleExport('txt', 'selected')}
            >
              <FileText className="size-5 shrink-0 text-muted-foreground" />
              <div className="min-w-0">
                <div className="text-sm font-medium whitespace-nowrap">{t('accounts.exportSelectedTxt')}</div>
                <div className="text-[11px] text-muted-foreground">{t('accounts.exportSelectedTxtDesc')}</div>
              </div>
            </button>
          </div>
        </Modal>

        <Modal
          show={showMigrate}
          title={t('accounts.migrateTitle')}
          contentClassName="sm:max-w-[520px]"
          onClose={() => { setShowMigrate(false); setMigrateUrl(''); setMigrateKey('') }}
        >
          <div className="space-y-4">
            <div className="rounded-xl border border-border bg-muted/30 px-4 py-3 text-sm text-muted-foreground">
              <p>{t('accounts.migrateDesc')}</p>
            </div>
            <div>
              <label className="block mb-2 text-sm font-semibold text-muted-foreground">{t('accounts.migrateUrlLabel')}</label>
              <Input
                placeholder={t('accounts.migrateUrlPlaceholder')}
                value={migrateUrl}
                onChange={(e: ChangeEvent<HTMLInputElement>) => setMigrateUrl(e.target.value)}
              />
            </div>
            <div>
              <label className="block mb-2 text-sm font-semibold text-muted-foreground">{t('accounts.migrateKeyLabel')}</label>
              <Input
                type="password"
                placeholder={t('accounts.migrateKeyPlaceholder')}
                value={migrateKey}
                onChange={(e: ChangeEvent<HTMLInputElement>) => setMigrateKey(e.target.value)}
              />
            </div>
            <div className="flex justify-end gap-2 pt-2">
              <Button variant="outline" onClick={() => { setShowMigrate(false); setMigrateUrl(''); setMigrateKey('') }}>
                {t('common.cancel')}
              </Button>
              <Button
                onClick={() => void handleMigrate()}
                disabled={migrating || !migrateUrl.trim() || !migrateKey.trim()}
              >
                {migrating ? t('accounts.migrating') : t('accounts.migrateConfirm')}
              </Button>
            </div>
          </div>
        </Modal>

        {testingAccount && (
          <TestConnectionModal
            account={testingAccount}
            onSettled={() => {
              void reloadSilently()
            }}
            onClose={() => setTestingAccount(null)}
          />
        )}

        <Modal
          show={rawInfoDialog !== null}
          title={t(rawInfoDialog?.kind === 'auth' ? 'accounts.authInfoModalTitle' : 'accounts.quotaInfoModalTitle', {
            account: rawInfoDialog?.account.email || `ID ${rawInfoDialog?.account.id ?? ''}`,
          })}
          contentClassName="sm:max-w-[960px]"
          onClose={() => setRawInfoDialog(null)}
        >
          {rawInfoDialog ? (
            <div className="space-y-4">
              <div className="grid gap-2 text-sm text-muted-foreground">
                <div>
                  {t('accounts.rawInfoFetchedAt')}: {formatBeijingTime(rawInfoDialog.fetchedAt)}
                </div>
                {rawInfoDialog.rawEndpoint ? (
                  <div>
                    {t('accounts.rawInfoEndpoint')}: {rawInfoDialog.rawEndpoint}
                  </div>
                ) : null}
                {rawInfoDialog.planSource ? (
                  <div>
                    {t('accounts.rawInfoPlanSource')}: {rawInfoDialog.planSource}
                  </div>
                ) : null}
                <div className="space-y-1">
                  <div>{t('accounts.rawInfoRefreshedFields')}:</div>
                  {Object.keys(rawInfoDialog.refreshedFields).length > 0 ? (
                    <div className="flex flex-wrap gap-2">
                      {Object.entries(rawInfoDialog.refreshedFields).map(([key, value]) => (
                        <span key={key} className="rounded-md border border-border px-2 py-1 text-xs text-foreground">
                          {key}: {value}
                        </span>
                      ))}
                    </div>
                  ) : (
                    <div className="text-xs">{t('accounts.rawInfoNoFieldUpdated')}</div>
                  )}
                </div>
              </div>
              <pre className="max-h-[62vh] overflow-auto rounded-md border border-border bg-muted/40 p-3 text-xs leading-5 whitespace-pre-wrap break-all">
                {rawInfoDialog.rawText || t('accounts.rawInfoEmpty')}
              </pre>
            </div>
          ) : null}
        </Modal>

        {usageAccount && (
          <AccountUsageModal account={usageAccount} onClose={() => setUsageAccount(null)} />
        )}

        <Modal
          show={importProgress.show}
          title={importProgress.done ? t('accounts.importDone') : t('accounts.importingProgress')}
          contentClassName="sm:max-w-[420px]"
          onClose={() => setImportProgress(p => ({ ...p, show: false }))}
        >
          <div className="space-y-4">
            <div className="w-full h-3 bg-muted rounded-full overflow-hidden">
              <div
                className="h-full bg-primary rounded-full transition-all duration-300 ease-out"
                style={{ width: importProgress.total > 0 ? `${Math.round((importProgress.current / importProgress.total) * 100)}%` : '0%' }}
              />
            </div>
            <div className="text-center text-sm text-muted-foreground">
              {importProgress.total > 0
                ? `${importProgress.current} / ${importProgress.total}  (${Math.round((importProgress.current / importProgress.total) * 100)}%)`
                : t('accounts.importPreparing')}
            </div>
            <div className="grid grid-cols-3 gap-3 text-center">
              <div className="rounded-xl bg-emerald-500/10 px-3 py-2">
                <div className="text-lg font-bold text-emerald-600">{importProgress.success}</div>
                <div className="text-[11px] text-muted-foreground">{t('accounts.importSuccess')}</div>
              </div>
              <div className="rounded-xl bg-amber-500/10 px-3 py-2">
                <div className="text-lg font-bold text-amber-600">{importProgress.duplicate}</div>
                <div className="text-[11px] text-muted-foreground">{t('accounts.importDuplicate')}</div>
              </div>
              <div className="rounded-xl bg-red-500/10 px-3 py-2">
                <div className="text-lg font-bold text-red-600">{importProgress.failed}</div>
                <div className="text-[11px] text-muted-foreground">{t('accounts.importFailedCount')}</div>
              </div>
            </div>
            {importProgress.done && (
              <p className="text-xs text-center text-muted-foreground">{t('accounts.importDoneHint')}</p>
            )}
          </div>
        </Modal>

        {confirmDialog}

        <ToastNotice toast={toast} />
      </>
    </StateShell>
    </div>
  )
}

function downloadBlob(blob: Blob, filename: string) {
  const url = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = url
  a.download = filename
  document.body.appendChild(a)
  a.click()
  document.body.removeChild(a)
  URL.revokeObjectURL(url)
}

function CompactStat({
  label,
  chipLabel,
  value,
  tone,
}: {
  label: string
  chipLabel?: string
  value: number | string
  tone: 'neutral' | 'success' | 'warning' | 'danger' | 'cyan'
}) {
  const toneStyle = {
    neutral: {
      chip: 'bg-slate-500/10 text-slate-600 dark:bg-slate-500/20 dark:text-slate-300',
      dot: 'bg-slate-500',
    },
    success: {
      chip: 'bg-emerald-500/10 text-emerald-600 dark:bg-emerald-500/20 dark:text-emerald-300',
      dot: 'bg-emerald-500',
    },
    warning: {
      chip: 'bg-amber-500/10 text-amber-600 dark:bg-amber-500/20 dark:text-amber-300',
      dot: 'bg-amber-500',
    },
    danger: {
      chip: 'bg-red-500/10 text-red-600 dark:bg-red-500/20 dark:text-red-300',
      dot: 'bg-red-500',
    },
    cyan: {
      chip: 'bg-cyan-500/10 text-cyan-600 dark:bg-cyan-500/20 dark:text-cyan-300',
      dot: 'bg-cyan-500',
    },
  }[tone]

  return (
    <div className="flex items-center justify-between rounded-2xl border border-border bg-white/65 px-4 py-3 shadow-[inset_0_1px_0_rgba(255,255,255,0.7)]">
      <div className="min-w-0">
        <div className="text-[12px] font-semibold text-muted-foreground">{label}</div>
        <div className="mt-1 text-[24px] font-bold leading-none tracking-tight text-foreground">{value}</div>
      </div>
      <div className={`inline-flex items-center gap-1.5 rounded-full px-2.5 py-1 text-[12px] font-semibold ${toneStyle.chip}`}>
        <span className={`size-2 rounded-full ${toneStyle.dot}`} />
        {chipLabel ?? label}
      </div>
    </div>
  )
}

function SchedulerChip({
  label,
  value,
  tone,
}: {
  label: string
  value: number
  tone: 'neutral' | 'success' | 'warning' | 'danger'
}) {
  const toneStyle = {
    neutral: 'bg-slate-500/10 text-slate-600 dark:bg-slate-500/20 dark:text-slate-300',
    success: 'bg-emerald-500/10 text-emerald-600 dark:bg-emerald-500/20 dark:text-emerald-300',
    warning: 'bg-amber-500/10 text-amber-600 dark:bg-amber-500/20 dark:text-amber-300',
    danger: 'bg-red-500/10 text-red-600 dark:bg-red-500/20 dark:text-red-300',
  }[tone]

  return (
    <span className={`inline-flex items-center gap-1.5 rounded-full px-2.5 py-1 font-semibold ${toneStyle}`}>
      <span>{label}</span>
      <span>{value}</span>
    </span>
  )
}

function formatHealthTier(healthTier?: string, t?: any) {
  if (!t) return 'Unknown'
  switch (healthTier) {
    case 'healthy':
      return t('accounts.healthy')
    case 'warm':
      return t('accounts.warm')
    case 'risky':
      return t('accounts.risky')
    case 'banned':
      return t('accounts.quarantine')
    default:
      return t('accounts.unknown')
  }
}

// ==================== 测试连接弹窗 ====================

interface TestEvent {
  type: 'test_start' | 'content' | 'test_complete' | 'error'
  text?: string
  model?: string
  success?: boolean
  error?: string
}

function formatTestErrorMessage(message: string) {
  const normalized = message.trim()
  const jsonStart = normalized.indexOf('{')

  if (jsonStart === -1) {
    return normalized
  }

  const prefix = normalized.slice(0, jsonStart).trim().replace(/[：:]\s*$/, '')
  const jsonText = normalized.slice(jsonStart)

  try {
    const parsed = JSON.parse(jsonText)
    const prettyJson = JSON.stringify(parsed, null, 2)
    return prefix ? `${prefix}\n${prettyJson}` : prettyJson
  } catch {
    return normalized
  }
}

function formatTestOutput(text: string) {
  try {
    const parsed = JSON.parse(text);
    return JSON.stringify(parsed, null, 2);
  } catch {
    return text;
  }
}

function TestConnectionModal({
  account,
  onClose,
  onSettled,
}: {
  account: AccountRow
  onClose: () => void
  onSettled: () => void
}) {
  const { t } = useTranslation()
  const [output, setOutput] = useState<string[]>([])
  const [status, setStatus] = useState<'connecting' | 'streaming' | 'success' | 'error'>('connecting')
  const [errorMsg, setErrorMsg] = useState('')
  const [model, setModel] = useState('')
  const abortRef = useRef<AbortController | null>(null)
  const outputEndRef = useRef<HTMLDivElement>(null)
  const settledRef = useRef(false)
  const onSettledRef = useRef(onSettled)
  onSettledRef.current = onSettled

  const markSettled = useCallback(() => {
    if (settledRef.current) return
    settledRef.current = true
    onSettledRef.current()
  }, [])

  useEffect(() => {
    // 重置状态（StrictMode 二次 mount 时清理上一次的残留）
    setOutput([])
    setStatus('connecting')
    setErrorMsg('')
    settledRef.current = false

    const controller = new AbortController()
    abortRef.current = controller

    const run = async () => {
      if (controller.signal.aborted) return

      try {
        const res = await fetch(`/api/admin/accounts/${account.id}/test`, {
          signal: controller.signal,
          headers: getAdminKey() ? { 'X-Admin-Key': getAdminKey() } : {},
        })

        if (!res.ok) {
          const body = await res.text()
          let msg = `HTTP ${res.status}`
          try {
            const parsed = JSON.parse(body)
            if (parsed.error) msg = parsed.error
          } catch { /* ignore */ }
          setStatus('error')
          setErrorMsg(msg)
          markSettled()
          return
        }

        const reader = res.body?.getReader()
        if (!reader) {
          setStatus('error')
          setErrorMsg(t('accounts.browserStreamingUnsupported'))
          markSettled()
          return
        }

        const decoder = new TextDecoder()
        let buffer = ''
        let receivedTerminalEvent = false

        const processEventLines = (lines: string[]) => {
          for (const line of lines) {
            const trimmed = line.trim()
            if (!trimmed.startsWith('data: ')) continue

            try {
              const event: TestEvent = JSON.parse(trimmed.slice(6))

              switch (event.type) {
                case 'test_start':
                  setModel(event.model || '')
                  setStatus('streaming')
                  break
                case 'content':
                  if (event.text) {
                    setOutput((prev) => [...prev, event.text!])
                  }
                  break
                case 'test_complete':
                  receivedTerminalEvent = true
                  setStatus(event.success ? 'success' : 'error')
                  markSettled()
                  break
                case 'error':
                  receivedTerminalEvent = true
                  setStatus('error')
                  setErrorMsg(event.error || t('accounts.unknownError'))
                  markSettled()
                  break
              }
            } catch { /* ignore non-JSON lines */ }
          }
        }

        while (true) {
          const { done, value } = await reader.read()
          if (done) {
            buffer += decoder.decode()
            break
          }

          buffer += decoder.decode(value, { stream: true })
          const lines = buffer.split('\n')
          buffer = lines.pop() || ''
          processEventLines(lines)
        }

        if (buffer.trim()) {
          processEventLines([buffer])
        }

        if (!receivedTerminalEvent) {
          setStatus('error')
          setErrorMsg(t('accounts.connectionEndedUnexpectedly'))
          markSettled()
        }
      } catch (err: unknown) {
        if (err instanceof DOMException && err.name === 'AbortError') return
        setStatus('error')
        setErrorMsg(err instanceof Error ? err.message : t('accounts.connectionFailed'))
        markSettled()
      }
    }

    // 延迟 50ms 启动，确保 StrictMode cleanup 有足够时间执行 abort
    const timer = window.setTimeout(() => {
      void run()
    }, 50)

    return () => {
      window.clearTimeout(timer)
      controller.abort()
    }
  }, [account.id, markSettled, t])

  useEffect(() => {
    outputEndRef.current?.scrollIntoView({ behavior: 'smooth' })
  }, [output])

  const statusLabel = {
    connecting: `⏳ ${t('accounts.connecting')}`,
    streaming: `🔄 ${t('accounts.receivingResponse')}`,
    success: `✅ ${t('accounts.testSuccess')}`,
    error: `❌ ${t('accounts.testFailed')}`,
  }[status]

  const statusColor = {
    connecting: 'text-muted-foreground',
    streaming: 'text-blue-500',
    success: 'text-emerald-500',
    error: 'text-red-500',
  }[status]
  const formattedErrorMsg = errorMsg ? formatTestErrorMessage(errorMsg) : ''

  return (
    <Modal
      show={true}
      title={t('accounts.testConnectionTitle', { account: account.email || `ID ${account.id}` })}
      onClose={() => {
        abortRef.current?.abort()
        onClose()
      }}
      footer={
        <Button
          variant="outline"
          onClick={() => {
            abortRef.current?.abort()
            onClose()
          }}
        >
          {t('common.close')}
        </Button>
      }
      contentClassName="sm:max-w-[680px]"
    >
      <div className="space-y-4">
        <div className="flex flex-wrap items-start justify-between gap-2">
          <span className={`flex items-center gap-1.5 text-sm font-semibold ${statusColor}`}>
            {statusLabel}
          </span>
          {model && (
            <span className="max-w-full rounded-md bg-muted px-2 py-0.5 font-mono text-xs break-all text-muted-foreground">
              {model}
            </span>
          )}
        </div>

        {(output.length > 0 || status === 'connecting' || status === 'streaming') && (
          <div
            className="min-h-[80px] max-h-[240px] overflow-auto rounded-xl border border-border bg-muted/30 p-3 text-[20px] leading-[1.8] whitespace-pre-wrap break-all"
            style={{ fontFamily: 'var(--font-geist-mono)' }}
          >
            {output.length === 0 && status === 'connecting' && (
              <span className="text-muted-foreground animate-pulse">{t('accounts.sendingTestRequest')}</span>
            )}
            {output.join('')}
            <div ref={outputEndRef} />
          </div>
        )}

        {errorMsg && (
          <div className="max-h-[40vh] overflow-auto rounded-xl border border-red-200 bg-red-50 p-3.5 text-red-600 dark:border-red-900/50 dark:bg-red-950/30 dark:text-red-400">
            <div className="mb-2 text-sm font-semibold">{t('accounts.failureDetails')}</div>
            <pre
              className="text-[20px] leading-[1.8] whitespace-pre-wrap break-all"
              style={{ fontFamily: 'var(--font-geist-mono)' }}
            >
              {formattedErrorMsg}
            </pre>
          </div>
        )}

        {status === 'success' && (
          <div className="flex items-center gap-2 rounded-xl border border-emerald-200 bg-emerald-50 px-4 py-2.5 text-sm text-emerald-700 dark:border-emerald-900/50 dark:bg-emerald-950/30 dark:text-emerald-400">
            <RotateCcw className="size-4 shrink-0" />
            {t('accounts.testAutoReset')}
          </div>
        )}
      </div>
    </Modal>
  )
}

// 格式化重置时间为具体时间
function formatResetAt(resetAt: string | undefined): string | null {
  if (!resetAt) return null
  const d = new Date(resetAt)
  if (d.getTime() <= Date.now()) return null
  const pad = (n: number) => String(n).padStart(2, '0')
  return `${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`
}

// 用量进度条颜色
function usageBarColor(pct: number): string {
  if (pct >= 90) return 'bg-red-500'
  if (pct >= 70) return 'bg-amber-500'
  return 'bg-emerald-500'
}

// 单行用量进度条
function UsageBar({ label, pct, resetAt }: { label: string; pct: number; resetAt?: string }) {
  const resetText = formatResetAt(resetAt)
  return (
    <div>
      <div className="flex items-center gap-1.5">
        <span className="text-[11px] font-medium text-muted-foreground w-5 shrink-0">{label}</span>
        <div className="flex-1 h-1.5 rounded-full bg-muted overflow-hidden min-w-[72px]">
          <div className={`h-full rounded-full transition-all ${usageBarColor(pct)}`} style={{ width: `${Math.min(100, pct)}%` }} />
        </div>
        <span className="text-[12px] font-semibold w-[42px] text-right shrink-0">{pct.toFixed(1)}%</span>
      </div>
      {resetText && <div className="text-[11px] font-medium text-muted-foreground mt-0.5 pl-[26px]">⏱ {resetText}</div>}
    </div>
  )
}

function ImageQuotaBar({
  remaining,
  total,
  official,
}: {
  remaining?: number | null
  total?: number | null
  official?: number | null
}) {
  const hasQuota = remaining !== null && remaining !== undefined && total !== null && total !== undefined
  const safeRemaining = Math.max(0, Number(remaining ?? 0))
  const safeTotal = Math.max(0, Number(total ?? 0))
  const safeOfficial = Math.max(0, Number(official ?? 0))
  const pct = hasQuota && safeTotal > 0 ? Math.max(0, Math.min(100, (safeRemaining / safeTotal) * 100)) : 0
  const text = hasQuota ? `${safeRemaining}/${safeTotal}+${safeOfficial}` : `-/-+${safeOfficial}`
  return (
    <div>
      <div className="flex items-center gap-1.5">
        <span className="text-[11px] font-medium text-muted-foreground w-8 shrink-0">image</span>
        <div className="flex-1 h-1.5 rounded-full bg-muted overflow-hidden min-w-[72px]">
          <div className="h-full rounded-full transition-all bg-cyan-500" style={{ width: `${pct}%` }} />
        </div>
        <span className="text-[12px] font-semibold w-[64px] text-right shrink-0">{text}</span>
      </div>
    </div>
  )
}

// 用量列组件
function UsageCell({ account, t }: { account: AccountRow; t: (key: string, options?: Record<string, unknown>) => string }) {
  const plan = (account.plan_type || '').toLowerCase()
  const has7d = account.usage_percent_7d !== null && account.usage_percent_7d !== undefined
  const has5h = account.usage_percent_5h !== null && account.usage_percent_5h !== undefined
  const hasImageWebRemaining = account.image_web_remaining !== null && account.image_web_remaining !== undefined
  const hasImageWebTotal = account.image_web_total !== null && account.image_web_total !== undefined
  const uploaderText = account.uploader_id
    ? t('accounts.uploadSourceUser', {
        id: account.uploader_id,
        amount: formatUSD(account.settlement_amount_usd),
      })
    : t('accounts.uploadSourceAdmin')

  const wrap = (content: ReactNode, widthClass: string) => (
    <div className={`${widthClass} space-y-1`}>
      <div className="text-[11px] text-muted-foreground">{uploaderText}</div>
      {content}
      <ImageQuotaBar
        remaining={hasImageWebRemaining ? account.image_web_remaining : null}
        total={hasImageWebTotal ? account.image_web_total : null}
        official={account.image_official_available}
      />
    </div>
  )

  if (plan === 'free') {
    if (!has7d) return wrap(<span className="text-[12px] text-muted-foreground">-</span>, 'w-40')
    return wrap(<UsageBar label="7d" pct={account.usage_percent_7d!} resetAt={account.reset_7d_at} />, 'w-40')
  }

  if (plan === 'plus' || plan === 'pro' || plan === 'team') {
    if (!has5h && !has7d) return wrap(<span className="text-[12px] text-muted-foreground">-</span>, 'w-48')
    return wrap(
      <div className="space-y-1.5">
        {has5h && <UsageBar label="5h" pct={account.usage_percent_5h!} resetAt={account.reset_5h_at} />}
        {has7d && <UsageBar label="7d" pct={account.usage_percent_7d!} resetAt={account.reset_7d_at} />}
      </div>,
      'w-48',
    )
  }

  if (has7d) {
    return wrap(<UsageBar label="7d" pct={account.usage_percent_7d!} resetAt={account.reset_7d_at} />, 'w-40')
  }
  return wrap(<span className="text-[13px] text-muted-foreground">-</span>, 'w-40')
}
