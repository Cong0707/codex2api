import type { PropsWithChildren } from 'react'
import { useCallback, useEffect, useState } from 'react'
import { getAdminKey, setAdminKey } from '../api'
import logoImg from '../assets/logo.png'

type AuthStatus = 'checking' | 'authenticated' | 'need_login'

export default function AuthGate({ children }: PropsWithChildren) {
  const [status, setStatus] = useState<AuthStatus>('checking')
  const [inputKey, setInputKey] = useState('')
  const [error, setError] = useState('')
  const [submitting, setSubmitting] = useState(false)

  const checkAuth = useCallback(async () => {
    try {
      const headers: Record<string, string> = {}
      const key = getAdminKey()
      if (key) headers['X-Admin-Key'] = key
      const res = await fetch('/api/admin/health', { headers })
      if (res.status === 401) {
        setStatus('need_login')
      } else {
        setStatus('authenticated')
      }
    } catch {
      // 网络错误等，允许进入
      setStatus('authenticated')
    }
  }, [])

  useEffect(() => {
    void checkAuth()
  }, [checkAuth])

  const handleLogin = async () => {
    if (!inputKey.trim()) {
      setError('请输入管理密钥')
      return
    }
    setSubmitting(true)
    setError('')
    try {
      const res = await fetch('/api/admin/health', {
        headers: { 'X-Admin-Key': inputKey.trim() },
      })
      if (res.status === 401) {
        setError('密钥错误，请重新输入')
      } else {
        setAdminKey(inputKey.trim())
        setStatus('authenticated')
      }
    } catch {
      setError('网络错误，请稍后重试')
    } finally {
      setSubmitting(false)
    }
  }

  if (status === 'checking') {
    return (
      <div className="flex items-center justify-center min-h-dvh">
        <div className="text-center">
          <div className="size-8 mx-auto mb-3 rounded-full border-3 border-primary/30 border-t-primary animate-spin" />
          <p className="text-sm text-muted-foreground">正在检查认证状态...</p>
        </div>
      </div>
    )
  }

  if (status === 'need_login') {
    return (
      <div className="flex items-center justify-center min-h-dvh bg-gradient-to-br from-slate-50 via-white to-blue-50/30">
        <div className="w-full max-w-[400px] mx-4">
          <div className="text-center mb-8">
            <img src={logoImg} alt="Codex2API" className="w-16 h-16 rounded-2xl object-cover shadow-[0_4px_20px_hsl(258_60%_63%/0.2)] mx-auto mb-4" />
            <h1 className="text-[28px] font-bold bg-gradient-to-br from-[hsl(258,60%,63%)] to-[hsl(210,80%,60%)] bg-clip-text text-transparent">
              Codex2API
            </h1>
            <p className="text-sm text-muted-foreground mt-1">请输入管理密钥以访问控制台</p>
          </div>

          <div className="rounded-3xl border border-border bg-white/80 shadow-xl shadow-black/[0.03] p-6 backdrop-blur-sm">
            <div className="space-y-4">
              <div>
                <label className="block mb-2 text-sm font-semibold text-muted-foreground">管理密钥</label>
                <input
                  type="password"
                  value={inputKey}
                  onChange={(e) => { setInputKey(e.target.value); setError('') }}
                  onKeyDown={(e) => { if (e.key === 'Enter') void handleLogin() }}
                  placeholder="输入管理密钥"
                  autoFocus
                  className="w-full h-11 px-4 rounded-xl border border-border bg-white text-[15px] outline-none transition-all focus:border-primary/40 focus:ring-2 focus:ring-primary/10"
                />
              </div>

              {error && (
                <div className="text-sm text-red-500 font-medium px-1">{error}</div>
              )}

              <button
                onClick={() => void handleLogin()}
                disabled={submitting}
                className="w-full h-11 rounded-xl bg-gradient-to-r from-[hsl(258,60%,63%)] to-[hsl(210,80%,60%)] text-white font-semibold text-[15px] shadow-lg shadow-primary/20 transition-all hover:opacity-90 disabled:opacity-50"
              >
                {submitting ? '验证中...' : '登录'}
              </button>
            </div>
          </div>

          <p className="text-center text-xs text-muted-foreground mt-6">
            管理密钥在「系统设置 → 安全」中配置
          </p>
        </div>
      </div>
    )
  }

  return <>{children}</>
}
