import { useCallback, useEffect, useMemo, useState } from 'react'
import { api } from '../api'
import PageHeader from '../components/PageHeader'
import Pagination from '../components/Pagination'
import StateShell from '../components/StateShell'
import ToastNotice from '../components/ToastNotice'
import { useDataLoader } from '../hooks/useDataLoader'
import { useConfirmDialog } from '../hooks/useConfirmDialog'
import { useToast } from '../hooks/useToast'
import type { UsageLog, UsageStats } from '../types'
import { Card, CardContent } from '@/components/ui/card'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'
import { Activity, Box, Clock, Zap, AlertTriangle } from 'lucide-react'

function formatTokens(value?: number | null): string {
  if (value === undefined || value === null) return '0'
  if (value >= 1_000_000_000) return `${(value / 1_000_000_000).toFixed(1)}B`
  if (value >= 1_000_000) return `${(value / 1_000_000).toFixed(1)}M`
  if (value >= 1_000) return `${(value / 1_000).toFixed(1)}K`
  return value.toLocaleString()
}

function formatTime(iso: string): string {
  try {
    // 后端如果直接按本地时间存入无时区 DB 字段，再序列化时会被默认当成 UTC 并加上 Z。
    // 这会导致浏览器根据本地时区再次 +8 小时。
    // 我们强制去掉时区后缀，让浏览器把这个字符串直接当作本地时间进行处理
    const normalizedIso = iso.replace(/(Z|[+-]\d{2}(:\d{2})?)$/, '')
    const d = new Date(normalizedIso)
    
    if (isNaN(d.getTime())) return '-'
    const now = new Date()
    const pad = (n: number) => String(n).padStart(2, '0')
    const time = `${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`
    
    // 如果不是今天则加上日期
    if (d.toDateString() !== now.toDateString()) {
      return `${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${time}`
    }
    return time
  } catch {
    return '-'
  }
}

function getStatusBadgeClassName(statusCode: number): string {
  if (statusCode === 200) {
    return 'border-transparent bg-emerald-500/14 text-emerald-600 dark:bg-emerald-500/20 dark:text-emerald-300'
  }
  if (statusCode === 401) {
    return 'border-transparent bg-red-500/14 text-red-600 dark:bg-red-500/20 dark:text-red-300'
  }
  if (statusCode === 429) {
    return 'border-transparent bg-amber-500/14 text-amber-600 dark:bg-amber-500/20 dark:text-amber-300'
  }
  if (statusCode >= 500) {
    return 'border-transparent bg-red-500/14 text-red-600 dark:bg-red-500/20 dark:text-red-300'
  }
  if (statusCode >= 400) {
    return 'border-transparent bg-amber-500/14 text-amber-600 dark:bg-amber-500/20 dark:text-amber-300'
  }
  return 'border-transparent bg-slate-500/14 text-slate-600 dark:bg-slate-500/20 dark:text-slate-300'
}

export default function Usage() {
  const { toast, showToast } = useToast()
  const { confirm, confirmDialog } = useConfirmDialog()
  const [page, setPage] = useState(1)
  const [clearing, setClearing] = useState(false)
  const PAGE_SIZE = 20

  const loadUsageData = useCallback(async () => {
    const [stats, logsResponse] = await Promise.all([api.getUsageStats(), api.getUsageLogs(100)])
    return {
      stats,
      logs: logsResponse.logs ?? [],
    }
  }, [])

  const { data, loading, error, reload, reloadSilently } = useDataLoader<{
    stats: UsageStats | null
    logs: UsageLog[]
  }>({
    initialData: {
      stats: null,
      logs: [],
    },
    load: loadUsageData,
  })

  useEffect(() => {
    const timer = window.setInterval(() => {
      void reloadSilently()
    }, 30000)

    return () => window.clearInterval(timer)
  }, [reloadSilently])

  const { stats, logs } = data
  const totalPages = Math.max(1, Math.ceil(logs.length / PAGE_SIZE))
  const pagedLogs = useMemo(() => logs.slice((page - 1) * PAGE_SIZE, page * PAGE_SIZE), [logs, page])
  const totalRequests = stats?.total_requests ?? 0
  const totalTokens = stats?.total_tokens ?? 0
  const totalPromptTokens = stats?.total_prompt_tokens ?? 0
  const totalCompletionTokens = stats?.total_completion_tokens ?? 0
  const todayRequests = stats?.today_requests ?? 0
  const rpm = stats?.rpm ?? 0
  const tpm = stats?.tpm ?? 0
  const errorRate = stats?.error_rate ?? 0
  const avgDurationMs = stats?.avg_duration_ms ?? 0
  const successRequests = totalRequests - Math.round(totalRequests * errorRate / 100)

  return (
    <StateShell
      variant="page"
      loading={loading}
      error={error}
      onRetry={() => void reload()}
      loadingTitle="正在加载使用统计"
      loadingDescription="请求日志和性能指标正在同步。"
      errorTitle="统计页加载失败"
    >
      <>
        <PageHeader
          title="使用统计"
          description="请求日志与性能指标"
          onRefresh={() => void reload()}
        />

        {/* Top stats: 2 columns */}
        <div className="grid grid-cols-2 gap-3 mb-3 max-sm:grid-cols-1">
          <Card className="py-0">
            <CardContent className="flex flex-col gap-2 p-4">
              <div className="flex items-center justify-between gap-3">
                <span className="text-[11px] font-bold tracking-[0.12em] uppercase text-muted-foreground">总请求数</span>
                <div className="size-10 flex items-center justify-center rounded-xl bg-primary/12 text-primary">
                  <Activity className="size-[18px]" />
                </div>
              </div>
              <div className="text-[28px] font-bold leading-none tracking-tighter">
                {formatTokens(totalRequests)}
              </div>
              <div className="text-[12px] text-muted-foreground leading-relaxed">
                <span className="text-[hsl(var(--success))]">● 成功: {formatTokens(successRequests)}</span>
                <span className="ml-2 text-muted-foreground">● 今日: {formatTokens(todayRequests)}</span>
              </div>
            </CardContent>
          </Card>

          <Card className="py-0">
            <CardContent className="flex flex-col gap-2 p-4">
              <div className="flex items-center justify-between gap-3">
                <span className="text-[11px] font-bold tracking-[0.12em] uppercase text-muted-foreground">总 Token 数</span>
                <div className="size-10 flex items-center justify-center rounded-xl bg-[hsl(var(--info-bg))] text-[hsl(var(--info))]">
                  <Box className="size-[18px]" />
                </div>
              </div>
              <div className="text-[28px] font-bold leading-none tracking-tighter">
                {formatTokens(totalTokens)}
              </div>
              <div className="text-[12px] text-muted-foreground leading-relaxed">
                <span>输入: {formatTokens(totalPromptTokens)}</span>
                <span className="ml-2">输出: {formatTokens(totalCompletionTokens)}</span>
              </div>
            </CardContent>
          </Card>
        </div>

        {/* Bottom stats: 3 columns */}
        <div className="grid grid-cols-3 gap-3 mb-6 max-sm:grid-cols-1">
          <Card className="py-0">
            <CardContent className="flex flex-col gap-2 p-4">
              <div className="flex items-center justify-between gap-3">
                <span className="text-[11px] font-bold tracking-[0.12em] uppercase text-muted-foreground">RPM</span>
                <div className="size-10 flex items-center justify-center rounded-xl bg-[hsl(var(--success-bg))] text-[hsl(var(--success))]">
                  <Clock className="size-[18px]" />
                </div>
              </div>
              <div className="text-[28px] font-bold leading-none tracking-tighter">
                {Math.round(rpm)}
              </div>
              <div className="text-[12px] text-muted-foreground">每分钟请求数</div>
            </CardContent>
          </Card>

          <Card className="py-0">
            <CardContent className="flex flex-col gap-2 p-4">
              <div className="flex items-center justify-between gap-3">
                <span className="text-[11px] font-bold tracking-[0.12em] uppercase text-muted-foreground">TPM</span>
                <div className="size-10 flex items-center justify-center rounded-xl bg-destructive/12 text-destructive">
                  <Zap className="size-[18px]" />
                </div>
              </div>
              <div className="text-[28px] font-bold leading-none tracking-tighter">
                {formatTokens(tpm)}
              </div>
              <div className="text-[12px] text-muted-foreground">每分钟 Token 数</div>
            </CardContent>
          </Card>

          <Card className="py-0">
            <CardContent className="flex flex-col gap-2 p-4">
              <div className="flex items-center justify-between gap-3">
                <span className="text-[11px] font-bold tracking-[0.12em] uppercase text-muted-foreground">错误率</span>
                <div className="size-10 flex items-center justify-center rounded-xl bg-[hsl(36_72%_40%/0.12)] text-[hsl(36,72%,40%)]">
                  <AlertTriangle className="size-[18px]" />
                </div>
              </div>
              <div className="text-[28px] font-bold leading-none tracking-tighter">
                {errorRate.toFixed(1)}%
              </div>
              <div className="text-[12px] text-muted-foreground">平均延迟: {Math.round(avgDurationMs)}ms</div>
            </CardContent>
          </Card>
        </div>

        {/* Logs table */}
        <Card>
          <CardContent className="p-6">
            <div className="flex items-center justify-between gap-4 mb-4 flex-wrap">
              <h3 className="text-base font-semibold text-foreground">请求记录</h3>
              <div className="flex items-center gap-3">
                <span className="text-xs text-muted-foreground">最近 {logs.length} 条</span>
                <Button
                  variant="destructive"
                  size="sm"
                  disabled={clearing || logs.length === 0}
                  onClick={async () => {
                    const confirmed = await confirm({
                      title: '清空使用日志',
                      description: '所有请求日志都会被清空，统计视图会立刻更新。该操作不可恢复。',
                      confirmText: '确认清空',
                      tone: 'destructive',
                      confirmVariant: 'destructive',
                    })
                    if (!confirmed) return
                    setClearing(true)
                    try {
                      await api.clearUsageLogs()
                      showToast('日志已清空')
                      setPage(1)
                      void reload()
                    } catch (e) {
                      showToast(`清空失败: ${String(e)}`, 'error')
                    } finally {
                      setClearing(false)
                    }
                  }}
                >
                  {clearing ? '清空中...' : '清空日志'}
                </Button>
              </div>
            </div>
            <StateShell
              variant="section"
              isEmpty={logs.length === 0}
              emptyTitle="暂无请求记录"
              emptyDescription="请求进入代理后，会在这里展示最近日志和状态码。"
            >
              <div className="overflow-auto border border-border rounded-xl">
                <Table>
                  <TableHeader>
                    <TableRow>
                      <TableHead className="text-[14px] font-semibold">状态</TableHead>
                      <TableHead className="text-[14px] font-semibold">模型</TableHead>
                      <TableHead className="text-[14px] font-semibold">来源账号</TableHead>
                      <TableHead className="text-[16px] font-semibold" style={{ fontFamily: "'Geist Mono', monospace" }}>端点</TableHead>
                      <TableHead className="text-[14px] font-semibold">类型</TableHead>
                      <TableHead className="text-[14px] font-semibold">TOKEN</TableHead>
                      <TableHead className="text-[14px] font-semibold">读取缓存</TableHead>
                      <TableHead className="text-[16px] font-semibold" style={{ fontFamily: "'Geist Mono', monospace" }}>首字时间</TableHead>
                      <TableHead className="text-[16px] font-semibold" style={{ fontFamily: "'Geist Mono', monospace" }}>总耗时</TableHead>
                      <TableHead className="text-[16px] font-semibold" style={{ fontFamily: "'Geist Mono', monospace" }}>时间</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {pagedLogs.map((log) => {
                      return (
                      <TableRow key={log.id}>
                        <TableCell>
                          <Badge
                            variant="outline"
                            className={`text-[14px] ${getStatusBadgeClassName(log.status_code)}`}
                          >
                            {log.status_code}
                          </Badge>
                        </TableCell>
                        <TableCell>
                          <div className="flex items-center gap-1.5 flex-wrap">
                            <Badge variant="outline" className="text-[14px]">
                              {log.model || '-'}
                            </Badge>
                            {log.reasoning_effort && (
                              <Badge
                                variant="outline"
                                className="text-[12px]"
                                style={{
                                  background: log.reasoning_effort === 'high' ? 'rgba(239, 68, 68, 0.12)' :
                                             log.reasoning_effort === 'medium' ? 'rgba(245, 158, 11, 0.12)' :
                                             'rgba(34, 197, 94, 0.12)',
                                  color: log.reasoning_effort === 'high' ? '#ef4444' :
                                         log.reasoning_effort === 'medium' ? '#f59e0b' :
                                         '#22c55e',
                                  borderColor: 'transparent',
                                }}
                              >
                                {log.reasoning_effort}
                              </Badge>
                            )}
                          </div>
                        </TableCell>
                        <TableCell className="text-[14px] text-muted-foreground">
                          {log.account_email || '-'}
                        </TableCell>
                        <TableCell>
                          <div className="text-[16px] leading-relaxed" style={{ fontFamily: "'Geist Mono', monospace" }}>
                            <span className="text-muted-foreground">
                              {log.inbound_endpoint || log.endpoint || '-'}
                            </span>
                            {log.upstream_endpoint && log.upstream_endpoint !== log.inbound_endpoint && (
                              <span className="text-muted-foreground"> → {log.upstream_endpoint}</span>
                            )}
                          </div>
                        </TableCell>
                        <TableCell>
                          <Badge
                            variant="outline"
                            className="text-[13px]"
                            style={{
                              background: log.stream ? 'rgba(99, 102, 241, 0.12)' : 'rgba(107, 114, 128, 0.12)',
                              color: log.stream ? '#6366f1' : '#6b7280',
                              borderColor: 'transparent',
                            }}
                          >
                            {log.stream ? 'stream' : 'sync'}
                          </Badge>
                        </TableCell>
                        <TableCell>
                          {log.status_code < 400 && (log.input_tokens > 0 || log.output_tokens > 0) ? (
                            <div className="text-[14px] leading-relaxed">
                              <span className="text-blue-500">↓{formatTokens(log.input_tokens)}</span>
                              <span className="mx-1 text-border">|</span>
                              <span className="text-emerald-500">↑{formatTokens(log.output_tokens)}</span>
                              {log.reasoning_tokens > 0 && (
                                <>
                                  <span className="mx-1 text-border">|</span>
                                  <span className="text-amber-500">💡{formatTokens(log.reasoning_tokens)}</span>
                                </>
                              )}
                            </div>
                          ) : (
                            <span className="text-[14px] text-muted-foreground">-</span>
                          )}
                        </TableCell>
                        <TableCell>
                          {log.cached_tokens > 0 ? (
                            <Badge variant="outline" className="text-[13px] gap-1" style={{ background: 'rgba(99, 102, 241, 0.10)', color: '#6366f1', borderColor: 'transparent' }}>
                              📦 {formatTokens(log.cached_tokens)}
                            </Badge>
                          ) : (
                            <span className="text-[14px] text-muted-foreground">-</span>
                          )}
                        </TableCell>
                        <TableCell>
                          {log.first_token_ms > 0 ? (
                            <span className={`text-[16px] ${log.first_token_ms > 5000 ? 'text-red-500' : log.first_token_ms > 2000 ? 'text-amber-500' : 'text-emerald-500'}`} style={{ fontFamily: "'Geist Mono', monospace" }}>
                              {log.first_token_ms > 1000 ? `${(log.first_token_ms / 1000).toFixed(1)}s` : `${log.first_token_ms}ms`}
                            </span>
                          ) : <span className="text-[16px] text-muted-foreground" style={{ fontFamily: "'Geist Mono', monospace" }}>-</span>}
                        </TableCell>
                        <TableCell>
                          <span className={`text-[16px] ${log.duration_ms > 30000 ? 'text-red-500' : log.duration_ms > 10000 ? 'text-amber-500' : 'text-muted-foreground'}`} style={{ fontFamily: "'Geist Mono', monospace" }}>
                            {log.duration_ms > 1000 ? `${(log.duration_ms / 1000).toFixed(1)}s` : `${log.duration_ms}ms`}
                          </span>
                        </TableCell>
                        <TableCell className="text-[16px] text-muted-foreground whitespace-nowrap" style={{ fontFamily: "'Geist Mono', monospace" }}>
                          {formatTime(log.created_at)}
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
                totalItems={logs.length}
                pageSize={PAGE_SIZE}
              />
            </StateShell>
          </CardContent>
        </Card>

        <ToastNotice toast={toast} />
        {confirmDialog}
      </>
    </StateShell>
  )
}
