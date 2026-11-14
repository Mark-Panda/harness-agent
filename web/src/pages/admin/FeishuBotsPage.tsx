// 管理员飞书机器人页：复用机器人管理组件，跨租户列出与维护全部机器人。
import FeishuBotManager from "../../components/FeishuBotManager";

export default function FeishuBotsPage() {
  return (
    <FeishuBotManager
      basePath="/api/v1/admin/feishu-bots"
      listPath="/api/v1/admin/feishu-bots"
      eyebrow="平台设置"
      title="飞书机器人"
      description="登记飞书应用机器人后，用户可在已发布的助手上绑定。一个机器人只能被一个助手绑定，绑定后仅助手所属用户可使用。"
      callbackHint
    />
  );
}
