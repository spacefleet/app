import createClient, { type Middleware } from "openapi-fetch";
import type { paths } from "./schema";

// Same-origin fetch: in dev, Vite proxies /api/* to the Go server; in prod,
// the Go binary serves both the SPA and /api/* from the same origin.
export const api = createClient<paths>({ baseUrl: "/" });

// The auth binder (see main.tsx) wires this up once Clerk is loaded. Before
// that, requests go out unauthenticated — the backend treats /api/health as
// public and will return 401 for anything else.
let tokenProvider: (() => Promise<string | null>) | null = null;

export function setAuthTokenProvider(
  fn: (() => Promise<string | null>) | null,
) {
  tokenProvider = fn;
}

const authMiddleware: Middleware = {
  async onRequest({ request }) {
    if (!tokenProvider) return;
    const token = await tokenProvider();
    if (token) request.headers.set("Authorization", `Bearer ${token}`);
  },
};

api.use(authMiddleware);
