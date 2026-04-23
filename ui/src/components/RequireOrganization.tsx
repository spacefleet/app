import { useOrganization, useOrganizationList } from "@clerk/react";
import { useEffect } from "react";
import { Navigate, Outlet, useParams } from "react-router";

// Treats the `:orgSlug` URL segment as the source of truth for the active
// organization. Syncs Clerk's active org to match (so session claims are
// correct) and redirects to /select-org if the user isn't a member.
export function RequireOrganization() {
  const { orgSlug } = useParams();
  const {
    isLoaded: listLoaded,
    userMemberships,
    setActive,
  } = useOrganizationList({ userMemberships: true });
  const { organization, isLoaded: orgLoaded } = useOrganization();

  const membership = userMemberships.data?.find(
    (m) => m.organization.slug === orgSlug,
  );

  useEffect(() => {
    if (!listLoaded || !setActive || !membership) return;
    if (organization?.id === membership.organization.id) return;
    void setActive({ organization: membership.organization.id });
  }, [listLoaded, setActive, membership, organization?.id]);

  if (!listLoaded || !orgLoaded || userMemberships.isLoading) return null;
  if (!membership) return <Navigate to="/select-org" replace />;
  if (organization?.id !== membership.organization.id) return null;
  return <Outlet />;
}
