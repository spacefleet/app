import { OrganizationSwitcher, useOrganization, UserButton } from "@clerk/react";
import {
  Boxes,
  Cloud,
  GitBranch,
  KeyRound,
  LayoutDashboard,
  PanelLeftClose,
  PanelLeftOpen,
} from "lucide-react";
import { useState } from "react";
import { Link, NavLink, Outlet } from "react-router";
import icon from "@/assets/spacefleet-icon.svg";
import { cn } from "@/lib/utils";

export function Layout() {
  const [collapsed, setCollapsed] = useState(false);
  const { organization } = useOrganization();
  const dashboardHref = organization ? `/${organization.slug}` : "/";

  return (
    <div className="flex h-screen flex-col bg-gray-50">
      <header className="flex h-14 shrink-0 items-center gap-4 bg-black px-4 text-white">
        <Link to="/" className="flex items-center" aria-label="Home">
          <img src={icon} alt="Spacefleet" className="h-7 w-7" />
        </Link>
        <OrganizationSwitcher
          hidePersonal
          afterSelectOrganizationUrl="/:slug"
          afterCreateOrganizationUrl="/:slug"
          appearance={{
            elements: {
              organizationSwitcherTrigger:
                "text-white! hover:bg-white/10!",
              organizationPreviewMainIdentifier__organizationSwitcherTrigger:
                "text-white!",
              organizationPreviewSecondaryIdentifier__organizationSwitcherTrigger:
                "text-white/70!",
              organizationSwitcherTriggerIcon: "text-white!",
            },
          }}
        />
        <div className="ml-auto flex items-center">
          <UserButton />
        </div>
      </header>

      <div className="flex min-h-0 flex-1">
        <aside
          className={cn(
            "flex shrink-0 flex-col border-r border-gray-200 bg-white transition-[width] duration-200 ease-in-out",
            collapsed ? "w-14" : "w-60",
          )}
        >
          <nav className="flex-1 space-y-1 p-2">
            <SidebarLink to={dashboardHref} icon={LayoutDashboard} label="Dashboard" collapsed={collapsed} end />
            {organization && (
              <>
                <SidebarLink
                  to={`/${organization.slug}/apps`}
                  icon={Boxes}
                  label="Apps"
                  collapsed={collapsed}
                />
                <SidebarLink
                  to={`/${organization.slug}/integrations/github`}
                  icon={GitBranch}
                  label="GitHub"
                  collapsed={collapsed}
                />
                <SidebarLink
                  to={`/${organization.slug}/integrations/aws`}
                  icon={Cloud}
                  label="AWS"
                  collapsed={collapsed}
                />
              </>
            )}
            <SidebarLink to="/account/tokens" icon={KeyRound} label="Account tokens" collapsed={collapsed} />
          </nav>
          <button
            type="button"
            onClick={() => setCollapsed((v) => !v)}
            className="flex h-10 items-center justify-center border-t border-gray-200 text-gray-500 hover:bg-gray-100 hover:text-gray-900"
            aria-label={collapsed ? "Expand sidebar" : "Collapse sidebar"}
            title={collapsed ? "Expand sidebar" : "Collapse sidebar"}
          >
            {collapsed ? <PanelLeftOpen className="h-4 w-4" /> : <PanelLeftClose className="h-4 w-4" />}
          </button>
        </aside>

        <main className="flex-1 overflow-auto p-8">
          <Outlet />
        </main>
      </div>
    </div>
  );
}

type SidebarLinkProps = {
  to: string;
  icon: React.ComponentType<{ className?: string }>;
  label: string;
  collapsed: boolean;
  end?: boolean;
};

function SidebarLink({ to, icon: Icon, label, collapsed, end }: SidebarLinkProps) {
  return (
    <NavLink
      to={to}
      end={end}
      title={collapsed ? label : undefined}
      className={({ isActive }) =>
        cn(
          "flex h-9 items-center gap-3 px-3 text-sm font-medium transition-colors",
          collapsed && "justify-center px-0",
          isActive
            ? "bg-gray-100 text-gray-900"
            : "text-gray-600 hover:bg-gray-100 hover:text-gray-900",
        )
      }
    >
      <Icon className="h-4 w-4 shrink-0" />
      {!collapsed && <span className="truncate">{label}</span>}
    </NavLink>
  );
}
