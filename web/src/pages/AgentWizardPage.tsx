// 助手创建向导：把“编排”拆成四步人话问答，最后检查并发布。
import { useEffect, useMemo, useRef, useState } from "react";
import { Link, useBlocker, useNavigate, useParams } from "react-router-dom";
import { useAuth } from "../lib/auth";
import { mutate, requestJSON } from "../lib/api";
import { describeError } from "../lib/errors";
import { useRemote } from "../lib/hooks";
import { MultiSelect } from "../components/MultiSelect";
import { Icon } from "../components/Icon";
import { useToast } from "../components/Toast";
import type { Agent, AgentInput, AgentMode, MCP, ModelPolicy, Page, Skill, UserSkill } from "../lib/types";
import { Badge, Button, Callout, Card, ChoiceCard, ConfirmDialog, ErrorBanner, Field, Input, LoadingState, PageHeader, Select, Textarea, cn } from "../components/ui";

const modes: Array<{ value: AgentMode; title: string; description: string; badge?: string }> = [
  {
    value: "react",
    title: "边做边想",
    description: "助手会根据每一步的结果调整下一步，回答更快、成本更低。",
    badge: "推荐",
  },
];

const stepTitles = ["基本信息", "工作方式", "模型与能力", "检查与发布"];

export default function AgentWizardPage() {
  const { agentId } = useParams();
  return <AgentWizard key={agentId ?? "new"} agentId={agentId} />;
}

function AgentWizard({ agentId }: { agentId?: string }) {
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
  const userSkills = useRemote(() => requestJSON<Page<UserSkill>>("/api/v1/user-skills?limit=200"), []);
  const mcps = useRemote(() => requestJSON<Page<MCP>>("/api/v1/mcps"), []);

  const [step, setStep] = useState(0);
  const [name, setName] = useState("");
  const [description, setDescription] = useState("");
  const [mode, setMode] = useState<AgentMode>("react");
  const [policyID, setPolicyID] = useState("");
  const [skillIDs, setSkillIDs] = useState<string[]>([]);
  const [userSkillIDs, setUserSkillIDs] = useState<string[]>([]);
  const [mcpIDs, setMcpIDs] = useState<string[]>([]);
  const [initialized, setInitialized] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState<"draft" | "publish" | null>(null);
  const allowNavigation = useRef(false);
  const [savedDraft, setSavedDraft] = useState<{ id: string; signature: string } | null>(null);
  const stepHeadingRef = useRef<HTMLHeadingElement>(null);
  const focusStepRef = useRef(false);

  useEffect(() => {
    if (focusStepRef.current) {
      stepHeadingRef.current?.focus();
      focusStepRef.current = false;
    }
  }, [step]);

  const goToStep = (index: number) => {
    if (index !== step) {
      focusStepRef.current = true;
      setStep(index);
    }
  };

  const publishedPolicies = (policies.data?.items ?? []).filter((policy) => policy.status === "published");
  const activeSkills = (skills.data?.items ?? []).filter((skill) => skill.status === "published");
  const activeUserSkills = (userSkills.data?.items ?? []).filter((skill) => skill.status === "published");
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
    setUserSkillIDs(editing.data.user_skill_ids ?? []);
    setMcpIDs(editing.data.mcp_ids ?? []);
    setInitialized(true);
  }, [editing.data, initialized]);

  // 新建时默认选中第一个已发布规则，减少“不知道为什么不能下一步”的困惑。
  useEffect(() => {
    if (!isEditing && !policyID && publishedPolicies.length > 0) {
      setPolicyID(publishedPolicies[0].id);
    }
  }, [isEditing, policyID, publishedPolicies]);

  const canLeaveBasics = name.trim().length >= 2;
  const canLeaveModel = selectedPolicy !== null;
  // 有未保存内容时，关闭浏览器和返回列表前都先确认，避免整页填写白费。
  const defaultPolicyID = publishedPolicies[0]?.id ?? "";
  const draftSignature = JSON.stringify([name.trim(), description.trim(), mode, policyID, [...skillIDs].sort(), [...userSkillIDs].sort(), [...mcpIDs].sort()]);
  const dirty = useMemo(() => {
    if (savedDraft?.signature === draftSignature) return false;
    if (isEditing && editing.data) {
      const sameIDs = (left: string[], right: string[]) => [...left].sort().join() === [...right].sort().join();
      return name.trim() !== editing.data.name
        || description !== editing.data.description
        || mode !== editing.data.mode
        || policyID !== (editing.data.model_policy_id ?? "")
        || !sameIDs(skillIDs, editing.data.skill_ids ?? [])
        || !sameIDs(userSkillIDs, editing.data.user_skill_ids ?? [])
        || !sameIDs(mcpIDs, editing.data.mcp_ids ?? []);
    }
    return name.trim().length > 0
      || description.trim().length > 0
      || mode !== "react"
      || policyID !== defaultPolicyID
      || skillIDs.length > 0
      || userSkillIDs.length > 0
      || mcpIDs.length > 0;
  }, [isEditing, editing.data, name, description, mode, policyID, defaultPolicyID, skillIDs, userSkillIDs, mcpIDs, draftSignature, savedDraft]);

  const blocker = useBlocker(({ currentLocation, nextLocation }) =>
    !allowNavigation.current && dirty && currentLocation.pathname !== nextLocation.pathname,
  );

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
    return index < 3 || canLeaveModel;
  };

  const submit = async (publish: boolean) => {
    if (busy) return;
    if (!canLeaveBasics) {
      setError("请先填写助手名称（至少 2 个字符）。");
      goToStep(0);
      return;
    }
    if (!selectedPolicy) {
      setError("请先选择一个选模规则。若列表为空，请联系管理员完成平台配置。");
      goToStep(2);
      return;
    }
    setBusy(publish ? "publish" : "draft");
    setError(null);
    let draftSaved = false;
    try {
      const body: AgentInput = {
        name: name.trim(),
        description: description.trim(),
        mode,
        model_policy_id: selectedPolicy.id,
        model_policy_version: selectedPolicy.version,
        skill_ids: skillIDs,
        user_skill_ids: userSkillIDs,
        mcp_ids: mcpIDs,
      };
      const draftID = agentId ?? savedDraft?.id;
      const saved = draftID
        ? await mutate<Agent>(`/api/v1/agents/${draftID}`, "PUT", body, crypto.randomUUID())
        : await mutate<Agent>("/api/v1/agents", "POST", body, crypto.randomUUID());
      if (publish) {
        setSavedDraft({ id: saved.id, signature: draftSignature });
        draftSaved = true;
        await mutate(`/api/v1/agents/${saved.id}/publish`, "POST", undefined, crypto.randomUUID());
      }
      toast.show(publish ? "助手已发布，可以在对话页使用" : "草稿已保存", "success");
      allowNavigation.current = true;
      navigate("/agents");
    } catch (failure) {
      setError(draftSaved ? `草稿已保存，发布失败。${describeError(failure)}` : describeError(failure));
    } finally {
      setBusy(null);
    }
  };

  const loading = (isEditing && editing.loading) || policies.loading || skills.loading || mcps.loading;
  if (loading) {
    return <LoadingState label="正在准备向导…" />;
  }
  if (isEditing && editing.error) {
    return <ErrorBanner message={editing.error} onRetry={editing.reload} />;
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
    <div className="mx-auto flex max-w-5xl flex-col gap-5">
      <PageHeader
        title={isEditing ? `编辑「${editing.data?.name ?? name}」` : "创建助手"}
        description="配置工作方式、模型和工具，再保存草稿或发布。"
        actions={
          <Button
            variant="ghost"
            disabled={busy !== null}
            onClick={() => navigate("/agents")}
          >
            <Icon name="back" className="size-4" /> 返回助手列表
          </Button>
        }
      />

      {policies.error ? <ErrorBanner message={policies.error} onRetry={policies.reload} /> : null}

      <nav aria-label="创建步骤" className="grid grid-cols-2 gap-2 rounded-xl border border-line bg-paper-raised p-2 sm:grid-cols-4">
        {stepTitles.map((title, index) => (
          <button
            key={title}
            type="button"
            aria-current={step === index ? "step" : undefined}
            onClick={() => goToStep(index)}
            disabled={busy !== null || !canVisitStep(index)}
            className={cn(
              "flex min-h-11 min-w-0 items-center gap-2 rounded-lg px-2 text-left text-sm font-medium transition-colors disabled:cursor-not-allowed disabled:opacity-50",
              step === index ? "bg-accent-soft text-accent" : "text-muted hover:bg-paper-deep hover:text-ink",
            )}
          >
            <span aria-hidden="true" className={cn("grid size-6 shrink-0 place-items-center rounded-full border text-xs tabular-nums", step === index ? "border-accent/35 bg-accent/10" : "border-line-strong")}>
              {index + 1}
            </span>
            {title}
          </button>
        ))}
      </nav>

      <h2 ref={stepHeadingRef} tabIndex={-1} className="scroll-mt-20 text-lg tracking-tight focus:outline-none">{stepTitles[step]}</h2>
      {error ? <ErrorBanner message={error} /> : null}

      <fieldset disabled={busy !== null} className="min-w-0">
      {step === 0 ? (
        <Card>
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
                name="agent-mode"
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
        <div className="grid max-w-3xl gap-4 rounded-xl border border-line bg-paper-raised p-4 sm:grid-cols-2 sm:p-5">
          <div className="min-w-0 sm:col-span-2">
            <Field
              label="选模规则"
              hint={selectedPolicy ? `${selectedPolicy.candidates.length} 个候选模型 · 最低上下文 ${selectedPolicy.min_context_tokens.toLocaleString("zh-CN")} Token` : undefined}
            >
              <Select required value={selectedPolicy?.id ?? ""} onChange={(event) => setPolicyID(event.target.value)} disabled={!publishedPolicies.length}>
                <option value="" disabled>{publishedPolicies.length ? "请选择已发布的规则" : "暂无可用规则"}</option>
                {publishedPolicies.map((policy) => (
                  <option key={policy.id} value={policy.id}>{policy.name} · v{policy.version}</option>
                ))}
              </Select>
            </Field>
            {!publishedPolicies.length && !policies.error ? (
              <p className="mt-2 text-xs leading-5 text-muted">
                {user?.role === "admin" ? <>请先<Link to="/admin/policies" className="mx-1 text-accent underline underline-offset-2">创建并发布选模规则</Link>。</> : "请联系管理员发布选模规则。"}
              </p>
            ) : null}
          </div>
          <div className="min-w-0">
            <MultiSelect
              label="技能"
              options={activeSkills.map((skill) => ({
                id: skill.id, label: skill.name, detail: skill.description,
                badge: <Badge>v{skill.version}</Badge>,
              }))}
              value={skillIDs}
              onChange={setSkillIDs}
              empty={skills.error ? "技能加载失败" : "暂无已发布技能"}
            />
            {skills.error ? <div className="mt-2"><ErrorBanner message={skills.error} onRetry={skills.reload} /></div> : null}
          </div>
          <div className="min-w-0 sm:col-span-2">
            <MultiSelect
              label="我的技能"
              options={activeUserSkills.map((skill) => ({
                id: skill.id, label: skill.name, detail: skill.description,
                badge: <Badge tone={skill.bundle ? "info" : "neutral"}>{skill.bundle ? `技能包 · ${skill.bundle.file_count} 文件` : `v${skill.version}`}</Badge>,
              }))}
              value={userSkillIDs}
              onChange={setUserSkillIDs}
              empty={userSkills.error ? "我的技能加载失败" : "暂无已发布的个人技能"}
            />
            {userSkills.error ? <div className="mt-2"><ErrorBanner message={userSkills.error} onRetry={userSkills.reload} /></div> : null}
          </div>
          <div className="min-w-0">
            <MultiSelect
              label="外部工具"
              options={activeMCPs.map((tool) => {
                const highRisk = tool.risk_level === "high" || tool.risk_level === "critical";
                const mediumRisk = tool.risk_level === "medium";
                return {
                  id: tool.id, label: tool.name, detail: tool.endpoint,
                  badge: <Badge tone={highRisk ? "danger" : mediumRisk ? "warning" : "neutral"}>
                    {highRisk ? "高风险" : mediumRisk ? "中风险" : "低风险"}
                  </Badge>,
                };
              })}
              value={mcpIDs}
              onChange={setMcpIDs}
              empty={mcps.error ? "工具加载失败" : "暂无已启用工具"}
            />
            {mcps.error ? <div className="mt-2"><ErrorBanner message={mcps.error} onRetry={mcps.reload} /></div> : null}
          </div>
        </div>
      ) : null}

      {step === 3 ? (
        <Card title="检查一下，然后发布" description="发布后配置会冻结为快照。需要修改时创建新版本，不会影响正在进行的对话。">
          <dl className="grid gap-4 md:grid-cols-2">
            <div className="rounded-lg border border-line bg-paper-raised p-4">
              <dt className="text-[11px] tracking-wide text-muted">名称</dt>
              <dd className="mt-1 text-lg text-ink">{name.trim() || "未填写"}</dd>
              <dd className="mt-1 text-xs leading-5 text-muted">{description.trim() || "暂无描述"}</dd>
            </div>
            <div className="rounded-lg border border-line bg-paper-raised p-4">
              <dt className="text-[11px] tracking-wide text-muted">工作方式</dt>
              <dd className="mt-1 text-lg text-ink">{selectedMode.title}</dd>
              <dd className="mt-1 text-xs leading-5 text-muted">{selectedMode.description}</dd>
            </div>
            <div className="rounded-lg border border-line bg-paper-raised p-4">
              <dt className="text-[11px] tracking-wide text-muted">选模规则</dt>
              <dd className="mt-1 text-lg text-ink">{selectedPolicy?.name ?? "未选择"}</dd>
              <dd className="mt-1 text-xs leading-5 text-muted">{selectedPolicy ? `v${selectedPolicy.version} · ${selectedPolicy.candidates.length} 个候选模型` : "发布前必须选择"}</dd>
            </div>
            <div className="rounded-lg border border-line bg-paper-raised p-4">
              <dt className="text-[11px] tracking-wide text-muted">技能与工具</dt>
              <dd className="mt-1 text-lg text-ink">{skillIDs.length + userSkillIDs.length} 个技能 · {mcpIDs.length} 个工具</dd>
              <dd className="mt-1 text-xs leading-5 text-muted">
                {skillIDs.length + userSkillIDs.length + mcpIDs.length === 0 ? "未绑定，只使用基础对话能力" : "已绑定，发布后可在对话中调用"}
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

      </fieldset>
      <div className="sticky bottom-2 z-20 flex flex-wrap items-center justify-between gap-2 rounded-xl border border-line-strong bg-paper-raised/95 p-3 shadow-lg backdrop-blur" aria-busy={busy !== null}>
        {step > 0 ? (
          <Button variant="ghost" disabled={busy !== null} onClick={() => goToStep(step - 1)}>
            <Icon name="back" className="size-4" /> 上一步
          </Button>
        ) : <p className="text-xs text-muted">{canLeaveBasics ? "第 1 步，共 4 步" : "助手名称至少 2 个字符"}</p>}
        <div className="ml-auto flex flex-wrap items-center gap-2">
          {canLeaveBasics && canLeaveModel ? (
            <Button variant="secondary" loading={busy === "draft"} disabled={busy !== null} onClick={() => void submit(false)}>保存草稿</Button>
          ) : null}
          {step < 3 ? (
            <Button disabled={busy !== null || !canVisitStep(step + 1)} onClick={() => goToStep(step + 1)}>
              下一步 <Icon name="arrow" className="size-4" />
            </Button>
          ) : (
            <Button loading={busy === "publish"} disabled={busy !== null || !canLeaveModel} onClick={() => void submit(true)}>
              发布助手
            </Button>
          )}
        </div>
      </div>

      <ConfirmDialog
        open={blocker.state === "blocked" && busy === null}
        title="放弃未保存的修改？"
        description="当前填写的内容还没有保存，离开后会丢失。"
        confirmLabel="放弃并离开"
        onCancel={() => { if (blocker.state === "blocked") blocker.reset(); }}
        onConfirm={() => { if (blocker.state === "blocked") blocker.proceed(); }}
      />
    </div>
  );
}
