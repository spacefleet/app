import { RedirectToSignIn, useAuth } from "@clerk/react";
import { Outlet } from "react-router";

export function RequireAuth() {
  const { isLoaded, isSignedIn } = useAuth();
  if (!isLoaded) return null;
  if (!isSignedIn) return <RedirectToSignIn />;
  return <Outlet />;
}
