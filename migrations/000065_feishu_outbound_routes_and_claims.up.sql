-- Preserve the verified sender independently of the bot registry's lifetime.
-- Existing app-only rows cannot identify the bot that accepted the event.
ALTER TABLE feishu_outbound_messages
  ADD COLUMN bot_id uuid,
  ADD COLUMN default_route boolean NOT NULL DEFAULT false,
  ADD COLUMN fencing_token bigint NOT NULL DEFAULT 0,
  ADD CONSTRAINT feishu_outbound_route_check CHECK (bot_id IS NULL OR (NOT default_route AND app_id <> ''));
