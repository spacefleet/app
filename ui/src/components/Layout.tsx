import { OrganizationSwitcher, UserButton } from "@clerk/react";
import { Link, Outlet } from "react-router";

export function Layout() {
  return (
    <div className="min-h-screen bg-white">
      <header className="border-b border-gray-200">
        <nav className="mx-auto flex max-w-4xl items-center gap-3 p-4">
          <Link to="/" className="mr-auto font-semibold tracking-tight">
            Spacefleet
          </Link>
          <OrganizationSwitcher
            hidePersonal
            afterSelectOrganizationUrl="/:slug"
            afterCreateOrganizationUrl="/:slug"
          />
          <UserButton />
        </nav>
      </header>
      <main className="mx-auto max-w-4xl p-8 font-sans">
        <Outlet />
      </main>
    </div>
  );
}
