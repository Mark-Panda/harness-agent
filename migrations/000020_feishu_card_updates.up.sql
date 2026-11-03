-- 000020：飞书可更新卡片的更新节流与去重。
-- 说明：card_updated_at 记录最近一次成功更新卡片的时间，跨副本限制更新频率；
-- card_fingerprint 记录最近一次成功写入卡片的标题与正文指纹，内容未变化时跳过更新，
-- 避免在高频 Run 状态下放大飞书卡片更新接口限流。

ALTER TABLE feishu_outbound_messages
  ADD COLUMN card_updated_at timestamptz,
  ADD COLUMN card_fingerprint text NOT NULL DEFAULT '';

COMMENT ON COLUMN feishu_outbound_messages.card_updated_at IS '最近一次卡片成功更新时间，用于限制更新频率';
COMMENT ON COLUMN feishu_outbound_messages.card_fingerprint IS '最近一次成功写入卡片的标题与正文指纹，用于跳过重复更新';
