import { useEffect, useRef, useState } from "react";
import { Link, useParams } from "react-router";
import { api } from "../api/client";
import type { components } from "../api/schema";

type Build = components["schemas"]["Build"];
type Stage = components["schemas"]["BuildStage"];
type LogEvent = components["schemas"]["BuildLogEvent"];

// BuildDetail polls the build every 2s while it's non-terminal, showing
// the stage timeline + outputs. Stage cards rebuild on every poll so a
// transition from running -> succeeded animates without manual refresh.
export function BuildDetail() {
  const { orgSlug, appSlug, buildId } = useParams();
  const [build, setBuild] = useState<Build | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (!orgSlug || !appSlug || !buildId) return;
    let cancelled = false;
    let timer: number | undefined;

    async function load() {
      const { data, error } = await api.GET(
        "/api/orgs/{slug}/apps/{appSlug}/builds/{buildId}",
        { params: { path: { slug: orgSlug!, appSlug: appSlug!, buildId: buildId! } } },
      );
      if (cancelled) return;
      if (error || !data) {
        setError(extractMessage(error) ?? "Failed to load build");
        return;
      }
      setBuild(data);
      if (data.status === "queued" || data.status === "running") {
        timer = window.setTimeout(load, 2000);
      }
    }
    void load();
    return () => {
      cancelled = true;
      if (timer) window.clearTimeout(timer);
    };
  }, [orgSlug, appSlug, buildId]);

  if (error) {
    return (
      <div className="space-y-4">
        <p className="bg-red-50 p-3 text-sm text-red-700">{error}</p>
        <Link
          to={`/${orgSlug}/apps/${appSlug}`}
          className="text-sm text-indigo-600 underline"
        >
          ← Back to app
        </Link>
      </div>
    );
  }
  if (!build) {
    return <p className="text-sm text-gray-500">Loading…</p>;
  }

  return (
    <>
      <div className="flex items-start justify-between gap-4">
        <div className="min-w-0">
          <p className="text-xs uppercase tracking-wide text-gray-500">Build</p>
          <h1 className="mt-1 truncate font-mono text-2xl font-bold tracking-tight">
            {build.source_sha ? build.source_sha.slice(0, 12) : build.source_ref}
          </h1>
          <p className="mt-1 truncate font-mono text-xs text-gray-600">
            {build.source_ref} · {build.source_sha}
          </p>
        </div>
        <Link
          to={`/${orgSlug}/apps/${appSlug}`}
          className="shrink-0 border border-gray-300 bg-white px-4 py-2 text-sm font-medium text-gray-700 shadow-sm hover:bg-gray-50"
        >
          ← App
        </Link>
      </div>

      <dl className="mt-6 grid grid-cols-1 gap-4 sm:grid-cols-3">
        <Field label="Status" value={build.status} mono />
        <Field
          label="Started"
          value={build.started_at ? formatDate(build.started_at) : "—"}
        />
        <Field
          label="Ended"
          value={build.ended_at ? formatDate(build.ended_at) : "—"}
        />
      </dl>

      {build.error_message && (
        <p className="mt-6 bg-red-50 p-3 font-mono text-sm text-red-700 whitespace-pre-wrap">
          {build.error_message}
        </p>
      )}

      <section className="mt-8">
        <h2 className="text-sm font-semibold uppercase tracking-wide text-gray-500">
          Timeline
        </h2>
        <Timeline stages={build.stages} />
      </section>

      <section className="mt-8">
        <h2 className="text-sm font-semibold uppercase tracking-wide text-gray-500">
          Logs
        </h2>
        <LogsPanel
          orgSlug={orgSlug!}
          appSlug={appSlug!}
          buildId={buildId!}
          buildStatus={build.status}
        />
      </section>

      {build.image_uri && (
        <section className="mt-8">
          <h2 className="text-sm font-semibold uppercase tracking-wide text-gray-500">
            Image
          </h2>
          <div className="mt-3 break-all border border-gray-200 bg-white p-4">
            <p className="font-mono text-sm text-gray-900">
              {build.image_uri}
            </p>
            {build.image_digest && (
              <p className="mt-2 font-mono text-xs text-gray-500">
                {build.image_digest}
              </p>
            )}
          </div>
        </section>
      )}
    </>
  );
}

const STAGE_ORDER = [
  "reconcile",
  "prepare",
  "dispatch",
  "clone",
  "build",
  "push",
];

// Timeline collapses the append-only stages array into one row per
// stage name, picking the latest event per stage to drive its visual
// status. We render stages in the canonical order so missing stages
// show as "pending" until they fire.
function Timeline({ stages }: { stages: Stage[] }) {
  const latestByName = new Map<string, Stage>();
  for (const s of stages) {
    latestByName.set(s.name, s);
  }
  return (
    <ol className="mt-3 divide-y divide-gray-200 border border-gray-200 bg-white">
      {STAGE_ORDER.map((name) => {
        const stage = latestByName.get(name);
        return <TimelineRow key={name} name={name} stage={stage} />;
      })}
    </ol>
  );
}

function TimelineRow({ name, stage }: { name: string; stage: Stage | undefined }) {
  const status = stage?.status ?? "pending";
  const dotClass = (
    {
      pending: "bg-gray-300",
      running: "bg-blue-500 animate-pulse",
      succeeded: "bg-green-500",
      failed: "bg-red-500",
    } as Record<string, string>
  )[status];
  return (
    <li className="flex items-center gap-3 px-4 py-3">
      <span className={`h-2.5 w-2.5 rounded-full ${dotClass}`} />
      <div className="flex-1">
        <p className="text-sm font-medium capitalize text-gray-900">{name}</p>
        {stage?.data?.error ? (
          <p className="mt-1 font-mono text-xs text-red-700">
            {String(stage.data.error)}
          </p>
        ) : null}
      </div>
      <p className="text-xs text-gray-500">
        {stage ? formatStageTime(stage) : "—"}
      </p>
    </li>
  );
}

function formatStageTime(s: Stage): string {
  const d = new Date(s.at);
  if (Number.isNaN(d.getTime())) return s.at;
  return d.toLocaleTimeString();
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

// LOGS_POLL_INTERVAL_MS is how often we re-hit the logs endpoint while
// the build is non-terminal *and* the server still reports more events.
// 2s matches the Build polling cadence; CloudWatch's GetLogEvents has
// no rate-limiting concerns at this volume.
const LOGS_POLL_INTERVAL_MS = 2000;

// LOGS_IDLE_INTERVAL_MS is how long we wait between polls when the
// stream reported has_more=false but the build is still running. The
// builder can fall silent (e.g. mid-Kaniko) for a while; backing off
// avoids hammering the API for zero-event responses.
const LOGS_IDLE_INTERVAL_MS = 5000;

// LogsPanel polls the logs endpoint, accumulates events, and renders
// them in a terminal-styled, monospaced panel. Polling stops once the
// server reports `build_terminal=true` *and* `has_more=false` — that's
// the moment we know no further events will appear.
function LogsPanel({
  orgSlug,
  appSlug,
  buildId,
  buildStatus,
}: {
  orgSlug: string;
  appSlug: string;
  buildId: string;
  buildStatus: string;
}) {
  const [events, setEvents] = useState<LogEvent[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [done, setDone] = useState(false);
  const tokenRef = useRef<string | undefined>(undefined);
  const containerRef = useRef<HTMLDivElement | null>(null);
  const pinToBottomRef = useRef(true);

  useEffect(() => {
    let cancelled = false;
    let timer: number | undefined;

    async function load() {
      const { data, error } = await api.GET(
        "/api/orgs/{slug}/apps/{appSlug}/builds/{buildId}/logs",
        {
          params: {
            path: { slug: orgSlug, appSlug, buildId },
            query: tokenRef.current
              ? { after: tokenRef.current }
              : undefined,
          },
        },
      );
      if (cancelled) return;
      if (error || !data) {
        setError(extractMessage(error) ?? "Failed to load logs");
        return;
      }
      // Append the new page's events. We don't dedupe — the API is
      // append-only via the next_token cursor, so a page never overlaps
      // with what we've already shown.
      if (data.events.length > 0) {
        setEvents((prev) => prev.concat(data.events));
      }
      tokenRef.current = data.next_token ?? tokenRef.current;

      const isTerminal = data.build_terminal && !data.has_more;
      if (isTerminal) {
        setDone(true);
        return;
      }
      const delay = data.has_more
        ? LOGS_POLL_INTERVAL_MS
        : LOGS_IDLE_INTERVAL_MS;
      timer = window.setTimeout(load, delay);
    }
    void load();
    return () => {
      cancelled = true;
      if (timer) window.clearTimeout(timer);
    };
    // buildId is the cursor; orgSlug/appSlug/buildStatus are stable per
    // page mount. We deliberately don't depend on `buildStatus` because
    // a status flip is the one thing we *want* the running poll loop to
    // observe via the API itself, not by being torn down and restarted.
  }, [orgSlug, appSlug, buildId]);

  // Auto-scroll to the bottom on each new event, but only if the user
  // hasn't scrolled up. Once they scroll up, we leave them alone.
  useEffect(() => {
    const el = containerRef.current;
    if (!el) return;
    if (pinToBottomRef.current) {
      el.scrollTop = el.scrollHeight;
    }
  }, [events]);

  function onScroll() {
    const el = containerRef.current;
    if (!el) return;
    const nearBottom =
      el.scrollHeight - el.scrollTop - el.clientHeight < 24;
    pinToBottomRef.current = nearBottom;
  }

  if (error) {
    return (
      <p className="mt-3 bg-red-50 p-3 font-mono text-sm text-red-700">
        {error}
      </p>
    );
  }

  return (
    <div className="mt-3">
      <div
        ref={containerRef}
        onScroll={onScroll}
        className="h-96 overflow-auto border border-gray-200 bg-gray-950 p-3 font-mono text-xs leading-relaxed text-gray-100"
      >
        {events.length === 0 ? (
          <p className="text-gray-500">
            {buildStatus === "queued"
              ? "Waiting for the build to start…"
              : "Waiting for the builder…"}
          </p>
        ) : (
          events.map((e, i) => (
            <LogLine key={`${e.timestamp}-${i}`} event={e} />
          ))
        )}
      </div>
      <p className="mt-2 text-xs text-gray-500">
        {done
          ? `Stream closed · ${events.length} line${events.length === 1 ? "" : "s"}`
          : `Streaming · ${events.length} line${events.length === 1 ? "" : "s"}`}
      </p>
    </div>
  );
}

function LogLine({ event }: { event: LogEvent }) {
  return (
    <div className="flex gap-3 whitespace-pre-wrap">
      <span className="shrink-0 text-gray-500">{formatLogTime(event.timestamp)}</span>
      <span className="break-all">{event.message}</span>
    </div>
  );
}

function formatLogTime(ms: number): string {
  const d = new Date(ms);
  if (Number.isNaN(d.getTime())) return "";
  // HH:MM:SS.mmm — short enough to share a row with the message and
  // still distinguish two events that happened in the same second.
  const hh = String(d.getHours()).padStart(2, "0");
  const mm = String(d.getMinutes()).padStart(2, "0");
  const ss = String(d.getSeconds()).padStart(2, "0");
  const mmm = String(d.getMilliseconds()).padStart(3, "0");
  return `${hh}:${mm}:${ss}.${mmm}`;
}
