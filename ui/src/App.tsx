import { Route, Routes } from "react-router";
import { ApiAuthBinder } from "./components/ApiAuthBinder";
import { Layout } from "./components/Layout";
import { RequireAuth } from "./components/RequireAuth";
import { RequireOrganization } from "./components/RequireOrganization";
import { AccountTokens } from "./routes/AccountTokens";
import { CliAuth } from "./routes/CliAuth";
import { Dashboard } from "./routes/Dashboard";
import { NotFound } from "./routes/NotFound";
import { RootRedirect } from "./routes/RootRedirect";
import { SelectOrganization } from "./routes/SelectOrganization";
import { SignInPage } from "./routes/SignInPage";
import { SignUpPage } from "./routes/SignUpPage";

export function App() {
  return (
    <Routes>
      <Route path="/sign-in/*" element={<SignInPage />} />
      <Route path="/sign-up/*" element={<SignUpPage />} />

      <Route element={<RequireAuth />}>
        <Route element={<ApiAuthBinder />}>
          <Route path="/select-org" element={<SelectOrganization />} />
          {/* CLI approval lives outside Layout — it's reached only from
              the CLI-generated URL and should be focused chrome. */}
          <Route path="/cli-auth" element={<CliAuth />} />

          <Route element={<Layout />}>
            <Route index element={<RootRedirect />} />
            <Route path="account/tokens" element={<AccountTokens />} />
            <Route path=":orgSlug" element={<RequireOrganization />}>
              <Route index element={<Dashboard />} />
            </Route>
            <Route path="*" element={<NotFound />} />
          </Route>
        </Route>
      </Route>
    </Routes>
  );
}
