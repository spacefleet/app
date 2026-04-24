import { useAuth } from "@clerk/react";
import { Outlet } from "react-router";
import { setAuthTokenProvider } from "../api/client";

// Wires Clerk's getToken into the openapi-fetch client so API calls carry
// a valid Bearer token. The provider is installed during render — not in
// useEffect — so it's in place before any descendant's mount effects fire.
// Child effects run before parent effects in React, and useEffect-based
// wiring would race the first protected API call from any child route.
export function ApiAuthBinder() {
  const { getToken } = useAuth();
  setAuthTokenProvider(() => getToken());
  return <Outlet />;
}
