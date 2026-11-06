// Run 执行详情：步骤树、Attempt 切换、脱敏载荷、审批结果与受控导出。
import { useMemo, useState } from "react";
import { Link, useParams } from "react-router-dom";
import { Badge, Button, Card, ConfirmDialog, EmptyState, ErrorBanner, Field, LoadingState, PageHeader, Textarea, cn } from "../components/ui";
import { ApiError, mutate, requestJSON } from "../lib/api";
import { describeError } from "../lib/errors";
import { formatCost, formatDateTime, formatDuration, formatNumber, statusLabel, statusTone } from "../lib/format";
import { useRemote } from "../lib/hooks";
import type { ExecutionPayload, ExecutionStep, ModelAttempt, Plan, Run, SupervisorTask, ToolApproval } from "../lib/types";

// modeLabel 把工作方式翻译成用户语言。
function modeLabel(mode: string): string {
  switch (mode) {
    case "plan":
      return "先计划再执行";
    case "super":
      return "多助手协作";
    default:
      return "边做边想";
  }
}

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
  const supervisorTasks = useRemote(() => requestJSON<{ items: SupervisorTask[] }>(`/api/v1/runs/${runId}/supervisor-tasks`), [runId]);
  const plan = useRemote(async () => {
    if (run.data?.mode !== "plan") {
      return null;
    }
    try {
      return await requestJSON<Plan>(`/api/v1/runs/${runId}/plan`);
    } catch (error) {
      if (error instanceof ApiError && error.status === 404) {
        return null;
      }
      throw error;
    }
  }, [runId, run.data?.mode]);

  const [payloads, setPayloads] = useState<ExecutionPayload[]>([]);
  const [payloadStepID, setPayloadStepID] = useState<string | null>(null);
  const [reason, setReason] = useState("");
  const [reasonError, setReasonError] = useState<string | null>(null);
  const [payloadBusy, setPayloadBusy] = useState(false);
  const [planBusy, setPlanBusy] = useState(false);
  const [planError, setPlanError] = useState<string | null>(null);
  const [confirmPlanCancel, setConfirmPlanCancel] = useState(false);
  const [reviseOpen, setReviseOpen] = useState(false);
  const [reviseText, setReviseText] = useState("");
  const [reviseError, setReviseError] = useState<string | null>(null);
  const [reviseBusy, setReviseBusy] = useState(false);
  const [reconcileReason, setReconcileReason] = useState("");
  const [reconcileError, setReconcileError] = useState<string | null>(null);
  const [reconcileBusy, setReconcileBusy] = useState(false);
  const [replayBusy, setReplayBusy] = useState(false);
  const [replayError, setReplayError] = useState<string | null>(null);

  const orderedSteps = [...(steps.data?.steps ?? [])].sort((left, right) => left.step_seq - right.step_seq);
  const modelNames = useMemo(() => candidateNames(run.data?.model_policy_snapshot), [run.data?.model_policy_snapshot]);

  const loadPayloads = async (stepID: string) => {
    if (reason.trim().length < 4) {
      setReasonError("请填写至少 4 个字符的查看理由，理由会进入审计记录。");
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
    } catch (failure) {
      setReasonError(describeError(failure));
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

  // actPlan 暂停、恢复或终止计划，结束后刷新计划与 Run 状态。
  const actPlan = async (action: "pause" | "resume" | "cancel") => {
    setPlanBusy(true);
    setPlanError(null);
    try {
      await mutate<Plan>(`/api/v1/runs/${runId}/plan/${action}`, "POST", undefined, crypto.randomUUID());
      plan.reload();
      run.reload();
      setConfirmPlanCancel(false);
    } catch (failure) {
      setPlanError(describeError(failure));
    } finally {
      setPlanBusy(false);
    }
  };

  // startRevise 以当前计划步骤预填 JSON，人工修改后提交为新版本。
  const startRevise = () => {
    if (!plan.data) {
      return;
    }
    const draft = {
      goal: plan.data.goal,
      steps: plan.data.steps.map((step) => ({
        step_no: step.step_no,
        title: step.title,
        description: step.description,
        depends_on: step.depends_on ?? [],
        tool_name: step.tool_name,
        arguments: step.arguments ?? {},
      })),
    };
    setReviseText(JSON.stringify(draft, null, 2));
    setReviseError(null);
    setReviseOpen(true);
  };

  // submitRevise 校验 JSON 后创建新计划版本并恢复执行。
  const submitRevise = async () => {
    let payload: unknown;
    try {
      payload = JSON.parse(reviseText);
    } catch {
      setReviseError("计划 JSON 格式错误。");
      return;
    }
    setReviseBusy(true);
    setReviseError(null);
    try {
      await mutate<Plan>(`/api/v1/runs/${runId}/plan/revise`, "POST", payload, crypto.randomUUID());
      setReviseOpen(false);
      plan.reload();
      run.reload();
    } catch (failure) {
      setReviseError(describeError(failure));
    } finally {
      setReviseBusy(false);
    }
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
        description={`${modeLabel(run.data.mode)} · 创建于 ${formatDateTime(run.data.created_at)} · 对话 ${run.data.session_id.slice(0, 8)}`}
        actions={
          <>
            <Link to={`/sessions/${run.data.session_id}`} className="inline-flex h-10 items-center rounded-[3px] border border-line-strong bg-paper-raised px-4 text-sm text-ink hover:border-accent">
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
      {supervisorTasks.error ? <ErrorBanner message={supervisorTasks.error} onRetry={supervisorTasks.reload} /> : null}

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
      {replayError ? <ErrorBanner message={`重新投递失败：${replayError}`} /> : null}
      {planError ? <ErrorBanner message={planError} /> : null}
      {plan.error ? <ErrorBanner message={`计划读取失败：${plan.error}`} onRetry={plan.reload} /> : null}

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

      {plan.data ? (
        <Card
          title={`处理计划 v${plan.data.version}`}
          description={plan.data.goal}
          actions={
            <>
              {(plan.data.status === "executing" || plan.data.status === "waiting_approval") ? (
                <Button size="sm" variant="secondary" loading={planBusy} onClick={() => void actPlan("pause")}>暂停计划</Button>
              ) : null}
              {plan.data.status === "paused" ? (
                <>
                  <Button size="sm" loading={planBusy} onClick={() => void actPlan("resume")}>恢复执行</Button>
                  <Button size="sm" variant="secondary" onClick={startRevise}>修订计划</Button>
                </>
              ) : null}
              {!["completed", "failed", "cancelled"].includes(plan.data.status) ? (
                <Button size="sm" variant="danger" onClick={() => setConfirmPlanCancel(true)}>终止计划</Button>
              ) : null}
            </>
          }
        >
          <div className="flex flex-wrap items-center gap-3 text-xs text-muted">
            <Badge tone={statusTone(plan.data.status)}>{statusLabel(plan.data.status)}</Badge>
            <span>修订 r{plan.data.revision}</span>
            <span>Token {formatNumber(plan.data.total_input_tokens)} / {formatNumber(plan.data.total_output_tokens)}</span>
            <span>费用 {formatCost(plan.data.estimated_cost)}</span>
            {plan.data.deadline_at ? <span>截止 {formatDateTime(plan.data.deadline_at)}</span> : null}
          </div>
          {plan.data.error_message ? <p className="mt-2 text-xs text-rust">{plan.data.error_code}：{plan.data.error_message}</p> : null}
          {reviseOpen ? (
            <div className="mt-4 rounded-lg border border-line-strong bg-paper-deep/60 p-3">
              <Field label="修订计划 JSON" hint="提交后创建新版本并沿用原配置快照，旧版本保留为审计历史。" error={reviseError}>
                <Textarea className="min-h-48 font-mono text-xs" value={reviseText} onChange={(event) => setReviseText(event.target.value)} spellCheck={false} />
              </Field>
              <div className="mt-3 flex justify-end gap-2">
                <Button size="sm" variant="secondary" onClick={() => setReviseOpen(false)} disabled={reviseBusy}>取消</Button>
                <Button size="sm" loading={reviseBusy} onClick={() => void submitRevise()}>提交修订</Button>
              </div>
            </div>
          ) : null}
          <ol className="mt-4 flex flex-col gap-2">
            {plan.data.steps.map((step) => (
              <li key={step.id} className="rounded-lg border border-line bg-paper-deep/40 p-3 text-xs">
                <div className="flex flex-wrap items-center justify-between gap-2">
                  <div className="flex flex-wrap items-center gap-2">
                    <Badge tone="neutral">#{step.step_no}</Badge>
                    <Badge tone={statusTone(step.status)}>{statusLabel(step.status)}</Badge>
                    <span className="text-ink">{step.title}</span>
                    {step.tool_name ? <span className="font-mono text-jade">{step.tool_name}</span> : null}
                  </div>
                  <span className="text-muted">尝试 {step.attempts}/{step.max_attempts}</span>
                </div>
                {step.description ? <p className="mt-1 text-muted">{step.description}</p> : null}
                {step.depends_on?.length ? <p className="mt-1 text-muted">依赖步骤：{step.depends_on.join(", ")}</p> : null}
                {step.error_message ? <p className="mt-1 text-rust">{step.error_code}：{step.error_message}</p> : null}
                {step.result?.["content"] ? (
                  <details className="mt-1 text-muted">
                    <summary className="cursor-pointer text-muted">查看步骤结果</summary>
                    <pre className="mt-1 max-h-40 overflow-auto whitespace-pre-wrap break-all rounded bg-paper-raised p-2 font-mono text-[11px]">{String(step.result["content"])}</pre>
                  </details>
                ) : null}
              </li>
            ))}
          </ol>
        </Card>
      ) : null}
      <ConfirmDialog
        open={confirmPlanCancel}
        title="终止执行计划？"
        description="终止后关联 Run 会被取消，已完成步骤保留审计事实，未执行步骤标记为跳过或取消。"
        confirmLabel="确认终止"
        busy={planBusy}
        onCancel={() => setConfirmPlanCancel(false)}
        onConfirm={() => void actPlan("cancel")}
      />

      <Card title="步骤明细" description="按顺序展示助手做了什么；失败与重试都会保留。">
        {steps.loading ? (
          <LoadingState />
        ) : orderedSteps.length === 0 ? (
          <EmptyState title="还没有步骤" description="助手可能仍在排队，或还没有开始执行。" />
        ) : (
          <div className="flex flex-col gap-3">
            {orderedSteps.map((step) => (
              <article key={step.id} className="rounded-lg border border-line bg-paper-deep/40 p-3">
                <div className="flex flex-wrap items-center justify-between gap-2">
                  <div className="flex flex-wrap items-center gap-2">
                    <Badge tone="neutral">#{step.step_seq}</Badge>
                    <Badge tone={statusTone(step.status)}>{statusLabel(step.status)}</Badge>
                    <span className="text-sm text-ink">{step.kind}</span>
                    <span className="text-xs text-muted">{step.name}</span>
                    {step.tool_name ? <span className="font-mono text-[11px] text-jade">{step.tool_name}</span> : null}
                    {step.risk_level ? <Badge tone={step.risk_level === "high" ? "danger" : step.risk_level === "medium" ? "warning" : "neutral"}>{step.risk_level === "high" ? "高风险" : step.risk_level === "medium" ? "需留意" : "低风险"}</Badge> : null}
                  </div>
                  <div className="flex items-center gap-2 text-xs text-muted">
                    <span>尝试 {step.attempt_no}</span>
                    <span>{formatDuration(step.started_at, step.finished_at)}</span>
                    <Button size="sm" variant="secondary" loading={payloadBusy && payloadStepID === step.id} onClick={() => void loadPayloads(step.id)}>查看输入输出</Button>
                  </div>
                </div>
                {step.error_code ? <p className="mt-2 text-xs text-rust">错误码：{step.error_code}</p> : null}
                {step.idempotency_key ? <p className="mt-1 font-mono text-[11px] text-muted">幂等键：{step.idempotency_key}</p> : null}
              </article>
            ))}
          </div>
        )}
      </Card>

      <Card title="模型使用记录" description="每次调用用了哪个模型、花了多少 Token 与费用，以及失败后切换到哪个模型。">
        {attempts.loading ? (
          <LoadingState />
        ) : attempts.data && attempts.data.items.length > 0 ? (
          <div className="overflow-x-auto">
            <table className="w-full min-w-[900px] text-left text-sm">
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
              <tbody className="divide-y divide-line">
                {attempts.data.items.map((attempt) => (
                  <tr key={attempt.id}>
                    <td className="py-3 pr-4">
                      <p className="text-ink">#{attempt.attempt_no}</p>
                      <p className="text-[11px] text-muted">策略 v{attempt.policy_version ?? "—"}</p>
                    </td>
                    <td className="py-3 pr-4 text-ink-soft">
                      {attempt.model_id ? modelNames.get(attempt.model_id) ?? attempt.model_id.slice(0, 8) : "—"}
                    </td>
                    <td className="py-3 pr-4"><Badge tone={statusTone(attempt.status)}>{statusLabel(attempt.status)}</Badge></td>
                    <td className="py-3 pr-4 text-xs text-muted">{attempt.duration_ms === null ? "—" : `${attempt.duration_ms} ms`}</td>
                    <td className="py-3 pr-4 text-xs text-ink-soft">
                      {formatNumber(attempt.input_tokens)} / {formatNumber(attempt.output_tokens)} · {formatCost(attempt.estimated_cost)}
                    </td>
                    <td className="py-3 pr-4 text-xs">
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

      {run.data.mode === "super" ? (
        <Card title="协作助手明细" description="每个子助手使用独立上下文；失败、取消或结果缺失都会如实显示。">
          {supervisorTasks.loading ? (
            <LoadingState />
          ) : supervisorTasks.data && supervisorTasks.data.items.length > 0 ? (
            (() => {
              const failedCount = supervisorTasks.data.items.filter((task) => task.status === "failed" || task.status === "unknown").length;
              const cancelledCount = supervisorTasks.data.items.filter((task) => task.status === "cancelled").length;
              return (
                <div className="flex flex-col gap-3">
                  <div className="flex flex-wrap items-center gap-2 text-xs text-muted">
                    <Badge tone={failedCount + cancelledCount > 0 ? "warning" : "success"}>
                      {failedCount + cancelledCount > 0 ? "部分失败" : "全部成功"}
                    </Badge>
                    <span>Worker {supervisorTasks.data!.items.length}</span>
                    <span>失败 {failedCount}</span>
                    <span>取消 {cancelledCount}</span>
                  </div>
                  {supervisorTasks.data.items.map((task) => (
                    <article key={task.id} className="rounded-lg border border-line bg-paper-deep/40 p-3 text-xs">
                      <div className="flex flex-wrap items-center justify-between gap-2">
                        <div className="flex flex-wrap items-center gap-2">
                          <Badge tone="neutral">#{task.worker_index}</Badge>
                          <Badge tone={statusTone(task.status)}>{statusLabel(task.status)}</Badge>
                          <span className="text-ink">{task.worker_name}</span>
                          <span className="text-muted">尝试 {task.attempts}/{task.max_attempts}</span>
                        </div>
                        <Link to={`/sessions/${task.child_session_id}`} className="text-accent underline underline-offset-2">子会话</Link>
                      </div>
                      <p className="mt-1 text-muted">{task.task}</p>
                      {task.tool_scope.length > 0 ? <p className="mt-1 text-muted">工具交集：{task.tool_scope.join(", ")}</p> : null}
                      {task.error_message ? <p className="mt-1 text-rust">{task.error_code}：{task.error_message}</p> : null}
                      {task.result?.["content"] ? (
                        <details className="mt-1 text-muted">
                          <summary className="cursor-pointer text-muted">查看 Worker 结果</summary>
                          <pre className="mt-1 max-h-40 overflow-auto whitespace-pre-wrap break-all rounded bg-paper-raised p-2 font-mono text-[11px]">{String(task.result["content"])}</pre>
                        </details>
                      ) : null}
                    </article>
                  ))}
                </div>
              );
            })()
          ) : (
            <EmptyState title="还没有 Worker 任务" description="Supervisor 仍在生成委派，或委派失败。" />
          )}
        </Card>
      ) : null}

      <Card title="查看输入与输出" description="这里保存模型请求、响应与工具读写的脱敏副本。查看需要填写理由，并会记入审计记录。">
        <div className="grid gap-4 md:grid-cols-[320px_1fr]">
          <div className="flex flex-col gap-3">
            <Field label="查看理由" error={reasonError}>
              <Textarea className="min-h-20" value={reason} onChange={(event) => setReason(event.target.value)} placeholder="例如：核对失败步骤的错误响应" />
            </Field>
            <p className="text-xs leading-5 text-muted">先在“步骤明细”里选择步骤，再点“查看输入输出”。敏感内容会按脱敏策略展示。</p>
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

      <Card title="工具审批" description="展示风险等级、参数、决策人和执行结果。">
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

      <Card title="当时的配置快照" description="这次处理开始时冻结的助手与选模规则，用于事后复现。">
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
