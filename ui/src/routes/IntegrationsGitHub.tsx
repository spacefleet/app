import { useEffect, useState } from "react";
import { useParams } from "react-router";
import { api } from "../api/client";
import type { components } from "../api/schema";

type Installation = components["schemas"]["GithubInstallation"];
type Repository = components["schemas"]["GithubRepository"];

// IntegrationsGitHub is the per-org page for connecting and inspecting
// GitHub App installations. Connecting kicks the user out to github.com;
// the install completes via the standalone /integrations/github/callback
// route and lands them back here.
export function IntegrationsGitHub() {
  const { orgSlug } = useParams();
  const [installations, setInstallations] = useState<Installation[] | null>(
    null,
  );
  const [error, setError] = useState<string | null>(null);
  const [connecting, setConnecting] = useState(false);

  async function refresh() {
    if (!orgSlug) return;
    const { data, error } = await api.GET(
      "/api/orgs/{slug}/github/installations",
      { params: { path: { slug: orgSlug } } },
    );
    if (error || !data) {
      setError("Failed to load installations");
      return;
    }
    setInstallations(data.installations);
  }

  useEffect(() => {
    void refresh();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [orgSlug]);

  async function connect() {
    if (!orgSlug) return;
    setConnecting(true);
    setError(null);
    const { data, error } = await api.POST(
      "/api/orgs/{slug}/github/installations/start",
      { params: { path: { slug: orgSlug } } },
    );
    setConnecting(false);
    if (error || !data) {
      setError(extractMessage(error) ?? "Failed to start install");
      return;
    }
    window.location.assign(data.url);
  }

  async function disconnect(installationId: number) {
    if (!orgSlug) return;
    await api.DELETE(
      "/api/orgs/{slug}/github/installations/{installationId}",
      {
        params: { path: { slug: orgSlug, installationId } },
      },
    );
    void refresh();
  }

  return (
    <>
      <h1 className="text-3xl font-bold tracking-tight">GitHub</h1>
      <p className="mt-2 text-sm text-gray-600">
        Connect a GitHub App installation so Spacefleet can fetch source for
        builds. Tokens are minted on demand and never stored.
      </p>

      {error && (
        <p className="mt-4 bg-red-50 p-3 text-sm text-red-700">{error}</p>
      )}

      <div className="mt-6">
        <button
          onClick={connect}
          disabled={connecting}
          className="bg-indigo-600 px-4 py-2 text-sm font-medium text-white shadow-sm hover:bg-indigo-700 disabled:cursor-not-allowed disabled:opacity-50"
        >
          {connecting ? "Opening GitHub…" : "Connect GitHub"}
        </button>
      </div>

      {installations === null ? (
        <p className="mt-6 text-sm text-gray-500">Loading…</p>
      ) : installations.length === 0 ? (
        <p className="mt-6 text-sm text-gray-500">
          No installations yet. Connecting takes you to GitHub to pick which
          repositories to grant access to.
        </p>
      ) : (
        <ul className="mt-6 divide-y divide-gray-200 border border-gray-200">
          {installations.map((inst) => (
            <InstallationItem
              key={inst.id}
              orgSlug={orgSlug!}
              installation={inst}
              onDisconnect={() => disconnect(inst.installation_id)}
            />
          ))}
        </ul>
      )}
    </>
  );
}

function InstallationItem({
  orgSlug,
  installation,
  onDisconnect,
}: {
  orgSlug: string;
  installation: Installation;
  onDisconnect: () => void;
}) {
  const [open, setOpen] = useState(false);
  const [repos, setRepos] = useState<Repository[] | null>(null);
  const [loadingRepos, setLoadingRepos] = useState(false);
  const [reposError, setReposError] = useState<string | null>(null);

  async function toggle() {
    if (open) {
      setOpen(false);
      return;
    }
    setOpen(true);
    if (repos !== null) return;
    setLoadingRepos(true);
    setReposError(null);
    const { data, error } = await api.GET(
      "/api/orgs/{slug}/github/installations/{installationId}/repositories",
      {
        params: {
          path: { slug: orgSlug, installationId: installation.installation_id },
        },
      },
    );
    setLoadingRepos(false);
    if (error || !data) {
      setReposError(extractMessage(error) ?? "Failed to load repositories");
      return;
    }
    setRepos(data.repositories);
  }

  return (
    <li className="p-4">
      <div className="flex items-center justify-between gap-4">
        <div className="min-w-0">
          <p className="truncate font-medium">
            {installation.account_login}{" "}
            <span className="text-xs font-normal text-gray-500">
              ({installation.account_type})
            </span>
          </p>
          <p className="text-xs text-gray-500">
            Installation #{installation.installation_id} · Connected{" "}
            {formatDate(installation.created_at)}
          </p>
          {installation.suspended_at && (
            <p className="mt-1 text-xs font-medium text-red-600">
              Suspended on GitHub since{" "}
              {formatDate(installation.suspended_at)}
            </p>
          )}
        </div>
        <div className="flex shrink-0 gap-2">
          <button
            onClick={toggle}
            className="border border-gray-300 bg-white px-3 py-1.5 text-sm font-medium text-gray-700 shadow-sm hover:bg-gray-50"
          >
            {open ? "Hide repos" : "Show repos"}
          </button>
          <button
            onClick={onDisconnect}
            className="border border-gray-300 bg-white px-3 py-1.5 text-sm font-medium text-gray-700 shadow-sm hover:bg-gray-50"
          >
            Disconnect
          </button>
        </div>
      </div>

      {open && (
        <div className="mt-3 border-t border-gray-200 pt-3">
          {loadingRepos ? (
            <p className="text-sm text-gray-500">Loading repositories…</p>
          ) : reposError ? (
            <p className="bg-red-50 p-2 text-sm text-red-700">{reposError}</p>
          ) : repos === null ? null : repos.length === 0 ? (
            <p className="text-sm text-gray-500">
              No repositories. Pick some on GitHub and reconnect.
            </p>
          ) : (
            <ul className="space-y-1 text-sm">
              {repos.map((r) => (
                <li
                  key={r.id}
                  className="flex items-center justify-between gap-3"
                >
                  <a
                    href={r.html_url}
                    target="_blank"
                    rel="noreferrer"
                    className="truncate font-mono text-indigo-700 hover:underline"
                  >
                    {r.full_name}
                  </a>
                  <span className="shrink-0 text-xs text-gray-500">
                    {r.private ? "private" : "public"} · {r.default_branch}
                  </span>
                </li>
              ))}
            </ul>
          )}
        </div>
      )}
    </li>
  );
}

function extractMessage(err: unknown): string | null {
  if (err && typeof err === "object" && "message" in err) {
    const msg = (err as { message: unknown }).message;
    if (typeof msg === "string") return msg;
  }
  return null;
}

function formatDate(iso: string) {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  return d.toLocaleString();
}
