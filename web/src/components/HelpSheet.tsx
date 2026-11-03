// 使用指南：用人类语言解释核心概念与首次使用顺序。
import { Badge, Button, Card, cn } from "./ui";

const flow = [
  { step: "01", title: "接入模型服务", detail: "你在用的模型由哪家公司提供，例如 OpenAI 兼容服务。" },
  { step: "02", title: "登记可用模型", detail: "告诉系统具体能调用哪个模型，以及它的上下文长度和价格。" },
  { step: "03", title: "设定选模规则", detail: "排一个主选与替补名单：主模型不可用时自动切换。" },
  { step: "04", title: "创建助手", detail: "选择工作方式、模型规则、技能与外部工具，然后发布。" },
  { step: "05", title: "开始对话", detail: "在对话页选择助手，发消息并观察执行过程。" },
];

const glossary = [
  { term: "助手", plain: "你创建的智能体。它决定怎么思考、用哪个模型、能做什么。" },
  { term: "模型服务", plain: "提供模型 API 的厂商或自建服务，需要填写地址和密钥。" },
  { term: "可用模型", plain: "某个服务下具体可调用的模型，例如 deepseek-chat。" },
  { term: "选模规则", plain: "模型的主选与替补顺序。主选失败或熔断时自动换下一个。" },
  { term: "技能", plain: "一段可复用的做事说明，像给助手的岗位手册。" },
  { term: "外部工具", plain: "助手可以调用的外部能力，例如查询、搜索或内部系统接口。" },
  { term: "运行", plain: "助手处理一次消息的完整过程，可能包含多步思考与工具调用。" },
  { term: "审批", plain: "高风险工具执行前需要你确认，避免误操作。" },
];

export function HelpSheet({ open, onClose }: { open: boolean; onClose: () => void }) {
  if (!open) {
    return null;
  }
  return (
    <div className="fixed inset-0 z-50 flex justify-end bg-ink/35" role="presentation" onClick={onClose}>
      <aside
        role="dialog"
        aria-modal="true"
        aria-label="使用指南"
        className="h-full w-full max-w-xl overflow-y-auto border-l border-line bg-paper px-6 py-6 shadow-[-20px_0_60px_rgb(31_27_21/0.18)]"
        onClick={(event) => event.stopPropagation()}
        onKeyDown={(event) => {
          if (event.key === "Escape") {
            onClose();
          }
        }}
      >
        <div className="flex items-start justify-between gap-4 border-b border-line pb-4">
          <div>
            <p className="text-[11px] font-semibold tracking-[0.18em] text-accent">使用指南</p>
            <h2 className="mt-1 text-2xl text-ink">先看这里，再动手</h2>
            <p className="mt-2 text-sm leading-6 text-muted">不用记术语。下面按“从零到第一次对话”的顺序解释每个功能。</p>
          </div>
          <Button size="sm" variant="secondary" onClick={onClose}>
            关闭
          </Button>
        </div>

        <section className="mt-6">
          <h3 className="text-lg text-ink">功能之间是怎么串起来的</h3>
          <ol className="mt-3 flex flex-col gap-2">
            {flow.map((item) => (
              <li key={item.step} className="flex gap-3 rounded-[3px] border border-line bg-paper-raised px-3 py-3">
                <span className="font-display text-lg leading-none text-accent">{item.step}</span>
                <span>
                  <span className="block text-sm font-medium text-ink">{item.title}</span>
                  <span className="mt-1 block text-xs leading-5 text-muted">{item.detail}</span>
                </span>
              </li>
            ))}
          </ol>
        </section>

        <section className="mt-8">
          <h3 className="text-lg text-ink">名词对照表</h3>
          <div className="mt-3 flex flex-col gap-2">
            {glossary.map((item) => (
              <div key={item.term} className="rounded-[3px] border border-line bg-paper-raised px-3 py-3">
                <div className="flex items-center gap-2">
                  <span className="text-sm font-medium text-ink">{item.term}</span>
                  <Badge tone="neutral">说明</Badge>
                </div>
                <p className="mt-1 text-xs leading-5 text-muted">{item.plain}</p>
              </div>
            ))}
          </div>
        </section>

        <Card className="mt-8" title="管理员才会看到的页面" description="普通用户只需要“工作台、对话、助手、运行记录”。">
          <ul className="flex flex-col gap-2 text-xs leading-5 text-ink-soft">
            <li>· <span className="font-medium">模型服务</span>：接入厂商密钥，通常由平台管理员一次性配置。</li>
            <li>· <span className="font-medium">可用模型</span>：登记具体模型并标注上下文长度、价格与能力。</li>
            <li>· <span className="font-medium">选模规则</span>：像排班一样安排主选与替补，不必写 JSON。</li>
            <li>· <span className="font-medium">技能与外部工具</span>：决定助手还能做什么；高风险工具会要求审批。</li>
          </ul>
          <p className={cn("mt-3 text-xs leading-5 text-muted")}>
            管理员完成前 3 步后，普通用户就能在“助手”页创建并发布自己的助手。
          </p>
        </Card>
      </aside>
    </div>
  );
}
