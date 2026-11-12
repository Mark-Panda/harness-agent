import { Icon } from "./Icon";

export function WorkflowArt() {
  return (
    <div aria-hidden="true" className="workflow-art pointer-events-none absolute inset-y-0 right-0 hidden w-[45%] items-center justify-center lg:flex">
      <div className="relative flex w-full max-w-80 items-center px-5 text-accent">
        <span className="grid size-12 shrink-0 place-items-center rounded-xl border border-accent/30 bg-paper-raised">
          <Icon name="chat" className="size-5" />
        </span>
        <span className="h-px flex-1 bg-accent/35" />
        <span className="grid size-18 shrink-0 place-items-center rounded-2xl border border-accent/60 bg-accent-soft shadow-[0_0_30px_rgb(115_239_166/0.15)]">
          <Icon name="agent" className="size-8" />
        </span>
        <span className="h-px flex-1 bg-accent/35" />
        <span className="grid size-12 shrink-0 place-items-center rounded-xl border border-accent/30 bg-paper-raised">
          <Icon name="activity" className="size-5" />
        </span>
      </div>
    </div>
  );
}
