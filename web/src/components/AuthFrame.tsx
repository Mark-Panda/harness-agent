// 登录与注册页共享的认证框架：左侧品牌说明，右侧表单。
import type { ReactNode } from "react";

export function AuthFrame({ title, description, children, footer }: { title: string; description: string; children: ReactNode; footer?: ReactNode }) {
  return (
    <main className="flex min-h-screen items-center justify-center bg-paper-raised px-4 py-10 text-ink">
      <div className="grid w-full max-w-4xl gap-8 rounded-2xl border border-line bg-paper-raised p-6 md:grid-cols-[1.1fr_1fr] md:p-10">
        <section className="flex flex-col justify-between gap-8">
          <div>
            <p className="text-xs font-semibold uppercase tracking-[0.28em] text-accent">HARNESS · 智能体工坊</p>
            <h1 className="mt-3 text-4xl leading-tight font-semibold tracking-tight">把重复的事，交给一个懂你的助手。</h1>
            <p className="mt-3 max-w-md text-sm leading-6 text-muted">
              创建助手、说清目标、看清每一步。模型接入由平台管理员完成，你只需要专注要解决的问题。
            </p>
          </div>
          <ul className="grid gap-2 text-xs text-muted">
            {["用自然语言描述目标", "高风险操作先征求你的同意", "随时回看完整处理过程"].map((item) => (
              <li key={item} className="flex items-center gap-2 rounded-lg border border-line bg-paper-deep/60 px-3 py-2">
                <span aria-hidden className="size-1.5 rounded-full bg-accent" />
                {item}
              </li>
            ))}
          </ul>
        </section>
        <section className="flex flex-col justify-center gap-5 rounded-xl border border-line bg-paper-deep/70 p-6">
          <div>
            <h2 className="text-xl font-semibold">{title}</h2>
            <p className="mt-1 text-xs leading-5 text-muted">{description}</p>
          </div>
          {children}
          {footer}
        </section>
      </div>
    </main>
  );
}
