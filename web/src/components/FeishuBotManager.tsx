// 飞书机器人管理：管理员登记平台机器人，普通用户自助管理归属自己的机器人。
import { useCallback, useState } from "react";
import { FormDrawer } from "./FormDrawer";
import { useToast } from "./Toast";
import { Badge, Button, Callout, Card, ConfirmDialog, EmptyState, ErrorBanner, Field, Input, LoadingState, MenuItem, PageHeader, Pager, RowMenu, Select } from "./ui";
import { mutate, requestJSON } from "../lib/api";
import { describeError } from "../lib/errors";
import { formatDateTime, statusLabel, statusTone } from "../lib/format";
import { useClientPage, useRemote } from "../lib/hooks";
import type { FeishuBot, FeishuBotInput, FeishuBotStatus } from "../lib/types";

interface BotFormState {
  name: string;
  appId: string;
  appSecret: string;
  verificationToken: string;
  encryptKey: string;
  botOpenId: string;
  status: FeishuBotStatus;
}

const emptyBot: BotFormState = {
  name: "", appId: "", appSecret: "", verificationToken: "", encryptKey: "", botOpenId: "", status: "enabled",
};

// FeishuBotManagerProps 描述租户视图与管理员视图的差异。
export interface FeishuBotManagerProps {
  // basePath 机器人写接口基址，区分管理员与普通用户路由。
  basePath: string;
  // listPath 机器人列表接口路径。
  listPath: string;
  // eyebrow 页面顶部小标题。
  eyebrow: string;
  // title 页面标题。
  title: string;
  // description 页面说明。
  description: string;
  // callbackHint 是否展示 Webhook 回调地址提示。
  callbackHint?: boolean;
  // createLabel 新增按钮文案。
  createLabel?: string;
  // emptyDescription 空状态说明。
  emptyDescription?: string;
}

// BotForm 录入或更新机器人；编辑时密钥留空表示保留原值。
function BotForm({ basePath, initial, onSaved, onCancel }: {
  basePath: string;
  initial: FeishuBot | null;
  onSaved: () => void;
  onCancel: () => void;
}) {
  const initialForm: BotFormState = initial ? {
    name: initial.name, appId: initial.app_id, appSecret: "", verificationToken: "", encryptKey: "",
    botOpenId: initial.bot_open_id, status: initial.status,
  } : emptyBot;
  const [form, setForm] = useState(initialForm);
  const toast = useToast();
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const revealCredential = useCallback(
    (field: "app_secret" | "verification_token" | "encrypt_key") => requestJSON<{ configured: boolean; value?: string }>(`${basePath}/${initial?.id}/reveal-credential`, {
      method: "POST",
      cache: "no-store",
      body: JSON.stringify({ field }),
    }),
    [basePath, initial?.id],
  );
  const revealAppSecret = useCallback(() => revealCredential("app_secret"), [revealCredential]);
  const revealVerificationToken = useCallback(() => revealCredential("verification_token"), [revealCredential]);
  const revealEncryptKey = useCallback(() => revealCredential("encrypt_key"), [revealCredential]);

  const submit = async (event: React.FormEvent) => {
    event.preventDefault();
    setBusy(true);
    setError(null);
    try {
      const body: FeishuBotInput = {
        name: form.name.trim(),
        app_id: form.appId.trim(),
        app_secret: form.appSecret.trim(),
        verification_token: form.verificationToken.trim(),
        encrypt_key: form.encryptKey.trim(),
        bot_open_id: form.botOpenId.trim(),
        status: form.status,
      };
      if (initial) {
        await mutate<FeishuBot>(`${basePath}/${initial.id}`, "PUT", body, crypto.randomUUID());
      } else {
        await mutate<FeishuBot>(basePath, "POST", body, crypto.randomUUID());
      }
      toast.show(initial ? "修改已保存" : "已登记机器人", "success");
      onSaved();
    } catch (failure) {
      setError(describeError(failure));
    } finally {
      setBusy(false);
    }
  };

  return (
    <FormDrawer
      title={initial ? `编辑机器人：${initial.name}` : "登记飞书机器人"}
      description={initial ? "应用密钥、Verification Token 与 Encrypt Key 留空时保留原值，点击眼睛可授权查看已保存凭据。" : "在飞书开放平台创建自建应用并启用机器人，然后填写应用凭据。"}
      submitLabel={initial ? "保存修改" : "登记机器人"}
      busy={busy}
      dirty={JSON.stringify(form) !== JSON.stringify(initialForm)}
      error={error}
      onSubmit={submit}
      onCancel={onCancel}
    >
      <Field label="显示名称" hint="只会出现在本系统里，用于区分多个机器人。">
        <Input value={form.name} onChange={(event) => setForm({ ...form, name: event.target.value })} required maxLength={128} />
      </Field>
      <Field label="状态">
        <Select value={form.status} onChange={(event) => setForm({ ...form, status: event.target.value as FeishuBotStatus })}>
          <option value="enabled">启用</option>
          <option value="disabled">停用</option>
        </Select>
      </Field>
      <Field label="App ID" hint="飞书开放平台应用的 App ID。">
        <Input value={form.appId} onChange={(event) => setForm({ ...form, appId: event.target.value })} required maxLength={128} />
      </Field>
      <Field label="机器人 open_id" hint="用于群聊 @ 判断；可在开放平台或事件日志中获取。">
        <Input value={form.botOpenId} onChange={(event) => setForm({ ...form, botOpenId: event.target.value })} maxLength={128} />
      </Field>
      <Field label="App Secret" hint={initial ? "留空保留原密钥；可点击眼睛授权查看。" : "普通查询不返回明文，编辑已保存机器人时可授权查看。"}>
        <Input type="password" value={form.appSecret} onChange={(event) => setForm({ ...form, appSecret: event.target.value })} onRevealSaved={initial ? revealAppSecret : undefined} required={!initial} autoComplete="new-password" />
      </Field>
      <Field label="Verification Token" hint={initial ? "留空保留原值；可点击眼睛授权查看。" : "事件订阅校验串，可留空；普通查询不返回明文，编辑时可授权查看。"}>
        <Input type="password" value={form.verificationToken} onChange={(event) => setForm({ ...form, verificationToken: event.target.value })} onRevealSaved={initial ? revealVerificationToken : undefined} autoComplete="new-password" />
      </Field>
      <Field label="Encrypt Key" hint={initial ? "留空保留原值；可点击眼睛授权查看。" : "事件加密密钥，可留空；普通查询不返回明文，编辑时可授权查看。"}>
        <Input type="password" value={form.encryptKey} onChange={(event) => setForm({ ...form, encryptKey: event.target.value })} onRevealSaved={initial ? revealEncryptKey : undefined} autoComplete="new-password" />
      </Field>
    </FormDrawer>
  );
}

// FeishuBotManager 渲染机器人列表与增删改测操作，供管理员页与租户自助页复用。
export default function FeishuBotManager(props: FeishuBotManagerProps) {
  const { basePath, listPath, eyebrow, title, description, callbackHint = false, createLabel = "登记机器人", emptyDescription } = props;
  const bots = useRemote(() => requestJSON<{ items: FeishuBot[] }>(listPath), [listPath]);
  const [editing, setEditing] = useState<FeishuBot | null | "new">(null);
  const [deleting, setDeleting] = useState<FeishuBot | null>(null);
  const [deleteError, setDeleteError] = useState<string | null>(null);
  const [testingID, setTestingID] = useState<string | null>(null);
  const [testResults, setTestResults] = useState<Record<string, { ok: boolean; message: string }>>({});
  const [busy, setBusy] = useState(false);
  const items = bots.data?.items ?? [];
  const paging = useClientPage(items, 10);

  // 测试连通：使用服务端保存的加密凭据换取飞书访问令牌，不在前端暴露密钥。
  const testBot = async (bot: FeishuBot) => {
    setTestingID(bot.id);
    try {
      const result = await mutate<{ ok: boolean; message: string }>(
        `${basePath}/${bot.id}/test`,
        "POST",
        undefined,
        crypto.randomUUID(),
      );
      setTestResults((current) => ({ ...current, [bot.id]: result }));
    } catch (failure) {
      setTestResults((current) => ({ ...current, [bot.id]: { ok: false, message: describeError(failure) } }));
    } finally {
      setTestingID(null);
    }
  };

  const remove = async () => {
    if (!deleting) {
      return;
    }
    setBusy(true);
    setDeleteError(null);
    try {
      await mutate<void>(`${basePath}/${deleting.id}`, "DELETE");
      setDeleting(null);
      bots.reload();
    } catch (failure) {
      setDeleteError(describeError(failure));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        eyebrow={eyebrow}
        title={title}
        description={description}
        actions={<Button size="sm" aria-haspopup="dialog" onClick={() => setEditing("new")}>{createLabel}</Button>}
      />
      {callbackHint ? (
        <Callout tone="info" title="事件回调地址">
          每个机器人使用独立回调：<code className="font-mono text-[11px]">/api/v1/feishu/events/&lt;机器人 ID&gt;</code>。登记后把对应地址填入飞书开放平台的事件订阅。
        </Callout>
      ) : null}

      {bots.error ? <ErrorBanner message={bots.error} onRetry={bots.reload} /> : null}
      {deleteError ? <ErrorBanner message={deleteError} /> : null}
      {editing !== null ? (
        <BotForm
          basePath={basePath}
          initial={editing === "new" ? null : editing}
          onCancel={() => setEditing(null)}
          onSaved={() => {
            setEditing(null);
            bots.reload();
          }}
        />
      ) : null}

      {bots.loading && !bots.data ? (
        <LoadingState />
      ) : (
        <Card title="已登记机器人" description={`共 ${items.length} 个机器人，每个机器人只能绑定一个助手。`}>
          {items.length > 0 ? (
            <div className="admin-table-container overflow-x-auto">
              <table role="table" className="admin-table w-full min-w-[860px] text-left text-sm">
                <thead>
                  <tr className="border-b border-line text-[11px] uppercase tracking-wide text-muted">
                    <th className="pb-2 pr-4 font-medium">名称</th>
                    <th className="pb-2 pr-4 font-medium">App ID</th>
                    <th className="pb-2 pr-4 font-medium">机器人 open_id</th>
                    <th className="pb-2 pr-4 font-medium">状态</th>
                    <th className="pb-2 pr-4 font-medium">绑定助手</th>
                    <th className="pb-2 pr-4 font-medium">更新时间</th>
                    <th className="pb-2 font-medium">操作</th>
                  </tr>
                </thead>
                <tbody>
                  {paging.pageItems.map((bot) => (
                    <tr role="row" key={bot.id} className="border-b border-line/60 align-top">
                      <td role="cell" data-row="main" className="py-3 pr-4">
                        <p className="font-medium text-ink">{bot.name}</p>
                        <p className="font-mono text-[11px] text-muted">{bot.id}</p>
                        {testResults[bot.id] ? (
                          <p role="status" className={`mt-1 text-[11px] ${testResults[bot.id].ok ? "text-accent" : "text-rust"}`}>
                            {testResults[bot.id].message}
                          </p>
                        ) : null}
                      </td>
                      <td role="cell" data-column="App ID" className="py-3 pr-4 font-mono text-xs text-ink-soft">{bot.app_id}</td>
                      <td role="cell" data-column="Bot Open ID" className="py-3 pr-4 font-mono text-xs text-muted">{bot.bot_open_id || "—"}</td>
                      <td role="cell" data-column="状态" className="py-3 pr-4"><Badge tone={statusTone(bot.status)}>{statusLabel(bot.status)}</Badge></td>
                      <td role="cell" data-column="绑定助手" className="py-3 pr-4 text-xs text-ink-soft">
                        {bot.bound_agent_id ? `${bot.bound_agent_name} v${bot.bound_agent_version ?? ""} · ${bot.bound_username}` : "未绑定"}
                      </td>
                      <td role="cell" data-column="更新时间" className="py-3 pr-4 text-xs text-muted">{formatDateTime(bot.updated_at)}</td>
                      <td role="cell" data-row="actions" className="py-3">
                        <RowMenu name={bot.name}>
                          <MenuItem icon="link" disabled={testingID === bot.id} onClick={() => void testBot(bot)}>测试连通</MenuItem>
                          <MenuItem icon="edit" onClick={() => setEditing(bot)}>编辑</MenuItem>
                          <MenuItem icon="trash" danger onClick={() => setDeleting(bot)}>删除</MenuItem>
                        </RowMenu>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          ) : (
            <EmptyState
              title="还没有飞书机器人"
              description={emptyDescription ?? "登记一个飞书应用机器人，助手发布后就能绑定它并在飞书中对话。"}
              action={<Button size="sm" aria-haspopup="dialog" onClick={() => setEditing("new")}>{createLabel}</Button>}
            />
          )}
          <Pager page={paging.page} totalPages={paging.totalPages} total={items.length} busy={bots.loading} onChange={paging.setPage} />
        </Card>
      )}

      <ConfirmDialog
        open={deleting !== null}
        title={`删除机器人「${deleting?.name ?? ""}」？`}
        description="删除后绑定关系会一并解除，已绑定助手将不能再通过该机器人对话。"
        confirmLabel="确认删除"
        busy={busy}
        onCancel={() => setDeleting(null)}
        onConfirm={() => void remove()}
      />
    </div>
  );
}
