import type { DatabaseSync } from "node:sqlite";
import { vi } from "vitest";
import type { Config } from "../../server/config.ts";
import { migrate, openDatabase } from "../../server/db.ts";
import { WaitlistStore } from "../../server/store.ts";
import type { WaitlistContext } from "../../server/waitlist.ts";
import { CONSENT_VERSION } from "../../shared/site.ts";

export const IP = "203.0.113.7";

export const testConfig: Config = {
  host: "127.0.0.1",
  port: 0,
  dbPath: ":memory:",
  siteUrl: "https://headmesh.dev",
  emailFrom: "Headmesh <waitlist@mail.headmesh.dev>",
  emailReplyTo: "hello@headmesh.dev",
  emailDelivery: "resend",
  turnstileSecretKey: "test-turnstile-secret",
  resendApiKey: "test-resend-key",
  confirmationTemplateId: "test-confirmation-template",
  waitlistSecret: "test-waitlist-secret-that-is-long-enough-0123456789",
  clientIpHeader: "x-real-ip",
};

/** A fresh in-memory database with the real migrations applied. */
export function createContext(config: Partial<Config> = {}): WaitlistContext & { db: DatabaseSync } {
  const db = openDatabase(":memory:");
  migrate(db);
  return { db, config: { ...testConfig, ...config }, store: new WaitlistStore(db) };
}

export function rows(db: DatabaseSync): Record<string, string | null>[] {
  return db.prepare("SELECT * FROM waitlist ORDER BY created_at").all() as Record<string, string | null>[];
}

export function signupBody(overrides: Record<string, unknown> = {}) {
  return {
    email: "Ada@Example.com",
    use_case: "Sharing GPUs with renters",
    consent: true,
    consent_version: CONSENT_VERSION,
    turnstile_token: "token-from-widget",
    source: "news.ycombinator.com",
    utm: { utm_source: "hn", utm_campaign: "launch" },
    ...overrides,
  };
}

export interface SentEmail {
  from: string;
  to: string[];
  reply_to?: string;
  template: { id: string; variables: { CONFIRM_URL: string; UNSUBSCRIBE_URL: string; SITE_URL: string } };
  headers: Record<string, string>;
}

export interface Upstreams {
  turnstile: { success: boolean; action?: string; "error-codes"?: string[] } | "down";
  resendStatus: number;
  resendRetryAfter: string | undefined;
  resendCalls: { email: SentEmail; idempotencyKey: string }[];
  sent: SentEmail[];
  turnstileCalls: { secret: string; response: string; remoteip?: string }[];
}

/**
 * Stub Turnstile and Resend at fetch. Requests to anything else (the service under test on 127.0.0.1) go through
 * to the real fetch.
 */
export function stubUpstreams(): Upstreams {
  const upstreams: Upstreams = {
    turnstile: { success: true, action: "waitlist" },
    resendStatus: 200,
    resendRetryAfter: undefined,
    resendCalls: [],
    sent: [],
    turnstileCalls: [],
  };
  const realFetch = globalThis.fetch;
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const url = input instanceof Request ? input.url : String(input);
    if (url === "https://challenges.cloudflare.com/turnstile/v0/siteverify") {
      upstreams.turnstileCalls.push(JSON.parse(String(init?.body)));
      if (upstreams.turnstile === "down") return new Response("bad gateway", { status: 502 });
      return Response.json(upstreams.turnstile);
    }
    if (url === "https://api.resend.com/emails") {
      const headers = new Headers(init?.headers);
      if (headers.get("authorization") !== "Bearer test-resend-key") throw new Error("wrong Resend key");
      if (!headers.get("idempotency-key")?.startsWith("waitlist-confirm/")) throw new Error("no idempotency key");
      upstreams.resendCalls.push({
        email: JSON.parse(String(init?.body)),
        idempotencyKey: headers.get("idempotency-key") ?? "",
      });
      if (upstreams.resendStatus === 200) upstreams.sent.push(JSON.parse(String(init?.body)));
      return Response.json(
        upstreams.resendStatus === 200
          ? { id: "email_123" }
          : { name: "api_error", message: "Simulated provider failure", statusCode: upstreams.resendStatus },
        {
          status: upstreams.resendStatus,
          ...(upstreams.resendRetryAfter && { headers: { "retry-after": upstreams.resendRetryAfter } }),
        },
      );
    }
    if (new URL(url).hostname === "127.0.0.1") return realFetch(input, init);
    throw new Error(`unexpected fetch to ${url}`);
  });
  return upstreams;
}

/** Pull the confirm and unsubscribe tokens out of the last email sent. */
export function tokensFromLastEmail(upstreams: Upstreams) {
  const email = upstreams.sent.at(-1);
  if (!email) throw new Error("no email sent");
  const confirmToken = new URL(email.template.variables.CONFIRM_URL).hash.slice("#token=".length);
  const unsubscribeToken = new URL(email.template.variables.UNSUBSCRIBE_URL).hash.slice("#token=".length);
  if (!confirmToken || !unsubscribeToken) throw new Error("tokens missing from email");
  return { confirmToken, unsubscribeToken };
}
