import { OrganizationList } from "@clerk/react";

export function SelectOrganization() {
  return (
    <div className="flex min-h-screen items-center justify-center bg-gray-50 p-4">
      <OrganizationList
        hidePersonal
        afterSelectOrganizationUrl="/:slug"
        afterCreateOrganizationUrl="/:slug"
      />
    </div>
  );
}
