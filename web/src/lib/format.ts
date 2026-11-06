// 展示格式化工具：时间、数字与状态文案统一，避免各页面重复实现。

// formatDateTime 将 ISO 时间格式化为本地可读时间；空值回退为占位符。
export function formatDateTime(value: string | null | undefined): string {
  if (!value) {
    return "—";
  }
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) {
    return "—";
  }
  return date.toLocaleString("zh-CN", { hour12: false });
}

// formatRelative 输出相对时间，超过一天回退到绝对时间。
export function formatRelative(value: string | null | undefined): string {
  if (!value) {
    return "—";
  }
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) {
    return "—";
  }
  const diff = Date.now() - date.getTime();
  if (diff < 60_000) {
    return "刚刚";
  }
  if (diff < 3_600_000) {
    return `${Math.floor(diff / 60_000)} 分钟前`;
  }
  if (diff < 86_400_000) {
    return `${Math.floor(diff / 3_600_000)} 小时前`;
  }
  return formatDateTime(value);
}

// formatNumber 为整数添加千分位。
export function formatNumber(value: number | null | undefined): string {
  if (value === null || value === undefined) {
    return "—";
  }
  return value.toLocaleString("zh-CN");
}

// formatCost 输出保留两位小数的费用。
export function formatCost(value: number | null | undefined): string {
  if (value === null || value === undefined) {
    return "—";
  }
  return `$${value.toFixed(2)}`;
}

// formatDuration 输出毫秒级耗时的人类可读形式。
export function formatDuration(startedAt: string | null, finishedAt: string | null): string {
  if (!startedAt) {
    return "—";
  }
  const start = new Date(startedAt).getTime();
  const end = finishedAt ? new Date(finishedAt).getTime() : Date.now();
  if (Number.isNaN(start) || Number.isNaN(end) || end < start) {
    return "—";
  }
  const seconds = Math.round((end - start) / 1000);
  if (seconds < 60) {
    return `${seconds}s`;
  }
  return `${Math.floor(seconds / 60)}m ${seconds % 60}s`;
}

type Tone = "neutral" | "success" | "warning" | "danger" | "info";

// statusTone 将后端状态映射为徽章色调。
export function statusTone(status: string | null | undefined): Tone {
  switch (status) {
    case "completed":
    case "published":
    case "enabled":
    case "active":
    case "success":
    case "approved":
      return "success";
    case "running":
    case "queued":
    case "pending":
    case "draft":
    case "waiting_approval":
    case "reconciliation_required":
    case "planning":
    case "executing":
      return "info";
    case "failed":
    case "cancelled":
    case "disabled":
    case "rejected":
    case "denied":
    case "deleted":
    case "unknown":
      return "danger";
    case "archived":
    case "partial":
    case "paused":
    case "skipped":
      return "warning";
    default:
      return "neutral";
  }
}

// statusLabel 输出后端状态的中文文案，未知状态原样展示。
export function statusLabel(status: string | null | undefined): string {
  if (!status) {
    return "未知";
  }
  const labels: Record<string, string> = {
    active: "活跃",
    archived: "已归档",
    deleted: "已删除",
    draft: "草稿",
    published: "已发布",
    disabled: "已停用",
    enabled: "启用",
    running: "执行中",
    queued: "排队中",
    pending: "待处理",
    waiting_approval: "等待审批",
    completed: "已完成",
    succeeded: "成功",
    failed: "失败",
    cancelled: "已取消",
    reconciliation_required: "待人工对账",
    approved: "已批准",
    denied: "已拒绝",
    rejected: "已拒绝",
    created: "已创建",
    processing: "处理中",
    retrying: "重试中",
    dead: "死信",
    planning: "规划中",
    paused: "已暂停",
    skipped: "已跳过",
    unknown: "待对账",
    executing: "执行中",
    healthy: "健康",
    degraded: "失败累积",
    half_open: "半开探测",
    open: "熔断打开",
  };
  return labels[status] ?? status;
}
