// 登录页：处理凭据校验、限流与错误提示，成功后进入工作台。
import { useEffect, useState } from "react";
import { Link, Navigate, useNavigate } from "react-router-dom";
import { AuthFrame } from "../components/AuthFrame";
import { Button, Field, Input } from "../components/ui";
import { ApiError, requestJSON } from "../lib/api";
import { useAuth } from "../lib/auth";
import { describeError } from "../lib/errors";

// feishuErrorMessage 把回调错误码翻译成用户可读提示。
function feishuErrorMessage(code: string): string {
  switch (code) {
    case "state":
      return "飞书登录校验已过期，请重新发起登录。";
    case "code":
      return "飞书未返回授权码，请重试。";
    case "unavailable":
      return "当前环境未开放飞书登录。";
    default:
      return "飞书登录失败，请稍后重试或使用账号密码登录。";
  }
}

export default function LoginPage() {
  const { status, login } = useAuth();
  const navigate = useNavigate();
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState<string | null>(() => {
    const code = new URLSearchParams(window.location.search).get("feishu_error");
    return code ? feishuErrorMessage(code) : null;
  });
  const [busy, setBusy] = useState(false);
  const [feishuEnabled, setFeishuEnabled] = useState(false);

  // 登录入口按平台配置显示，未配置飞书应用时不展示无效按钮。
  useEffect(() => {
    let cancelled = false;
    requestJSON<{ enabled: boolean }>("/api/v1/auth/feishu/status")
      .then((result) => {
        if (!cancelled) {
          setFeishuEnabled(Boolean(result.enabled));
        }
      })
      .catch(() => {
        // 状态读取失败时保持账号密码登录可用。
      });
    return () => {
      cancelled = true;
    };
  }, []);

  if (status === "authenticated") {
    return <Navigate to="/" replace />;
  }

  const submit = async (event: React.FormEvent) => {
    event.preventDefault();
    setError(null);
    setBusy(true);
    try {
      await login(username.trim(), password);
      navigate("/");
    } catch (submitError) {
      if (submitError instanceof ApiError && submitError.status === 401) {
        setError("用户名或密码错误。");
      } else {
        setError(describeError(submitError));
      }
    } finally {
      setBusy(false);
    }
  };

  return (
    <AuthFrame title="登录" description="使用账号密码登录，继续你的助手工作。" footer={<p className="text-xs text-muted">还没有账号？<Link to="/register" className="text-accent underline">注册</Link></p>}>
      <form className="flex flex-col gap-4" onSubmit={submit}>
        <Field label="用户名">
          <Input autoComplete="username" value={username} onChange={(event) => setUsername(event.target.value)} required minLength={3} maxLength={64} />
        </Field>
        <Field label="密码">
          <Input type="password" autoComplete="current-password" value={password} onChange={(event) => setPassword(event.target.value)} required minLength={12} />
        </Field>
        {error ? (
          <p role="alert" className="rounded-lg border border-rust bg-rust-soft px-3 py-2 text-xs text-rust">
            {error}
          </p>
        ) : null}
        <Button type="submit" loading={busy} disabled={!username || password.length < 12}>
          登录
        </Button>
      </form>
      {feishuEnabled ? (
        <>
          <div className="flex items-center gap-3 text-[11px] text-muted">
            <span className="h-px flex-1 bg-line" />
            或
            <span className="h-px flex-1 bg-line" />
          </div>
          <a
            href="/api/v1/auth/feishu/start"
            className="inline-flex h-10 w-full items-center justify-center rounded-[3px] border border-line-strong bg-paper-raised px-4 text-sm text-ink transition hover:border-accent hover:text-accent"
          >
            使用飞书登录
          </a>
        </>
      ) : null}
    </AuthFrame>
  );
}
