// 助手列表：用“我的助手”替代“Agent 编排”，突出创建、发布与开始对话。
import { useState } from "react";
import { Link } from "react-router-dom";
import { Badge, Button, Callout, Card, ConfirmDialog, CursorPager, EmptyState, ErrorBanner, LoadingState, PageHeader } from "../components/ui";
import { mutate, requestJSON } from "../lib/api";
import { describeError } from "../lib/errors";
import { formatDateTime, statusLabel, statusTone } from "../lib/format";
import { useCursorStack, useRemote } from "../lib/hooks";
import type { Agent, AgentMode, MCP, ModelPolicy, Page, Skill } from "../lib/types";

// modeLabel 把技术模式翻译成用户能理解的工作方式。
function modeLabel(mode: AgentMode): string {
  switch (mode) {
    case "plan":
      return "先计划再执行";
    case "super":
      return "多助手协作";
    default:
      return "边做边想";
  }
}

export default function AgentsPage() {
  const paging = useCursorStack();
  const agents = useRemote(
    () => requestJSON<Page<Agent>>(`/api/v1/agents?limit=12${paging.cursor ? `&cursor=${encodeURIComponent(paging.cursor)}` : ""}`),
    [paging.cursor],
  );
  const policies = useRemote(() => requestJSON<Page<ModelPolicy>>("/api/v1/model-policies"), []);
  const skills = useRemote(() => requestJSON<Page<Skill>>("/api/v1/skills"), []);
  const mcps = useRemote(() => requestJSON<Page<MCP>>("/api/v1/mcps"), []);
  const [publishing, setPublishing] = useState<Agent | null>(null);
  const [deleting, setDeleting] = useState<Agent | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const publishedPolicies = (policies.data?.items ?? []).filter((policy) => policy.status === "published");
  const policyNames = new Map((policies.data?.items ?? []).map((policy) => [policy.id, policy.name]));
  const skillNames = new Map((skills.data?.items ?? []).map((skill) => [skill.id, skill.name]));
  const mcpNames = new Map((mcps.data?.items ?? []).map((mcp) => [mcp.id, mcp.name]));

  const runAction = async (action: () => Promise<unknown>) => {
    setBusy(true);
    setActionError(null);
    try {
      await action();
      agents.reload();
    } catch (failure) {
      setActionError(describeError(failure));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        eyebrow="助手"
        title="我的助手"
        description="每个助手有自己的工作方式、模型规则、技能与工具。先创建草稿，确认后再发布；发布后即可开始对话。"
        actions={
          <Link
            to="/agents/new"
            className="inline-flex h-10 items-center rounded-[3px] bg-ink px-4 text-sm font-medium tracking-wide text-paper-raised transition hover:bg-accent focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-accent"
          >
            创建助手
          </Link>
        }
      />

      {publishedPolicies.length === 0 ? (
        <Callout tone="warning" title="还差一步：选模规则">
          创建助手需要一套已发布的选模规则。如果你是管理员，请先到“平台设置 → 选模规则”完成配置；否则请联系管理员。
        </Callout>
      ) : null}

      {agents.error ? <ErrorBanner message={agents.error} onRetry={agents.reload} /> : null}
      {actionError ? <ErrorBanner message={actionError} /> : null}

      {agents.loading ? (
        <LoadingState />
      ) : agents.data && agents.data.items.length > 0 ? (
        <div className="flex flex-col gap-3">
          <div className="grid gap-4 md:grid-cols-2">
            {agents.data.items.map((agent) => (
            <article key={agent.id} className="flex flex-col gap-4 rounded-[4px] border border-line bg-paper-raised p-5 transition hover:border-line-strong">
              <header className="flex flex-wrap items-start justify-between gap-3">
                <div className="min-w-0">
                  <div className="flex flex-wrap items-center gap-2">
                    <h2 className="text-xl text-ink">{agent.name}</h2>
                    <Badge tone={statusTone(agent.status)}>{statusLabel(agent.status)}</Badge>
                  </div>
                  <p className="mt-1 text-xs leading-5 text-muted">{agent.description || "还没有写描述，编辑时可以补上它擅长什么。"}</p>
                </div>
                <Badge tone="info">{modeLabel(agent.mode)}</Badge>
              </header>

              <dl className="grid grid-cols-2 gap-2 text-xs">
                <div className="rounded-[3px] border border-line bg-paper-deep/40 px-3 py-2">
                  <dt className="text-muted">选模规则</dt>
                  <dd className="mt-0.5 truncate text-ink">
                    {agent.model_policy_id ? policyNames.get(agent.model_policy_id) ?? `v${agent.model_policy_version}` : "未绑定"}
                  </dd>
                </div>
                <div className="rounded-[3px] border border-line bg-paper-deep/40 px-3 py-2">
                  <dt className="text-muted">版本</dt>
                  <dd className="mt-0.5 text-ink">v{agent.version} · {formatDateTime(agent.updated_at)}</dd>
                </div>
                <div className="rounded-[3px] border border-line bg-paper-deep/40 px-3 py-2">
                  <dt className="text-muted">技能</dt>
                  <dd className="mt-0.5 truncate text-ink">{agent.skill_ids.length > 0 ? agent.skill_ids.map((id) => skillNames.get(id) ?? "技能").join("、") : "未绑定"}</dd>
                </div>
                <div className="rounded-[3px] border border-line bg-paper-deep/40 px-3 py-2">
                  <dt className="text-muted">外部工具</dt>
                  <dd className="mt-0.5 truncate text-ink">{agent.mcp_ids.length > 0 ? agent.mcp_ids.map((id) => mcpNames.get(id) ?? "工具").join("、") : "未绑定"}</dd>
                </div>
              </dl>

              <div className="flex flex-wrap gap-2">
                {agent.status === "draft" ? (
                  <>
                    <Link
                      to={`/agents/${agent.id}/edit`}
                      className="inline-flex h-8 items-center rounded-[3px] border border-line-strong bg-paper-raised px-3 text-xs text-ink transition hover:border-accent hover:text-accent"
                    >
                      继续编辑
                    </Link>
                    <Button size="sm" onClick={() => setPublishing(agent)} disabled={publishedPolicies.length === 0}>发布</Button>
                    <Button size="sm" variant="danger" onClick={() => setDeleting(agent)}>删除草稿</Button>
                  </>
                ) : (
                  <>
                    <Link
                      to={`/sessions?agent=${agent.id}`}
                      className="inline-flex h-8 items-center rounded-[3px] bg-ink px-3 text-xs text-paper-raised transition hover:bg-accent"
                    >
                      用它开始对话
                    </Link>
                    <Button
                      size="sm"
                      variant="secondary"
                      loading={busy}
                      onClick={() => void runAction(() => mutate(`/api/v1/agents/${agent.id}/versions`, "POST", undefined, crypto.randomUUID()))}
                    >
                      创建新版本
                    </Button>
                  </>
                )}
              </div>

              {agent.config_snapshot ? (
                <details className="text-xs text-muted">
                  <summary className="cursor-pointer hover:text-accent">查看发布快照（高级）</summary>
                  <pre className="mt-2 max-h-48 overflow-auto rounded-[3px] border border-line bg-paper-deep/40 p-3 font-mono text-[11px] text-ink-soft">{JSON.stringify(agent.config_snapshot, null, 2)}</pre>
                </details>
              ) : null}
            </article>
          ))}
          </div>
          <CursorPager
            page={paging.page}
            count={agents.data.items.length}
            hasNext={Boolean(agents.data.next_cursor)}
            onPrev={paging.prev}
            onNext={() => paging.next(agents.data?.next_cursor ?? null)}
          />
        </div>
      ) : (
        <Card>
          <EmptyState
            title="还没有助手"
            description="创建一个助手，选择它的工作方式与模型规则，然后发布。整个过程大约两分钟。"
            action={
              <Link
                to="/agents/new"
                className="inline-flex h-9 items-center rounded-[3px] bg-ink px-4 text-sm text-paper-raised transition hover:bg-accent"
              >
                创建第一个助手
              </Link>
            }
          />
        </Card>
      )}

      <ConfirmDialog
        open={publishing !== null}
        title={`发布「${publishing?.name ?? ""}」？`}
        description="发布后配置会冻结为快照，并出现在对话页的助手列表中。之后要修改，请创建新版本。"
        confirmLabel="确认发布"
        tone="primary"
        busy={busy}
        onCancel={() => setPublishing(null)}
        onConfirm={() => {
          if (!publishing) {
            return;
          }
          const target = publishing;
          void runAction(async () => {
            await mutate(`/api/v1/agents/${target.id}/publish`, "POST", undefined, crypto.randomUUID());
            setPublishing(null);
          });
        }}
      />
      <ConfirmDialog
        open={deleting !== null}
        title={`删除草稿「${deleting?.name ?? ""}」？`}
        description="只有草稿可以删除。删除后无法恢复。"
        confirmLabel="确认删除"
        busy={busy}
        onCancel={() => setDeleting(null)}
        onConfirm={() => {
          if (!deleting) {
            return;
          }
          const target = deleting;
          void runAction(async () => {
            await mutate<void>(`/api/v1/agents/${target.id}`, "DELETE");
            setDeleting(null);
          });
        }}
      />
    </div>
  );
}
