import { useUser } from "@clerk/react";
import { useMemo, useState } from "react";
import { useSearchParams } from "react-router";
import { api } from "../api/client";

// CliAuth is the browser half of the CLI loopback flow. The CLI opens this
// page with ?port&state&challenge&name; after the user confirms, we POST
// to /api/cli/auth/approve to get a one-time code, then redirect to the
// CLI's localhost callback with {state, code}. The real token is fetched
// server-to-server by the CLI in a follow-up /exchange call — it never
// passes through this page.
export function CliAuth() {
  const [params] = useSearchParams();
  const port = params.get("port");
  const state = params.get("state");
  const challenge = params.get("challenge");
  const suggestedName = params.get("name") ?? "cli";

  const { user } = useUser();
  const [name, setName] = useState(suggestedName);
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const callbackBase = useMemo(() => {
    const portNum = Number(port);
    if (!port || !state || !challenge) return null;
    if (!Number.isInteger(portNum) || portNum <= 0 || portNum > 65535) {
      return null;
    }
    return `http://127.0.0.1:${portNum}/callback`;
  }, [port, state, challenge]);

  if (!callbackBase) {
    return (
      <Centered>
        <h1 className="text-xl font-semibold">Invalid CLI request</h1>
        <p className="mt-2 text-sm text-gray-600">
          This page needs <code>port</code>, <code>state</code>, and{" "}
          <code>challenge</code> query parameters. Re-run the CLI's login
          command to start over.
        </p>
      </Centered>
    );
  }

  async function authorize() {
    setSubmitting(true);
    setError(null);
    const { data, error } = await api.POST("/api/cli/auth/approve", {
      body: { name, challenge: challenge! },
    });
    if (error || !data) {
      setSubmitting(false);
      setError(
        typeof error === "object" && error && "message" in error
          ? String((error as { message: unknown }).message)
          : "Approval failed",
      );
      return;
    }
    const url = new URL(callbackBase!);
    url.searchParams.set("state", state!);
    url.searchParams.set("code", data.code);
    window.location.replace(url.toString());
  }

  function cancel() {
    const url = new URL(callbackBase!);
    url.searchParams.set("state", state!);
    url.searchParams.set("error", "cancelled");
    window.location.replace(url.toString());
  }

  return (
    <Centered>
      <h1 className="text-xl font-semibold tracking-tight">
        Authorize Spacefleet CLI
      </h1>
      <p className="mt-2 text-sm text-gray-600">
        Signed in as{" "}
        <strong>{user?.primaryEmailAddress?.emailAddress ?? "…"}</strong>. The
        token will be valid for 90 days and can act on any organization this
        account has access to.
      </p>

      <label className="mt-6 block text-sm font-medium text-gray-700">
        Token name
        <input
          value={name}
          onChange={(e) => setName(e.target.value)}
          className="mt-1 w-full rounded-md border border-gray-300 px-3 py-2 text-sm shadow-sm focus:border-indigo-500 focus:outline-none focus:ring-1 focus:ring-indigo-500"
          placeholder="e.g. my-laptop"
        />
      </label>

      {error && (
        <p className="mt-3 rounded-md bg-red-50 p-2 text-sm text-red-700">
          {error}
        </p>
      )}

      <div className="mt-6 flex gap-2">
        <button
          onClick={authorize}
          disabled={submitting || name.trim() === ""}
          className="flex-1 rounded-md bg-indigo-600 px-4 py-2 text-sm font-medium text-white shadow-sm hover:bg-indigo-700 disabled:cursor-not-allowed disabled:opacity-50"
        >
          {submitting ? "Authorizing…" : "Authorize"}
        </button>
        <button
          onClick={cancel}
          disabled={submitting}
          className="rounded-md border border-gray-300 bg-white px-4 py-2 text-sm font-medium text-gray-700 shadow-sm hover:bg-gray-50 disabled:opacity-50"
        >
          Cancel
        </button>
      </div>
    </Centered>
  );
}

function Centered({ children }: { children: React.ReactNode }) {
  return (
    <div className="flex min-h-screen items-center justify-center bg-gray-50 p-4">
      <div className="w-full max-w-md rounded-lg bg-white p-8 shadow-sm ring-1 ring-gray-200">
        {children}
      </div>
    </div>
  );
}
