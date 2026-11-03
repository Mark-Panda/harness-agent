// 注册页：创建账号并自动登录，服务端默认分配普通用户角色。
import { useState } from "react";
import { Link, Navigate, useNavigate } from "react-router-dom";
import { AuthFrame } from "../components/AuthFrame";
import { Button, Field, Input } from "../components/ui";
import { useAuth } from "../lib/auth";
import { describeError } from "../lib/errors";

export default function RegisterPage() {
  const { status, register } = useAuth();
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
      await register(username.trim(), password);
      navigate("/");
    } catch (submitError) {
      setError(describeError(submitError));
    } finally {
      setBusy(false);
    }
  };

  return (
    <AuthFrame title="注册账号" description="用户名 3–64 位，密码至少 12 位。注册后将自动登录。" footer={<p className="text-xs text-muted">已有账号？<Link to="/login" className="text-accent underline">登录</Link></p>}>
      <form className="flex flex-col gap-4" onSubmit={submit}>
        <Field label="用户名" hint="3–64 位字符，将统一转为小写。">
          <Input autoComplete="username" value={username} onChange={(event) => setUsername(event.target.value)} required minLength={3} maxLength={64} />
        </Field>
        <Field label="密码" hint="至少 12 位，建议使用长随机口令。">
          <Input type="password" autoComplete="new-password" value={password} onChange={(event) => setPassword(event.target.value)} required minLength={12} />
        </Field>
        {error ? (
          <p role="alert" className="rounded-lg border border-rust bg-rust-soft px-3 py-2 text-xs text-rust">
            {error}
          </p>
        ) : null}
        <Button type="submit" loading={busy} disabled={username.trim().length < 3 || password.length < 12}>
          创建账号
        </Button>
      </form>
    </AuthFrame>
  );
}
