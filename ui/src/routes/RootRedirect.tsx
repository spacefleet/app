import { useOrganization, useOrganizationList } from "@clerk/react";
import { Navigate } from "react-router";

// Root path: send the signed-in user to their active org, or to /select-org
// if they don't have one yet.
export function RootRedirect() {
  const { organization, isLoaded: orgLoaded } = useOrganization();
  const { isLoaded: listLoaded, userMemberships } = useOrganizationList({
    userMemberships: true,
  });

  if (!orgLoaded || !listLoaded || userMemberships.isLoading) return null;

  if (organization?.slug) {
    return <Navigate to={`/${organization.slug}`} replace />;
  }

  const firstSlug = userMemberships.data?.[0]?.organization.slug;
  if (firstSlug) return <Navigate to={`/${firstSlug}`} replace />;

  return <Navigate to="/select-org" replace />;
}
