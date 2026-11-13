// 应用外壳：工作区侧栏、移动导航、平台设置和账户操作。
import { useEffect, useLayoutEffect, useRef, useState } from "react";
import type { ReactNode } from "react";
import { Link, Navigate, Outlet, useLocation, useNavigationType } from "react-router-dom";
import { useAuth } from "../lib/auth";
import { HelpSheet } from "./HelpSheet";
import { Icon } from "./Icon";
import type { IconName } from "./Icon";
import { Badge, Button, ConfirmDialog, DropdownMenu, EmptyState, MenuItem, cn } from "./ui";

const workItems: Array<{ to: string; label: string; icon: IconName }> = [
  { to: "/", label: "工作台", icon: "dashboard" },
  { to: "/sessions", label: "对话", icon: "chat" },
  { to: "/agents", label: "助手", icon: "agent" },
  { to: "/memory", label: "记忆", icon: "memory" },
  { to: "/audit", label: "运行记录", icon: "activity" },
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

function WorkspaceNav({ activePath, onNavigate, onHelp, onClose }: {
  activePath?: string;
  onNavigate?: () => void;
  onHelp: () => void;
  onClose?: () => void;
}) {
  return (
    <div className="flex min-h-full flex-col px-3 py-4">
      <div className="flex items-center justify-between gap-2 px-2">
        <Link to="/" onClick={onNavigate} className="flex min-w-0 items-center gap-3 rounded-lg">
          <span className="grid size-10 shrink-0 place-items-center rounded-xl bg-accent text-accent-ink">
            <Icon name="logo" className="size-6" />
          </span>
          <span className="min-w-0">
            <span className="block text-base font-semibold leading-tight tracking-tight">Harness</span>
            <span className="block text-xs text-muted">智能体工作区</span>
          </span>
        </Link>
        {onClose ? <Button size="sm" variant="ghost" aria-label="关闭导航" onClick={onClose}><Icon name="close" /></Button> : null}
      </div>

      <Link to="/sessions?new=1" onClick={onNavigate} className="mx-1 mt-7 flex min-h-10 items-center gap-2 rounded-lg border border-accent/25 bg-accent-soft/55 px-3 text-sm font-semibold text-accent transition-colors hover:border-accent/50 hover:bg-accent-soft">
        <Icon name="plus" />
        新建对话
      </Link>

      <nav aria-label="主导航" className="mt-7 flex flex-col gap-1">
        {workItems.map(({ to, label, icon }) => (
          <Link
            key={to}
            to={to}
            aria-current={activePath === to ? "page" : undefined}
            onClick={onNavigate}
            className={cn(
              "relative flex min-h-11 items-center gap-3 rounded-lg px-3 text-sm font-medium transition-colors",
              activePath === to ? "bg-accent-soft text-accent" : "text-ink-soft hover:bg-paper-deep hover:text-ink",
            )}
          >
            <Icon name={icon} />
            {label}
          </Link>
        ))}
      </nav>

      <div className="mt-auto pt-6">
        <div className="border-t border-line pt-3">
          <Button size="sm" variant="ghost" onClick={onHelp} className="w-full justify-start text-sm">
            <Icon name="help" />
            使用指南
          </Button>
        </div>
      </div>
    </div>
  );
}

export function AppShell() {
  const { user, logout, logoutAll } = useAuth();
  const { pathname, search } = useLocation();
  const navigationType = useNavigationType();
  const chatLayout = /^\/sessions\/[^/]+\/?$/.test(pathname);
  const lastPath = useRef(pathname);
  const mainRef = useRef<HTMLElement>(null);
  const shellRef = useRef<HTMLDivElement>(null);
  const [confirmLogoutAll, setConfirmLogoutAll] = useState(false);
  const [helpOpen, setHelpOpen] = useState(false);
  const [busy, setBusy] = useState(false);
  const navigationRef = useRef<HTMLDialogElement>(null);
  const sectionPath = pathname.startsWith("/runs/") ? "/audit" : pathname;
  const navItem = workItems.find(({ to }) => sectionPath === to || sectionPath.startsWith(to + "/"));
  const adminItem = adminGroups.flatMap((group) => group.items).find(({ to }) => pathname === to);
  const currentLabel = navItem?.label ?? adminItem?.label ?? "工作区";

  useLayoutEffect(() => {
    const changed = lastPath.current !== pathname;
    lastPath.current = pathname;
    if (!changed || navigationType === "POP") return;
    window.scrollTo({ top: 0, behavior: "instant" });
    const main = mainRef.current;
    if (!document.querySelector("dialog:modal") && !main?.contains(document.activeElement)) {
      main?.focus({ preventScroll: true });
    }
  }, [pathname, navigationType]);

  useLayoutEffect(() => {
    const viewport = window.visualViewport;
    const shell = shellRef.current;
    if (!chatLayout || !viewport || !shell) return;
    const resize = () => {
      // 放大页面时保留正常文档缩放；软键盘弹出时缩短工作区高度。
      if (viewport.scale === 1) shell.style.setProperty("--chat-viewport", `${viewport.height}px`);
      else shell.style.removeProperty("--chat-viewport");
    };
    resize();
    viewport.addEventListener("resize", resize);
    window.addEventListener("resize", resize);
    return () => {
      viewport.removeEventListener("resize", resize);
      window.removeEventListener("resize", resize);
      shell.style.removeProperty("--chat-viewport");
    };
  }, [chatLayout]);

  useEffect(() => {
    if (navigationRef.current?.open) navigationRef.current.close();
  }, [pathname, search]);

  useEffect(() => {
    const desktop = window.matchMedia?.("(min-width: 1024px)");
    if (!desktop) return;
    const closeOnDesktop = () => {
      if (desktop.matches && navigationRef.current?.open) {
        navigationRef.current?.close();
      }
    };
    desktop.addEventListener("change", closeOnDesktop);
    return () => desktop.removeEventListener("change", closeOnDesktop);
  }, []);

  const closeNav = () => navigationRef.current?.close();
  const openHelp = () => {
    closeNav();
    setHelpOpen(true);
  };

  return (
    <div ref={shellRef} className={cn("text-ink lg:grid lg:grid-cols-[228px_minmax(0,1fr)]", chatLayout ? "h-[var(--chat-viewport,100dvh)] overflow-hidden" : "min-h-screen")}>
      <a
        href="#main-content"
        className="sr-only focus:not-sr-only focus:fixed focus:left-4 focus:top-4 focus:z-50 focus:rounded-lg focus:bg-accent focus:px-3 focus:py-2 focus:text-accent-ink"
      >
        跳到主内容
      </a>

      <aside className="sticky top-0 hidden h-dvh overflow-y-auto border-r border-line bg-paper-raised lg:block">
        <WorkspaceNav activePath={navItem?.to} onHelp={openHelp} />
      </aside>
      <dialog
        ref={navigationRef}
        aria-label="工作区导航"
        className="fixed inset-y-0 left-0 m-0 h-dvh max-h-none w-[min(18rem,85vw)] max-w-none overflow-y-auto border-r border-line bg-paper-raised text-ink"
        onClick={(event) => {
          if (event.target === event.currentTarget) {
            closeNav();
          }
        }}
      >
        <WorkspaceNav activePath={navItem?.to} onNavigate={closeNav} onClose={closeNav} onHelp={openHelp} />
      </dialog>

      <div className={cn("min-w-0", chatLayout && "flex h-full min-h-0 flex-col")}>
        <header className={cn("sticky top-0 z-30 flex shrink-0 items-center gap-2 border-b border-line bg-paper/95 px-3 backdrop-blur-sm sm:gap-4 sm:px-6 lg:px-8", chatLayout ? "min-h-12" : "min-h-16")}>
          <Button size="sm" variant="ghost" className="size-10 shrink-0 p-0 lg:hidden" aria-label="打开导航" aria-haspopup="dialog" onClick={() => navigationRef.current?.showModal()}>
            <Icon name="menu" />
          </Button>
          <div className="flex min-w-0 items-center gap-2 text-sm">
            <span className="hidden text-muted sm:inline">工作区</span>
            <Icon name="chevron" className="hidden size-3.5 shrink-0 text-muted sm:block" />
            <span className="truncate font-medium">{currentLabel}</span>
          </div>
          <div className="ml-auto flex shrink-0 items-center gap-1 sm:gap-2">
            {user?.role === "admin" ? (
              <DropdownMenu
                label="平台设置菜单"
                triggerLabel="平台设置"
                indicator
                panelClassName="w-80"
                triggerClassName={cn("px-2.5 text-ink-soft hover:bg-paper-deep hover:text-ink", adminItem && "bg-paper-deep text-ink")}
                trigger={<><Icon name="settings" /><span className="hidden sm:inline">平台设置</span></>}
              >
                {adminGroups.map((group, index) => (
                  <div
                    key={group.title}
                    role="group"
                    aria-label={group.title}
                    className={cn("px-0.5 py-1", index > 0 && "mt-1 border-t border-line pt-2")}
                  >
                    <p aria-hidden className="px-2.5 pb-1 text-xs font-medium text-muted">
                      {group.title}
                    </p>
                    <div className="flex flex-col gap-0.5">
                      {group.items.map((item) => (
                        <MenuItem key={item.to} to={item.to}>
                          <span className="block font-medium">{item.label}</span>
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
              triggerClassName="min-h-10 px-2 text-ink-soft hover:bg-paper-deep hover:text-ink"
              trigger={
                <>
                  <span aria-hidden className="grid size-7 place-items-center rounded-full border border-accent/25 bg-accent-soft text-xs text-accent">
                    {(user?.username ?? "?").slice(0, 1).toUpperCase()}
                  </span>
                  <span className="sr-only max-w-28 truncate sm:not-sr-only">{user?.username ?? "账户"}</span>
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
                <MenuItem icon="logout" onClick={() => void logout()}>注销当前设备</MenuItem>
                <MenuItem icon="logout" danger onClick={() => setConfirmLogoutAll(true)}>注销全部设备</MenuItem>
              </div>
            </DropdownMenu>
          </div>
        </header>

        <main id="main-content" ref={mainRef} tabIndex={-1} className={cn("w-full focus:outline-none", chatLayout ? "flex min-h-0 min-w-0 flex-1 flex-col" : "mx-auto max-w-[1520px] px-4 py-6 sm:px-6 sm:py-8 lg:px-8")}>
          <Outlet />
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
