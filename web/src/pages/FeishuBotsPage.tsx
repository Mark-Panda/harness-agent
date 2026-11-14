// 租户飞书机器人页：普通用户自助登记与维护归属自己的机器人。
import FeishuBotManager from "../components/FeishuBotManager";

export default function FeishuBotsPage() {
  return (
    <FeishuBotManager
      basePath="/api/v1/feishu/bots"
      listPath="/api/v1/feishu/my-bots"
      eyebrow="接入"
      title="我的飞书机器人"
      description="登记自己的飞书应用机器人，把它绑定到已发布的助手上，即可在飞书单聊或群聊中对话。只有你能管理和绑定这些机器人。"
      createLabel="新增机器人"
      emptyDescription="新增一个飞书应用机器人，绑定助手后就能在飞书中与它对话。"
    />
  );
}
