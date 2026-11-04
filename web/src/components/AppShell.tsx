// 应用外壳：顶栏导航、平台设置入口、使用指南与账户操作。
import { useState } from "react";
import type { ReactNode } from "react";
import { Link, NavLink, Navigate, Outlet } from "react-router-dom";
import { useAuth } from "../lib/auth";
import { HelpSheet } from "./HelpSheet";
import { Badge, Button, ConfirmDialog, EmptyState, cn } from "./ui";

const workItems = [
  { to: "/", label: "工作台", detail: "从哪里开始" },
  { to: "/sessions", label: "对话", detail: "和助手聊天" },
  { to: "/agents", label: "助手", detail: "创建与发布" },
  { to: "/audit", label: "运行记录", detail: "每一步做了什么" },
];

const adminItems = [
  { to: "/admin/providers", label: "模型服务", detail: "接入厂商与密钥" },
  { to: "/admin/models", label: "可用模型", detail: "登记具体模型" },
  { to: "/admin/policies", label: "选模规则", detail: "主选与替补顺序" },
  { to: "/admin/skills", label: "技能", detail: "可复用的做事说明" },
  { to: "/admin/mcps", label: "外部工具", detail: "助手能调用的能力" },
  { to: "/admin/feishu-bots", label: "飞书机器人", detail: "机器人登记与绑定" },
  { to: "/admin/users", label: "用户", detail: "账号、禁用与重置密码" },
];

function TopNavItem({ to, label, detail }: { to: string; label: string; detail: string }) {
  return (
    <NavLink
      to={to}
      end={to === "/"}
      className={({ isActive }) =>
        cn(
          "group flex flex-col gap-0.5 border-b-2 px-1 py-1 transition focus-visible:outline focus-visible:outline-2 focus-visible:outline-accent",
          isActive ? "border-accent text-accent" : "border-transparent text-ink-soft hover:border-line-strong hover:text-ink",
        )
      }
    >
      <span className="text-sm font-medium tracking-wide">{label}</span>
      <span className="text-[10px] text-muted group-hover:text-muted">{detail}</span>
    </NavLink>
  );
}

export function AppShell() {
  const { user, logout, logoutAll } = useAuth();
  const [confirmLogoutAll, setConfirmLogoutAll] = useState(false);
  const [helpOpen, setHelpOpen] = useState(false);
  const [settingsOpen, setSettingsOpen] = useState(false);
  const [accountOpen, setAccountOpen] = useState(false);
  const [busy, setBusy] = useState(false);

  return (
    <div className="min-h-screen text-ink">
      <a
        href="#main-content"
        className="sr-only focus:not-sr-only focus:absolute focus:left-4 focus:top-4 focus:z-50 focus:rounded focus:bg-ink focus:px-3 focus:py-2 focus:text-paper-raised"
      >
        跳到主内容
      </a>

      <header className="sticky top-0 z-30 border-b border-line bg-paper/90 backdrop-blur-sm">
        <div className="mx-auto flex w-full max-w-6xl flex-wrap items-center gap-x-8 gap-y-3 px-4 py-3 lg:px-6">
          <Link to="/" className="flex items-baseline gap-2 focus-visible:outline focus-visible:outline-2 focus-visible:outline-accent">
            <span className="font-display text-xl tracking-[0.16em] text-ink">HARNESS</span>
            <span className="text-[11px] tracking-[0.22em] text-accent">智能体工坊</span>
          </Link>

          <nav aria-label="主导航" className="flex flex-wrap items-center gap-5">
            {workItems.map((item) => (
              <TopNavItem key={item.to} {...item} />
            ))}
          </nav>

          <div className="ml-auto flex flex-wrap items-center gap-2">
            <Button size="sm" variant="secondary" onClick={() => setHelpOpen(true)}>
              使用指南
            </Button>
            {user?.role === "admin" ? (
              <div className="relative">
                <Button size="sm" variant="ghost" onClick={() => setSettingsOpen((value) => !value)} aria-expanded={settingsOpen}>
                  平台设置 {settingsOpen ? "▴" : "▾"}
                </Button>
                {settingsOpen ? (
                  <div className="absolute right-0 z-40 mt-2 w-72 rounded-[3px] border border-line bg-paper-raised p-2 shadow-[0_16px_40px_rgb(31_27_21/0.18)]">
                    <p className="px-2 pb-1 text-[11px] leading-5 tracking-wide text-muted">先完成前 3 项（服务、模型、规则），助手才能选到模型。</p>
                    {adminItems.map((item) => (
                      <NavLink
                        key={item.to}
                        to={item.to}
                        onClick={() => setSettingsOpen(false)}
                        className={({ isActive }) =>
                          cn(
                            "block rounded-[2px] px-2 py-2 transition hover:bg-paper-deep",
                            isActive ? "bg-paper-deep" : "",
                          )
                        }
                      >
                        <span className="block text-xs font-medium text-ink">{item.label}</span>
                        <span className="block text-[11px] text-muted">{item.detail}</span>
                      </NavLink>
                    ))}
                  </div>
                ) : null}
              </div>
            ) : null}
            <div className="relative">
              <Button size="sm" variant="ghost" onClick={() => setAccountOpen((value) => !value)} aria-expanded={accountOpen}>
                <span aria-hidden className="flex size-6 items-center justify-center rounded-full border border-line-strong font-display text-[11px] text-accent">
                  {(user?.username ?? "?").slice(0, 1).toUpperCase()}
                </span>
                {user?.username ?? "账户"} {accountOpen ? "▴" : "▾"}
              </Button>
              {accountOpen ? (
                <div className="absolute right-0 z-40 mt-2 w-56 rounded-[3px] border border-line bg-paper-raised p-3 shadow-[0_16px_40px_rgb(31_27_21/0.18)]">
                  <div className="flex items-center justify-between gap-2">
                    <div className="min-w-0">
                      <p className="truncate text-sm font-medium text-ink">{user?.username ?? "未登录"}</p>
                      <p className="text-[11px] text-muted">{user?.role === "admin" ? "管理员" : "普通用户"}</p>
                    </div>
                    <Badge tone={user?.status === "disabled" ? "danger" : "success"}>{user?.status === "disabled" ? "已禁用" : "在线"}</Badge>
                  </div>
                  <div className="mt-3 flex flex-col gap-2">
                    <Button size="sm" variant="secondary" onClick={() => void logout()}>
                      注销当前设备
                    </Button>
                    <Button size="sm" variant="ghost" onClick={() => setConfirmLogoutAll(true)}>
                      注销全部设备
                    </Button>
                  </div>
                </div>
              ) : null}
            </div>
          </div>
        </div>
      </header>

      <div className="mx-auto w-full max-w-6xl px-4 py-8 lg:px-6">
        <main id="main-content" className="min-w-0 pb-16">
          <div className="page-rise">
            <Outlet />
          </div>
        </main>
      </div>

      <HelpSheet open={helpOpen} onClose={() => setHelpOpen(false)} />
      <ConfirmDialog
        open={confirmLogoutAll}
        title="注销全部设备？"
        description="所有设备的刷新令牌都会被撤销，其他标签页需要重新登录。"
        confirmLabel="确认注销全部"
        busy={busy}
        onCancel={() => setConfirmLogoutAll(false)}
        onConfirm={async () => {
          setBusy(true);
          try {
            await logoutAll();
          } finally {
            setBusy(false);
            setConfirmLogoutAll(false);
          }
        }}
      />
    </div>
  );
}

// ProtectedRoute 在会话恢复完成后决定放行或跳转登录。
export function ProtectedRoute({ children }: { children: ReactNode }) {
  const { status } = useAuth();
  if (status === "loading") {
    return (
      <div className="flex min-h-screen items-center justify-center bg-paper text-sm text-muted" role="status">
        正在恢复会话…
      </div>
    );
  }
  if (status === "anonymous") {
    return <Navigate to="/login" replace />;
  }
  return <>{children}</>;
}

// AdminRoute 在非管理员访问管理页面时给出明确拒绝。
export function AdminRoute({ children }: { children: ReactNode }) {
  const { user } = useAuth();
  if (user?.role !== "admin") {
    return (
      <EmptyState title="需要管理员权限" description="当前账号无权访问平台设置。请联系管理员提升角色，或回到工作台继续对话。" />
    );
  }
  return <>{children}</>;
}
