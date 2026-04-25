import { useEffect, useRef, useState } from "react";
import { useNavigate, useSearchParams } from "react-router";
import { api } from "../api/client";

// GitHubCallback is the landing page configured as the App's setup URL on
// github.com. After a user installs the App, GitHub bounces them here
// with `?installation_id=…&setup_action=install&state=…`. We post the
// state + installation id to /api/github/installations/complete; on
// success the server returns the org slug and we redirect to its
// integrations page.
export function GitHubCallback() {
  const [params] = useSearchParams();
  const navigate = useNavigate();
  const installationId = params.get("installation_id");
  const state = params.get("state");
  const setupAction = params.get("setup_action");

  const [error, setError] = useState<string | null>(null);
  // StrictMode runs effects twice in dev; the state record is single-use,
  // so we'd 409 on the second post without this guard.
  const completed = useRef(false);

  useEffect(() => {
    if (completed.current) return;
    if (!installationId || !state) {
      setError("Missing installation_id or state");
      return;
    }
    if (setupAction && setupAction !== "install" && setupAction !== "update") {
      setError(`Unsupported setup action: ${setupAction}`);
      return;
    }
    completed.current = true;

    (async () => {
      const { data, error } = await api.POST(
        "/api/github/installations/complete",
        {
          body: {
            state,
            installation_id: Number(installationId),
          },
        },
      );
      if (error || !data) {
        setError(
          (error && typeof error === "object" && "message" in error
            ? String((error as { message: unknown }).message)
            : null) ?? "Failed to complete install",
        );
        return;
      }
      navigate(`/${data.org_slug}/integrations/github`, { replace: true });
    })();
  }, [installationId, state, setupAction, navigate]);

  return (
    <div className="flex min-h-screen items-center justify-center bg-gray-50 p-4">
      <div className="w-full max-w-md bg-white p-8 shadow-sm ring-1 ring-gray-200">
        <h1 className="text-xl font-semibold tracking-tight">
          Connecting GitHub…
        </h1>
        {error ? (
          <p className="mt-4 bg-red-50 p-3 text-sm text-red-700">{error}</p>
        ) : (
          <p className="mt-2 text-sm text-gray-600">
            Completing the install handshake. You'll be redirected back to
            Spacefleet in a moment.
          </p>
        )}
      </div>
    </div>
  );
}
