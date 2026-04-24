import { useOrganization, useOrganizationList } from "@clerk/react";
import { useEffect } from "react";
import { Navigate, Outlet, useParams } from "react-router";

// Treats the `:orgSlug` URL segment as the source of truth for the active
// organization. Syncs Clerk's active org to match (so session claims are
// correct) and redirects to /select-org if the user isn't a member.
export function RequireOrganization() {
  const { orgSlug } = useParams();
  const { organization, isLoaded: orgLoaded } = useOrganization();
  const {
    isLoaded: listLoaded,
    userMemberships,
    setActive,
  } = useOrganizationList({ userMemberships: { infinite: true } });

  const membership = userMemberships.data?.find(
    (m) => m.organization.slug === orgSlug,
  );

  // Sync Clerk's active org to match the URL once we've located the
  // matching membership. Effect rather than render-time call so we don't
  // trigger a state update during render.
  useEffect(() => {
    if (!setActive || !membership) return;
    if (organization?.id === membership.organization.id) return;
    void setActive({ organization: membership.organization.id });
  }, [setActive, membership, organization?.id]);

  // Happy path: Clerk's active org already matches the URL. Trust it and
  // render immediately — don't wait on the memberships list (which may
  // trail the session or, on some SDK versions, never populate at all).
  if (orgLoaded && organization?.slug === orgSlug) return <Outlet />;

  if (!listLoaded || !orgLoaded || userMemberships.isLoading) return null;
  if (!membership) return <Navigate to="/select-org" replace />;
  if (organization?.id !== membership.organization.id) return null;
  return <Outlet />;
}
