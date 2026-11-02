import { useEffect, useState } from "react";

type BackendStatus = "checking" | "ready" | "unavailable";

const statusText: Record<BackendStatus, string> = {
  checking: "正在检查后端状态",
  ready: "后端连接正常",
  unavailable: "后端暂不可用",
};

export default function App() {
  const [status, setStatus] = useState<BackendStatus>("checking");

  useEffect(() => {
    let mounted = true;
    fetch("/health/ready", { headers: { Accept: "application/json" } })
      .then((response) => {
        if (!response.ok) throw new Error("backend is not ready");
        return response.json();
      })
      .then(() => mounted && setStatus("ready"))
      .catch(() => mounted && setStatus("unavailable"));
    return () => {
      mounted = false;
    };
  }, []);

  return (
    <main className="min-h-screen bg-slate-950 px-6 py-10 text-slate-100">
      <div className="mx-auto flex max-w-5xl flex-col gap-8">
        <header className="flex items-center justify-between border-b border-slate-800 pb-6">
          <div>
            <p className="text-sm font-medium text-emerald-400">ENGINEERING BASELINE</p>
            <h1 className="mt-2 text-3xl font-semibold tracking-tight">Harness Agent</h1>
          </div>
          <span
            aria-live="polite"
            className="rounded-full border border-slate-700 bg-slate-900 px-3 py-1 text-sm text-slate-300"
          >
            {statusText[status]}
          </span>
        </header>

        <section className="grid gap-4 md:grid-cols-3" aria-label="工程能力">
          {[
            ["多用户隔离", "DDD 分层、RLS 与私有数据边界"],
            ["可恢复执行", "Run、Attempt、工具与审计全链路"],
            ["生产级交付", "单镜像、健康检查与可观测基线"],
          ].map(([title, description]) => (
            <article key={title} className="rounded-xl border border-slate-800 bg-slate-900 p-5">
              <h2 className="font-medium text-slate-100">{title}</h2>
              <p className="mt-2 text-sm leading-6 text-slate-400">{description}</p>
            </article>
          ))}
        </section>

        <section className="rounded-xl border border-emerald-900/70 bg-emerald-950/20 p-6">
          <h2 className="text-lg font-medium text-emerald-300">阶段 0 已启动</h2>
          <p className="mt-2 max-w-2xl text-sm leading-6 text-slate-400">
            当前页面由 React 构建并通过 Go 二进制嵌入提供服务。后续能力将按实施计划逐阶段交付。
          </p>
        </section>
      </div>
    </main>
  );
}
