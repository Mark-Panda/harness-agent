// 登录页：处理凭据校验、限流与错误提示，成功后进入工作台。
import { useState } from "react";
import { Link, Navigate, useNavigate } from "react-router-dom";
import { AuthFrame } from "../components/AuthFrame";
import { Button, Field, Input } from "../components/ui";
import { ApiError } from "../lib/api";
import { useAuth } from "../lib/auth";
import { describeError } from "../lib/errors";

export default function LoginPage() {
  const { status, login } = useAuth();
  const navigate = useNavigate();
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

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
    </AuthFrame>
  );
}
