import type { ChangeEvent, KeyboardEvent } from 'react'
import { useCallback, useEffect, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { api, resetAdminAuthState, setAdminKey } from '../api'
import { getTimezone, setTimezone } from '../utils/time'
import PageHeader from '../components/PageHeader'
import Pagination from '../components/Pagination'
import StateShell from '../components/StateShell'
import ToastNotice from '../components/ToastNotice'
import { useDataLoader } from '../hooks/useDataLoader'
import { useConfirmDialog } from '../hooks/useConfirmDialog'
import { useToast } from '../hooks/useToast'
import type { APIKeyRow, HealthResponse, ModelInfo, RedeemCodeSummary, SystemSettings } from '../types'
import { getErrorMessage } from '../utils/error'
import { formatRelativeTime } from '../utils/time'
import { Card, CardContent } from '@/components/ui/card'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Select } from '@/components/ui/select'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'

function maskKey(key: string): string {
  if (!key || key.length < 12) return key
  return key.slice(0, 7) + '???????' + key.slice(-4)
}

function normalizeFullUsageMode(mode: string | undefined, legacyEnabled: boolean): 'off' | 'delete' | 'wait' {
  if (mode === 'delete' || mode === 'wait') {
    return mode
  }
  if (mode === 'off') {
    return 'off'
  }
  return legacyEnabled ? 'delete' : 'off'
}

export default function Settings() {
  const PUBLIC_KEYS_PAGE_SIZE = 10
  const { t } = useTranslation()
  const booleanOptions = [
    { label: t('common.disabled'), value: 'false' },
    { label: t('common.enabled'), value: 'true' },
  ]
  const fullUsageModeOptions = [
    { label: t('settings.autoCleanFullUsageModeOff'), value: 'off' },
    { label: t('settings.autoCleanFullUsageModeDelete'), value: 'delete' },
    { label: t('settings.autoCleanFullUsageModeWait'), value: 'wait' },
  ]
  const imageRoutePriorityOptions = [
    { label: t('settings.imageRoutePriorityOfficialFirst'), value: 'official_first' },
    { label: t('settings.imageRoutePriorityWebFirst'), value: 'web_first' },
  ]
  const [newKeyName, setNewKeyName] = useState('')
  const [newKeyValue, setNewKeyValue] = useState('')
  const [createdKey, setCreatedKey] = useState<string | null>(null)
  const [newPublicKeyName, setNewPublicKeyName] = useState('')
  const [newPublicKeyValue, setNewPublicKeyValue] = useState('')
  const [createdPublicKey, setCreatedPublicKey] = useState<string | null>(null)
  const [redeemAmount, setRedeemAmount] = useState('1')
  const [redeemCodesText, setRedeemCodesText] = useState('')
  const [importingRedeemCodes, setImportingRedeemCodes] = useState(false)
  const [publicKeysPage, setPublicKeysPage] = useState(1)
  const [settingsForm, setSettingsForm] = useState<SystemSettings>({
    max_concurrency: 2,
    global_rpm: 0,
    test_model: '',
    test_concurrency: 50,
    background_refresh_interval_minutes: 2,
    usage_probe_max_age_minutes: 10,
    recovery_probe_interval_minutes: 30,
    pg_max_conns: 50,
    redis_pool_size: 30,
    auto_clean_unauthorized: false,
    auto_clean_rate_limited: false,
    auto_clean_error: false,
    auto_clean_expired: false,
    admin_secret: '',
    admin_auth_source: 'disabled',
    auto_clean_full_usage: false,
    auto_clean_full_usage_mode: 'off',
    proxy_pool_enabled: false,
    fast_scheduler_enabled: false,
    plus_port_enabled: false,
    plus_port_access_free: true,
    image_route_priority: 'official_first',
    scheduler_preferred_plan: '',
    scheduler_plan_bonus: 0,
    quota_rate_plus: 10,
    quota_rate_pro: 100,
    quota_rate_team: 10,
    max_retries: 2,
    allow_remote_migration: false,
    public_initial_credit_usd: 0.1,
    public_full_credit_usd: 2,
    database_driver: 'postgres',
    database_label: 'PostgreSQL',
    cache_driver: 'redis',
    cache_label: 'Redis',
  })
  const [savingSettings, setSavingSettings] = useState(false)
  const [loadedAdminSecret, setLoadedAdminSecret] = useState('')
  const [modelList, setModelList] = useState<string[]>([])
  const [modelItems, setModelItems] = useState<ModelInfo[]>([])
  const [modelsLastSyncedAt, setModelsLastSyncedAt] = useState<string | undefined>()
  const [modelsSourceURL, setModelsSourceURL] = useState('')
  const [syncingModels, setSyncingModels] = useState(false)
  const { toast, showToast } = useToast()
  const { confirm, confirmDialog } = useConfirmDialog()

  const loadSettingsData = useCallback(async () => {
    const [health, keysResponse, pubKeysResponse, redeemSummaryResponse, settings, modelsResp] = await Promise.all([
      api.getHealth(),
      api.getAPIKeys(),
      api.getPublicAPIKeys(),
      api.getRedeemCodeSummaries(),
      api.getSettings(),
      api.getModels(),
    ])
    const fullUsageMode = normalizeFullUsageMode(settings.auto_clean_full_usage_mode, settings.auto_clean_full_usage)
    setSettingsForm({
      ...settings,
      auto_clean_full_usage_mode: fullUsageMode,
      auto_clean_full_usage: fullUsageMode !== 'off',
      plus_port_enabled: settings.plus_port_enabled ?? false,
      plus_port_access_free: settings.plus_port_access_free ?? true,
      image_route_priority: settings.image_route_priority ?? 'official_first',
      scheduler_preferred_plan: settings.scheduler_preferred_plan ?? '',
      scheduler_plan_bonus: settings.scheduler_plan_bonus ?? 0,
      quota_rate_plus: settings.quota_rate_plus ?? 10,
      quota_rate_pro: settings.quota_rate_pro ?? 100,
      quota_rate_team: settings.quota_rate_team ?? 10,
    })
    setLoadedAdminSecret(settings.admin_secret ?? '')
    setModelList(modelsResp.models ?? [])
    setModelItems(modelsResp.items ?? [])
    setModelsLastSyncedAt(modelsResp.last_synced_at)
    setModelsSourceURL(modelsResp.source_url ?? '')
    return {
      health,
      keys: keysResponse.keys ?? [],
      pubKeys: pubKeysResponse.keys ?? [],
      redeemSummaries: redeemSummaryResponse.items ?? [],
    }
  }, [])

  const { data, loading, error, reload } = useDataLoader<{
    health: HealthResponse | null
    keys: APIKeyRow[]
    pubKeys: APIKeyRow[]
    redeemSummaries: RedeemCodeSummary[]
  }>({
    initialData: {
      health: null,
      keys: [],
      pubKeys: [],
      redeemSummaries: [],
    },
    load: loadSettingsData,
  })

  const handleCreateKey = async () => {
    try {
      const result = await api.createAPIKey(newKeyName.trim() || 'default', newKeyValue.trim() || undefined)
      setCreatedKey(result.key)
      setNewKeyName('')
      setNewKeyValue('')
      showToast(t('settings.keyCreateSuccess'))
      void reload()
    } catch (error) {
      showToast(`${t('settings.createFailed')}: ${getErrorMessage(error)}`, 'error')
    }
  }

  const handleDeleteKey = async (id: number) => {
    const confirmed = await confirm({
      title: t('settings.deleteKeyTitle'),
      description: t('settings.deleteKeyDesc'),
      confirmText: t('settings.confirmDelete'),
      tone: 'destructive',
      confirmVariant: 'destructive',
    })
    if (!confirmed) {
      return
    }

    try {
      await api.deleteAPIKey(id)
      showToast(t('settings.keyDeleted'))
      void reload()
    } catch (error) {
      showToast(`${t('settings.deleteFailed')}: ${getErrorMessage(error)}`, 'error')
    }
  }

  const handleCreatePublicKey = async () => {
    try {
      const result = await api.createPublicAPIKey(newPublicKeyName.trim() || 'public-upload', newPublicKeyValue.trim() || undefined)
      setCreatedPublicKey(result.key)
      setNewPublicKeyName('')
      setNewPublicKeyValue('')
      showToast(t('settings.publicKeyCreateSuccess'))
      void reload()
    } catch (error) {
      showToast(`${t('settings.createFailed')}: ${getErrorMessage(error)}`, 'error')
    }
  }

  const handleDeletePublicKey = async (id: number) => {
    const confirmed = await confirm({
      title: t('settings.deletePublicKeyTitle'),
      description: t('settings.deletePublicKeyDesc'),
      confirmText: t('settings.confirmDelete'),
      tone: 'destructive',
      confirmVariant: 'destructive',
    })
    if (!confirmed) {
      return
    }

    try {
      await api.deletePublicAPIKey(id)
      showToast(t('settings.publicKeyDeleted'))
      void reload()
    } catch (error) {
      showToast(`${t('settings.deleteFailed')}: ${getErrorMessage(error)}`, 'error')
    }
  }

  const handleImportRedeemCodes = async () => {
    const amount = Number.parseFloat(redeemAmount)
    if (!Number.isFinite(amount) || amount <= 0) {
      showToast(t('settings.redeemAmountInvalid'), 'error')
      return
    }
    if (!redeemCodesText.trim()) {
      showToast(t('settings.redeemCodesEmpty'), 'error')
      return
    }

    setImportingRedeemCodes(true)
    try {
      const result = await api.importRedeemCodes({
        amount_usd: amount,
        codes: redeemCodesText,
      })
      showToast(
        t('settings.redeemImportSuccess', {
          inserted: result.inserted,
          duplicates: result.duplicates,
        }),
      )
      setRedeemCodesText('')
      void reload()
    } catch (error) {
      showToast(`${t('settings.redeemImportFailed')}: ${getErrorMessage(error)}`, 'error')
    } finally {
      setImportingRedeemCodes(false)
    }
  }

  const handleCopy = async (text: string) => {
    try {
      if (navigator.clipboard?.writeText) {
        await navigator.clipboard.writeText(text)
        showToast(t('common.copied'))
        return
      }

      const textarea = document.createElement('textarea')
      textarea.value = text
      textarea.setAttribute('readonly', 'true')
      textarea.style.position = 'fixed'
      textarea.style.opacity = '0'
      textarea.style.pointerEvents = 'none'
      document.body.appendChild(textarea)
      textarea.select()
      textarea.setSelectionRange(0, text.length)
      const copied = document.execCommand('copy')
      document.body.removeChild(textarea)

      if (!copied) {
        throw new Error('copy failed')
      }

      showToast(t('common.copied'))
    } catch {
      showToast(t('common.copyFailed'), 'error')
    }
  }

  const handleSaveSettings = async () => {
    setSavingSettings(true)
    try {
      const adminSecretChanged = settingsForm.admin_auth_source !== 'env' && settingsForm.admin_secret !== loadedAdminSecret
      const updated = await api.updateSettings(settingsForm)
      const fullUsageMode = normalizeFullUsageMode(updated.auto_clean_full_usage_mode, updated.auto_clean_full_usage)
      setSettingsForm({
        ...updated,
        auto_clean_full_usage_mode: fullUsageMode,
        auto_clean_full_usage: fullUsageMode !== 'off',
        plus_port_enabled: updated.plus_port_enabled ?? false,
        plus_port_access_free: updated.plus_port_access_free ?? true,
        image_route_priority: updated.image_route_priority ?? 'official_first',
        scheduler_preferred_plan: updated.scheduler_preferred_plan ?? '',
        scheduler_plan_bonus: updated.scheduler_plan_bonus ?? 0,
        quota_rate_plus: updated.quota_rate_plus ?? 10,
        quota_rate_pro: updated.quota_rate_pro ?? 100,
        quota_rate_team: updated.quota_rate_team ?? 10,
      })
      setLoadedAdminSecret(updated.admin_secret ?? '')
      if (updated.admin_auth_source !== 'env') {
        setAdminKey(updated.admin_secret ?? '')
      }
      if (adminSecretChanged) {
        resetAdminAuthState()
        return
      }
      if (updated.expired_cleaned && updated.expired_cleaned > 0) {
        showToast(t('settings.expiredCleanedResult', { count: updated.expired_cleaned }))
      } else {
        showToast(t('settings.saveSuccess'))
      }
    } catch (error) {
      showToast(`${t('settings.saveFailed')}: ${getErrorMessage(error)}`, 'error')
    } finally {
      setSavingSettings(false)
    }
  }

  const handleSyncModels = async () => {
    setSyncingModels(true)
    try {
      const result = await api.syncModels()
      setModelList(result.models ?? [])
      setModelItems(result.items ?? [])
      setModelsLastSyncedAt(result.last_synced_at)
      setModelsSourceURL(result.source_url ?? '')
      showToast(
        t('settings.modelsSyncSuccess', {
          added: result.added,
          updated: result.updated,
          skipped: result.skipped?.length ?? 0,
        }),
      )
    } catch (error) {
      showToast(`${t('settings.modelsSyncFailed')}: ${getErrorMessage(error)}`, 'error')
    } finally {
      setSyncingModels(false)
    }
  }

  const { health, keys, pubKeys, redeemSummaries } = data
  const publicKeysTotalPages = Math.max(1, Math.ceil(pubKeys.length / PUBLIC_KEYS_PAGE_SIZE))
  const publicKeysSafePage = Math.min(publicKeysPage, publicKeysTotalPages)
  const publicKeysPageItems = pubKeys.slice((publicKeysSafePage - 1) * PUBLIC_KEYS_PAGE_SIZE, publicKeysSafePage * PUBLIC_KEYS_PAGE_SIZE)
  const isExternalDatabase = settingsForm.database_driver === 'postgres'
  const isExternalCache = settingsForm.cache_driver === 'redis'
  const showConnectionPool = isExternalDatabase || isExternalCache
  const canConfigureRemoteMigration = settingsForm.admin_auth_source === 'env' || settingsForm.admin_secret.trim() !== ''
  useEffect(() => {
    if (publicKeysPage > publicKeysTotalPages) {
      setPublicKeysPage(publicKeysTotalPages)
    }
  }, [publicKeysPage, publicKeysTotalPages])

  const preferredPlanOptions = [
    { label: t('settings.schedulerPreferredPlanOff'), value: '' },
    { label: 'Free', value: 'free' },
    { label: 'Plus', value: 'plus' },
    { label: 'Pro', value: 'pro' },
    { label: 'Team', value: 'team' },
    { label: 'Enterprise', value: 'enterprise' },
  ]
  const visibleModelItems = modelItems.length > 0
    ? modelItems
    : modelList.map((id) => ({
        id,
        enabled: true,
        category: id.includes('image') ? 'image' : 'codex',
        source: 'builtin',
        pro_only: id === 'gpt-5.3-codex-spark' || id === 'gpt-5.5-pro',
        api_key_auth_available: id !== 'gpt-5.5',
      }))
  const textModelOptions = visibleModelItems
    .filter((model) => model.enabled && model.category !== 'image' && !model.id.includes('image'))
    .map((model) => ({ label: model.id, value: model.id }))
  const enabledModelCount = visibleModelItems.filter((model) => model.enabled).length
  const modelsLastSyncedLabel = modelsLastSyncedAt
    ? formatRelativeTime(modelsLastSyncedAt, { variant: 'compact' })
    : t('settings.modelsNeverSynced')
  const modelsSourceLabel = modelsSourceURL || 'https://developers.openai.com/codex/models'

  return (
    <StateShell
      variant="page"
      loading={loading}
      error={error}
      onRetry={() => void reload()}
      loadingTitle={t('settings.loadingTitle')}
      loadingDescription={t('settings.loadingDesc')}
      errorTitle={t('settings.errorTitle')}
    >
      <>
        <PageHeader
          title={t('settings.title')}
          description={t('settings.description')}
        />

        {/* API Keys */}
        <Card className="mb-4">
          <CardContent className="p-6">
            <div className="flex items-center justify-between gap-4 mb-4">
              <h3 className="text-base font-semibold text-foreground">{t('settings.apiKeys')}</h3>
            </div>

            <div className="flex gap-2 mb-4 flex-wrap">
              <Input
                className="flex-[1_1_120px]"
                placeholder={t('settings.keyNamePlaceholder')}
                value={newKeyName}
                onChange={(event: ChangeEvent<HTMLInputElement>) => setNewKeyName(event.target.value)}
              />
              <Input
                className="flex-[2_1_240px]"
                placeholder={t('settings.keyValuePlaceholder')}
                value={newKeyValue}
                onChange={(event: ChangeEvent<HTMLInputElement>) => setNewKeyValue(event.target.value)}
                onKeyDown={(event: KeyboardEvent<HTMLInputElement>) => {
                  if (event.key === 'Enter') {
                    void handleCreateKey()
                  }
                }}
              />
              <Button onClick={() => void handleCreateKey()} className="whitespace-nowrap">
                {t('settings.createKey')}
              </Button>
            </div>

            {createdKey ? (
              <div className="p-3 mb-4 rounded-xl bg-[hsl(var(--success-bg))] border border-[hsl(var(--success))]/20 text-sm">
                <div className="font-semibold mb-1 text-[hsl(var(--success))]">{t('settings.keyCreated')}</div>
                <div className="flex items-center gap-2">
                  <code className="flex-1 font-mono text-[13px] break-all">{createdKey}</code>
                  <Button variant="outline" size="sm" onClick={() => void handleCopy(createdKey)}>{t('common.copy')}</Button>
                </div>
              </div>
            ) : null}

            <StateShell
              variant="section"
              isEmpty={keys.length === 0}
              emptyTitle={t('settings.noKeys')}
              emptyDescription={t('settings.noKeysDesc')}
            >
              <div className="overflow-auto border border-border rounded-xl">
                <Table>
                  <TableHeader>
                    <TableRow>
                      <TableHead className="text-[13px] font-semibold">{t('common.name')}</TableHead>
                      <TableHead className="text-[13px] font-semibold">{t('common.key')}</TableHead>
                      <TableHead className="text-[13px] font-semibold">{t('common.createdAt')}</TableHead>
                      <TableHead className="text-[13px] font-semibold">{t('common.actions')}</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {keys.map((keyRow) => (
                      <TableRow key={keyRow.id}>
                        <TableCell className="text-[14px] font-medium">{keyRow.name}</TableCell>
                        <TableCell>
                          <span className="font-mono text-[20px]">{maskKey(keyRow.key)}</span>
                        </TableCell>
                        <TableCell className="text-[14px] text-muted-foreground">
                          {formatRelativeTime(keyRow.created_at, { variant: 'compact' })}
                        </TableCell>
                        <TableCell>
                          <Button variant="destructive" size="sm" onClick={() => void handleDeleteKey(keyRow.id)}>
                            {t('common.delete')}
                          </Button>
                        </TableCell>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
              </div>
            </StateShell>

            <div className="text-xs text-muted-foreground mt-3">
              {t('settings.keyAuthNote')}
            </div>
          </CardContent>
        </Card>

        {/* Public Upload API Keys + Redeem Codes */}
        <Card className="mb-4">
          <CardContent className="p-6">
            <div className="grid gap-6 lg:grid-cols-2">
              <div>
                <h3 className="text-base font-semibold text-foreground mb-4">{t('settings.publicApiKeys')}</h3>
                <div className="flex gap-2 mb-4 flex-wrap">
                  <Input
                    className="flex-[1_1_120px]"
                    placeholder={t('settings.keyNamePlaceholder')}
                    value={newPublicKeyName}
                    onChange={(event: ChangeEvent<HTMLInputElement>) => setNewPublicKeyName(event.target.value)}
                  />
                  <Input
                    className="flex-[2_1_240px]"
                    placeholder={t('settings.keyValuePlaceholder')}
                    value={newPublicKeyValue}
                    onChange={(event: ChangeEvent<HTMLInputElement>) => setNewPublicKeyValue(event.target.value)}
                    onKeyDown={(event: KeyboardEvent<HTMLInputElement>) => {
                      if (event.key === 'Enter') {
                        void handleCreatePublicKey()
                      }
                    }}
                  />
                  <Button onClick={() => void handleCreatePublicKey()} className="whitespace-nowrap">
                    {t('settings.createKey')}
                  </Button>
                </div>

                {createdPublicKey ? (
                  <div className="p-3 mb-4 rounded-xl bg-[hsl(var(--success-bg))] border border-[hsl(var(--success))]/20 text-sm">
                    <div className="font-semibold mb-1 text-[hsl(var(--success))]">{t('settings.publicKeyCreated')}</div>
                    <div className="flex items-center gap-2">
                      <code className="flex-1 font-mono text-[13px] break-all">{createdPublicKey}</code>
                      <Button variant="outline" size="sm" onClick={() => void handleCopy(createdPublicKey)}>{t('common.copy')}</Button>
                    </div>
                  </div>
                ) : null}

                <StateShell
                  variant="section"
                  isEmpty={pubKeys.length === 0}
                  emptyTitle={t('settings.noPublicKeys')}
                  emptyDescription={t('settings.noPublicKeysDesc')}
                >
                  <div className="overflow-auto border border-border rounded-xl">
                    <Table>
                      <TableHeader>
                        <TableRow>
                          <TableHead className="text-[13px] font-semibold">{t('common.name')}</TableHead>
                          <TableHead className="text-[13px] font-semibold">{t('common.key')}</TableHead>
                          <TableHead className="text-[13px] font-semibold">{t('common.createdAt')}</TableHead>
                          <TableHead className="text-[13px] font-semibold">{t('common.actions')}</TableHead>
                        </TableRow>
                      </TableHeader>
                      <TableBody>
                        {publicKeysPageItems.map((keyRow) => (
                          <TableRow key={keyRow.id}>
                            <TableCell className="text-[14px] font-medium">{keyRow.name}</TableCell>
                            <TableCell>
                              <span className="font-mono text-[20px]">{maskKey(keyRow.key)}</span>
                            </TableCell>
                            <TableCell className="text-[14px] text-muted-foreground">
                              {formatRelativeTime(keyRow.created_at, { variant: 'compact' })}
                            </TableCell>
                            <TableCell>
                              <Button variant="destructive" size="sm" onClick={() => void handleDeletePublicKey(keyRow.id)}>
                                {t('common.delete')}
                              </Button>
                            </TableCell>
                          </TableRow>
                        ))}
                      </TableBody>
                    </Table>
                  </div>
                  <Pagination
                    page={publicKeysSafePage}
                    totalPages={publicKeysTotalPages}
                    onPageChange={setPublicKeysPage}
                    totalItems={pubKeys.length}
                    pageSize={PUBLIC_KEYS_PAGE_SIZE}
                  />
                </StateShell>

                <div className="text-xs text-muted-foreground mt-3">
                  {t('settings.publicKeyAuthNote')}
                </div>
              </div>

              <div>
                <h3 className="text-base font-semibold text-foreground mb-4">{t('settings.redeemCodesTitle')}</h3>
                <div className="space-y-3">
                  <div>
                    <label className="block mb-2 text-sm font-semibold text-muted-foreground">{t('settings.redeemAmountLabel')}</label>
                    <Input
                      type="number"
                      min={0.0001}
                      step="0.0001"
                      value={redeemAmount}
                      onChange={(event: ChangeEvent<HTMLInputElement>) => setRedeemAmount(event.target.value)}
                    />
                  </div>
                  <div>
                    <label className="block mb-2 text-sm font-semibold text-muted-foreground">{t('settings.redeemCodesLabel')}</label>
                    <textarea
                      className="w-full min-h-[140px] rounded-md border border-input bg-transparent px-3 py-2 text-sm"
                      placeholder={t('settings.redeemCodesPlaceholder')}
                      value={redeemCodesText}
                      onChange={(event) => setRedeemCodesText(event.target.value)}
                    />
                  </div>
                  <Button onClick={() => void handleImportRedeemCodes()} disabled={importingRedeemCodes}>
                    {importingRedeemCodes ? t('settings.redeemImporting') : t('settings.redeemImportBtn')}
                  </Button>
                </div>

                <div className="mt-4 overflow-auto border border-border rounded-xl">
                  <Table>
                    <TableHeader>
                      <TableRow>
                        <TableHead className="text-[13px] font-semibold">{t('settings.redeemAmountCol')}</TableHead>
                        <TableHead className="text-[13px] font-semibold">{t('settings.redeemCountCol')}</TableHead>
                      </TableRow>
                    </TableHeader>
                    <TableBody>
                      {redeemSummaries.length === 0 ? (
                        <TableRow>
                          <TableCell colSpan={2} className="text-[13px] text-muted-foreground">
                            {t('settings.redeemEmpty')}
                          </TableCell>
                        </TableRow>
                      ) : (
                        redeemSummaries.map((row) => (
                          <TableRow key={`${row.amount_usd}`}>
                            <TableCell className="font-mono text-[14px]">${row.amount_usd}</TableCell>
                            <TableCell className="text-[14px]">{row.count}</TableCell>
                          </TableRow>
                        ))
                      )}
                    </TableBody>
                  </Table>
                </div>
                <div className="text-xs text-muted-foreground mt-3">
                  {t('settings.redeemHint')}
                </div>
              </div>
            </div>
          </CardContent>
        </Card>

        {/* System Status */}
        <Card className="mb-4">
          <CardContent className="p-6">
            <h3 className="text-base font-semibold text-foreground mb-4">{t('settings.systemStatus')}</h3>
            <div className="grid grid-cols-[repeat(auto-fit,minmax(220px,1fr))] gap-3.5">
              <div className="flex flex-col gap-1.5 p-3.5 rounded-2xl bg-white/40 border border-border">
                <label className="text-xs font-bold text-muted-foreground">{t('settings.service')}</label>
                <div className="text-[15px] font-semibold">
                  <Badge variant={health?.status === 'ok' ? 'default' : 'destructive'} className="gap-1.5">
                    <span className={`size-1.5 rounded-full ${health?.status === 'ok' ? 'bg-emerald-500' : 'bg-red-400'}`} />
                    {health?.status === 'ok' ? t('common.running') : t('common.error')}
                  </Badge>
                </div>
              </div>
              <div className="flex flex-col gap-1.5 p-3.5 rounded-2xl bg-white/40 border border-border">
                <label className="text-xs font-bold text-muted-foreground">{t('settings.accountsLabel')}</label>
                <div className="text-[15px] font-semibold">{health?.available ?? 0} / {health?.total ?? 0}</div>
              </div>
              <div className="flex flex-col gap-1.5 p-3.5 rounded-2xl bg-white/40 border border-border">
                <label className="text-xs font-bold text-muted-foreground">{settingsForm.database_label}</label>
                <div className="text-[15px] font-semibold">
                  <Badge variant="default" className="gap-1.5">
                    <span className="size-1.5 rounded-full bg-emerald-500" />
                    {isExternalDatabase ? t('common.connected') : t('common.running')}
                  </Badge>
                </div>
              </div>
              <div className="flex flex-col gap-1.5 p-3.5 rounded-2xl bg-white/40 border border-border">
                <label className="text-xs font-bold text-muted-foreground">{settingsForm.cache_label}</label>
                <div className="text-[15px] font-semibold">
                  <Badge variant="default" className="gap-1.5">
                    <span className="size-1.5 rounded-full bg-emerald-500" />
                    {isExternalCache ? t('common.connected') : t('common.running')}
                  </Badge>
                </div>
              </div>
            </div>
          </CardContent>
        </Card>

        {/* Protection Settings */}
        <Card className="mb-4">
          <CardContent className="p-6">
            <h3 className="text-base font-semibold text-foreground mb-4">{t('settings.trafficProtection')}</h3>
            <div className="grid grid-cols-[repeat(auto-fit,minmax(220px,1fr))] gap-4 mb-4">
              <div>
                <label className="block mb-2 text-sm font-semibold text-muted-foreground">{t('settings.maxConcurrency')}</label>
                <Input
                  type="number"
                  min={1}
                  max={50}
                  value={settingsForm.max_concurrency}
                  onChange={(e: ChangeEvent<HTMLInputElement>) => setSettingsForm(f => ({ ...f, max_concurrency: parseInt(e.target.value) || 1 }))}
                />
                <p className="text-xs text-muted-foreground mt-1">{t('settings.maxConcurrencyRange')}</p>
              </div>
              <div>
                <label className="block mb-2 text-sm font-semibold text-muted-foreground">{t('settings.globalRpm')}</label>
                <Input
                  type="number"
                  min={0}
                  value={settingsForm.global_rpm}
                  onChange={(e: ChangeEvent<HTMLInputElement>) => setSettingsForm(f => ({ ...f, global_rpm: parseInt(e.target.value) || 0 }))}
                />
                <p className="text-xs text-muted-foreground mt-1">{t('settings.globalRpmRange')}</p>
              </div>
              <div>
                <label className="block mb-2 text-sm font-semibold text-muted-foreground">{t('settings.maxRetries')}</label>
                <Input
                  type="number"
                  min={0}
                  max={10}
                  value={settingsForm.max_retries}
                  onChange={(e: ChangeEvent<HTMLInputElement>) => setSettingsForm(f => ({ ...f, max_retries: parseInt(e.target.value) || 0 }))}
                />
                <p className="text-xs text-muted-foreground mt-1">{t('settings.maxRetriesRange')}</p>
              </div>
              <div>
                <label className="block mb-2 text-sm font-semibold text-muted-foreground">{t('settings.publicInitialCredit')}</label>
                <Input
                  type="number"
                  min={0}
                  step="0.0001"
                  value={settingsForm.public_initial_credit_usd}
                  onChange={(e: ChangeEvent<HTMLInputElement>) =>
                    setSettingsForm((f) => ({ ...f, public_initial_credit_usd: Number.parseFloat(e.target.value) || 0 }))
                  }
                />
                <p className="text-xs text-muted-foreground mt-1">{t('settings.publicInitialCreditDesc')}</p>
              </div>
              <div>
                <label className="block mb-2 text-sm font-semibold text-muted-foreground">{t('settings.publicFullCredit')}</label>
                <Input
                  type="number"
                  min={0}
                  step="0.0001"
                  value={settingsForm.public_full_credit_usd}
                  onChange={(e: ChangeEvent<HTMLInputElement>) =>
                    setSettingsForm((f) => ({ ...f, public_full_credit_usd: Number.parseFloat(e.target.value) || 0 }))
                  }
                />
                <p className="text-xs text-muted-foreground mt-1">{t('settings.publicFullCreditDesc')}</p>
              </div>
              <div>
                <label className="block mb-2 text-sm font-semibold text-muted-foreground">{t('settings.quotaRatePlus')}</label>
                <Input
                  type="number"
                  min={0.0001}
                  step="0.0001"
                  value={settingsForm.quota_rate_plus}
                  onChange={(e: ChangeEvent<HTMLInputElement>) =>
                    setSettingsForm((f) => ({ ...f, quota_rate_plus: Number.parseFloat(e.target.value) || 0 }))
                  }
                />
                <p className="text-xs text-muted-foreground mt-1">{t('settings.quotaRatePlusDesc')}</p>
              </div>
              <div>
                <label className="block mb-2 text-sm font-semibold text-muted-foreground">{t('settings.quotaRatePro')}</label>
                <Input
                  type="number"
                  min={0.0001}
                  step="0.0001"
                  value={settingsForm.quota_rate_pro}
                  onChange={(e: ChangeEvent<HTMLInputElement>) =>
                    setSettingsForm((f) => ({ ...f, quota_rate_pro: Number.parseFloat(e.target.value) || 0 }))
                  }
                />
                <p className="text-xs text-muted-foreground mt-1">{t('settings.quotaRateProDesc')}</p>
              </div>
              <div>
                <label className="block mb-2 text-sm font-semibold text-muted-foreground">{t('settings.quotaRateTeam')}</label>
                <Input
                  type="number"
                  min={0.0001}
                  step="0.0001"
                  value={settingsForm.quota_rate_team}
                  onChange={(e: ChangeEvent<HTMLInputElement>) =>
                    setSettingsForm((f) => ({ ...f, quota_rate_team: Number.parseFloat(e.target.value) || 0 }))
                  }
                />
                <p className="text-xs text-muted-foreground mt-1">{t('settings.quotaRateTeamDesc')}</p>
              </div>
              <div>
                <label className="block mb-2 text-sm font-semibold text-muted-foreground">{t('settings.testModelLabel')}</label>
                <Select
                  value={settingsForm.test_model}
                  onValueChange={(value) => setSettingsForm((f) => ({ ...f, test_model: value }))}
                  options={textModelOptions}
                />
                <p className="text-xs text-muted-foreground mt-1">{t('settings.testModelHint')}</p>
              </div>
              <div>
                <label className="block mb-2 text-sm font-semibold text-muted-foreground">{t('settings.testConcurrency')}</label>
                <Input
                  type="number"
                  min={1}
                  max={200}
                  value={settingsForm.test_concurrency}
                  onChange={(e: ChangeEvent<HTMLInputElement>) => setSettingsForm(f => ({ ...f, test_concurrency: parseInt(e.target.value) || 1 }))}
                />
                <p className="text-xs text-muted-foreground mt-1">{t('settings.testConcurrencyRange')}</p>
              </div>
              <div>
                <label className="block mb-2 text-sm font-semibold text-muted-foreground">{t('settings.backgroundRefreshInterval')}</label>
                <Input
                  type="number"
                  min={1}
                  max={1440}
                  value={settingsForm.background_refresh_interval_minutes}
                  onChange={(e: ChangeEvent<HTMLInputElement>) => setSettingsForm(f => ({ ...f, background_refresh_interval_minutes: parseInt(e.target.value) || 1 }))}
                />
                <p className="text-xs text-muted-foreground mt-1">{t('settings.backgroundRefreshIntervalDesc')}</p>
              </div>
              <div>
                <label className="block mb-2 text-sm font-semibold text-muted-foreground">{t('settings.usageProbeMaxAge')}</label>
                <Input
                  type="number"
                  min={1}
                  max={10080}
                  value={settingsForm.usage_probe_max_age_minutes}
                  onChange={(e: ChangeEvent<HTMLInputElement>) => setSettingsForm(f => ({ ...f, usage_probe_max_age_minutes: parseInt(e.target.value) || 1 }))}
                />
                <p className="text-xs text-muted-foreground mt-1">{t('settings.usageProbeMaxAgeDesc')}</p>
              </div>
              <div>
                <label className="block mb-2 text-sm font-semibold text-muted-foreground">{t('settings.recoveryProbeInterval')}</label>
                <Input
                  type="number"
                  min={1}
                  max={10080}
                  value={settingsForm.recovery_probe_interval_minutes}
                  onChange={(e: ChangeEvent<HTMLInputElement>) => setSettingsForm(f => ({ ...f, recovery_probe_interval_minutes: parseInt(e.target.value) || 1 }))}
                />
                <p className="text-xs text-muted-foreground mt-1">{t('settings.recoveryProbeIntervalDesc')}</p>
              </div>
            </div>
            {showConnectionPool ? (
              <>
                <h3 className="text-base font-semibold text-foreground mb-4 mt-6">{t('settings.connectionPool')}</h3>
                <div className="grid grid-cols-[repeat(auto-fit,minmax(220px,1fr))] gap-4 mb-4">
                  {isExternalDatabase ? (
                    <div>
                      <label className="block mb-2 text-sm font-semibold text-muted-foreground">{t('settings.pgMaxConns')}</label>
                      <Input
                        type="number"
                        min={5}
                        max={500}
                        value={settingsForm.pg_max_conns}
                        onChange={(e: ChangeEvent<HTMLInputElement>) => setSettingsForm(f => ({ ...f, pg_max_conns: parseInt(e.target.value) || 50 }))}
                      />
                      <p className="text-xs text-muted-foreground mt-1">{t('settings.pgMaxConnsRange')}</p>
                    </div>
                  ) : null}
                  {isExternalCache ? (
                    <div>
                      <label className="block mb-2 text-sm font-semibold text-muted-foreground">{t('settings.redisPoolSize')}</label>
                      <Input
                        type="number"
                        min={5}
                        max={500}
                        value={settingsForm.redis_pool_size}
                        onChange={(e: ChangeEvent<HTMLInputElement>) => setSettingsForm(f => ({ ...f, redis_pool_size: parseInt(e.target.value) || 30 }))}
                      />
                      <p className="text-xs text-muted-foreground mt-1">{t('settings.redisPoolSizeRange')}</p>
                    </div>
                  ) : null}
                </div>
              </>
            ) : null}
            <h3 className="text-base font-semibold text-foreground mb-4 mt-6">{t('settings.autoCleanup')}</h3>
            <div className="grid grid-cols-[repeat(auto-fit,minmax(280px,1fr))] gap-4 mb-4">
              <div>
                <label className="block mb-2 text-sm font-semibold text-muted-foreground">{t('settings.autoCleanUnauthorized')}</label>
                <Select
                  value={settingsForm.auto_clean_unauthorized ? 'true' : 'false'}
                  onValueChange={(value) => setSettingsForm((f) => ({ ...f, auto_clean_unauthorized: value === 'true' }))}
                  options={booleanOptions}
                />
                <p className="text-xs text-muted-foreground mt-1">{t('settings.autoCleanUnauthorizedDesc')}</p>
              </div>
              <div>
                <label className="block mb-2 text-sm font-semibold text-muted-foreground">{t('settings.autoCleanRateLimited')}</label>
                <Select
                  value={settingsForm.auto_clean_rate_limited ? 'true' : 'false'}
                  onValueChange={(value) => setSettingsForm((f) => ({ ...f, auto_clean_rate_limited: value === 'true' }))}
                  options={booleanOptions}
                />
                <p className="text-xs text-muted-foreground mt-1">{t('settings.autoCleanRateLimitedDesc')}</p>
              </div>
              <div>
                <label className="block mb-2 text-sm font-semibold text-muted-foreground">{t('settings.autoCleanFullUsage')}</label>
                <Select
                  value={normalizeFullUsageMode(settingsForm.auto_clean_full_usage_mode, settingsForm.auto_clean_full_usage)}
                  onValueChange={(value) => {
                    const nextMode = normalizeFullUsageMode(value, false)
                    setSettingsForm((f) => ({
                      ...f,
                      auto_clean_full_usage_mode: nextMode,
                      auto_clean_full_usage: nextMode !== 'off',
                    }))
                  }}
                  options={fullUsageModeOptions}
                />
                <p className="text-xs text-muted-foreground mt-1">{t('settings.autoCleanFullUsageDesc')}</p>
              </div>
              <div>
                <label className="block mb-2 text-sm font-semibold text-muted-foreground">{t('settings.autoCleanError')}</label>
                <Select
                  value={settingsForm.auto_clean_error ? 'true' : 'false'}
                  onValueChange={(value) => setSettingsForm((f) => ({ ...f, auto_clean_error: value === 'true' }))}
                  options={booleanOptions}
                />
                <p className="text-xs text-muted-foreground mt-1">{t('settings.autoCleanErrorDesc')}</p>
              </div>
              <div>
                <label className="block mb-2 text-sm font-semibold text-muted-foreground">{t('settings.autoCleanExpired')}</label>
                <Select
                  value={settingsForm.auto_clean_expired ? 'true' : 'false'}
                  onValueChange={(value) => setSettingsForm((f) => ({ ...f, auto_clean_expired: value === 'true' }))}
                  options={booleanOptions}
                />
                <p className="text-xs text-muted-foreground mt-1">{t('settings.autoCleanExpiredDesc')}</p>
              </div>
            </div>
            <h3 className="text-base font-semibold text-foreground mb-4 mt-6">{t('settings.scheduler')}</h3>
            <div className="grid grid-cols-[repeat(auto-fit,minmax(280px,1fr))] gap-4 mb-4">
              <div>
                <label className="block mb-2 text-sm font-semibold text-muted-foreground">{t('settings.fastSchedulerEnabled')}</label>
                <Select
                  value={settingsForm.fast_scheduler_enabled ? 'true' : 'false'}
                  onValueChange={(value) => setSettingsForm((f) => ({ ...f, fast_scheduler_enabled: value === 'true' }))}
                  options={booleanOptions}
                />
                <p className="text-xs text-muted-foreground mt-1">{t('settings.fastSchedulerEnabledDesc')}</p>
              </div>
              <div>
                <label className="block mb-2 text-sm font-semibold text-muted-foreground">{t('settings.plusPortEnabled')}</label>
                <Select
                  value={settingsForm.plus_port_enabled ? 'true' : 'false'}
                  onValueChange={(value) => setSettingsForm((f) => ({ ...f, plus_port_enabled: value === 'true' }))}
                  options={booleanOptions}
                />
                <p className="text-xs text-muted-foreground mt-1">{t('settings.plusPortEnabledDesc')}</p>
              </div>
              <div>
                <label className="block mb-2 text-sm font-semibold text-muted-foreground">{t('settings.plusPortAccessFree')}</label>
                <Select
                  value={settingsForm.plus_port_access_free ? 'true' : 'false'}
                  disabled={!settingsForm.plus_port_enabled}
                  onValueChange={(value) => setSettingsForm((f) => ({ ...f, plus_port_access_free: value === 'true' }))}
                  options={booleanOptions}
                />
                <p className="text-xs text-muted-foreground mt-1">{t('settings.plusPortAccessFreeDesc')}</p>
              </div>
              <div>
                <label className="block mb-2 text-sm font-semibold text-muted-foreground">{t('settings.imageRoutePriority')}</label>
                <Select
                  value={settingsForm.image_route_priority || 'official_first'}
                  disabled={!settingsForm.plus_port_enabled}
                  onValueChange={(value) => setSettingsForm((f) => ({ ...f, image_route_priority: value }))}
                  options={imageRoutePriorityOptions}
                />
                <p className="text-xs text-muted-foreground mt-1">{t('settings.imageRoutePriorityDesc')}</p>
              </div>
              <div>
                <label className="block mb-2 text-sm font-semibold text-muted-foreground">{t('settings.schedulerPreferredPlan')}</label>
                <Select
                  value={settingsForm.scheduler_preferred_plan || ''}
                  onValueChange={(value) => setSettingsForm((f) => ({ ...f, scheduler_preferred_plan: value }))}
                  options={preferredPlanOptions}
                />
                <p className="text-xs text-muted-foreground mt-1">{t('settings.schedulerPreferredPlanDesc')}</p>
              </div>
              <div>
                <label className="block mb-2 text-sm font-semibold text-muted-foreground">{t('settings.schedulerPlanBonus')}</label>
                <Input
                  type="number"
                  min={0}
                  max={200}
                  value={settingsForm.scheduler_plan_bonus}
                  onChange={(e: ChangeEvent<HTMLInputElement>) =>
                    setSettingsForm((f) => ({ ...f, scheduler_plan_bonus: Number.parseInt(e.target.value, 10) || 0 }))
                  }
                />
                <p className="text-xs text-muted-foreground mt-1">{t('settings.schedulerPlanBonusDesc')}</p>
              </div>
            </div>
            <h3 className="text-base font-semibold text-foreground mb-4 mt-6">{t('settings.display')}</h3>
            <div className="grid grid-cols-[repeat(auto-fit,minmax(280px,1fr))] gap-4 mb-4">
              <div>
                <label className="block mb-2 text-sm font-semibold text-muted-foreground">{t('settings.timezone')}</label>
                <Select
                  value={getTimezone()}
                  onValueChange={(value) => {
                    setTimezone(value)
                    window.location.reload()
                  }}
                  options={[
                    { label: t('settings.timezoneAuto'), value: Intl.DateTimeFormat().resolvedOptions().timeZone },
                    { label: '(UTC) UTC', value: 'UTC' },
                    { label: '(GMT+08:00) Asia/Shanghai', value: 'Asia/Shanghai' },
                    { label: '(GMT+09:00) Asia/Tokyo', value: 'Asia/Tokyo' },
                    { label: '(GMT+09:00) Asia/Seoul', value: 'Asia/Seoul' },
                    { label: '(GMT+08:00) Asia/Singapore', value: 'Asia/Singapore' },
                    { label: '(GMT+08:00) Asia/Hong_Kong', value: 'Asia/Hong_Kong' },
                    { label: '(GMT+08:00) Asia/Taipei', value: 'Asia/Taipei' },
                    { label: '(GMT+07:00) Asia/Bangkok', value: 'Asia/Bangkok' },
                    { label: '(GMT+04:00) Asia/Dubai', value: 'Asia/Dubai' },
                    { label: '(GMT+05:30) Asia/Kolkata', value: 'Asia/Kolkata' },
                    { label: '(GMT+01:00) Europe/London', value: 'Europe/London' },
                    { label: '(GMT+02:00) Europe/Paris', value: 'Europe/Paris' },
                    { label: '(GMT+02:00) Europe/Berlin', value: 'Europe/Berlin' },
                    { label: '(GMT+03:00) Europe/Moscow', value: 'Europe/Moscow' },
                    { label: '(GMT+02:00) Europe/Amsterdam', value: 'Europe/Amsterdam' },
                    { label: '(GMT+02:00) Europe/Rome', value: 'Europe/Rome' },
                    { label: '(GMT-04:00) America/New_York', value: 'America/New_York' },
                    { label: '(GMT-07:00) America/Los_Angeles', value: 'America/Los_Angeles' },
                    { label: '(GMT-05:00) America/Chicago', value: 'America/Chicago' },
                    { label: '(GMT-03:00) America/Sao_Paulo', value: 'America/Sao_Paulo' },
                    { label: '(GMT+10:00) Australia/Sydney', value: 'Australia/Sydney' },
                    { label: '(GMT+12:00) Pacific/Auckland', value: 'Pacific/Auckland' },
                  ]}
                />
                <p className="text-xs text-muted-foreground mt-1">{t('settings.timezoneDesc')}</p>
              </div>
            </div>
            <h3 className="text-base font-semibold text-foreground mb-4 mt-6">{t('settings.security')}</h3>
            <div className="grid grid-cols-[repeat(auto-fit,minmax(280px,1fr))] gap-4 mb-4">
              <div>
                <label className="block mb-2 text-sm font-semibold text-muted-foreground">{t('settings.adminSecret')}</label>
                <Input
                  type="text"
                  placeholder={t('settings.adminSecretPlaceholder')}
                  value={settingsForm.admin_secret}
                  disabled={settingsForm.admin_auth_source === 'env'}
                  onChange={(e: ChangeEvent<HTMLInputElement>) => setSettingsForm(f => {
                    const nextSecret = e.target.value
                    return {
                      ...f,
                      admin_secret: nextSecret,
                      allow_remote_migration: nextSecret.trim() === '' ? false : f.allow_remote_migration,
                    }
                  })}
                />
                <p className="text-xs text-muted-foreground mt-1">{t('settings.adminSecretDesc')}</p>
                {settingsForm.admin_auth_source === 'env' ? (
                  <p className="text-xs text-amber-600 mt-1">{t('settings.adminSecretEnvOverride')}</p>
                ) : null}
              </div>
              <div>
                <label className="block mb-2 text-sm font-semibold text-muted-foreground">{t('settings.allowRemoteMigration')}</label>
                <Select
                  value={settingsForm.allow_remote_migration ? 'true' : 'false'}
                  disabled={!canConfigureRemoteMigration}
                  onValueChange={(value) => setSettingsForm((f) => ({ ...f, allow_remote_migration: value === 'true' }))}
                  options={booleanOptions}
                />
                <p className="text-xs text-muted-foreground mt-1">{t('settings.allowRemoteMigrationDesc')}</p>
                {!canConfigureRemoteMigration ? (
                  <p className="text-xs text-amber-600 mt-1">{t('settings.allowRemoteMigrationRequiresSecret')}</p>
                ) : null}
              </div>
            </div>
            <Button onClick={() => void handleSaveSettings()} disabled={savingSettings}>
              {savingSettings ? t('common.saving') : t('settings.saveSettings')}
            </Button>
          </CardContent>
        </Card>

        <Card className="mb-4">
          <CardContent className="p-6">
            <div className="flex flex-wrap items-start justify-between gap-4 mb-4">
              <div>
                <h3 className="text-base font-semibold text-foreground">{t('settings.syncUpstreamModels')}</h3>
                <p className="text-sm text-muted-foreground mt-1 break-all">{modelsSourceLabel}</p>
              </div>
              <Button variant="outline" onClick={() => void handleSyncModels()} disabled={syncingModels}>
                {syncingModels ? t('settings.modelsSyncing') : t('settings.syncUpstreamModels')}
              </Button>
            </div>
            <div className="grid gap-3 md:grid-cols-2 lg:grid-cols-3 mb-4">
              <div className="rounded-xl border border-border bg-muted/20 px-4 py-3">
                <div className="text-xs text-muted-foreground mb-1">{t('settings.modelsEnabled')}</div>
                <div className="text-lg font-semibold text-foreground">{enabledModelCount}</div>
              </div>
              <div className="rounded-xl border border-border bg-muted/20 px-4 py-3">
                <div className="text-xs text-muted-foreground mb-1">{t('settings.modelsLastSynced')}</div>
                <div className="text-sm font-semibold text-foreground">{modelsLastSyncedLabel}</div>
              </div>
              <div className="rounded-xl border border-border bg-muted/20 px-4 py-3">
                <div className="text-xs text-muted-foreground mb-1">{t('settings.modelList')}</div>
                <div className="text-sm font-semibold text-foreground">{visibleModelItems.length}</div>
              </div>
            </div>
            <div className="flex max-h-[220px] flex-wrap gap-2 overflow-auto rounded-xl border border-border bg-muted/10 p-3">
              {visibleModelItems.map((model) => (
                <div key={model.id} className="flex flex-wrap items-center gap-1.5 rounded-lg border border-border bg-background px-2.5 py-1.5">
                  <span className="font-mono text-xs font-semibold text-foreground">{model.id}</span>
                  <Badge variant={model.source === 'official_codex_docs' ? 'default' : 'secondary'} className="text-[11px]">
                    {model.source === 'official_codex_docs' ? t('settings.modelSourceOfficial') : t('settings.modelSourceBuiltin')}
                  </Badge>
                  {model.pro_only ? <Badge variant="outline" className="text-[11px]">{t('settings.modelProOnly')}</Badge> : null}
                  {model.category === 'image' ? <Badge variant="outline" className="text-[11px]">{t('settings.modelImage')}</Badge> : null}
                </div>
              ))}
            </div>
          </CardContent>
        </Card>

        {/* API Endpoints */}
        <Card>
          <CardContent className="p-6">
            <h3 className="text-base font-semibold text-foreground mb-4">{t('settings.apiEndpoints')}</h3>
            <div className="overflow-auto border border-border rounded-xl">
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead className="text-[13px] font-semibold">{t('settings.method')}</TableHead>
                    <TableHead className="text-[13px] font-semibold">{t('settings.path')}</TableHead>
                    <TableHead className="text-[13px] font-semibold">{t('settings.endpointDesc')}</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  <TableRow>
                    <TableCell><Badge variant="default" className="text-[13px]">POST</Badge></TableCell>
                    <TableCell className="font-mono text-[20px]">/v1/chat/completions</TableCell>
                    <TableCell className="text-[14px] text-muted-foreground">{t('settings.openaiCompat')}</TableCell>
                  </TableRow>
                  <TableRow>
                    <TableCell><Badge variant="outline" className="text-[13px]">POST</Badge></TableCell>
                    <TableCell className="font-mono text-[20px]">/v1/responses</TableCell>
                    <TableCell className="text-[14px] text-muted-foreground">{t('settings.responsesApi')}</TableCell>
                  </TableRow>
                  <TableRow>
                    <TableCell><Badge variant="secondary" className="text-[13px]">GET</Badge></TableCell>
                    <TableCell className="font-mono text-[20px]">/v1/models</TableCell>
                    <TableCell className="text-[14px] text-muted-foreground">{t('settings.modelList')}</TableCell>
                  </TableRow>
                  <TableRow>
                    <TableCell><Badge variant="default" className="text-[13px]">POST</Badge></TableCell>
                    <TableCell className="font-mono text-[20px]">/v1/images/generations</TableCell>
                    <TableCell className="text-[14px] text-muted-foreground">{t('settings.imageGenerationsDesc')}</TableCell>
                  </TableRow>
                  <TableRow>
                    <TableCell><Badge variant="default" className="text-[13px]">POST</Badge></TableCell>
                    <TableCell className="font-mono text-[20px]">/v1/images/edits</TableCell>
                    <TableCell className="text-[14px] text-muted-foreground">{t('settings.imageEditsDesc')}</TableCell>
                  </TableRow>
                  <TableRow>
                    <TableCell><Badge variant="default" className="text-[13px]">POST</Badge></TableCell>
                    <TableCell className="font-mono text-[20px]">/public/generate</TableCell>
                    <TableCell className="text-[14px] text-muted-foreground">{t('settings.publicGenerateDesc')}</TableCell>
                  </TableRow>
                  <TableRow>
                    <TableCell><Badge variant="outline" className="text-[13px]">POST</Badge></TableCell>
                    <TableCell className="font-mono text-[20px]">/public/redeem</TableCell>
                    <TableCell className="text-[14px] text-muted-foreground">{t('settings.publicRedeemDesc')}</TableCell>
                  </TableRow>
                  <TableRow>
                    <TableCell><Badge variant="outline" className="text-[13px]">POST</Badge></TableCell>
                    <TableCell className="font-mono text-[20px]">/v0/management/auth-files</TableCell>
                    <TableCell className="text-[14px] text-muted-foreground">{t('settings.publicUploadDesc')}</TableCell>
                  </TableRow>
                </TableBody>
              </Table>
            </div>
          </CardContent>
        </Card>

        <ToastNotice toast={toast} />
        {confirmDialog}
      </>
    </StateShell>
  )
}
