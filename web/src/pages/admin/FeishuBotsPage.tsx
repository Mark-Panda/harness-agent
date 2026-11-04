// 飞书机器人管理：管理员登记应用凭据，助手发布后可绑定其中一个机器人。
import { useState } from "react";
import { Badge, Button, Callout, Card, ConfirmDialog, EmptyState, ErrorBanner, Field, Input, LoadingState, PageHeader, Pager, Select } from "../../components/ui";
import { mutate, requestJSON } from "../../lib/api";
import { describeError } from "../../lib/errors";
import { formatDateTime, statusLabel, statusTone } from "../../lib/format";
import { useClientPage, useRemote } from "../../lib/hooks";
import type { FeishuBot, FeishuBotInput, FeishuBotStatus } from "../../lib/types";

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

// BotForm 录入或更新机器人；编辑时密钥留空表示保留原值。
function BotForm({ initial, onSaved, onCancel }: { initial: FeishuBot | null; onSaved: () => void; onCancel: () => void }) {
  const [form, setForm] = useState<BotFormState>(initial ? {
    name: initial.name, appId: initial.app_id, appSecret: "", verificationToken: "", encryptKey: "",
    botOpenId: initial.bot_open_id, status: initial.status,
  } : emptyBot);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

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
        await mutate<FeishuBot>(`/api/v1/admin/feishu-bots/${initial.id}`, "PUT", body, crypto.randomUUID());
      } else {
        await mutate<FeishuBot>("/api/v1/admin/feishu-bots", "POST", body, crypto.randomUUID());
      }
      onSaved();
    } catch (failure) {
      setError(describeError(failure));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Card
      title={initial ? `编辑机器人：${initial.name}` : "登记飞书机器人"}
      description={initial ? "应用密钥、Verification Token 与 Encrypt Key 留空时保留原值。" : "在飞书开放平台创建自建应用并启用机器人，然后填写应用凭据。"}
    >
      <form onSubmit={submit} className="grid gap-4 md:grid-cols-2">
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
        <Field label="App Secret" hint={initial ? "留空保留原密钥。" : "只写入一次，保存后任何接口都不返回。"}>
          <Input type="password" value={form.appSecret} onChange={(event) => setForm({ ...form, appSecret: event.target.value })} required={!initial} autoComplete="new-password" />
        </Field>
        <Field label="Verification Token" hint="事件订阅校验串，可留空；填写时会加密保存。">
          <Input type="password" value={form.verificationToken} onChange={(event) => setForm({ ...form, verificationToken: event.target.value })} autoComplete="new-password" />
        </Field>
        <Field label="Encrypt Key" hint="事件加密密钥，可留空；填写时会加密保存。">
          <Input type="password" value={form.encryptKey} onChange={(event) => setForm({ ...form, encryptKey: event.target.value })} autoComplete="new-password" />
        </Field>
        {error ? <p role="alert" className="rounded-lg border border-rust bg-rust-soft px-3 py-2 text-xs text-rust md:col-span-2">{error}</p> : null}
        <div className="flex justify-end gap-2 md:col-span-2">
          <Button type="button" variant="secondary" onClick={onCancel}>取消</Button>
          <Button type="submit" loading={busy}>{initial ? "保存修改" : "登记机器人"}</Button>
        </div>
      </form>
    </Card>
  );
}

export default function FeishuBotsPage() {
  const bots = useRemote(() => requestJSON<{ items: FeishuBot[] }>("/api/v1/admin/feishu-bots"), []);
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
        `/api/v1/admin/feishu-bots/${bot.id}/test`,
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
      await mutate<void>(`/api/v1/admin/feishu-bots/${deleting.id}`, "DELETE");
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
        eyebrow="平台设置"
        title="飞书机器人"
        description="登记飞书应用机器人后，用户可在已发布的助手上绑定。一个机器人只能被一个助手绑定，绑定后仅助手所属用户可使用。"
        actions={<Button size="sm" onClick={() => setEditing("new")}>登记机器人</Button>}
      />
      <Callout tone="info" title="事件回调地址">
        每个机器人使用独立回调：<code className="font-mono text-[11px]">/api/v1/feishu/events/&lt;机器人 ID&gt;</code>。登记后把对应地址填入飞书开放平台的事件订阅。
      </Callout>

      {bots.error ? <ErrorBanner message={bots.error} onRetry={bots.reload} /> : null}
      {deleteError ? <ErrorBanner message={deleteError} /> : null}
      {editing !== null ? (
        <BotForm
          initial={editing === "new" ? null : editing}
          onCancel={() => setEditing(null)}
          onSaved={() => {
            setEditing(null);
            bots.reload();
          }}
        />
      ) : null}

      {bots.loading ? (
        <LoadingState />
      ) : items.length > 0 ? (
        <>
          <Card title="已登记机器人" description={`共 ${items.length} 个机器人，每个机器人只能绑定一个助手。`}>
            <div className="overflow-x-auto">
              <table className="w-full min-w-[860px] text-left text-sm">
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
                    <tr key={bot.id} className="border-b border-line/60 align-top">
                      <td className="py-3 pr-4">
                        <p className="font-medium text-ink">{bot.name}</p>
                        <p className="font-mono text-[11px] text-muted">{bot.id}</p>
                        {testResults[bot.id] ? (
                          <p role="status" className={`mt-1 text-[11px] ${testResults[bot.id].ok ? "text-accent" : "text-rust"}`}>
                            {testResults[bot.id].message}
                          </p>
                        ) : null}
                      </td>
                      <td className="py-3 pr-4 font-mono text-xs text-ink-soft">{bot.app_id}</td>
                      <td className="py-3 pr-4 font-mono text-xs text-muted">{bot.bot_open_id || "—"}</td>
                      <td className="py-3 pr-4"><Badge tone={statusTone(bot.status)}>{statusLabel(bot.status)}</Badge></td>
                      <td className="py-3 pr-4 text-xs text-ink-soft">
                        {bot.bound_agent_id ? `${bot.bound_agent_name} v${bot.bound_agent_version ?? ""} · ${bot.bound_username}` : "未绑定"}
                      </td>
                      <td className="py-3 pr-4 text-xs text-muted">{formatDateTime(bot.updated_at)}</td>
                      <td className="py-3">
                        <div className="flex flex-wrap gap-2">
                          <Button size="sm" variant="secondary" loading={testingID === bot.id} onClick={() => void testBot(bot)}>测试连通</Button>
                          <Button size="sm" variant="secondary" onClick={() => setEditing(bot)}>编辑</Button>
                          <Button size="sm" variant="danger" onClick={() => setDeleting(bot)}>删除</Button>
                        </div>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
            <div className="mt-4">
              <Pager page={paging.page} totalPages={paging.totalPages} total={items.length} onChange={paging.setPage} />
            </div>
          </Card>
        </>
      ) : (
        <Card>
          <EmptyState
            title="还没有飞书机器人"
            description="登记一个飞书应用机器人，助手发布后就能绑定它并在飞书中对话。"
            action={<Button size="sm" onClick={() => setEditing("new")}>登记第一个机器人</Button>}
          />
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
