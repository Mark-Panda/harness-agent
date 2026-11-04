// 应用路由：公开认证页 + 受保护工作台与管理后台。
import { BrowserRouter, Route, Routes } from "react-router-dom";
import { AdminRoute, AppShell, ProtectedRoute } from "./components/AppShell";
import { EmptyState } from "./components/ui";
import { AuthProvider } from "./lib/auth";
import AgentWizardPage from "./pages/AgentWizardPage";
import AgentsPage from "./pages/AgentsPage";
import AuditPage from "./pages/AuditPage";
import ChatPage from "./pages/ChatPage";
import HomePage from "./pages/HomePage";
import LoginPage from "./pages/LoginPage";
import RegisterPage from "./pages/RegisterPage";
import RunPage from "./pages/RunPage";
import SessionsPage from "./pages/SessionsPage";
import MCPsPage from "./pages/admin/MCPsPage";
import ModelsPage from "./pages/admin/ModelsPage";
import PoliciesPage from "./pages/admin/PoliciesPage";
import ProvidersPage from "./pages/admin/ProvidersPage";
import SkillsPage from "./pages/admin/SkillsPage";
import UsersPage from "./pages/admin/UsersPage";
import FeishuBotsPage from "./pages/admin/FeishuBotsPage";

export default function App() {
  return (
    <AuthProvider>
      <BrowserRouter>
        <Routes>
          <Route path="/login" element={<LoginPage />} />
          <Route path="/register" element={<RegisterPage />} />
          <Route
            element={
              <ProtectedRoute>
                <AppShell />
              </ProtectedRoute>
            }
          >
            <Route path="/" element={<HomePage />} />
            <Route path="/agents" element={<AgentsPage />} />
            <Route path="/agents/new" element={<AgentWizardPage />} />
            <Route path="/agents/:agentId/edit" element={<AgentWizardPage />} />
            <Route path="/sessions" element={<SessionsPage />} />
            <Route path="/sessions/:sessionId" element={<ChatPage />} />
            <Route path="/audit" element={<AuditPage />} />
            <Route path="/runs/:runId" element={<RunPage />} />
            <Route path="/admin/providers" element={<AdminRoute><ProvidersPage /></AdminRoute>} />
            <Route path="/admin/models" element={<AdminRoute><ModelsPage /></AdminRoute>} />
            <Route path="/admin/policies" element={<AdminRoute><PoliciesPage /></AdminRoute>} />
            <Route path="/admin/skills" element={<AdminRoute><SkillsPage /></AdminRoute>} />
            <Route path="/admin/mcps" element={<AdminRoute><MCPsPage /></AdminRoute>} />
            <Route path="/admin/feishu-bots" element={<AdminRoute><FeishuBotsPage /></AdminRoute>} />
            <Route path="/admin/users" element={<AdminRoute><UsersPage /></AdminRoute>} />
            <Route path="*" element={<EmptyState title="页面不存在" description="请从顶部导航重新进入所需功能。" />} />
          </Route>
        </Routes>
      </BrowserRouter>
    </AuthProvider>
  );
}
