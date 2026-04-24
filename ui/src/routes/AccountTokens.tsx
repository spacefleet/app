import { useEffect, useState } from "react";
import { api } from "../api/client";
import type { components } from "../api/schema";

type CliToken = components["schemas"]["CliToken"];

export function AccountTokens() {
  const [tokens, setTokens] = useState<CliToken[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busyId, setBusyId] = useState<string | null>(null);

  async function refresh() {
    const { data, error } = await api.GET("/api/cli/tokens");
    if (error || !data) {
      setError("Failed to load tokens");
      return;
    }
    setTokens(data.tokens);
  }

  useEffect(() => {
    void refresh();
  }, []);

  async function revoke(id: string) {
    setBusyId(id);
    await api.DELETE("/api/cli/tokens/{id}", { params: { path: { id } } });
    setBusyId(null);
    void refresh();
  }

  return (
    <>
      <h1 className="text-3xl font-bold tracking-tight">CLI tokens</h1>
      <p className="mt-2 text-sm text-gray-600">
        Long-lived credentials issued to the Spacefleet CLI. Revoking a token
        immediately blocks the next request from any machine holding it.
      </p>

      {error && (
        <p className="mt-4 rounded-md bg-red-50 p-3 text-sm text-red-700">
          {error}
        </p>
      )}

      {tokens === null ? (
        <p className="mt-6 text-sm text-gray-500">Loading…</p>
      ) : tokens.length === 0 ? (
        <p className="mt-6 text-sm text-gray-500">
          No tokens yet. Run <code>spacefleet auth login</code> from your CLI
          to create one.
        </p>
      ) : (
        <ul className="mt-6 divide-y divide-gray-200 rounded-md border border-gray-200">
          {tokens.map((t) => (
            <li
              key={t.id}
              className="flex items-center justify-between gap-4 p-4"
            >
              <div className="min-w-0">
                <p className="truncate font-medium">{t.name}</p>
                <p className="text-xs text-gray-500">
                  Created {formatDate(t.created_at)} · Expires{" "}
                  {formatDate(t.expires_at)}
                  {t.last_used_at
                    ? ` · Last used ${formatDate(t.last_used_at)}`
                    : " · Never used"}
                </p>
                {t.revoked_at && (
                  <p className="mt-1 text-xs font-medium text-red-600">
                    Revoked {formatDate(t.revoked_at)}
                  </p>
                )}
              </div>
              {!t.revoked_at && (
                <button
                  onClick={() => revoke(t.id)}
                  disabled={busyId === t.id}
                  className="rounded-md border border-gray-300 bg-white px-3 py-1.5 text-sm font-medium text-gray-700 shadow-sm hover:bg-gray-50 disabled:opacity-50"
                >
                  {busyId === t.id ? "Revoking…" : "Revoke"}
                </button>
              )}
            </li>
          ))}
        </ul>
      )}
    </>
  );
}

function formatDate(iso: string) {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  return d.toLocaleString();
}
