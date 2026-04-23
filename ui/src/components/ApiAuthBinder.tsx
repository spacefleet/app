import { useAuth } from "@clerk/react";
import { useEffect } from "react";
import { Outlet } from "react-router";
import { setAuthTokenProvider } from "../api/client";

// Wires Clerk's getToken into the openapi-fetch client so API calls carry a
// valid Bearer token. Mount this inside <ClerkProvider>; it doesn't render
// any UI of its own.
export function ApiAuthBinder() {
  const { getToken } = useAuth();

  useEffect(() => {
    setAuthTokenProvider(() => getToken());
    return () => setAuthTokenProvider(null);
  }, [getToken]);

  return <Outlet />;
}
