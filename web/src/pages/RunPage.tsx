// Run 执行详情：步骤树、Attempt 切换、脱敏载荷、审批结果与受控导出。
import { useMemo, useState } from "react";
import { Link, useParams } from "react-router-dom";
import { Badge, Button, Card, EmptyState, ErrorBanner, Field, LoadingState, PageHeader, Textarea, cn } from "../components/ui";
import { mutate, requestJSON } from "../lib/api";
import { describeError } from "../lib/errors";
import { formatCost, formatDateTime, formatDuration, formatNumber, statusLabel, statusTone } from "../lib/format";
import { useRemote } from "../lib/hooks";
import type { ExecutionPayload, ExecutionStep, ModelAttempt, Run, ToolApproval } from "../lib/types";

// candidateNames 从 Run 策略快照提取模型 ID 到远程模型名的映射。
function candidateNames(snapshot: Record<string, unknown> | null | undefined): Map<string, string> {
  const names = new Map<string, string>();
  const candidates = snapshot?.["candidates"];
  if (!Array.isArray(candidates)) {
    return names;
  }
  for (const candidate of candidates) {
    if (candidate && typeof candidate === "object") {
      const item = candidate as Record<string, unknown>;
      if (typeof item.model_id === "string" && typeof item.remote_model_name === "string") {
        names.set(item.model_id, item.remote_model_name);
      }
    }
  }
  return names;
}

// downloadText 在浏览器本地生成审计导出文件，避免把敏感内容放入 URL。
function downloadText(filename: string, content: string) {
  const blob = new Blob([content], { type: "application/json" });
  const url = URL.createObjectURL(blob);
  const anchor = document.createElement("a");
  anchor.href = url;
  anchor.download = filename;
  document.body.append(anchor);
  anchor.click();
  anchor.remove();
  URL.revokeObjectURL(url);
}

export default function RunPage() {
  const { runId = "" } = useParams();
  const run = useRemote(() => requestJSON<Run>(`/api/v1/runs/${runId}`), [runId]);
  const steps = useRemote(() => requestJSON<{ steps: ExecutionStep[] }>(`/api/v1/runs/${runId}/steps`), [runId]);
  const approvals = useRemote(() => requestJSON<{ items: ToolApproval[] }>(`/api/v1/runs/${runId}/approvals`), [runId]);
  const attempts = useRemote(() => requestJSON<{ items: ModelAttempt[] }>(`/api/v1/runs/${runId}/attempts`), [runId]);

  const [payloads, setPayloads] = useState<ExecutionPayload[]>([]);
  const [payloadStepID, setPayloadStepID] = useState<string | null>(null);
  const [payloadOpen, setPayloadOpen] = useState(false);
  const [reason, setReason] = useState("");
  const [reasonError, setReasonError] = useState<string | null>(null);
  const [payloadBusy, setPayloadBusy] = useState(false);
  const [reconcileReason, setReconcileReason] = useState("");
  const [reconcileError, setReconcileError] = useState<string | null>(null);
  const [reconcileBusy, setReconcileBusy] = useState(false);
  const [replayBusy, setReplayBusy] = useState(false);
  const [replayError, setReplayError] = useState<string | null>(null);

  const orderedSteps = [...(steps.data?.steps ?? [])].sort((left, right) => left.step_seq - right.step_seq);
  // 按 parent_step_id 还原执行树：父步骤作为节点，模型与工具步骤挂在其下。
  const stepRows = useMemo(() => {
    const byID = new Map(orderedSteps.map((step) => [step.id, step]));
    const children = new Map<string, ExecutionStep[]>();
    const roots: ExecutionStep[] = [];
    for (const step of orderedSteps) {
      if (step.parent_step_id && byID.has(step.parent_step_id)) {
        const list = children.get(step.parent_step_id) ?? [];
        list.push(step);
        children.set(step.parent_step_id, list);
      } else {
        roots.push(step);
      }
    }
    const rows: Array<{ step: ExecutionStep; depth: number }> = [];
    const visit = (step: ExecutionStep, depth: number) => {
      rows.push({ step, depth });
      for (const child of children.get(step.id) ?? []) {
        visit(child, depth + 1);
      }
    };
    for (const root of roots) {
      visit(root, 0);
    }
    return rows;
  }, [orderedSteps]);
  const modelNames = useMemo(() => candidateNames(run.data?.model_policy_snapshot), [run.data?.model_policy_snapshot]);

  // openPayloadPanel 展开输入输出区域并滚动定位；需要填写理由时同时聚焦输入框。
  const openPayloadPanel = (focusReason: boolean) => {
    setPayloadOpen(true);
    window.requestAnimationFrame(() => {
      document.getElementById("payloads")?.scrollIntoView({ behavior: "smooth", block: "start" });
      if (focusReason) {
        document.getElementById("payload-reason")?.focus();
      }
    });
  };

  const loadPayloads = async (stepID: string) => {
    if (reason.trim().length < 4) {
      setReasonError("请先在下方的“查看输入与输出”区域填写至少 4 个字符的查看理由，理由会进入审计记录。");
      // 面板默认折叠，必须展开并定位，否则用户点击按钮后看不到任何反馈。
      openPayloadPanel(true);
      return;
    }
    setPayloadBusy(true);
    setReasonError(null);
    try {
      const response = await requestJSON<{ steps: ExecutionStep[]; payloads?: ExecutionPayload[] }>(
        `/api/v1/runs/${runId}/steps?include_payloads=1&step_id=${encodeURIComponent(stepID)}&reason=${encodeURIComponent(reason.trim())}`,
      );
      setPayloads(response.payloads ?? []);
      setPayloadStepID(stepID);
      openPayloadPanel(false);
    } catch (failure) {
      setReasonError(describeError(failure));
      openPayloadPanel(false);
    } finally {
      setPayloadBusy(false);
    }
  };

  const exportAudit = () => {
    const bundle = {
      exported_at: new Date().toISOString(),
      run: run.data,
      steps: steps.data?.steps ?? [],
      approvals: approvals.data?.items ?? [],
      payloads,
      payload_access_reason: reason.trim() || null,
    };
    downloadText(`run-${runId}.json`, JSON.stringify(bundle, null, 2));
  };

  // resolveReconciliation 由所有者对未知副作用做出书面结案，避免 Run 永久停留在对账状态。
  const resolveReconciliation = async (outcome: "completed" | "failed") => {
    if (reconcileReason.trim().length < 4) {
      setReconcileError("请填写至少 4 个字符的对账说明。");
      return;
    }
    setReconcileBusy(true);
    setReconcileError(null);
    try {
      await mutate<Run>(`/api/v1/runs/${runId}/reconciliation`, "POST", {
        outcome,
        reason: reconcileReason.trim(),
      }, crypto.randomUUID());
      setReconcileReason("");
      run.reload();
      steps.reload();
      attempts.reload();
    } catch (failure) {
      setReconcileError(describeError(failure));
    } finally {
      setReconcileBusy(false);
    }
  };

  // replayRun 人工重新投递卡在队列/执行中的 Run 任务。
  const replayRun = async () => {
    setReplayBusy(true);
    setReplayError(null);
    try {
      await mutate(`/api/v1/runs/${runId}/replay`, "POST", undefined, crypto.randomUUID());
      run.reload();
    } catch (failure) {
      setReplayError(describeError(failure));
    } finally {
      setReplayBusy(false);
    }
  };

  if (run.loading) {
    return <LoadingState label="正在加载处理记录" />;
  }
  if (run.error || !run.data) {
    return <ErrorBanner message={run.error ?? "处理记录不存在"} onRetry={run.reload} />;
  }

  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        eyebrow="运行记录"
        title="这一次处理"
        description={`边做边想 · 创建于 ${formatDateTime(run.data.created_at)} · 对话 ${run.data.session_id.slice(0, 8)}`}
        actions={
          <>
            <Link to={`/sessions/${run.data.session_id}`} className="inline-flex h-10 items-center rounded-lg border border-line-strong bg-paper-raised px-4 text-sm text-ink hover:border-accent">
              返回对话
            </Link>
            {(run.data.status === "queued" || run.data.status === "running") ? (
              <Button variant="secondary" loading={replayBusy} onClick={() => void replayRun()}>重新投递</Button>
            ) : null}
            <Button variant="secondary" onClick={exportAudit}>导出详情 JSON</Button>
          </>
        }
      />
      {steps.error ? <ErrorBanner message={steps.error} onRetry={steps.reload} /> : null}
      {attempts.error ? <ErrorBanner message={attempts.error} onRetry={attempts.reload} /> : null}

      <div className="grid gap-4 md:grid-cols-4">
        {[
          ["状态", <Badge key="status" tone={statusTone(run.data.status)}>{statusLabel(run.data.status)}</Badge>],
          ["尝试次数", String(run.data.attempt_no)],
          ["当前步骤", String(run.data.current_step_seq)],
          ["耗时", formatDuration(run.data.started_at, run.data.finished_at)],
        ].map(([label, value]) => (
          <div key={label as string} className="rounded-xl border border-line bg-paper-raised px-4 py-3">
            <p className="text-[11px] uppercase tracking-wider text-muted">{label}</p>
            <div className="mt-1 text-sm text-ink">{value}</div>
          </div>
        ))}
      </div>
      {run.data.error_message ? (
        <ErrorBanner message={`${run.data.error_code ?? "error"}：${run.data.error_message}`} />
      ) : null}
      <nav aria-label="本页分区" className="flex flex-wrap gap-2 text-xs">
        {[
          ["#steps", "步骤明细"],
          ["#models", "模型使用记录"],
          ["#payloads", "输入与输出"],
          ["#approvals", "工具审批"],
          ["#snapshot", "配置快照"],
        ].map(([href, label]) => (
          <a
            key={href}
            href={href}
            className="rounded-full border border-line bg-paper-raised px-3 py-1.5 text-muted transition-colors hover:border-accent/50 hover:text-accent"
          >
            {label}
          </a>
        ))}
      </nav>
      {replayError ? <ErrorBanner message={`重新投递失败：${replayError}`} /> : null}

      {run.data.status === "reconciliation_required" ? (
        <Card
          title="需要人工对账"
          description="工具副作用结果未知，系统不会自动重试。请核对下游结果后填写说明并结案。"
        >
          <Field label="对账说明" hint="至少 4 个字符，会写入审计事件。">
            <Textarea
              rows={3}
              value={reconcileReason}
              onChange={(event) => setReconcileReason(event.target.value)}
              placeholder="例如：已在邮件系统中确认邮件实际发送成功。"
            />
          </Field>
          {reconcileError ? <p className="mt-2 text-xs text-rust">{reconcileError}</p> : null}
          <div className="mt-3 flex flex-wrap gap-2">
            <Button loading={reconcileBusy} onClick={() => void resolveReconciliation("completed")}>确认为已完成</Button>
            <Button variant="danger" loading={reconcileBusy} onClick={() => void resolveReconciliation("failed")}>确认为失败</Button>
          </div>
        </Card>
      ) : null}

      <Card id="steps" title="步骤明细" description="按顺序展示助手做了什么；失败与重试都会保留。">
        {steps.loading ? (
          <LoadingState />
        ) : stepRows.length === 0 ? (
          <EmptyState title="还没有步骤" description="助手可能仍在排队，或还没有开始执行。" />
        ) : (
          <div className="flex flex-col gap-3">
            {stepRows.map(({ step, depth }) => (
              <article
                key={step.id}
                className="rounded-lg border border-line bg-paper-deep/40 p-3"
                style={depth > 0 ? { marginLeft: depth * 18 } : undefined}
              >
                <div className="flex flex-wrap items-center justify-between gap-2">
                  <div className="flex flex-wrap items-center gap-2">
                    <Badge tone="neutral">#{step.step_seq}</Badge>
                    <Badge tone={statusTone(step.status)}>{statusLabel(step.status)}</Badge>
                    <span className="text-sm text-ink">{step.kind}</span>
                    <span className="text-xs text-muted">{step.name}</span>
                    {step.tool_name ? <span className="font-mono text-[11px] text-jade">{step.tool_name}</span> : null}
                    {step.parent_step_id ? <span className="text-[11px] text-muted">子步骤</span> : null}
                    {step.risk_level ? <Badge tone={step.risk_level === "high" ? "danger" : step.risk_level === "medium" ? "warning" : "neutral"}>{step.risk_level === "high" ? "高风险" : step.risk_level === "medium" ? "需留意" : "低风险"}</Badge> : null}
                  </div>
                  <div className="flex items-center gap-2 text-xs text-muted">
                    <span>调用 #{step.attempt_no}</span>
                    <span>{formatDuration(step.started_at, step.finished_at)}</span>
                    <Button
                      size="sm"
                      variant="secondary"
                      loading={payloadBusy && payloadStepID === step.id}
                      title="点击后会在下方展开输入输出；首次查看需要填写查看理由"
                      onClick={() => void loadPayloads(step.id)}
                    >
                      查看输入输出
                    </Button>
                  </div>
                </div>
                {step.error_code ? <p className="mt-2 text-xs text-rust">错误码：{step.error_code}</p> : null}
                {step.idempotency_key ? <p className="mt-1 font-mono text-[11px] text-muted">幂等键：{step.idempotency_key}</p> : null}
              </article>
            ))}
          </div>
        )}
      </Card>

      <Card id="models" collapsible defaultOpen={false} title="模型使用记录" description="每次调用用了哪个模型、花了多少 Token 与费用，以及失败后切换到哪个模型。">
        {attempts.loading ? (
          <LoadingState />
        ) : attempts.data && attempts.data.items.length > 0 ? (
          <div className="admin-table-container overflow-x-auto">
            <table role="table" className="admin-table w-full min-w-[900px] text-left text-sm">
              <thead className="text-xs uppercase tracking-wide text-muted">
                <tr>
                  <th className="pb-3 pr-4 font-medium">尝试</th>
                  <th className="pb-3 pr-4 font-medium">模型</th>
                  <th className="pb-3 pr-4 font-medium">状态</th>
                  <th className="pb-3 pr-4 font-medium">耗时</th>
                  <th className="pb-3 pr-4 font-medium">Token / 费用</th>
                  <th className="pb-3 pr-4 font-medium">切换 / 错误</th>
                </tr>
              </thead>
              <tbody role="rowgroup" className="divide-y divide-line">
                {attempts.data.items.map((attempt) => (
                  <tr role="row" key={attempt.id}>
                    <td role="cell" data-row="main" className="py-3 pr-4">
                      <p className="text-ink">#{attempt.attempt_no}</p>
                      <p className="text-[11px] text-muted">策略 v{attempt.policy_version ?? "—"}</p>
                    </td>
                    <td role="cell" data-column="模型" className="py-3 pr-4 text-ink-soft">
                      {attempt.model_id ? modelNames.get(attempt.model_id) ?? attempt.model_id.slice(0, 8) : "—"}
                    </td>
                    <td role="cell" data-column="状态" className="py-3 pr-4"><Badge tone={statusTone(attempt.status)}>{statusLabel(attempt.status)}</Badge></td>
                    <td role="cell" data-column="耗时" className="py-3 pr-4 text-xs text-muted">{attempt.duration_ms === null ? "—" : `${attempt.duration_ms} ms`}</td>
                    <td role="cell" data-column="Token / 费用" className="py-3 pr-4 text-xs text-ink-soft">
                      {formatNumber(attempt.input_tokens)} / {formatNumber(attempt.output_tokens)} · {formatCost(attempt.estimated_cost)}
                    </td>
                    <td role="cell" data-column="切换 / 错误" className="py-3 pr-4 text-xs">
                      {attempt.switch_reason ? <p className="text-ochre">切换：{attempt.switch_reason}</p> : null}
                      {attempt.error_message ? <p className="max-w-[260px] truncate text-rust" title={attempt.error_message}>{attempt.error_class || "error"}：{attempt.error_message}</p> : null}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        ) : (
          <EmptyState title="还没有模型调用记录" description="助手可能仍在排队，或还没有进入模型调用阶段。" />
        )}
      </Card>

      <Card
        id="payloads"
        collapsible
        open={payloadOpen}
        onOpenChange={setPayloadOpen}
        title="查看输入与输出"
        description="这里保存模型请求、响应与工具读写的脱敏副本。查看需要填写理由，并会记入审计记录。"
      >
        <div className="grid gap-4 md:grid-cols-[320px_1fr]">
          <div className="flex flex-col gap-3">
            <Field label="查看理由" error={reasonError}>
              <Textarea
                id="payload-reason"
                className="min-h-20"
                value={reason}
                onChange={(event) => setReason(event.target.value)}
                placeholder="例如：核对失败步骤的错误响应"
              />
            </Field>
            <p className="text-xs leading-5 text-muted">先填写查看理由，再点击任意步骤的“查看输入输出”；点击后会自动定位到这里。敏感内容会按脱敏策略展示。</p>
          </div>
          <div className="min-w-0">
            {payloads.length === 0 ? (
              <p className="text-xs text-muted">还没有加载输入输出。</p>
            ) : (
              <div className="flex flex-col gap-3">
                {payloads.map((payload) => (
                  <article key={payload.id} className="rounded-lg border border-line bg-paper-deep/40 p-3">
                    <div className="flex flex-wrap items-center gap-2 text-xs text-muted">
                      <Badge tone="info">{payload.type}</Badge>
                      <span>{payload.capture_mode}</span>
                      {payload.truncated ? <Badge tone="warning">已截断</Badge> : null}
                      {payload.partial ? <Badge tone="warning">部分响应</Badge> : null}
                      <span>{payload.saved_size_bytes} / {payload.original_size_bytes} 字节</span>
                    </div>
                    <pre className="mt-2 max-h-64 overflow-auto whitespace-pre-wrap break-all rounded bg-paper-raised p-2 font-mono text-[11px] text-ink-soft">
                      {payload.content_json ? JSON.stringify(payload.content_json, null, 2) : payload.content_text ?? ""}
                    </pre>
                  </article>
                ))}
              </div>
            )}
          </div>
        </div>
      </Card>

      <Card id="approvals" collapsible defaultOpen={false} title="工具审批" description="展示风险等级、参数、决策人和执行结果。">
        {approvals.data && approvals.data.items.length > 0 ? (
          <div className="flex flex-col gap-3">
            {approvals.data.items.map((approval) => (
              <article key={approval.id} className="rounded-lg border border-line bg-paper-deep/40 p-3 text-xs">
                <div className="flex flex-wrap items-center justify-between gap-2">
                  <div className="flex flex-wrap items-center gap-2">
                    <span className="text-sm text-ink">{approval.tool_name}</span>
                    <Badge tone={approval.risk_level === "high" ? "danger" : approval.risk_level === "medium" ? "warning" : "neutral"}>{approval.risk_level === "high" ? "高风险" : approval.risk_level === "medium" ? "需留意" : "低风险"}</Badge>
                    <Badge tone={statusTone(approval.status)}>{statusLabel(approval.status)}</Badge>
                  </div>
                  <span className="text-muted">{formatDateTime(approval.created_at)}</span>
                </div>
                {approval.arguments ? (
                  <pre className="mt-2 max-h-40 overflow-auto whitespace-pre-wrap rounded bg-paper-raised p-2 font-mono text-[11px] text-muted">{JSON.stringify(approval.arguments, null, 2)}</pre>
                ) : null}
                {approval.error_message ? <p className={cn("mt-1", "text-rust")}>{approval.error_code}: {approval.error_message}</p> : null}
              </article>
            ))}
          </div>
        ) : (
          <p className="text-xs text-muted">这次处理没有需要审批的工具调用。</p>
        )}
      </Card>

      <Card id="snapshot" collapsible defaultOpen={false} title="当时的配置快照" description="这次处理开始时冻结的助手与选模规则，用于事后复现。">
        <div className="grid gap-3 md:grid-cols-2">
          <div>
            <p className="mb-1 text-xs font-medium text-ink-soft">助手配置</p>
            <pre className="max-h-64 overflow-auto rounded border border-line bg-paper-raised p-3 font-mono text-[11px] text-muted">{JSON.stringify(run.data.config_snapshot, null, 2)}</pre>
          </div>
          <div>
            <p className="mb-1 text-xs font-medium text-ink-soft">选模规则</p>
            <pre className="max-h-64 overflow-auto rounded border border-line bg-paper-raised p-3 font-mono text-[11px] text-muted">{JSON.stringify(run.data.model_policy_snapshot, null, 2)}</pre>
          </div>
        </div>
      </Card>
    </div>
  );
}
