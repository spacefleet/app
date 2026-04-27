import { useEffect, useState } from "react";
import { Link, useParams } from "react-router";
import { api } from "../api/client";
import type { components } from "../api/schema";

type App = components["schemas"]["App"];

// Apps lists every app registered for the current org. The list is the
// landing page for picking an app to view; the "New App" button drops
// the user into the registration flow at /:org/apps/new.
export function Apps() {
  const { orgSlug } = useParams();
  const [apps, setApps] = useState<App[] | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (!orgSlug) return;
    void (async () => {
      const { data, error } = await api.GET("/api/orgs/{slug}/apps", {
        params: { path: { slug: orgSlug } },
      });
      if (error || !data) {
        setError(extractMessage(error) ?? "Failed to load apps");
        return;
      }
      setApps(data.apps);
    })();
  }, [orgSlug]);

  return (
    <>
      <div className="flex items-start justify-between gap-4">
        <div>
          <h1 className="text-3xl font-bold tracking-tight">Apps</h1>
          <p className="mt-2 text-sm text-gray-600">
            Each app is one repo + Dockerfile + AWS account. Builds produce a
            Docker image in the customer's ECR.
          </p>
        </div>
        <Link
          to={`/${orgSlug}/apps/new`}
          className="bg-indigo-600 px-4 py-2 text-sm font-medium text-white shadow-sm hover:bg-indigo-700"
        >
          New App
        </Link>
      </div>

      {error && (
        <p className="mt-4 bg-red-50 p-3 text-sm text-red-700">{error}</p>
      )}

      {apps === null ? (
        <p className="mt-6 text-sm text-gray-500">Loading…</p>
      ) : apps.length === 0 ? (
        <p className="mt-6 text-sm text-gray-500">
          No apps yet. Click <strong>New App</strong> to register one.
        </p>
      ) : (
        <ul className="mt-6 divide-y divide-gray-200 border border-gray-200">
          {apps.map((a) => (
            <li key={a.id}>
              <Link
                to={`/${orgSlug}/apps/${a.slug}`}
                className="block p-4 hover:bg-gray-50"
              >
                <div className="flex items-baseline justify-between gap-4">
                  <p className="truncate font-medium">{a.name}</p>
                  <p className="shrink-0 text-xs text-gray-500">
                    {formatDate(a.created_at)}
                  </p>
                </div>
                <p className="mt-1 truncate font-mono text-xs text-gray-600">
                  {a.github_repo_full_name} · {a.default_branch}
                </p>
                {a.deleting_at && (
                  <p className="mt-1 text-xs text-yellow-700">
                    Tearing down — resources are being destroyed.
                  </p>
                )}
              </Link>
            </li>
          ))}
        </ul>
      )}
    </>
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
