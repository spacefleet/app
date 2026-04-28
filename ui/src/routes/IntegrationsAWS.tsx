import { useEffect, useState } from "react";
import { useParams } from "react-router";
import { api } from "../api/client";
import type { components } from "../api/schema";

type CloudAccount = components["schemas"]["CloudAccount"];
type StartResponse = components["schemas"]["CloudAccountStartResponse"];

// IntegrationsAWS is the per-org page for connecting AWS accounts.
// Connecting opens an inline panel with the CloudFormation Quick Create
// link and a paste-the-role-ARN field. Customers (or whoever they hand
// the URL to) launch the stack in their own AWS Console; we never hold
// AWS credentials on their behalf.
export function IntegrationsAWS() {
  const { orgSlug } = useParams();
  const [accounts, setAccounts] = useState<CloudAccount[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [pending, setPending] = useState<StartResponse | null>(null);

  async function refresh() {
    if (!orgSlug) return;
    const { data, error } = await api.GET("/api/orgs/{slug}/aws/accounts", {
      params: { path: { slug: orgSlug } },
    });
    if (error || !data) {
      setError(extractMessage(error) ?? "Failed to load cloud accounts");
      return;
    }
    setAccounts(data.accounts);
  }

  useEffect(() => {
    void refresh();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [orgSlug]);

  return (
    <>
      <h1 className="text-3xl font-bold tracking-tight">AWS</h1>
      <p className="mt-2 text-sm text-gray-600">
        Connect AWS accounts so Spacefleet can deploy and manage infrastructure
        on your behalf. We use cross-account IAM roles with external IDs — no
        long-lived credentials are stored.
      </p>

      {error && (
        <p className="mt-4 bg-red-50 p-3 text-sm text-red-700">{error}</p>
      )}

      {pending ? (
        <CompletePanel
          orgSlug={orgSlug!}
          start={pending}
          onCancel={() => setPending(null)}
          onCompleted={() => {
            setPending(null);
            void refresh();
          }}
        />
      ) : (
        <StartForm
          orgSlug={orgSlug!}
          onStarted={(res) => {
            setPending(res);
            void refresh();
          }}
        />
      )}

      {accounts === null ? (
        <p className="mt-6 text-sm text-gray-500">Loading…</p>
      ) : accounts.length === 0 ? (
        <p className="mt-6 text-sm text-gray-500">
          No AWS accounts connected yet.
        </p>
      ) : (
        <ul className="mt-6 divide-y divide-gray-200 border border-gray-200">
          {accounts.map((acct) => (
            <AccountItem
              key={acct.id}
              orgSlug={orgSlug!}
              account={acct}
              onChange={refresh}
            />
          ))}
        </ul>
      )}
    </>
  );
}

function StartForm({
  orgSlug,
  onStarted,
}: {
  orgSlug: string;
  onStarted: (res: StartResponse) => void;
}) {
  const [label, setLabel] = useState("");
  const [region, setRegion] = useState("us-east-1");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    setBusy(true);
    setErr(null);
    const { data, error } = await api.POST("/api/orgs/{slug}/aws/accounts", {
      params: { path: { slug: orgSlug } },
      body: { label, region: region || undefined },
    });
    setBusy(false);
    if (error || !data) {
      setErr(extractMessage(error) ?? "Failed to start onboarding");
      return;
    }
    onStarted(data);
  }

  return (
    <form
      onSubmit={submit}
      className="mt-6 max-w-lg space-y-4 border border-gray-200 bg-white p-4"
    >
      <div>
        <label className="block text-sm font-medium text-gray-700">Label</label>
        <input
          value={label}
          onChange={(e) => setLabel(e.target.value)}
          required
          minLength={1}
          maxLength={64}
          pattern="[a-zA-Z0-9][a-zA-Z0-9-]*"
          placeholder="acme-prod"
          className="mt-1 block w-full border border-gray-300 px-3 py-2 text-sm shadow-sm focus:border-indigo-500 focus:outline-none"
        />
        <p className="mt-1 text-xs text-gray-500">
          Human-friendly name for this AWS account, unique within your org.
        </p>
      </div>

      <div>
        <label className="block text-sm font-medium text-gray-700">
          Default region
        </label>
        <input
          value={region}
          onChange={(e) => setRegion(e.target.value)}
          placeholder="us-east-1"
          className="mt-1 block w-full border border-gray-300 px-3 py-2 text-sm shadow-sm focus:border-indigo-500 focus:outline-none"
        />
      </div>

      {err && <p className="bg-red-50 p-2 text-sm text-red-700">{err}</p>}

      <button
        type="submit"
        disabled={busy || !label}
        className="bg-indigo-600 px-4 py-2 text-sm font-medium text-white shadow-sm hover:bg-indigo-700 disabled:cursor-not-allowed disabled:opacity-50"
      >
        {busy ? "Starting…" : "Connect AWS Account"}
      </button>
    </form>
  );
}

function CompletePanel({
  orgSlug,
  start,
  onCancel,
  onCompleted,
}: {
  orgSlug: string;
  start: StartResponse;
  onCancel: () => void;
  onCompleted: () => void;
}) {
  const [roleArn, setRoleArn] = useState("");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [verifyError, setVerifyError] = useState<string | null>(null);

  async function complete(e: React.FormEvent) {
    e.preventDefault();
    setBusy(true);
    setErr(null);
    setVerifyError(null);
    const { data, error } = await api.POST(
      "/api/orgs/{slug}/aws/accounts/{id}/complete",
      {
        params: { path: { slug: orgSlug, id: start.account.id } },
        body: { role_arn: roleArn },
      },
    );
    setBusy(false);
    if (error || !data) {
      setErr(extractMessage(error) ?? "Failed to complete onboarding");
      return;
    }
    if (data.status !== "connected") {
      setVerifyError(
        data.last_verification_error ??
          `Verification failed (status: ${data.status})`,
      );
      return;
    }
    onCompleted();
  }

  return (
    <div className="mt-6 max-w-2xl space-y-5 border border-indigo-200 bg-indigo-50 p-5">
      <div>
        <h2 className="text-lg font-semibold tracking-tight">
          Onboarding {start.account.label}
        </h2>
        <p className="mt-1 text-sm text-gray-700">
          Two steps left: launch the CloudFormation stack in your AWS account,
          then paste the role ARN from its outputs.
        </p>
      </div>

      <ol className="space-y-5 text-sm">
        <li>
          <p className="font-semibold">1. Launch the CloudFormation stack</p>
          <p className="mt-1 text-gray-700">
            Opens AWS Console with the parameters pre-filled. You only need to
            tick the IAM acknowledgement and click Create.
          </p>
          <a
            href={start.quick_create_url}
            target="_blank"
            rel="noreferrer"
            className="mt-2 inline-block bg-indigo-600 px-4 py-2 text-sm font-medium text-white shadow-sm hover:bg-indigo-700"
          >
            Launch CloudFormation in AWS
          </a>
          <details className="mt-3 text-xs text-gray-700">
            <summary className="cursor-pointer">
              Don't have AWS access? Send these to whoever does.
            </summary>
            <CopyField label="Quick Create URL" value={start.quick_create_url} />
            <CopyField label="External ID" value={start.external_id} />
            <CopyField
              label="Trusted account"
              value={start.platform_account_id}
            />
          </details>
        </li>

        <li>
          <p className="font-semibold">2. Paste the role ARN</p>
          <p className="mt-1 text-gray-700">
            After the stack creates (about 30 seconds), copy the{" "}
            <code className="bg-white px-1">RoleArn</code> output and paste it
            below.
          </p>
          <form onSubmit={complete} className="mt-2 space-y-3">
            <input
              value={roleArn}
              onChange={(e) => setRoleArn(e.target.value)}
              placeholder="arn:aws:iam::123456789012:role/SpacefleetIntegrationRole"
              required
              className="block w-full border border-gray-300 px-3 py-2 font-mono text-sm shadow-sm focus:border-indigo-500 focus:outline-none"
            />
            {err && <p className="bg-red-50 p-2 text-sm text-red-700">{err}</p>}
            {verifyError && (
              <p className="bg-yellow-50 p-2 text-sm text-yellow-800">
                <strong>Saved, but verification failed:</strong> {verifyError}
                . Fix the issue in AWS and click Verify on the account row.
              </p>
            )}
            <div className="flex gap-2">
              <button
                type="submit"
                disabled={busy || !roleArn}
                className="bg-indigo-600 px-4 py-2 text-sm font-medium text-white shadow-sm hover:bg-indigo-700 disabled:cursor-not-allowed disabled:opacity-50"
              >
                {busy ? "Verifying…" : "Complete onboarding"}
              </button>
              <button
                type="button"
                onClick={onCancel}
                className="border border-gray-300 bg-white px-4 py-2 text-sm font-medium text-gray-700 shadow-sm hover:bg-gray-50"
              >
                Save & finish later
              </button>
            </div>
          </form>
        </li>
      </ol>
    </div>
  );
}

function CopyField({ label, value }: { label: string; value: string }) {
  const [copied, setCopied] = useState(false);
  return (
    <div className="mt-2">
      <p className="text-xs font-medium text-gray-600">{label}</p>
      <div className="mt-1 flex items-stretch gap-2">
        <input
          readOnly
          value={value}
          onFocus={(e) => e.currentTarget.select()}
          className="flex-1 border border-gray-300 bg-white px-2 py-1 font-mono text-xs"
        />
        <button
          type="button"
          onClick={async () => {
            await navigator.clipboard.writeText(value);
            setCopied(true);
            setTimeout(() => setCopied(false), 1500);
          }}
          className="border border-gray-300 bg-white px-3 py-1 text-xs font-medium text-gray-700 shadow-sm hover:bg-gray-50"
        >
          {copied ? "Copied" : "Copy"}
        </button>
      </div>
    </div>
  );
}

function AccountItem({
  orgSlug,
  account,
  onChange,
}: {
  orgSlug: string;
  account: CloudAccount;
  onChange: () => void;
}) {
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  async function verify() {
    setBusy(true);
    setErr(null);
    const { error } = await api.POST(
      "/api/orgs/{slug}/aws/accounts/{id}/verify",
      { params: { path: { slug: orgSlug, id: account.id } } },
    );
    setBusy(false);
    if (error) {
      setErr(extractMessage(error) ?? "Verification failed");
    }
    onChange();
  }

  async function disconnect() {
    if (!confirm(`Disconnect ${account.label}?`)) return;
    setBusy(true);
    await api.DELETE("/api/orgs/{slug}/aws/accounts/{id}", {
      params: { path: { slug: orgSlug, id: account.id } },
    });
    setBusy(false);
    onChange();
  }

  return (
    <li className="p-4">
      <div className="flex items-start justify-between gap-4">
        <div className="min-w-0">
          <p className="truncate font-medium">
            {account.label}{" "}
            <StatusPill status={account.status} />
          </p>
          <p className="mt-0.5 text-xs text-gray-500">
            {account.account_id ? (
              <>
                AWS account {account.account_id}
                {account.region ? ` · ${account.region}` : ""}
                {" · "}
                Connected {formatDate(account.created_at)}
              </>
            ) : (
              <>
                Pending — finish onboarding to populate the role.{" "}
                {formatDate(account.created_at)}
              </>
            )}
          </p>
          {account.role_arn && (
            <p className="mt-1 truncate font-mono text-xs text-gray-600">
              {account.role_arn}
            </p>
          )}
          {account.last_verified_at && (
            <p className="mt-1 text-xs text-gray-500">
              Last verified {formatDate(account.last_verified_at)}
            </p>
          )}
          {account.last_verification_error && (
            <p className="mt-1 bg-yellow-50 p-2 text-xs text-yellow-800">
              {account.last_verification_error}
            </p>
          )}
          {err && <p className="mt-1 bg-red-50 p-2 text-xs text-red-700">{err}</p>}
        </div>
        <div className="flex shrink-0 gap-2">
          {account.role_arn && (
            <button
              type="button"
              onClick={verify}
              disabled={busy}
              className="border border-gray-300 bg-white px-3 py-1.5 text-sm font-medium text-gray-700 shadow-sm hover:bg-gray-50 disabled:opacity-50"
            >
              Verify
            </button>
          )}
          {account.update_stack_url && (
            <a
              href={account.update_stack_url}
              target="_blank"
              rel="noreferrer"
              title="Open the AWS Console Update Stack wizard with the latest Spacefleet template pre-filled."
              className="border border-gray-300 bg-white px-3 py-1.5 text-sm font-medium text-gray-700 shadow-sm hover:bg-gray-50"
            >
              Update permissions
            </a>
          )}
          <button
            type="button"
            onClick={disconnect}
            disabled={busy}
            className="border border-gray-300 bg-white px-3 py-1.5 text-sm font-medium text-gray-700 shadow-sm hover:bg-gray-50 disabled:opacity-50"
          >
            Disconnect
          </button>
        </div>
      </div>
    </li>
  );
}

function StatusPill({ status }: { status: string }) {
  const cls =
    status === "connected"
      ? "bg-green-100 text-green-800"
      : status === "pending"
        ? "bg-gray-100 text-gray-800"
        : status === "error"
          ? "bg-red-100 text-red-800"
          : "bg-gray-100 text-gray-700";
  return (
    <span
      className={`ml-2 px-2 py-0.5 text-xs font-medium ${cls}`}
    >
      {status}
    </span>
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
