// 前端共享类型：与后端 HTTP 出参保持一致，所有时间均为 ISO 字符串。

export type Role = "user" | "admin";
export type UserStatus = "active" | "disabled";

export interface User {
  id: string;
  username: string;
  role: Role;
  status: UserStatus;
}

// AdminUser 是管理员用户目录条目，包含账号时间信息。
export interface AdminUser {
  id: string;
  username: string;
  role: Role;
  status: UserStatus | "deleted";
  created_at: string;
  updated_at: string;
}

export interface AuthSession {
  user: User;
  access_token: string;
  token_type: string;
  access_expires_at: string;
  refresh_expires_at: string;
  csrf_token: string;
}

export type ProviderStatus = "enabled" | "disabled";

export interface Provider {
  id: string;
  name: string;
  provider_type: string;
  base_url: string;
  key_version: number;
  status: ProviderStatus;
  data_region: string;
  created_at: string;
  updated_at: string;
}

export interface ProviderInput {
  name: string;
  base_url: string;
  api_key: string;
  status: ProviderStatus;
  data_region: string;
}

export type ModelKind = "chat" | "embedding" | "rerank";
export type ModelStatus = "enabled" | "disabled";

export interface ModelCapabilities {
  streaming?: boolean;
  tools?: boolean;
  structured_output?: boolean;
}

export interface Model {
  id: string;
  provider_id: string;
  remote_model_name: string;
  kind: ModelKind;
  context_window_tokens: number;
  max_output_tokens: number;
  capabilities: ModelCapabilities;
  input_price_per_million: number;
  output_price_per_million: number;
  data_region: string;
  verification_source: string;
  verified_at: string | null;
  status: ModelStatus;
  created_at: string;
  updated_at: string;
}

// RemoteModel 是供应商站点返回的可用模型摘要。
export interface RemoteModel {
  id: string;
  owned_by: string;
}

export interface PolicyCandidate {
  model_id: string;
  priority?: number;
  max_attempts?: number;
}

export type PolicyStatus = "draft" | "published" | "disabled";

export interface ModelPolicy {
  id: string;
  name: string;
  version: number;
  min_context_tokens: number;
  rules: Record<string, unknown>;
  status: PolicyStatus;
  candidates: PolicyCandidate[];
  created_at: string;
  updated_at: string;
}

export type SkillStatus = "draft" | "published" | "disabled";

export interface Skill {
  id: string;
  name: string;
  version: number;
  description: string;
  prompt_template: string;
  parameters: Record<string, unknown>;
  status: SkillStatus;
  created_at: string;
  updated_at: string;
}

export type MCPStatus = "enabled" | "disabled";

export interface MCP {
  id: string;
  name: string;
  endpoint: string;
  auth_type: string;
  key_version: number;
  risk_level: string;
  status: MCPStatus;
  tool_timeouts: Record<string, number>;
  created_at: string;
  updated_at: string;
}

export interface MCPInput {
  name: string;
  endpoint: string;
  auth_type: string;
  credentials: string;
  risk_level: string;
  status: MCPStatus;
  tool_timeouts: Record<string, number>;
}

// MCPTool 是 MCP 服务暴露的单个工具能力。
export interface MCPTool {
  name: string;
  description: string;
  input_schema: Record<string, unknown> | null;
}

export type AgentMode = "react" | "plan" | "super";
export type AgentStatus = "draft" | "published" | "disabled" | "archived";

// FeishuBotStatus 飞书机器人启停状态。
export type FeishuBotStatus = "enabled" | "disabled";

// FeishuBot 管理员登记的飞书机器人；响应不包含应用密钥。
export interface FeishuBot {
  id: string;
  name: string;
  app_id: string;
  bot_open_id: string;
  key_version: string;
  status: FeishuBotStatus;
  bound_user_id: string | null;
  bound_username: string;
  bound_agent_id: string | null;
  bound_agent_name: string;
  bound_agent_version: number | null;
  created_at: string;
  updated_at: string;
}

// FeishuBotOption 是普通用户可见的机器人绑定选项。
export interface FeishuBotOption {
  id: string;
  name: string;
  bot_open_id: string;
  status: FeishuBotStatus;
  bound_agent_id: string | null;
  bound_agent_name: string;
  available: boolean;
}

// FeishuBotInput 是机器人创建/更新的写请求；密钥留空表示更新时保留。
export interface FeishuBotInput {
  name: string;
  app_id: string;
  app_secret: string;
  verification_token: string;
  encrypt_key: string;
  bot_open_id: string;
  status: FeishuBotStatus;
}

export interface Agent {
  id: string;
  user_id: string;
  name: string;
  description: string;
  mode: AgentMode;
  model_policy_id: string | null;
  model_policy_version: number | null;
  version: number;
  config_snapshot: Record<string, unknown> | null;
  skill_ids: string[];
  mcp_ids: string[];
  status: AgentStatus;
  created_at: string;
  updated_at: string;
}

export interface AgentInput {
  name: string;
  description: string;
  mode: AgentMode;
  model_policy_id: string | null;
  model_policy_version: number | null;
  skill_ids: string[];
  mcp_ids: string[];
}

export type SessionStatus = "active" | "archived" | "deleted";
export type SessionSource = "web" | "feishu" | "api";

export interface Session {
  id: string;
  agent_id: string | null;
  title: string;
  source: SessionSource;
  status: SessionStatus;
  version: number;
  last_message_at: string | null;
  created_at: string;
  updated_at: string;
}

export type MessageRole = "system" | "user" | "assistant" | "tool";

export interface Message {
  id: string;
  session_id: string;
  role: MessageRole;
  content: string;
  tool_calls: unknown[] | null;
  token_count: number | null;
  created_at: string;
}

export type RunStatus = string;

export interface Run {
  id: string;
  session_id: string;
  user_id?: string;
  agent_id: string | null;
  agent_version: number | null;
  mode: AgentMode;
  status: RunStatus;
  current_step_seq: number;
  attempt_no: number;
  config_snapshot: Record<string, unknown> | null;
  model_policy_snapshot: Record<string, unknown> | null;
  error_code: string | null;
  error_message: string | null;
  deadline_at: string | null;
  started_at: string | null;
  finished_at: string | null;
  created_at: string;
  updated_at: string;
}

// AdminRun 是管理员全局运行记录的展示条目，附带会话来源与发起用户。
export interface AdminRun extends Run {
  source: SessionSource;
  session_title: string;
  username: string;
}

// AdminAgent 是管理员全局助手视图条目，附带所有者用户名。
export interface AdminAgent {
  id: string;
  user_id: string;
  username: string;
  name: string;
  description: string;
  mode: AgentMode;
  status: AgentStatus;
  version: number;
  model_policy_id: string | null;
  model_policy_version: number | null;
  created_at: string;
  updated_at: string;
}

export interface ExecutionStep {
  id: string;
  run_id: string;
  step_seq: number;
  parent_step_id: string | null;
  kind: string;
  name: string;
  status: string;
  tool_name: string | null;
  attempt_no: number;
  risk_level: string | null;
  idempotency_key: string | null;
  error_code: string | null;
  started_at: string | null;
  finished_at: string | null;
}

export interface ExecutionPayload {
  id: string;
  run_id: string;
  step_id: string;
  type: string;
  content_json: Record<string, unknown> | null;
  content_text: string | null;
  capture_mode: string;
  truncated: boolean;
  partial: boolean;
  original_size_bytes: number;
  saved_size_bytes: number;
  created_at: string;
}

export interface ToolApproval {
  id: string;
  run_id: string;
  step_id: string;
  tool_call_id: string;
  tool_name: string;
  risk_level: string;
  status: string;
  arguments: Record<string, unknown> | null;
  idempotency_key: string | null;
  result: Record<string, unknown> | null;
  error_code: string | null;
  error_message: string | null;
  decided_by: string | null;
  decided_at: string | null;
  delivered_at: string | null;
  created_at: string;
  updated_at: string;
}

export interface ModelHealth {
  model_id: string;
  provider_id: string;
  remote_model_name: string;
  status: "healthy" | "degraded" | "half_open" | "open" | "unknown";
  failures: number;
  checked_at: string;
}

export interface ModelAttempt {
  id: string;
  run_id: string;
  step_id: string | null;
  attempt_no: number;
  provider_id: string | null;
  model_id: string | null;
  policy_version: number | null;
  status: "running" | "succeeded" | "failed" | "cancelled";
  provider_request_id: string;
  request_manifest: Record<string, unknown> | null;
  input_tokens: number;
  output_tokens: number;
  estimated_cost: number;
  duration_ms: number | null;
  finish_reason: string;
  switch_reason: string;
  error_class: string;
  error_message: string;
  created_at: string;
  completed_at: string | null;
}

export type PlanStatus = "planning" | "executing" | "waiting_approval" | "paused" | "completed" | "failed" | "cancelled";
export type PlanStepStatus = "pending" | "running" | "waiting_approval" | "completed" | "failed" | "skipped" | "cancelled" | "unknown";

export interface PlanStep {
  id: string;
  plan_id: string;
  step_no: number;
  title: string;
  description: string;
  depends_on: number[];
  tool_name: string;
  arguments: Record<string, unknown> | null;
  idempotency_key: string;
  status: PlanStepStatus;
  attempts: number;
  max_attempts: number;
  execution_step_id: string | null;
  result: Record<string, unknown> | null;
  error_code: string;
  error_message: string;
  started_at: string | null;
  finished_at: string | null;
}

export interface Plan {
  id: string;
  run_id: string;
  session_id: string;
  version: number;
  parent_plan_id: string | null;
  status: PlanStatus;
  goal: string;
  config_snapshot: Record<string, unknown> | null;
  max_steps: number;
  total_input_tokens: number;
  total_output_tokens: number;
  estimated_cost: number;
  deadline_at: string | null;
  error_code: string;
  error_message: string;
  revision: number;
  created_at: string;
  updated_at: string;
  finished_at: string | null;
  steps: PlanStep[];
}

export type SupervisorTaskStatus = "pending" | "running" | "completed" | "failed" | "cancelled" | "unknown";

export interface SupervisorTask {
  id: string;
  parent_run_id: string;
  child_session_id: string;
  worker_name: string;
  worker_index: number;
  task: string;
  tool_scope: string[];
  status: SupervisorTaskStatus;
  attempts: number;
  max_attempts: number;
  result: Record<string, unknown> | null;
  error_code: string;
  error_message: string;
  depth: number;
  started_at: string | null;
  finished_at: string | null;
  created_at: string;
}

export interface DeletionRequest {
  id: string;
  resource_type: string;
  resource_id: string;
  status: string;
  requested_source: string;
  attempts: number;
  max_attempts: number;
  last_error?: string;
  created_at: string;
  completed_at?: string | null;
}

export type SSEPayload = Record<string, unknown>;

export interface RunEvent {
  seq: number;
  event: string;
  data: SSEPayload;
}

export interface Page<T> {
  items: T[];
  next_cursor: string | null;
}
