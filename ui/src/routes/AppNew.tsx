import { useEffect, useState } from "react";
import { Link, useNavigate, useParams } from "react-router";
import { api } from "../api/client";
import type { components } from "../api/schema";

type CloudAccount = components["schemas"]["CloudAccount"];
type Installation = components["schemas"]["GithubInstallation"];
type Repository = components["schemas"]["GithubRepository"];

// AppNew is the step-by-step registration flow:
//   1. Pick a connected GitHub installation.
//   2. Pick a repo accessible to that installation.
//   3. Pick a connected AWS account.
//   4. Name the app and submit.
//
// Each step gates the next so we never offer the user a choice that
// can't lead to a valid app — e.g. we don't show repo pickers for an
// installation we haven't fetched yet, and we don't show AWS accounts
// in any state but `connected`.
export function AppNew() {
  const navigate = useNavigate();
  const { orgSlug } = useParams();

  const [installations, setInstallations] = useState<Installation[] | null>(
    null,
  );
  const [accounts, setAccounts] = useState<CloudAccount[] | null>(null);

  const [installationId, setInstallationId] = useState<string>("");
  const [repos, setRepos] = useState<Repository[] | null>(null);
  const [reposLoading, setReposLoading] = useState(false);
  const [reposError, setReposError] = useState<string | null>(null);

  const [repoFullName, setRepoFullName] = useState<string>("");
  const [cloudAccountId, setCloudAccountId] = useState<string>("");
  const [name, setName] = useState("");

  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // Load installations + cloud accounts on mount.
  useEffect(() => {
    if (!orgSlug) return;
    void (async () => {
      const inst = await api.GET("/api/orgs/{slug}/github/installations", {
        params: { path: { slug: orgSlug } },
      });
      if (!inst.error && inst.data) setInstallations(inst.data.installations);

      const aws = await api.GET("/api/orgs/{slug}/aws/accounts", {
        params: { path: { slug: orgSlug } },
      });
      if (!aws.error && aws.data) setAccounts(aws.data.accounts);
    })();
  }, [orgSlug]);

  // Load repos whenever the user picks a different installation.
  useEffect(() => {
    if (!orgSlug || !installationId) {
      setRepos(null);
      return;
    }
    const inst = installations?.find((i) => i.id === installationId);
    if (!inst) return;

    setReposLoading(true);
    setReposError(null);
    setRepos(null);
    void (async () => {
      const { data, error } = await api.GET(
        "/api/orgs/{slug}/github/installations/{installationId}/repositories",
        {
          params: {
            path: { slug: orgSlug, installationId: inst.installation_id },
          },
        },
      );
      setReposLoading(false);
      if (error || !data) {
        setReposError(extractMessage(error) ?? "Failed to load repositories");
        return;
      }
      setRepos(data.repositories);
    })();
  }, [orgSlug, installationId, installations]);

  const connectedAccounts =
    accounts?.filter((a) => a.status === "connected") ?? [];
  const canSubmit =
    !!name &&
    !!installationId &&
    !!repoFullName &&
    !!cloudAccountId &&
    !busy;

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    if (!orgSlug || !canSubmit) return;
    setBusy(true);
    setError(null);
    const { data, error } = await api.POST("/api/orgs/{slug}/apps", {
      params: { path: { slug: orgSlug } },
      body: {
        name,
        cloud_account_id: cloudAccountId,
        github_installation_id: installationId,
        github_repo_full_name: repoFullName,
      },
    });
    setBusy(false);
    if (error || !data) {
      setError(extractMessage(error) ?? "Failed to create app");
      return;
    }
    navigate(`/${orgSlug}/apps/${data.slug}`);
  }

  return (
    <>
      <div className="flex items-start justify-between gap-4">
        <div>
          <h1 className="text-3xl font-bold tracking-tight">New App</h1>
          <p className="mt-2 text-sm text-gray-600">
            Pick a repo and an AWS account; we'll build images into ECR there.
          </p>
        </div>
        <Link
          to={`/${orgSlug}/apps`}
          className="border border-gray-300 bg-white px-4 py-2 text-sm font-medium text-gray-700 shadow-sm hover:bg-gray-50"
        >
          Cancel
        </Link>
      </div>

      <form onSubmit={submit} className="mt-6 max-w-xl space-y-6">
        <Section title="1. GitHub installation">
          {installations === null ? (
            <p className="text-sm text-gray-500">Loading…</p>
          ) : installations.length === 0 ? (
            <NeedSetup
              kind="GitHub"
              href={`/${orgSlug}/integrations/github`}
              cta="Connect a GitHub App installation"
            />
          ) : (
            <select
              value={installationId}
              onChange={(e) => {
                setInstallationId(e.target.value);
                setRepoFullName("");
              }}
              className="block w-full border border-gray-300 px-3 py-2 text-sm shadow-sm focus:border-indigo-500 focus:outline-none"
              required
            >
              <option value="">Select an installation…</option>
              {installations.map((i) => (
                <option key={i.id} value={i.id}>
                  {i.account_login}
                </option>
              ))}
            </select>
          )}
        </Section>

        <Section title="2. Repository">
          {!installationId ? (
            <p className="text-sm text-gray-500">
              Pick an installation first.
            </p>
          ) : reposLoading ? (
            <p className="text-sm text-gray-500">Loading repositories…</p>
          ) : reposError ? (
            <p className="bg-red-50 p-2 text-sm text-red-700">{reposError}</p>
          ) : repos && repos.length === 0 ? (
            <p className="text-sm text-gray-500">
              That installation has no repositories.
            </p>
          ) : repos ? (
            <select
              value={repoFullName}
              onChange={(e) => setRepoFullName(e.target.value)}
              className="block w-full border border-gray-300 px-3 py-2 text-sm shadow-sm focus:border-indigo-500 focus:outline-none"
              required
            >
              <option value="">Select a repository…</option>
              {repos.map((r) => (
                <option key={r.id} value={r.full_name}>
                  {r.full_name} ({r.default_branch})
                </option>
              ))}
            </select>
          ) : null}
        </Section>

        <Section title="3. AWS account">
          {accounts === null ? (
            <p className="text-sm text-gray-500">Loading…</p>
          ) : connectedAccounts.length === 0 ? (
            <NeedSetup
              kind="AWS"
              href={`/${orgSlug}/integrations/aws`}
              cta="Connect an AWS account"
            />
          ) : (
            <select
              value={cloudAccountId}
              onChange={(e) => setCloudAccountId(e.target.value)}
              className="block w-full border border-gray-300 px-3 py-2 text-sm shadow-sm focus:border-indigo-500 focus:outline-none"
              required
            >
              <option value="">Select an AWS account…</option>
              {connectedAccounts.map((a) => (
                <option key={a.id} value={a.id}>
                  {a.label}
                  {a.account_id ? ` · ${a.account_id}` : ""}
                  {a.region ? ` · ${a.region}` : ""}
                </option>
              ))}
            </select>
          )}
        </Section>

        <Section title="4. App name">
          <input
            value={name}
            onChange={(e) => setName(e.target.value)}
            placeholder="e.g. Web API"
            required
            maxLength={200}
            className="block w-full border border-gray-300 px-3 py-2 text-sm shadow-sm focus:border-indigo-500 focus:outline-none"
          />
          <p className="mt-1 text-xs text-gray-500">
            We'll generate a URL slug from this. Slugs are immutable.
          </p>
        </Section>

        {error && (
          <p className="bg-red-50 p-3 text-sm text-red-700">{error}</p>
        )}

        <button
          type="submit"
          disabled={!canSubmit}
          className="bg-indigo-600 px-4 py-2 text-sm font-medium text-white shadow-sm hover:bg-indigo-700 disabled:cursor-not-allowed disabled:opacity-50"
        >
          {busy ? "Creating…" : "Create app"}
        </button>
      </form>
    </>
  );
}

function Section({
  title,
  children,
}: {
  title: string;
  children: React.ReactNode;
}) {
  return (
    <div>
      <h2 className="text-sm font-semibold tracking-tight text-gray-900">
        {title}
      </h2>
      <div className="mt-2">{children}</div>
    </div>
  );
}

function NeedSetup({
  kind,
  href,
  cta,
}: {
  kind: string;
  href: string;
  cta: string;
}) {
  return (
    <div className="border border-yellow-200 bg-yellow-50 p-3 text-sm text-yellow-900">
      <p>No {kind} configured for this org yet.</p>
      <Link to={href} className="mt-2 inline-block underline">
        {cta} →
      </Link>
    </div>
  );
}

function extractMessage(err: unknown): string | null {
  if (err && typeof err === "object" && "message" in err) {
    const msg = (err as { message: unknown }).message;
    if (typeof msg === "string") return msg;
  }
  return null;
}
