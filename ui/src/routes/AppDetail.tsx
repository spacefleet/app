import { useEffect, useState } from "react";
import { Link, useNavigate, useParams } from "react-router";
import { api } from "../api/client";
import type { components } from "../api/schema";

type App = components["schemas"]["App"];
type Build = components["schemas"]["Build"];

// AppDetail shows the registered metadata and a delete CTA. Phase 5
// replaces the placeholder card below with a builds list and a "New
// Build" button — the rest of the page (header + delete) lives on as
// is.
export function AppDetail() {
  const navigate = useNavigate();
  const { orgSlug, appSlug } = useParams();

  const [app, setApp] = useState<App | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [reload, setReload] = useState(0);

  useEffect(() => {
    if (!orgSlug || !appSlug) return;
    void (async () => {
      const { data, error } = await api.GET(
        "/api/orgs/{slug}/apps/{appSlug}",
        { params: { path: { slug: orgSlug, appSlug } } },
      );
      if (error || !data) {
        setError(extractMessage(error) ?? "Failed to load app");
        return;
      }
      setApp(data);
    })();
  }, [orgSlug, appSlug, reload]);

  if (error) {
    return (
      <div className="space-y-4">
        <p className="bg-red-50 p-3 text-sm text-red-700">{error}</p>
        <Link
          to={`/${orgSlug}/apps`}
          className="text-sm text-indigo-600 underline"
        >
          ← Back to apps
        </Link>
      </div>
    );
  }
  if (!app) {
    return <p className="text-sm text-gray-500">Loading…</p>;
  }

  return (
    <>
      <div className="flex items-start justify-between gap-4">
        <div className="min-w-0">
          <p className="text-xs uppercase tracking-wide text-gray-500">App</p>
          <h1 className="mt-1 truncate text-3xl font-bold tracking-tight">
            {app.name}
          </h1>
          <p className="mt-1 truncate font-mono text-xs text-gray-600">
            {app.github_repo_full_name} · {app.default_branch}
          </p>
          {app.deleting_at && (
            <p className="mt-2 bg-yellow-50 p-2 text-sm text-yellow-800">
              Tearing down — AWS resources are being destroyed in the
              background.
            </p>
          )}
        </div>
        <Link
          to={`/${orgSlug}/apps`}
          className="shrink-0 border border-gray-300 bg-white px-4 py-2 text-sm font-medium text-gray-700 shadow-sm hover:bg-gray-50"
        >
          ← Apps
        </Link>
      </div>

      <dl className="mt-6 grid grid-cols-1 gap-4 sm:grid-cols-2">
        <Field label="Slug" value={app.slug} mono />
        <Field label="Created" value={formatDate(app.created_at)} />
        <Field label="Created by" value={app.created_by} mono />
      </dl>

      <BuildsPanel orgSlug={orgSlug!} app={app} />


      <div className="mt-10 border border-red-200 bg-red-50 p-5">
        <h2 className="text-sm font-semibold text-red-900">Danger zone</h2>
        <DeleteApp
          orgSlug={orgSlug!}
          app={app}
          onDeleted={() => navigate(`/${orgSlug}/apps`)}
          onPending={() => setReload((r) => r + 1)}
        />
      </div>
    </>
  );
}

function DeleteApp({
  orgSlug,
  app,
  onDeleted,
  onPending,
}: {
  orgSlug: string;
  app: App;
  onDeleted: () => void;
  onPending: () => void;
}) {
  const [destroyResources, setDestroyResources] = useState(false);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  async function submit() {
    if (
      !confirm(
        destroyResources
          ? `Delete ${app.name} AND destroy its AWS resources? This is not reversible.`
          : `Delete ${app.name}? AWS resources will be left in place.`,
      )
    ) {
      return;
    }
    setBusy(true);
    setErr(null);
    const { response, error } = await api.DELETE(
      "/api/orgs/{slug}/apps/{appSlug}",
      {
        params: { path: { slug: orgSlug, appSlug: app.slug } },
        body: { destroy_resources: destroyResources },
      },
    );
    setBusy(false);
    if (error) {
      setErr(extractMessage(error) ?? "Failed to delete app");
      return;
    }
    if (response.status === 202) {
      // Destroy enqueued — keep the user on the page so they see
      // the deleting_at marker until the row eventually disappears.
      onPending();
      return;
    }
    onDeleted();
  }

  return (
    <div className="mt-3 space-y-3">
      <label className="flex items-start gap-2 text-sm text-red-900">
        <input
          type="checkbox"
          checked={destroyResources}
          onChange={(e) => setDestroyResources(e.target.checked)}
          className="mt-1"
          disabled={!!app.deleting_at}
        />
        <span>
          Also destroy AWS resources (ECR repo, builder role, log group). The
          per-app Pulumi stack will be torn down by a background worker.
        </span>
      </label>
      {err && <p className="bg-red-100 p-2 text-sm text-red-800">{err}</p>}
      <button
        type="button"
        onClick={submit}
        disabled={busy || !!app.deleting_at}
        className="bg-red-600 px-4 py-2 text-sm font-medium text-white shadow-sm hover:bg-red-700 disabled:cursor-not-allowed disabled:opacity-50"
      >
        {app.deleting_at
          ? "Tearing down…"
          : busy
            ? "Working…"
            : destroyResources
              ? "Delete & destroy"
              : "Delete app"}
      </button>
    </div>
  );
}

function Field({
  label,
  value,
  mono,
}: {
  label: string;
  value: string;
  mono?: boolean;
}) {
  return (
    <div>
      <dt className="text-xs uppercase tracking-wide text-gray-500">{label}</dt>
      <dd
        className={`mt-1 truncate text-sm text-gray-900 ${mono ? "font-mono" : ""}`}
      >
        {value}
      </dd>
    </div>
  );
}

// BuildsPanel lists builds and exposes a "New Build" form. Polls every
// 3s while any non-terminal build is visible so progress shows up
// without a manual refresh.
function BuildsPanel({ orgSlug, app }: { orgSlug: string; app: App }) {
  const [builds, setBuilds] = useState<Build[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [ref, setRef] = useState("");
  const [reload, setReload] = useState(0);

  // Poll while any build is non-terminal. The dependency on `reload`
  // re-fires after a successful create so the new row appears
  // immediately.
  useEffect(() => {
    let cancelled = false;
    let timer: number | undefined;

    async function load() {
      const { data, error } = await api.GET(
        "/api/orgs/{slug}/apps/{appSlug}/builds",
        { params: { path: { slug: orgSlug, appSlug: app.slug } } },
      );
      if (cancelled) return;
      if (error || !data) {
        setError(extractMessage(error) ?? "Failed to load builds");
        return;
      }
      setBuilds(data.builds);
      const hasInflight = data.builds.some(
        (b) => b.status === "queued" || b.status === "running",
      );
      if (hasInflight) {
        timer = window.setTimeout(load, 3000);
      }
    }

    void load();
    return () => {
      cancelled = true;
      if (timer) window.clearTimeout(timer);
    };
  }, [orgSlug, app.slug, reload]);

  async function startBuild(e: React.FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError(null);
    const body = ref.trim() ? { ref: ref.trim() } : {};
    const { error } = await api.POST(
      "/api/orgs/{slug}/apps/{appSlug}/builds",
      {
        params: { path: { slug: orgSlug, appSlug: app.slug } },
        body,
      },
    );
    setBusy(false);
    if (error) {
      setError(extractMessage(error) ?? "Failed to start build");
      return;
    }
    setRef("");
    setReload((r) => r + 1);
  }

  return (
    <section className="mt-10">
      <div className="flex items-end justify-between">
        <h2 className="text-sm font-semibold uppercase tracking-wide text-gray-500">
          Builds
        </h2>
      </div>

      <form
        onSubmit={startBuild}
        className="mt-3 flex flex-wrap items-end gap-3 border border-gray-200 bg-white p-4"
      >
        <div className="flex-1 min-w-[200px]">
          <label className="block text-xs font-medium uppercase tracking-wide text-gray-500">
            Ref (optional)
          </label>
          <input
            type="text"
            value={ref}
            onChange={(e) => setRef(e.target.value)}
            placeholder={app.default_branch}
            className="mt-1 w-full border border-gray-300 px-3 py-2 font-mono text-sm focus:border-indigo-500 focus:outline-none"
            disabled={!!app.deleting_at || busy}
          />
          <p className="mt-1 text-xs text-gray-500">
            Branch, tag, or commit SHA. Defaults to{" "}
            <code className="font-mono">{app.default_branch}</code>.
          </p>
        </div>
        <button
          type="submit"
          disabled={!!app.deleting_at || busy}
          className="bg-indigo-600 px-4 py-2 text-sm font-medium text-white shadow-sm hover:bg-indigo-700 disabled:cursor-not-allowed disabled:opacity-50"
        >
          {busy ? "Starting…" : "New build"}
        </button>
      </form>
      {error && (
        <p className="mt-3 bg-red-50 p-2 text-sm text-red-700">{error}</p>
      )}

      <div className="mt-4 border border-gray-200 bg-white">
        {builds === null ? (
          <p className="p-4 text-sm text-gray-500">Loading builds…</p>
        ) : builds.length === 0 ? (
          <p className="p-4 text-sm text-gray-500">
            No builds yet. Click "New build" to start one.
          </p>
        ) : (
          <ul className="divide-y divide-gray-200">
            {builds.map((b) => (
              <BuildRow key={b.id} build={b} orgSlug={orgSlug} appSlug={app.slug} />
            ))}
          </ul>
        )}
      </div>
    </section>
  );
}

function BuildRow({
  build,
  orgSlug,
  appSlug,
}: {
  build: Build;
  orgSlug: string;
  appSlug: string;
}) {
  const currentStage = latestRunningStage(build.stages);
  return (
    <li>
      <Link
        to={`/${orgSlug}/apps/${appSlug}/builds/${build.id}`}
        className="block px-4 py-3 hover:bg-gray-50"
      >
        <div className="flex items-center justify-between gap-4">
          <div className="min-w-0">
            <p className="flex items-center gap-2 text-sm">
              <StatusBadge status={build.status} />
              <span className="font-mono text-gray-900">
                {build.source_sha
                  ? build.source_sha.slice(0, 7)
                  : build.source_ref}
              </span>
              <span className="text-gray-500">·</span>
              <span className="text-gray-700">{build.source_ref}</span>
            </p>
            {currentStage && (
              <p className="mt-1 text-xs text-gray-500">
                Currently in <span className="font-medium">{currentStage}</span>
              </p>
            )}
            {build.error_message && (
              <p className="mt-1 truncate text-xs text-red-700">
                {build.error_message}
              </p>
            )}
          </div>
          <p className="shrink-0 text-xs text-gray-500">
            {formatRelativeTime(build.created_at)}
          </p>
        </div>
      </Link>
    </li>
  );
}

function StatusBadge({ status }: { status: Build["status"] }) {
  const colors: Record<Build["status"], string> = {
    queued: "bg-gray-100 text-gray-700",
    running: "bg-blue-100 text-blue-800",
    succeeded: "bg-green-100 text-green-800",
    failed: "bg-red-100 text-red-800",
  };
  return (
    <span
      className={`inline-block px-2 py-0.5 text-xs font-medium ${colors[status]}`}
    >
      {status}
    </span>
  );
}

function latestRunningStage(stages: Build["stages"]): string {
  for (let i = stages.length - 1; i >= 0; i--) {
    const s = stages[i];
    if (s.status === "running") return s.name;
    if (s.status === "succeeded" || s.status === "failed") return "";
  }
  return "";
}

function formatRelativeTime(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  const diffMs = Date.now() - d.getTime();
  const diffSec = Math.round(diffMs / 1000);
  if (diffSec < 60) return `${diffSec}s ago`;
  if (diffSec < 3600) return `${Math.floor(diffSec / 60)}m ago`;
  if (diffSec < 86400) return `${Math.floor(diffSec / 3600)}h ago`;
  return d.toLocaleDateString();
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
