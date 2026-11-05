// 应用外壳：顶栏导航、平台设置入口、使用指南与账户操作。
import { useState } from "react";
import type { ReactNode } from "react";
import { Link, NavLink, Navigate, Outlet } from "react-router-dom";
import { useAuth } from "../lib/auth";
import { HelpSheet } from "./HelpSheet";
import { Badge, Button, ConfirmDialog, DropdownMenu, EmptyState, MenuItem, cn } from "./ui";

const workItems = [
  { to: "/", label: "工作台", detail: "从哪里开始" },
  { to: "/sessions", label: "对话", detail: "和助手聊天" },
  { to: "/agents", label: "助手", detail: "创建与发布" },
  { to: "/memory", label: "记忆", detail: "知识图谱" },
  { to: "/audit", label: "运行记录", detail: "每一步做了什么" },
];

// 管理入口按配置顺序分组，避免八项平铺后找不到先后关系。
const adminGroups = [
  {
    title: "模型接入",
    items: [
      { to: "/admin/providers", label: "模型服务", detail: "接入厂商与密钥" },
      { to: "/admin/models", label: "可用模型", detail: "登记具体模型" },
      { to: "/admin/policies", label: "选模规则", detail: "主选与替补顺序" },
    ],
  },
  {
    title: "能力配置",
    items: [
      { to: "/admin/skills", label: "技能", detail: "可复用的做事说明" },
      { to: "/admin/mcps", label: "外部工具", detail: "助手能调用的能力" },
      { to: "/admin/feishu-bots", label: "飞书机器人", detail: "机器人登记与绑定" },
    ],
  },
  {
    title: "平台管理",
    items: [
      { to: "/admin/users", label: "用户", detail: "账号、禁用与重置密码" },
      { to: "/admin/agents", label: "全部助手", detail: "跨用户只读审计" },
    ],
  },
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
      <span className="hidden text-[10px] text-muted sm:block">{detail}</span>
    </NavLink>
  );
}

export function AppShell() {
  const { user, logout, logoutAll } = useAuth();
  const [confirmLogoutAll, setConfirmLogoutAll] = useState(false);
  const [helpOpen, setHelpOpen] = useState(false);
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
        <div className="mx-auto flex w-full max-w-[1600px] flex-wrap items-center gap-x-8 gap-y-3 px-4 py-3 sm:px-6 lg:px-8">
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
              <DropdownMenu
                label="平台设置菜单"
                indicator
                panelClassName="w-80"
                triggerClassName="px-3 text-ink-soft hover:bg-paper-deep hover:text-ink"
                trigger={<span>平台设置</span>}
              >
                <p className="px-2 pb-2 text-[11px] leading-5 text-muted">
                  按顺序配置：先完成「模型接入」，助手才能选到模型。
                </p>
                {adminGroups.map((group, index) => (
                  <div
                    key={group.title}
                    role="group"
                    aria-label={group.title}
                    className={cn("py-1", index > 0 && "mt-1 border-t border-line pt-2")}
                  >
                    <p aria-hidden className="px-2 pb-1 text-[11px] font-semibold tracking-[0.14em] text-muted">
                      {group.title}
                    </p>
                    <div className="flex flex-col gap-0.5">
                      {group.items.map((item) => (
                        <MenuItem key={item.to} to={item.to}>
                          <span className="block font-medium text-ink">{item.label}</span>
                          <span className="block text-[11px] text-muted">{item.detail}</span>
                        </MenuItem>
                      ))}
                    </div>
                  </div>
                ))}
              </DropdownMenu>
            ) : null}
            <DropdownMenu
              label="账户菜单"
              indicator
              panelClassName="w-60 p-3"
              triggerClassName="px-3 text-ink-soft hover:bg-paper-deep hover:text-ink"
              trigger={
                <>
                  <span aria-hidden className="flex size-6 items-center justify-center rounded-full border border-line-strong font-display text-[11px] text-accent">
                    {(user?.username ?? "?").slice(0, 1).toUpperCase()}
                  </span>
                  <span className="max-w-28 truncate">{user?.username ?? "账户"}</span>
                </>
              }
            >
              <div className="flex items-center justify-between gap-2">
                <div className="min-w-0">
                  <p className="truncate text-sm font-medium text-ink">{user?.username ?? "未登录"}</p>
                  <p className="text-[11px] text-muted">{user?.role === "admin" ? "管理员" : "普通用户"}</p>
                </div>
                <Badge tone={user?.status === "disabled" ? "danger" : "success"}>{user?.status === "disabled" ? "已禁用" : "在线"}</Badge>
              </div>
              <div className="mt-3 flex flex-col gap-0.5 border-t border-line pt-2">
                <MenuItem onClick={() => void logout()}>注销当前设备</MenuItem>
                <MenuItem onClick={() => setConfirmLogoutAll(true)}>注销全部设备</MenuItem>
              </div>
            </DropdownMenu>
          </div>
        </div>
      </header>

      <div className="mx-auto w-full max-w-[1600px] px-4 py-8 sm:px-6 lg:px-8">
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
