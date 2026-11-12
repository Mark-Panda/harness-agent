import { Link } from "react-router-dom";
import { Icon } from "../components/Icon";
import type { IconName } from "../components/Icon";
import { WorkflowArt } from "../components/WorkflowArt";
import { ActionLink, Callout, Card, EmptyState, ErrorBanner, LoadingState, cn } from "../components/ui";
import { requestJSON } from "../lib/api";
import { useAuth } from "../lib/auth";
import { formatRelative, formatSessionSource } from "../lib/format";
import { useRemote } from "../lib/hooks";
import type { Agent, Model, ModelPolicy, Page, Provider, Session, VectorStatus } from "../lib/types";

const shortcuts: Array<{ to: string; label: string; detail: string; icon: IconName }> = [
  { to: "/sessions", label: "我的对话", detail: "搜索与管理会话", icon: "chat" },
  { to: "/agents", label: "助手管理", detail: "创建、配置与发布助手", icon: "agent" },
  { to: "/audit", label: "运行记录", detail: "查看执行过程与审批", icon: "activity" },
];

export default function HomePage() {
  const { user } = useAuth();
  const isAdmin = user?.role === "admin";
  const providers = useRemote(
    () => isAdmin ? requestJSON<Page<Provider>>("/api/v1/providers") : Promise.resolve(null),
    [isAdmin],
  );
  const models = useRemote(
    () => isAdmin ? requestJSON<Page<Model>>("/api/v1/models") : Promise.resolve(null),
    [isAdmin],
  );
  const policies = useRemote(
    () => isAdmin ? requestJSON<Page<ModelPolicy>>("/api/v1/model-policies") : Promise.resolve(null),
    [isAdmin],
  );
  const agents = useRemote(() => requestJSON<Page<Agent>>("/api/v1/agents?limit=50"), []);
  const sessions = useRemote(() => requestJSON<Page<Session>>("/api/v1/sessions?limit=5"), []);
  const vector = useRemote(
    () => isAdmin ? requestJSON<VectorStatus>("/api/v1/memory/vector-status") : Promise.resolve(null),
    [isAdmin],
  );

  const hasAssistant = agents.data?.items.some((agent) => agent.status === "published") ?? false;
  const assistantStep = { title: "创建并发布助手", to: "/agents", done: hasAssistant };
  const setupSteps = isAdmin ? [
    { title: "接入模型服务", to: "/admin/providers", done: Boolean(providers.data?.items.length) },
    { title: "登记并验证聊天模型", to: "/admin/models", done: models.data?.items.some((model) => model.kind === "chat" && model.verified_at) ?? false },
    { title: "发布选模规则", to: "/admin/policies", done: policies.data?.items.some((policy) => policy.status === "published") ?? false },
    assistantStep,
  ] : [assistantStep];
  const setupRequests = isAdmin ? [providers, models, policies, agents] : [agents];
  const setupLoading = setupRequests.some((request) => request.loading);
  const setupError = setupRequests.find((request) => request.error)?.error;
  const nextStep = setupSteps.find((step) => !step.done);
  const recentSessions = sessions.data?.items ?? [];
  const showSetup = isAdmin || !hasAssistant;
  const primaryAction = hasAssistant || setupLoading || setupError
    ? { label: "新建对话", to: "/sessions?new=1", icon: "plus" as const }
    : nextStep?.to.startsWith("/admin/")
      ? { label: nextStep.title, to: nextStep.to, icon: "arrow" as const }
      : { label: "创建助手", to: "/agents/new", icon: "plus" as const };

  return (
    <div className="flex flex-col gap-5 sm:gap-6">
      <section className="workspace-grid relative isolate overflow-hidden rounded-2xl border border-accent/20 px-5 py-7 sm:px-8 sm:py-9">
        <WorkflowArt />
        <div className="relative max-w-xl lg:max-w-[55%]">
          <h1 className="text-3xl leading-tight tracking-tight sm:text-4xl">工作台</h1>
          <p className="mt-3 max-w-md text-sm leading-6 text-ink-soft">
            {hasAssistant ? "选择助手开始新任务，或继续最近的对话。" : "配置并发布助手后，即可开始对话。"}
          </p>
          <div className="mt-6 flex flex-wrap gap-2.5">
            <ActionLink to={primaryAction.to}>
              <Icon name={primaryAction.icon} />
              {primaryAction.label}
            </ActionLink>
            {primaryAction.to !== "/agents/new" ? (
              <ActionLink to="/agents" variant="secondary">查看助手</ActionLink>
            ) : null}
          </div>
        </div>
      </section>

      <div className="grid gap-3 sm:grid-cols-3">
        {shortcuts.map(({ to, label, detail, icon }) => (
          <Link key={to} to={to} className="group flex min-w-0 items-center gap-3 rounded-xl border border-line bg-paper-raised p-4 transition-colors hover:border-accent/45 hover:bg-paper-deep">
            <span className="grid size-10 shrink-0 place-items-center rounded-lg border border-accent/20 bg-accent-soft text-accent">
              <Icon name={icon} />
            </span>
            <span className="min-w-0 flex-1">
              <span className="block text-sm font-semibold">{label}</span>
              <span className="mt-0.5 block text-xs leading-5 text-muted">{detail}</span>
            </span>
            <Icon name="arrow" className="size-4 shrink-0 text-muted group-hover:text-accent" />
          </Link>
        ))}
      </div>

      <div className={cn("grid items-start gap-5 sm:gap-6", showSetup && "xl:grid-cols-[minmax(0,1.65fr)_minmax(0,1fr)]")}>
        <Card
          title="最近对话"
          actions={<ActionLink to="/sessions" size="sm" variant="ghost">全部对话 <Icon name="arrow" className="size-4" /></ActionLink>}
        >
          {sessions.error ? <ErrorBanner message={sessions.error} onRetry={sessions.reload} /> : sessions.loading ? (
            <LoadingState label="加载最近对话" />
          ) : recentSessions.length > 0 ? (
            <ul className="divide-y divide-line">
              {recentSessions.map((session) => (
                <li key={session.id}>
                  <Link to={`/sessions/${session.id}`} className="group flex items-center gap-3 rounded-lg px-2 py-3 transition-colors hover:bg-paper-deep">
                    <span className="grid size-9 shrink-0 place-items-center rounded-lg border border-line bg-paper text-muted group-hover:text-accent">
                      <Icon name="chat" className="size-4" />
                    </span>
                    <span className="min-w-0 flex-1">
                      <span className="block truncate text-sm font-medium">{session.title}</span>
                      <span className="mt-0.5 block text-xs text-muted">{formatSessionSource(session.source)} · {formatRelative(session.last_message_at ?? session.created_at)}</span>
                    </span>
                    <Icon name="chevron" className="size-4 shrink-0 text-muted group-hover:text-accent" />
                  </Link>
                </li>
              ))}
            </ul>
          ) : (
            <EmptyState title="还没有对话" description={hasAssistant ? "选择一个已发布的助手，开始第一次对话。" : "发布助手后，你可以在这里继续最近的对话。"} />
          )}
        </Card>

        {showSetup ? (
          <Card title={isAdmin ? "配置进度" : "开始使用"}>
            {setupLoading ? <LoadingState label="读取配置进度" /> : setupError ? (
              <ErrorBanner message={setupError} onRetry={() => setupRequests.filter((request) => request.error).forEach((request) => request.reload())} />
            ) : (
              <ol className="flex flex-col gap-1.5">
                {setupSteps.map((step) => (
                  <li key={step.to}>
                    <Link
                      to={step.to}
                      className={cn("flex min-h-11 items-center gap-3 rounded-lg px-2 py-2 text-sm transition-colors hover:bg-paper-deep", nextStep === step && "bg-accent-soft/60")}
                    >
                      <span className={cn("grid size-6 shrink-0 place-items-center rounded-full border", step.done ? "border-accent/30 bg-accent-soft text-accent" : "border-line-strong text-muted")}>
                        {step.done ? <Icon name="check" className="size-3.5" /> : <span aria-hidden className="size-1.5 rounded-full bg-current" />}
                      </span>
                      <span className="min-w-0 flex-1">{step.title}</span>
                      <span className={cn("shrink-0 text-xs", nextStep === step ? "text-accent" : "text-muted")}>
                        {step.done ? "已完成" : nextStep === step ? "下一步" : "待配置"}
                      </span>
                    </Link>
                  </li>
                ))}
              </ol>
            )}
            {isAdmin && vector.data && !vector.data.healthy ? (
              <div className="mt-4">
                <Callout tone="warning" title="向量记忆未生效">
                  {vector.data.warning ?? "尚未配置向量模型。"} <Link to="/admin/models" className="text-ochre underline underline-offset-2">配置模型</Link>
                </Callout>
              </div>
            ) : null}
          </Card>
        ) : null}
      </div>
    </div>
  );
}
