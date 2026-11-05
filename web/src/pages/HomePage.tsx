// 工作台：用“下一步该做什么”替代功能罗列，降低首次使用门槛。
import { Link } from "react-router-dom";
import { useAuth } from "../lib/auth";
import { requestJSON } from "../lib/api";
import { formatRelative } from "../lib/format";
import { useRemote } from "../lib/hooks";
import type { Agent, Model, ModelPolicy, Page, Provider, Session, VectorStatus } from "../lib/types";
import { Badge, Card, EmptyState, PageHeader, cn } from "../components/ui";

interface SetupStep {
  key: string;
  order: string;
  title: string;
  plain: string;
  done: boolean;
  to: string;
  action: string;
  adminOnly: boolean;
}

export default function HomePage() {
  const { user } = useAuth();
  const isAdmin = user?.role === "admin";
  const providers = useRemote(
    () => (isAdmin ? requestJSON<Page<Provider>>("/api/v1/providers") : Promise.resolve({ items: [], next_cursor: null })),
    [isAdmin],
  );
  const models = useRemote(
    () => (isAdmin ? requestJSON<Page<Model>>("/api/v1/models") : Promise.resolve({ items: [], next_cursor: null })),
    [isAdmin],
  );
  const policies = useRemote(
    () => (isAdmin ? requestJSON<Page<ModelPolicy>>("/api/v1/model-policies") : Promise.resolve({ items: [], next_cursor: null })),
    [isAdmin],
  );
  const agents = useRemote(() => requestJSON<Page<Agent>>("/api/v1/agents?limit=50"), []);
  const sessions = useRemote(() => requestJSON<Page<Session>>("/api/v1/sessions?limit=10"), []);
  const vector = useRemote(
    () => (isAdmin ? requestJSON<VectorStatus>("/api/v1/memory/vector-status") : Promise.resolve(null)),
    [isAdmin],
  );

  const chatModels = (models.data?.items ?? []).filter((model) => model.kind === "chat" && model.verified_at);
  const publishedPolicies = (policies.data?.items ?? []).filter((policy) => policy.status === "published");
  const publishedAgents = (agents.data?.items ?? []).filter((agent) => agent.status === "published");
  const recentSessions = sessions.data?.items.slice(0, 4) ?? [];

  const steps: SetupStep[] = [
    {
      key: "provider",
      order: "01",
      title: "接入模型服务",
      plain: "填写模型厂商的接口地址和密钥。通常只需管理员做一次。",
      done: (providers.data?.items.length ?? 0) > 0,
      to: "/admin/providers",
      action: "去接入",
      adminOnly: true,
    },
    {
      key: "model",
      order: "02",
      title: "登记可用模型",
      plain: "告诉系统具体能调用哪个模型，例如 deepseek-chat。",
      done: chatModels.length > 0,
      to: "/admin/models",
      action: "去登记",
      adminOnly: true,
    },
    {
      key: "policy",
      order: "03",
      title: "设定选模规则",
      plain: "安排主选与替补模型。主模型不可用时会自动切换。",
      done: publishedPolicies.length > 0,
      to: "/admin/policies",
      action: "去设定",
      adminOnly: true,
    },
    {
      key: "agent",
      order: "04",
      title: "创建并发布助手",
      plain: "给助手起名、选择工作方式与工具，发布后就能对话。",
      done: publishedAgents.length > 0,
      to: "/agents",
      action: publishedAgents.length > 0 ? "管理助手" : "创建助手",
      adminOnly: false,
    },
  ];
  const visibleSteps = isAdmin ? steps : steps.filter((step) => !step.adminOnly);
  const nextStep = visibleSteps.find((step) => !step.done) ?? null;
  const adminReady = publishedPolicies.length > 0;

  return (
    <div className="flex flex-col gap-8">
      <section className="grid gap-6 rounded-[4px] border border-line bg-paper-raised px-6 py-8 md:grid-cols-[1.6fr_0.8fr] md:items-center">
        <div>
          <PageHeader
            eyebrow="工作台"
            title="把事交给助手，你只管说清楚。"
            description="先配置模型，再创建助手，之后就能像和同事协作一样对话。下面的清单会告诉你现在走到哪一步。"
            actions={
              <Link
                to={adminReady || !isAdmin ? "/agents" : "/admin/policies"}
                className="inline-flex h-10 items-center rounded-[3px] bg-ink px-4 text-sm font-medium tracking-wide text-paper-raised transition hover:bg-accent focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-accent"
              >
                {adminReady || !isAdmin ? "创建助手" : "先设定选模规则"}
              </Link>
            }
          />
          {nextStep ? (
            <p className="mt-4 text-sm leading-6 text-ink-soft">
              下一步：<span className="font-medium text-accent">{nextStep.title}</span>。{nextStep.plain}
            </p>
          ) : (
            <p className="mt-4 text-sm leading-6 text-jade">配置已完成，可以直接开始对话。</p>
          )}
        </div>
        <div aria-hidden className="hidden justify-self-end border border-accent/40 px-6 py-5 text-center md:block">
          <p className="font-display text-4xl tracking-[0.3em] text-accent">工坊</p>
          <p className="mt-2 text-[10px] tracking-[0.28em] text-muted">WORKBENCH</p>
          <p className="mt-3 border-t border-line pt-2 text-[10px] tracking-[0.2em] text-muted">纸墨 · 人机协作</p>
        </div>
      </section>

      <div className="grid gap-4 md:grid-cols-3">
        <Link to="/sessions" className="group rounded-[3px] border border-line bg-paper-raised p-5 transition hover:border-accent hover:shadow-[0_8px_24px_rgb(31_27_21/0.08)]">
          <p className="text-[11px] tracking-[0.18em] text-muted">继续工作</p>
          <p className="mt-2 font-display text-xl text-ink group-hover:text-accent">{sessions.data?.items.length ?? 0} 个对话</p>
          <p className="mt-2 text-xs leading-5 text-muted">打开最近对话，或新建一个会话。</p>
        </Link>
        <Link to="/agents" className="group rounded-[3px] border border-line bg-paper-raised p-5 transition hover:border-accent hover:shadow-[0_8px_24px_rgb(31_27_21/0.08)]">
          <p className="text-[11px] tracking-[0.18em] text-muted">可用助手</p>
          <p className="mt-2 font-display text-xl text-ink group-hover:text-accent">{publishedAgents.length} 个已发布</p>
          <p className="mt-2 text-xs leading-5 text-muted">每个助手有自己的工作方式、技能与工具。</p>
        </Link>
        <Link to="/audit" className="group rounded-[3px] border border-line bg-paper-raised p-5 transition hover:border-accent hover:shadow-[0_8px_24px_rgb(31_27_21/0.08)]">
          <p className="text-[11px] tracking-[0.18em] text-muted">过程可查</p>
          <p className="mt-2 font-display text-xl text-ink group-hover:text-accent">运行记录</p>
          <p className="mt-2 text-xs leading-5 text-muted">回看每一步思考、工具调用与审批。</p>
        </Link>
      </div>

      <Card
        title={isAdmin ? "配置进度" : "开始使用"}
        description={isAdmin ? "按顺序完成即可。任何一步都可以点进去修改。" : "平台已由管理员配置。你只需要创建助手并开始对话。"}
      >
        <ol className="flex flex-col gap-2">
          {visibleSteps.map((step) => (
            <li
              key={step.key}
              className={cn(
                "flex flex-wrap items-center gap-3 rounded-[3px] border px-4 py-3 transition",
                step.done ? "border-jade/40 bg-jade-soft/30" : nextStep?.key === step.key ? "border-accent bg-accent-soft/40" : "border-line bg-paper-raised",
              )}
            >
              <span
                aria-hidden
                className={cn(
                  "flex size-8 shrink-0 items-center justify-center rounded-full border font-display text-sm",
                  step.done ? "border-jade bg-jade-soft text-jade" : "border-line-strong text-muted",
                )}
              >
                {step.done ? "✓" : step.order}
              </span>
              <div className="min-w-0 flex-1">
                <div className="flex flex-wrap items-center gap-2">
                  <p className="text-sm font-medium text-ink">{step.title}</p>
                  {step.done ? <Badge tone="success">已完成</Badge> : nextStep?.key === step.key ? <Badge tone="warning">下一步</Badge> : null}
                </div>
                <p className="mt-0.5 text-xs leading-5 text-muted">{step.plain}</p>
              </div>
              {isAdmin || !step.adminOnly ? (
                <Link
                  to={step.to}
                  className="inline-flex h-8 items-center rounded-[3px] border border-line-strong bg-paper-raised px-3 text-xs text-ink transition hover:border-accent hover:text-accent"
                >
                  {step.action}
                </Link>
              ) : (
                <span className="text-xs text-muted">由管理员配置</span>
              )}
            </li>
          ))}
        </ol>
        {!isAdmin && !adminReady ? (
          <p className="mt-3 rounded-[3px] border-l-4 border-ochre bg-ochre-soft/60 px-4 py-3 text-xs leading-5 text-ochre">
            管理员还没有完成模型配置。你仍然可以创建助手草稿，但发布前需要等待选模规则就绪。
          </p>
        ) : null}
        {isAdmin && vector.data && !vector.data.healthy ? (
          <p className="mt-3 rounded-[3px] border-l-4 border-ochre bg-ochre-soft/60 px-4 py-3 text-xs leading-5 text-ochre">
            向量记忆未生效：{vector.data.warning ?? "尚未配置生效的向量模型。"}
            <Link to="/admin/models" className="ml-2 underline underline-offset-2">去模型目录配置</Link>
          </p>
        ) : null}
      </Card>

      <div className="grid gap-4 md:grid-cols-2">
        <Card title="最近的对话" description="从上次停下的地方继续。">
          {sessions.loading ? (
            <p className="text-xs text-muted">正在读取…</p>
          ) : recentSessions.length > 0 ? (
            <div className="flex flex-col gap-2">
              {recentSessions.map((session) => (
                <Link
                  key={session.id}
                  to={`/sessions/${session.id}`}
                  className="flex flex-wrap items-center justify-between gap-2 rounded-[3px] border border-line bg-paper-raised px-3 py-2.5 transition hover:border-accent"
                >
                  <span className="min-w-0">
                    <span className="block truncate text-sm text-ink">{session.title}</span>
                    <span className="text-[11px] text-muted">{formatRelative(session.last_message_at)} · 来自 {session.source === "web" ? "网页" : session.source === "feishu" ? "飞书" : "接口"}</span>
                  </span>
                  <span aria-hidden className="text-accent">→</span>
                </Link>
              ))}
            </div>
          ) : (
            <EmptyState title="还没有对话" description="创建助手后，从“对话”页开始第一次交流。" />
          )}
        </Card>

        <Card title="三件值得知道的事" description="减少误操作，也让过程更可控。">
          <ul className="flex flex-col gap-3 text-sm leading-6 text-ink-soft">
            <li>
              <span className="font-medium text-ink">说目标，不必说步骤。</span>
              <span className="text-muted">例如“把这份需求整理成三条结论”，助手会自己拆解。</span>
            </li>
            <li>
              <span className="font-medium text-ink">高风险操作会先问你。</span>
              <span className="text-muted">涉及外部系统的工具需要审批，确认后才会执行。</span>
            </li>
            <li>
              <span className="font-medium text-ink">不满意可以回看过程。</span>
              <span className="text-muted">运行记录保留每一步的输入、输出与失败原因。</span>
            </li>
          </ul>
        </Card>
      </div>
    </div>
  );
}
