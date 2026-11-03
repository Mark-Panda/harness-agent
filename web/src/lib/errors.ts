// 错误文案映射：将后端稳定错误码转换为用户可读提示。
import { ApiError } from "./api";

// describeError 输出通用错误提示。
export function describeError(error: unknown): string {
  if (error instanceof ApiError) {
    switch (error.code) {
      case "unauthorized":
        return "登录状态已失效，请重新登录。";
      case "user_already_exists":
        return "用户名已被占用。";
      case "invalid_credentials":
        return "用户名或密码不符合要求：用户名 3–64 位，密码至少 12 位。";
      case "rate_limited":
        return "请求过于频繁，请稍后重试。";
      case "rate_limit_unavailable":
        return "限流服务暂不可用，请稍后重试。";
      case "forbidden":
        return "当前账号无权执行该操作。";
      case "quota_exceeded":
        return "已超出账号配额，请先清理不再使用的资源。";
      case "budget_exceeded":
        return "已超出预算上限，请调整预算或稍后再试。";
      case "resource_not_found":
        return "资源不存在或已被删除。";
      case "resource_conflict":
        return "资源状态冲突，请刷新后重试。";
      case "invalid_configuration":
        return "配置不合规，请检查必填项与取值。";
      case "csrf_token_missing":
      case "csrf_token_invalid":
        return "安全校验失败，请刷新页面后重试。";
      default:
        return `请求失败（${error.code}）。`;
    }
  }
  if (error instanceof TypeError) {
    return "网络连接失败，请检查网络或稍后重试。";
  }
  return "请求失败，请稍后重试。";
}
