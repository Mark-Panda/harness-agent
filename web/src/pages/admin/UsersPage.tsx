// 用户管理：管理员查看账号目录、邀请新用户、启用/禁用账号并签发一次性改密令牌。
import { useState } from "react";
import { Badge, Button, Callout, Card, CursorPager, ConfirmDialog, EmptyState, ErrorBanner, Field, Input, LoadingState, PageHeader, Select, Textarea } from "../../components/ui";
import { mutate, requestJSON } from "../../lib/api";
import { describeError } from "../../lib/errors";
import { formatDateTime, statusLabel, statusTone } from "../../lib/format";
import { useCursorStack, useRemote } from "../../lib/hooks";
import { useAuth } from "../../lib/auth";
import type { AdminUser, Page, Role } from "../../lib/types";

// statusOptions 是用户目录的状态筛选选项。
const statusOptions = [
  { value: "", label: "全部状态" },
  { value: "active", label: "正常" },
  { value: "disabled", label: "已禁用" },
  { value: "deleted", label: "已删除" },
];

export default function UsersPage() {
  const { user: currentUser } = useAuth();
  const paging = useCursorStack();
  const [status, setStatus] = useState("");
  const [searchDraft, setSearchDraft] = useState("");
  const [search, setSearch] = useState("");
  const users = useRemote(() => {
    const params = new URLSearchParams({ limit: "20" });
    if (status) {
      params.set("status", status);
    }
    if (search) {
      params.set("q", search);
    }
    if (paging.cursor) {
      params.set("cursor", paging.cursor);
    }
    return requestJSON<Page<AdminUser>>(`/api/v1/admin/users?${params.toString()}`);
  }, [paging.cursor, status, search]);

  const [formOpen, setFormOpen] = useState(false);
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [creating, setCreating] = useState(false);
  const [createError, setCreateError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);

  const [statusTarget, setStatusTarget] = useState<AdminUser | null>(null);
  const [reason, setReason] = useState("");
  const [statusBusy, setStatusBusy] = useState(false);
  const [statusError, setStatusError] = useState<string | null>(null);

  const [resetTarget, setResetTarget] = useState<AdminUser | null>(null);
  const [resetBusy, setResetBusy] = useState(false);
  const [resetError, setResetError] = useState<string | null>(null);
  const [resetResult, setResetResult] = useState<{ user: AdminUser; token: string; expiresAt: string } | null>(null);
  const [copied, setCopied] = useState(false);

  const [roleTarget, setRoleTarget] = useState<{ user: AdminUser; role: Role } | null>(null);
  const [roleBusy, setRoleBusy] = useState(false);
  const [roleError, setRoleError] = useState<string | null>(null);
  const [roleReason, setRoleReason] = useState("");

  const items = users.data?.items ?? [];

  // createUser 邀请创建普通用户；初始密码通过安全渠道交给对方。
  const createUser = async (event: React.FormEvent) => {
    event.preventDefault();
    setCreating(true);
    setCreateError(null);
    setNotice(null);
    try {
      const created = await mutate<{ id: string; username: string }>("/api/v1/admin/users", "POST", {
        username: username.trim(),
        password,
      }, crypto.randomUUID());
      setNotice(`已创建用户「${created.username}」，请通过安全渠道把初始密码交给对方。`);
      setUsername("");
      setPassword("");
      setFormOpen(false);
      paging.reset();
      users.reload();
    } catch (failure) {
      setCreateError(describeError(failure));
    } finally {
      setCreating(false);
    }
  };

  // changeStatus 启用或禁用账号；禁用理由会进入审计记录。
  const changeStatus = async (user: AdminUser, next: "active" | "disabled", why: string) => {
    setStatusBusy(true);
    setStatusError(null);
    setActionError(null);
    try {
      await mutate<void>(`/api/v1/admin/users/${user.id}/status`, "POST", {
        status: next,
        reason: why.trim(),
      }, crypto.randomUUID());
      setStatusTarget(null);
      setReason("");
      users.reload();
    } catch (failure) {
      setStatusError(describeError(failure));
    } finally {
      setStatusBusy(false);
    }
  };

  // changeRole 调整用户角色；不能移除自己的管理员角色。
  const changeRole = async () => {
    if (!roleTarget) {
      return;
    }
    setRoleBusy(true);
    setRoleError(null);
    try {
      await mutate<void>(`/api/v1/admin/users/${roleTarget.user.id}/role`, "POST", {
        role: roleTarget.role,
        reason: roleReason.trim(),
      }, crypto.randomUUID());
      setRoleTarget(null);
      setRoleReason("");
      users.reload();
    } catch (failure) {
      setRoleError(describeError(failure));
    } finally {
      setRoleBusy(false);
    }
  };

  // issueReset 生成一次性改密令牌；明文只展示这一次。
  const issueReset = async () => {
    if (!resetTarget) {
      return;
    }
    setResetBusy(true);
    setResetError(null);
    try {
      const response = await mutate<{ token: string; expires_at: string }>(
        `/api/v1/admin/users/${resetTarget.id}/password-reset`,
        "POST",
        undefined,
        crypto.randomUUID(),
      );
      setResetResult({ user: resetTarget, token: response.token, expiresAt: response.expires_at });
      setResetTarget(null);
    } catch (failure) {
      setResetError(describeError(failure));
    } finally {
      setResetBusy(false);
    }
  };

  const copyToken = async () => {
    if (!resetResult) {
      return;
    }
    try {
      await navigator.clipboard.writeText(resetResult.token);
      setCopied(true);
      window.setTimeout(() => setCopied(false), 1500);
    } catch {
      setActionError("复制失败，请手动选择令牌文本。");
    }
  };

  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        eyebrow="平台设置 · 账号"
        title="用户管理"
        description="查看账号状态、邀请新用户、处理离职或异常账号。禁用会立即撤销该账号的全部登录令牌。"
        actions={<Button onClick={() => setFormOpen((value) => !value)}>{formOpen ? "收起邀请表单" : "邀请用户"}</Button>}
      />

      {users.error ? <ErrorBanner message={users.error} onRetry={users.reload} /> : null}
      {actionError ? <ErrorBanner message={actionError} /> : null}
      {notice ? <Callout tone="success">{notice}</Callout> : null}

      {formOpen ? (
        <Card title="邀请新用户" description="新账号默认是普通用户；初始密码至少 12 位，请通过安全渠道交付。">
          <form onSubmit={createUser} className="grid gap-4 md:grid-cols-[1fr_1fr_auto] md:items-end">
            <Field label="用户名" hint="3–64 位，会统一转为小写。">
              <Input value={username} onChange={(event) => setUsername(event.target.value)} required minLength={3} maxLength={64} placeholder="例如：zhangsan" />
            </Field>
            <Field label="初始密码" hint="至少 12 位，建议使用长随机口令。">
              <Input type="password" autoComplete="new-password" value={password} onChange={(event) => setPassword(event.target.value)} required minLength={12} placeholder="至少 12 位" />
            </Field>
            <Button type="submit" loading={creating} disabled={username.trim().length < 3 || password.length < 12}>创建账号</Button>
          </form>
          {createError ? <p role="alert" className="mt-3 text-xs text-rust">{createError}</p> : null}
        </Card>
      ) : null}

      <Card
        title="账号目录"
        description={`本页 ${items.length} 个账号，按创建时间倒序。`}
        actions={
          <div className="flex flex-wrap items-center gap-2">
            <form
              className="flex items-center gap-2"
              onSubmit={(event) => {
                event.preventDefault();
                setSearch(searchDraft.trim());
                paging.reset();
              }}
            >
              <Input className="w-48" value={searchDraft} onChange={(event) => setSearchDraft(event.target.value)} placeholder="搜索用户名" aria-label="搜索用户名" />
              <Button size="sm" variant="secondary" type="submit">搜索</Button>
            </form>
            <Select
              className="w-36"
              value={status}
              onChange={(event) => {
                setStatus(event.target.value);
                paging.reset();
              }}
              aria-label="按账号状态筛选"
            >
              {statusOptions.map((option) => (
                <option key={option.value} value={option.value}>{option.label}</option>
              ))}
            </Select>
          </div>
        }
      >
        {users.loading ? (
          <LoadingState />
        ) : users.data && users.data.items.length > 0 ? (
          <>
            <div className="overflow-x-auto">
              <table className="w-full min-w-[820px] text-left text-sm">
                <thead className="text-xs uppercase tracking-wide text-muted">
                  <tr>
                    <th className="pb-3 pr-4 font-medium">用户名</th>
                    <th className="pb-3 pr-4 font-medium">角色</th>
                    <th className="pb-3 pr-4 font-medium">状态</th>
                    <th className="pb-3 pr-4 font-medium">创建时间</th>
                    <th className="pb-3 pr-4 font-medium">更新时间</th>
                    <th className="pb-3 font-medium">操作</th>
                  </tr>
                </thead>
                <tbody className="divide-y divide-line">
                  {items.map((user) => (
                    <tr key={user.id}>
                      <td className="py-3 pr-4">
                        <p className="font-medium text-ink">{user.username}</p>
                        <p className="font-mono text-[11px] text-muted">{user.id.slice(0, 8)}</p>
                      </td>
                      <td className="py-3 pr-4">
                        <Badge tone={user.role === "admin" ? "info" : "neutral"}>{user.role === "admin" ? "管理员" : "普通用户"}</Badge>
                      </td>
                      <td className="py-3 pr-4"><Badge tone={statusTone(user.status)}>{statusLabel(user.status)}</Badge></td>
                      <td className="py-3 pr-4 text-xs text-muted">{formatDateTime(user.created_at)}</td>
                      <td className="py-3 pr-4 text-xs text-muted">{formatDateTime(user.updated_at)}</td>
                      <td className="py-3">
                        {user.status === "deleted" ? (
                          <span className="text-xs text-muted">账号已删除</span>
                        ) : (
                          <div className="flex flex-wrap gap-2">
                            {user.status === "disabled" ? (
                              <Button size="sm" variant="secondary" onClick={() => void changeStatus(user, "active", "")}>启用</Button>
                            ) : (
                              <Button size="sm" variant="danger" onClick={() => { setStatusTarget(user); setReason(""); setStatusError(null); }}>禁用</Button>
                            )}
                            <Button size="sm" variant="secondary" onClick={() => { setResetTarget(user); setResetError(null); }}>重置密码</Button>
                            {user.id === currentUser?.id ? (
                              <span className="text-[11px] text-muted">当前账号</span>
                            ) : (
                              <Button
                                size="sm"
                                variant="secondary"
                                onClick={() => {
                                  setRoleTarget({ user, role: user.role === "admin" ? "user" : "admin" });
                                  setRoleReason("");
                                  setRoleError(null);
                                }}
                              >
                                {user.role === "admin" ? "降为普通用户" : "设为管理员"}
                              </Button>
                            )}
                          </div>
                        )}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
            <CursorPager
              page={paging.page}
              count={items.length}
              hasNext={Boolean(users.data.next_cursor)}
              onPrev={paging.prev}
              onNext={() => paging.next(users.data?.next_cursor ?? null)}
            />
          </>
        ) : (
          <EmptyState title="没有匹配的账号" description="调整状态筛选或搜索关键字；也可以直接邀请一个新用户。" />
        )}
      </Card>

      {resetResult ? (
        <Callout tone="warning" title={`「${resetResult.user.username}」的改密令牌（只显示这一次）`}>
          <p className="break-all font-mono text-xs text-ink">{resetResult.token}</p>
          <p className="mt-2 text-xs text-muted">有效期至 {formatDateTime(resetResult.expiresAt)}。请通过安全渠道交给对方，并让对方立即使用。</p>
          <div className="mt-3 flex gap-2">
            <Button size="sm" variant="secondary" onClick={() => void copyToken()}>{copied ? "已复制" : "复制令牌"}</Button>
            <Button size="sm" variant="ghost" onClick={() => setResetResult(null)}>关闭</Button>
          </div>
        </Callout>
      ) : null}

      <ConfirmDialog
        open={statusTarget !== null}
        title={`禁用账号「${statusTarget?.username ?? ""}」？`}
        description="禁用后该账号的所有登录令牌立即失效，已排队任务不会继续执行。之后可以随时重新启用。"
        confirmLabel="确认禁用"
        busy={statusBusy}
        onCancel={() => setStatusTarget(null)}
        onConfirm={() => {
          if (statusTarget) {
            void changeStatus(statusTarget, "disabled", reason);
          }
        }}
      >
        <Field label="禁用理由（可选）" hint="理由会写入审计记录，便于后续追溯。">
          <Textarea className="min-h-20" value={reason} onChange={(event) => setReason(event.target.value)} maxLength={512} placeholder="例如：账号交接、异常登录排查" />
        </Field>
        {statusError ? <p role="alert" className="mt-2 text-xs text-rust">{statusError}</p> : null}
      </ConfirmDialog>

      <ConfirmDialog
        open={roleTarget !== null}
        title={`${roleTarget?.role === "admin" ? "设为管理员" : "降为普通用户"}：${roleTarget?.user.username ?? ""}？`}
        description={roleTarget?.role === "admin" ? "管理员可以访问平台设置、用户管理与全部配置。请确认对方确实需要这些权限。" : "降级后对方将无法访问平台设置与用户管理，但已有对话和数据会保留。"}
        confirmLabel="确认调整"
        tone={roleTarget?.role === "admin" ? "primary" : "danger"}
        busy={roleBusy}
        onCancel={() => setRoleTarget(null)}
        onConfirm={() => void changeRole()}
      >
        <Field label="调整理由（可选）" hint="理由会写入审计记录。">
          <Textarea className="min-h-20" value={roleReason} onChange={(event) => setRoleReason(event.target.value)} maxLength={512} placeholder="例如：新增平台维护人员" />
        </Field>
        {roleError ? <p role="alert" className="mt-2 text-xs text-rust">{roleError}</p> : null}
      </ConfirmDialog>

      <ConfirmDialog
        open={resetTarget !== null}
        title={`为「${resetTarget?.username ?? ""}」生成改密令牌？`}
        description="令牌 15 分钟内有效且只能使用一次；生成后请通过安全渠道交给对方。"
        confirmLabel="生成令牌"
        tone="primary"
        busy={resetBusy}
        onCancel={() => setResetTarget(null)}
        onConfirm={() => void issueReset()}
      >
        {resetError ? <p role="alert" className="text-xs text-rust">{resetError}</p> : null}
      </ConfirmDialog>
    </div>
  );
}
