import type { ReactNode } from "react";
import { Icon } from "./Icon";
import { WorkflowArt } from "./WorkflowArt";

export function AuthFrame({ title, description, children, footer }: { title: string; description: string; children: ReactNode; footer?: ReactNode }) {
  return (
    <main className="flex min-h-dvh items-center justify-center px-4 py-8 text-ink sm:p-10">
      <div className="grid w-full max-w-5xl overflow-hidden rounded-2xl border border-line bg-paper-raised md:min-h-[510px] md:grid-cols-[1.15fr_1fr]">
        <section className="workspace-grid relative isolate flex flex-col justify-between gap-8 overflow-hidden border-b border-line p-6 sm:p-9 md:border-r md:border-b-0">
          <div className="relative flex items-center gap-3">
            <span className="grid size-10 place-items-center rounded-xl bg-accent text-accent-ink"><Icon name="logo" className="size-6" /></span>
            <span className="text-lg font-semibold tracking-tight">Harness</span>
          </div>
          <div className="relative">
            <p className="max-w-sm text-2xl leading-tight font-semibold tracking-tight sm:text-4xl">智能体工作区</p>
            <p className="mt-3 max-w-xs text-sm leading-6 text-ink-soft">创建助手、协作对话，随时查看执行过程。</p>
          </div>
          <div className="relative hidden h-28 overflow-hidden rounded-xl border border-accent/20 bg-paper/40 lg:block [&>.workflow-art]:w-full">
            <WorkflowArt />
          </div>
        </section>
        <section className="flex min-w-0 flex-col justify-center gap-6 p-6 sm:p-9">
          <div>
            <h1 className="text-2xl tracking-tight">{title}</h1>
            <p className="mt-2 text-sm leading-6 text-muted">{description}</p>
          </div>
          {children}
          {footer}
        </section>
      </div>
    </main>
  );
}
