-- 会话附件：用户上传的文件元数据与二进制内容，按 user_id 与会话隔离。
CREATE TABLE IF NOT EXISTS attachments (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  session_id uuid NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  message_id uuid REFERENCES messages(id) ON DELETE SET NULL,
  file_name text NOT NULL,
  mime_type text NOT NULL DEFAULT 'application/octet-stream',
  size_bytes bigint NOT NULL,
  sha256 text NOT NULL DEFAULT '',
  text_extracted boolean NOT NULL DEFAULT false,
  text_chars integer NOT NULL DEFAULT 0,
  text_truncated boolean NOT NULL DEFAULT false,
  created_at timestamptz NOT NULL DEFAULT now(),
  CHECK (btrim(file_name) <> ''),
  CHECK (octet_length(file_name) <= 1024),
  CHECK (size_bytes > 0 AND size_bytes <= 20971520),
  CHECK (text_chars >= 0)
);

-- 文件内容单独存放，避免消息列表等元数据查询误读大字段。
CREATE TABLE IF NOT EXISTS attachment_contents (
  attachment_id uuid PRIMARY KEY REFERENCES attachments(id) ON DELETE CASCADE,
  data bytea NOT NULL,
  text_content text NOT NULL DEFAULT '',
  CHECK (octet_length(data) <= 20971520)
);

ALTER TABLE attachments ENABLE ROW LEVEL SECURITY;
ALTER TABLE attachments FORCE ROW LEVEL SECURITY;
CREATE POLICY user_isolation ON attachments FOR ALL TO harness_app
  USING (user_id = nullif(current_setting('app.user_id', true), '')::uuid)
  WITH CHECK (user_id = nullif(current_setting('app.user_id', true), '')::uuid);

ALTER TABLE attachment_contents ENABLE ROW LEVEL SECURITY;
ALTER TABLE attachment_contents FORCE ROW LEVEL SECURITY;
CREATE POLICY content_owner ON attachment_contents FOR ALL TO harness_app
  USING (
    EXISTS (
      SELECT 1 FROM attachments
      WHERE attachments.id = attachment_contents.attachment_id
        AND attachments.user_id = nullif(current_setting('app.user_id', true), '')::uuid
    )
  )
  WITH CHECK (
    EXISTS (
      SELECT 1 FROM attachments
      WHERE attachments.id = attachment_contents.attachment_id
        AND attachments.user_id = nullif(current_setting('app.user_id', true), '')::uuid
    )
  );

GRANT SELECT, INSERT, UPDATE, DELETE ON attachments, attachment_contents TO harness_app;

COMMENT ON TABLE attachments IS '会话附件元数据：上传文件归属用户与会话，可绑定到一条消息';
COMMENT ON COLUMN attachments.message_id IS '绑定的消息 ID，未发送前为空';
COMMENT ON COLUMN attachments.text_extracted IS '是否已抽取可注入上下文的文本内容';
COMMENT ON TABLE attachment_contents IS '附件二进制内容与抽取文本，与元数据一对一';
COMMENT ON COLUMN attachment_contents.text_content IS '文本类文件抽取内容，供执行器注入上下文';
