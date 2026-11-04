// 助手创建向导：把“编排”拆成四步人话问答，最后检查并发布。
import { useEffect, useMemo, useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { useAuth } from "../lib/auth";
import { mutate, requestJSON } from "../lib/api";
import { describeError } from "../lib/errors";
import { useRemote } from "../lib/hooks";
import { useToast } from "../components/Toast";
import type { Agent, AgentInput, AgentMode, MCP, ModelPolicy, Page, Skill } from "../lib/types";
import { Badge, Button, Callout, Card, ChoiceCard, ConfirmDialog, ErrorBanner, Field, Input, LoadingState, PageHeader, Textarea, cn } from "../components/ui";

const modes: Array<{ value: AgentMode; title: string; description: string; badge?: string }> = [
  {
    value: "react",
    title: "边做边想",
    description: "适合大多数日常任务。助手会根据每一步的结果调整下一步，回答更快、成本更低。",
    badge: "推荐",
  },
  {
    value: "plan",
    title: "先列计划，再执行",
    description: "适合步骤较多的任务。助手先生成计划，你可以中途暂停、恢复或修改计划。",
  },
  {
    value: "super",
    title: "多助手协作",
    description: "把大任务拆给多个子助手并行处理。适合复杂调研或长任务，耗时与费用更高。",
  },
];

const stepTitles = ["基本情况", "工作方式", "模型与能力", "检查并发布"];

export default function AgentWizardPage() {
  const { agentId } = useParams();
  const navigate = useNavigate();
  const toast = useToast();
  const { user } = useAuth();
  const isEditing = Boolean(agentId);
  const editing = useRemote(
    () => (agentId ? requestJSON<Agent>(`/api/v1/agents/${agentId}`) : Promise.resolve(null)),
    [agentId],
  );
  const policies = useRemote(() => requestJSON<Page<ModelPolicy>>("/api/v1/model-policies"), []);
  const skills = useRemote(() => requestJSON<Page<Skill>>("/api/v1/skills"), []);
  const mcps = useRemote(() => requestJSON<Page<MCP>>("/api/v1/mcps"), []);

  const [step, setStep] = useState(0);
  const [name, setName] = useState("");
  const [description, setDescription] = useState("");
  const [mode, setMode] = useState<AgentMode>("react");
  const [policyID, setPolicyID] = useState("");
  const [skillIDs, setSkillIDs] = useState<string[]>([]);
  const [mcpIDs, setMcpIDs] = useState<string[]>([]);
  const [initialized, setInitialized] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState<"draft" | "publish" | null>(null);
  const [confirmLeave, setConfirmLeave] = useState(false);

  const publishedPolicies = (policies.data?.items ?? []).filter((policy) => policy.status === "published");
  const activeSkills = (skills.data?.items ?? []).filter((skill) => skill.status === "published");
  const activeMCPs = (mcps.data?.items ?? []).filter((mcp) => mcp.status === "enabled");
  const selectedPolicy = publishedPolicies.find((policy) => policy.id === policyID) ?? null;
  const selectedMode = modes.find((item) => item.value === mode) ?? modes[0];

  // 编辑草稿时只在首次加载后回填，避免用户输入被轮询覆盖。
  useEffect(() => {
    if (initialized || !editing.data) {
      return;
    }
    setName(editing.data.name);
    setDescription(editing.data.description);
    setMode(editing.data.mode);
    setPolicyID(editing.data.model_policy_id ?? "");
    setSkillIDs(editing.data.skill_ids ?? []);
    setMcpIDs(editing.data.mcp_ids ?? []);
    setInitialized(true);
  }, [editing.data, initialized]);

  // 新建时默认选中第一个已发布规则，减少“不知道为什么不能下一步”的困惑。
  useEffect(() => {
    if (!policyID && publishedPolicies.length > 0) {
      setPolicyID(publishedPolicies[0].id);
    }
  }, [policyID, publishedPolicies]);

  const canLeaveBasics = name.trim().length >= 2;
  const canLeaveModel = selectedPolicy !== null;
  // 有未保存内容时，关闭浏览器和返回列表前都先确认，避免整页填写白费。
  const defaultPolicyID = publishedPolicies[0]?.id ?? "";
  const dirty = useMemo(() => {
    if (isEditing && editing.data) {
      const sameIDs = (left: string[], right: string[]) => [...left].sort().join() === [...right].sort().join();
      return name.trim() !== editing.data.name
        || description !== editing.data.description
        || mode !== editing.data.mode
        || policyID !== (editing.data.model_policy_id ?? "")
        || !sameIDs(skillIDs, editing.data.skill_ids ?? [])
        || !sameIDs(mcpIDs, editing.data.mcp_ids ?? []);
    }
    return name.trim().length > 0
      || description.trim().length > 0
      || mode !== "react"
      || policyID !== defaultPolicyID
      || skillIDs.length > 0
      || mcpIDs.length > 0;
  }, [isEditing, editing.data, name, description, mode, policyID, defaultPolicyID, skillIDs, mcpIDs]);

  useEffect(() => {
    if (!dirty) {
      return;
    }
    const handler = (event: BeforeUnloadEvent) => {
      event.preventDefault();
      event.returnValue = "";
    };
    window.addEventListener("beforeunload", handler);
    return () => window.removeEventListener("beforeunload", handler);
  }, [dirty]);

  // 步骤门槛：先填名称，选好模型规则后才能进入检查与发布。
  const canVisitStep = (index: number) => {
    if (index === 0) {
      return true;
    }
    if (!canLeaveBasics) {
      return false;
    }
    return index === 1 || canLeaveModel;
  };

  const toggle = (values: string[], id: string) => (values.includes(id) ? values.filter((value) => value !== id) : [...values, id]);

  const submit = async (publish: boolean) => {
    if (!canLeaveBasics) {
      setError("请先填写助手名称（至少 2 个字符）。");
      setStep(0);
      return;
    }
    if (!selectedPolicy) {
      setError("请先选择一个选模规则。若列表为空，请联系管理员完成平台配置。");
      setStep(2);
      return;
    }
    setBusy(publish ? "publish" : "draft");
    setError(null);
    try {
      const body: AgentInput = {
        name: name.trim(),
        description: description.trim(),
        mode,
        model_policy_id: selectedPolicy.id,
        model_policy_version: selectedPolicy.version,
        skill_ids: skillIDs,
        mcp_ids: mcpIDs,
      };
      const saved = isEditing && agentId
        ? await mutate<Agent>(`/api/v1/agents/${agentId}`, "PUT", body, crypto.randomUUID())
        : await mutate<Agent>("/api/v1/agents", "POST", body, crypto.randomUUID());
      if (publish) {
        await mutate(`/api/v1/agents/${saved.id}/publish`, "POST", undefined, crypto.randomUUID());
      }
      toast.show(publish ? "助手已发布，可以在对话页使用" : "草稿已保存", "success");
      navigate("/agents");
    } catch (failure) {
      setError(describeError(failure));
    } finally {
      setBusy(null);
    }
  };

  const loading = (isEditing && editing.loading) || policies.loading || skills.loading || mcps.loading;
  if (loading) {
    return <LoadingState label="正在准备向导…" />;
  }
  // 已发布版本是冻结快照，直接编辑会破坏已有对话的复现能力。
  if (isEditing && editing.data && editing.data.status !== "draft") {
    return (
      <div className="flex flex-col gap-4">
        <PageHeader eyebrow="编辑助手" title="这个版本已发布" description="已发布版本不可直接修改，避免影响正在使用它的对话。" />
        <Callout tone="info" title="下一步怎么做">
          回到助手列表，点击“创建新版本”，系统会复制当前配置生成一个可编辑草稿。
        </Callout>
        <div>
          <Link to="/agents" className="text-sm text-accent underline underline-offset-4">返回助手列表</Link>
        </div>
      </div>
    );
  }

  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        eyebrow={isEditing ? "编辑助手" : "创建助手"}
        title={isEditing ? `编辑「${editing.data?.name ?? name}」` : "一步一步创建你的助手"}
        description="不需要懂“编排”。只要回答四个问题：它是谁、怎么工作、用哪些模型和工具，最后确认发布。"
        actions={
          <button
            type="button"
            onClick={() => {
              if (dirty) {
                setConfirmLeave(true);
              } else {
                navigate("/agents");
              }
            }}
            className="text-sm text-muted underline underline-offset-4 hover:text-accent"
          >
            返回助手列表
          </button>
        }
      />

      {editing.error ? <ErrorBanner message={editing.error} onRetry={editing.reload} /> : null}
      {policies.error ? <ErrorBanner message={policies.error} onRetry={policies.reload} /> : null}

      <div className="flex flex-col gap-2">
        <nav aria-label="创建步骤" className="flex flex-wrap gap-2">
          {stepTitles.map((title, index) => (
            <button
              key={title}
              type="button"
              aria-current={step === index ? "step" : undefined}
              onClick={() => setStep(index)}
              disabled={!canVisitStep(index)}
              className={cn(
                "flex items-center gap-2 rounded-[3px] border px-3 py-2 text-xs transition disabled:cursor-not-allowed disabled:opacity-50",
                step === index ? "border-accent bg-accent-soft/50 text-accent" : "border-line bg-paper-raised text-muted hover:border-line-strong",
              )}
            >
              <span className="font-display text-sm">{String(index + 1).padStart(2, "0")}</span>
              {title}
            </button>
          ))}
        </nav>
        {!canVisitStep(stepTitles.length - 1) ? (
          <p className="text-[11px] leading-5 text-muted">
            灰色步骤暂不可进入：{!canLeaveBasics ? "先填写助手名称（至少 2 个字符）" : "先选择一套选模规则"}。
          </p>
        ) : null}
      </div>

      {error ? <ErrorBanner message={error} /> : null}

      {step === 0 ? (
        <Card title="它是谁？" description="名字和描述会出现在对话页，帮助你和同事记住它的用途。">
          <div className="grid gap-5 md:grid-cols-2">
            <Field label="助手名称" hint="例如：需求整理助手、代码审查搭档。至少 2 个字符。">
              <Input value={name} onChange={(event) => setName(event.target.value)} maxLength={128} autoFocus placeholder="给助手起个名字" />
            </Field>
            <div className="md:col-span-2">
              <Field label="它擅长什么？（可选）" hint="写工作偏好、输出格式或禁止事项；这段描述也会出现在助手卡片上。">
                <Textarea value={description} onChange={(event) => setDescription(event.target.value)} maxLength={512} placeholder="例如：把零散需求整理成结构化结论；先给结论，再列依据；不确定时主动说明。" />
              </Field>
            </div>
          </div>
        </Card>
      ) : null}

      {step === 1 ? (
        <Card title="它是怎么工作的？" description="没有绝对的好坏：日常任务选“边做边想”，步骤多的任务选“先列计划”。">
          <div role="radiogroup" aria-label="工作方式" className="flex flex-col gap-3">
            {modes.map((item) => (
              <ChoiceCard
                key={item.value}
                selected={mode === item.value}
                title={item.title}
                description={item.description}
                badge={item.badge ? <Badge tone="warning">{item.badge}</Badge> : undefined}
                onSelect={() => setMode(item.value)}
              />
            ))}
          </div>
        </Card>
      ) : null}

      {step === 2 ? (
        <div className="flex flex-col gap-5">
          <Card title="用哪套模型规则？" description="规则里已经安排了主选与替补模型。你只需要选一套，不需要填写 API 地址。">
            {publishedPolicies.length === 0 ? (
              <Callout tone="warning" title="还没有可用的选模规则">
                {user?.role === "admin" ? (
                  <span>
                    你是管理员，可以先到 <Link to="/admin/policies" className="text-accent underline">选模规则</Link> 创建并发布一套规则，再回来创建助手。
                  </span>
                ) : (
                  <span>请联系管理员完成模型接入与选模规则配置。配置完成后刷新本页即可继续。</span>
                )}
              </Callout>
            ) : (
              <div role="radiogroup" aria-label="选模规则" className="flex flex-col gap-3">
                {publishedPolicies.map((policy) => (
                  <ChoiceCard
                    key={policy.id}
                    selected={policyID === policy.id}
                    title={policy.name}
                    description={`版本 v${policy.version} · ${policy.candidates.length} 个候选模型 · 最低上下文 ${policy.min_context_tokens.toLocaleString("zh-CN")} Token`}
                    onSelect={() => setPolicyID(policy.id)}
                  />
                ))}
              </div>
            )}
          </Card>

          <Card title="需要哪些技能？" description="技能是做事说明。不选也能对话，选了会更贴合你的工作习惯。">
            {activeSkills.length === 0 ? (
              <p className="text-xs text-muted">暂无已发布技能。可以跳过，之后再添加。</p>
            ) : (
              <div className="grid gap-2 md:grid-cols-2">
                {activeSkills.map((skill) => {
                  const selected = skillIDs.includes(skill.id);
                  return (
                    <button
                      key={skill.id}
                      type="button"
                      aria-pressed={selected}
                      onClick={() => setSkillIDs(toggle(skillIDs, skill.id))}
                      className={cn(
                        "rounded-[3px] border p-3 text-left transition",
                        selected ? "border-accent bg-accent-soft/40" : "border-line bg-paper-raised hover:border-line-strong",
                      )}
                    >
                      <span className="flex items-center justify-between gap-2">
                        <span className="text-sm font-medium text-ink">{skill.name}</span>
                        <Badge tone="neutral">v{skill.version}</Badge>
                      </span>
                      <span className="mt-1 block text-xs leading-5 text-muted">{skill.description || "暂无说明"}</span>
                    </button>
                  );
                })}
              </div>
            )}
          </Card>

          <Card title="需要哪些外部工具？" description="外部工具让助手能查资料或操作内部系统。高风险工具执行前会要求你审批。">
            {activeMCPs.length === 0 ? (
              <p className="text-xs text-muted">暂无已启用外部工具。可以跳过。</p>
            ) : (
              <div className="grid gap-2 md:grid-cols-2">
                {activeMCPs.map((mcp) => {
                  const selected = mcpIDs.includes(mcp.id);
                  const risk = mcp.risk_level === "high" || mcp.risk_level === "critical";
                  return (
                    <button
                      key={mcp.id}
                      type="button"
                      aria-pressed={selected}
                      onClick={() => setMcpIDs(toggle(mcpIDs, mcp.id))}
                      className={cn(
                        "rounded-[3px] border p-3 text-left transition",
                        selected ? "border-accent bg-accent-soft/40" : "border-line bg-paper-raised hover:border-line-strong",
                      )}
                    >
                      <span className="flex flex-wrap items-center gap-2">
                        <span className="text-sm font-medium text-ink">{mcp.name}</span>
                        <Badge tone={risk ? "danger" : mcp.risk_level === "medium" ? "warning" : "neutral"}>
                          {risk ? "高风险" : mcp.risk_level === "medium" ? "需留意" : "低风险"}
                        </Badge>
                      </span>
                      <span className="mt-1 block truncate text-xs text-muted">{mcp.endpoint}</span>
                    </button>
                  );
                })}
              </div>
            )}
          </Card>
        </div>
      ) : null}

      {step === 3 ? (
        <Card title="检查一下，然后发布" description="发布后配置会冻结为快照。需要修改时创建新版本，不会影响正在进行的对话。">
          <dl className="grid gap-4 md:grid-cols-2">
            <div className="rounded-[3px] border border-line bg-paper-raised p-4">
              <dt className="text-[11px] tracking-wide text-muted">名称</dt>
              <dd className="mt-1 text-lg text-ink">{name.trim() || "未填写"}</dd>
              <dd className="mt-1 text-xs leading-5 text-muted">{description.trim() || "暂无描述"}</dd>
            </div>
            <div className="rounded-[3px] border border-line bg-paper-raised p-4">
              <dt className="text-[11px] tracking-wide text-muted">工作方式</dt>
              <dd className="mt-1 text-lg text-ink">{selectedMode.title}</dd>
              <dd className="mt-1 text-xs leading-5 text-muted">{selectedMode.description}</dd>
            </div>
            <div className="rounded-[3px] border border-line bg-paper-raised p-4">
              <dt className="text-[11px] tracking-wide text-muted">选模规则</dt>
              <dd className="mt-1 text-lg text-ink">{selectedPolicy?.name ?? "未选择"}</dd>
              <dd className="mt-1 text-xs leading-5 text-muted">{selectedPolicy ? `v${selectedPolicy.version} · ${selectedPolicy.candidates.length} 个候选模型` : "发布前必须选择"}</dd>
            </div>
            <div className="rounded-[3px] border border-line bg-paper-raised p-4">
              <dt className="text-[11px] tracking-wide text-muted">技能与工具</dt>
              <dd className="mt-1 text-lg text-ink">{skillIDs.length} 个技能 · {mcpIDs.length} 个工具</dd>
              <dd className="mt-1 text-xs leading-5 text-muted">
                {skillIDs.length + mcpIDs.length === 0 ? "未绑定，只使用基础对话能力" : "已绑定，发布后可在对话中调用"}
              </dd>
            </div>
          </dl>

          <ol className="mt-5 flex flex-col gap-2 text-xs leading-5 text-muted">
            <li>· 发布后，助手会出现在“对话”页的助手列表里。</li>
            <li>· 如果之后想改配置，从助手卡片点“创建新版本”，旧版本仍可用于已有会话。</li>
            <li>· 高风险外部工具在第一次执行前会请求你审批。</li>
          </ol>
        </Card>
      ) : null}

      <div className="sticky bottom-3 z-20 flex flex-wrap items-center justify-between gap-3 rounded-[4px] border border-line-strong bg-paper-raised/95 px-4 py-3 shadow-[0_10px_30px_rgb(31_27_21/0.10)] backdrop-blur">
        {step === 0 ? (
          <p className="text-xs text-muted">填写助手名称后即可继续。</p>
        ) : (
          <Button variant="secondary" onClick={() => setStep(step - 1)}>上一步</Button>
        )}
        <div className="flex flex-wrap items-center gap-2">
          {step === 0 ? (
            <Button disabled={!canLeaveBasics} onClick={() => setStep(1)}>下一步：它是怎么工作的</Button>
          ) : null}
          {step === 1 ? <Button onClick={() => setStep(2)}>下一步：用哪个模型</Button> : null}
          {step === 2 ? (
            <Button disabled={!canLeaveModel} onClick={() => setStep(3)}>下一步：检查并发布</Button>
          ) : null}
          {step === 3 ? (
            <>
              <Button variant="secondary" loading={busy === "draft"} disabled={busy !== null} onClick={() => void submit(false)}>
                先保存草稿
              </Button>
              <Button loading={busy === "publish"} disabled={busy !== null} onClick={() => void submit(true)}>
                发布并使用
              </Button>
            </>
          ) : null}
        </div>
      </div>

      <ConfirmDialog
        open={confirmLeave}
        title="放弃未保存的修改？"
        description="当前填写的内容还没有保存，离开后会丢失。"
        confirmLabel="放弃并离开"
        onCancel={() => setConfirmLeave(false)}
        onConfirm={() => {
          setConfirmLeave(false);
          navigate("/agents");
        }}
      />
    </div>
  );
}
