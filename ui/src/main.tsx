import { ClerkProvider } from "@clerk/react";
import { ui } from "@clerk/ui";
import React from "react";
import ReactDOM from "react-dom/client";
import { BrowserRouter, useNavigate } from "react-router";
import { App } from "./App";
import "./index.css";

function ClerkProviderWithRouter({ children }: { children: React.ReactNode }) {
  const navigate = useNavigate();
  return (
    <ClerkProvider
      publishableKey={window.appConfig.clerkPublishableKey}
      ui={ui}
      routerPush={(to) => navigate(to)}
      routerReplace={(to) => navigate(to, { replace: true })}
      appearance={{
        variables: { borderRadius: "0" },
        options: { shimmer: false },
        elements: {
          avatarBox: "rounded-none!",
          userButtonAvatarBox: "rounded-none!",
          organizationPreviewAvatarBox: "rounded-none!",
          organizationSwitcherTriggerAvatarBox: "rounded-none!",
        },
      }}
    >
      {children}
    </ClerkProvider>
  );
}

ReactDOM.createRoot(document.getElementById("root")!).render(
  <React.StrictMode>
    <BrowserRouter>
      <ClerkProviderWithRouter>
        <App />
      </ClerkProviderWithRouter>
    </BrowserRouter>
  </React.StrictMode>,
);
