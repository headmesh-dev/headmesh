import { createPagesEventContext } from "cloudflare:test";
import { env } from "cloudflare:workers";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { onRequestPost as signupFunction } from "../../functions/api/waitlist";
import { onRequestPost as confirmFunction } from "../../functions/api/waitlist/confirm";
import { onRequestPost as unsubscribeFunction } from "../../functions/api/waitlist/unsubscribe";
import type { Env } from "../../server/env";
import { handleSignup, RESEND_CONFIRMATION_AFTER_MS, SIGNUPS_PER_IP_PER_HOUR } from "../../server/waitlist";
import { CONSENT_VERSION } from "../../shared/site";

// The committed SENDER has no postal address yet (it must be filled in before launch); tests supply one.
const sender = vi.hoisted(() => ({
  legalName: "Xcelsior Compute Inc.",
  postalAddress: "1 Test Street, Toronto ON M5V 0A1, Canada",
  email: "hello@headmesh.dev",
  website: "https://headmesh.dev",
}));
vi.mock(import("../../shared/site"), async (importOriginal) => ({ ...(await importOriginal()), SENDER: sender }));

const IncomingRequest = Request<unknown, IncomingRequestCfProperties>;

// All three Functions share one signature, so one context factory fits them all.
const pagesContext = (request: Request) =>
  createPagesEventContext<typeof signupFunction>({
    request: request as InstanceType<typeof IncomingRequest>,
    params: {},
    data: {},
  });

interface SentEmail {
  from: string;
  to: string[];
  reply_to?: string;
  subject: string;
  text: string;
  html: string;
  headers: Record<string, string>;
}

let turnstileOutcome: { success: boolean; action?: string; "error-codes"?: string[] } | "down";
let resendStatus: number;
let sentEmails: SentEmail[];
let turnstileCalls: { secret: string; response: string; remoteip?: string }[];

beforeEach(() => {
  turnstileOutcome = { success: true, action: "waitlist" };
  resendStatus = 200;
  sentEmails = [];
  turnstileCalls = [];
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const url = input instanceof Request ? input.url : String(input);
    const body = JSON.parse(String(init?.body ?? "{}"));
    if (url === "https://challenges.cloudflare.com/turnstile/v0/siteverify") {
      turnstileCalls.push(body);
      if (turnstileOutcome === "down") return new Response("bad gateway", { status: 502 });
      return Response.json(turnstileOutcome);
    }
    if (url === "https://api.resend.com/emails") {
      expect(new Headers(init?.headers).get("authorization")).toBe("Bearer test-resend-key");
      expect(new Headers(init?.headers).get("idempotency-key")).toMatch(/^waitlist-confirm\//);
      if (resendStatus === 200) sentEmails.push(body);
      return Response.json({ id: "email_123" }, { status: resendStatus });
    }
    throw new Error(`unexpected fetch to ${url}`);
  });
});

afterEach(() => {
  vi.restoreAllMocks();
  vi.useRealTimers();
});

const IP = "203.0.113.7";

function signupBody(overrides: Record<string, unknown> = {}) {
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

function jsonRequest(path: string, body: unknown, headers: Record<string, string> = {}) {
  return new IncomingRequest(`https://headmesh.dev${path}`, {
    method: "POST",
    headers: { "content-type": "application/json", "cf-connecting-ip": IP, ...headers },
    body: JSON.stringify(body),
  });
}

const signup = (body: unknown, headers?: Record<string, string>) =>
  signupFunction(pagesContext(jsonRequest("/api/waitlist", body, headers)));

const confirm = (token: string) => confirmFunction(pagesContext(jsonRequest("/api/waitlist/confirm", { token })));

const unsubscribe = (token: string) =>
  unsubscribeFunction(pagesContext(jsonRequest("/api/waitlist/unsubscribe", { token })));

function rows() {
  return env.DB.prepare("SELECT * FROM waitlist ORDER BY created_at").all<Record<string, string | null>>();
}

/** Pull the confirm and unsubscribe tokens out of the last email sent. */
function tokensFromLastEmail() {
  const email = sentEmails.at(-1);
  if (!email) throw new Error("no email sent");
  const confirmToken = /\/waitlist\/confirm\/#token=([^\s"]+)/.exec(email.text)?.[1];
  const unsubscribeToken = /\/waitlist\/unsubscribe\/#token=([^\s"]+)/.exec(email.text)?.[1];
  if (!confirmToken || !unsubscribeToken) throw new Error("tokens missing from email");
  return { confirmToken, unsubscribeToken };
}

describe("POST /api/waitlist", () => {
  it("stores a normalised sign-up and sends a CASL-compliant double opt-in email", async () => {
    const response = await signup(signupBody());
    expect(response.status).toBe(202);
    expect(await response.json()).toEqual({ ok: true });
    expect(response.headers.get("cache-control")).toBe("no-store");

    const { results } = await rows();
    expect(results).toHaveLength(1);
    const row = results[0];
    expect(row).toMatchObject({
      email: "ada@example.com",
      use_case: "Sharing GPUs with renters",
      source: "news.ycombinator.com",
      consent_version: CONSENT_VERSION,
      confirmed_at: null,
      unsubscribed_at: null,
    });
    expect(JSON.parse(row?.utm ?? "")).toEqual({ utm_source: "hn", utm_campaign: "launch" });
    expect(row?.ip_hash).toMatch(/^[A-Za-z0-9_-]{32}$/);
    expect(row?.ip_hash).not.toContain("203");
    expect(row?.confirm_sent_at).not.toBeNull();

    expect(turnstileCalls).toEqual([
      expect.objectContaining({ secret: "test-turnstile-secret", response: "token-from-widget", remoteip: IP }),
    ]);

    expect(sentEmails).toHaveLength(1);
    const email = sentEmails[0];
    expect(email?.to).toEqual(["ada@example.com"]);
    expect(email?.from).toBe("Headmesh <waitlist@mail.headmesh.dev>");
    expect(email?.reply_to).toBe("hello@headmesh.dev");
    // Sender identification and an unsubscribe mechanism, in both parts.
    for (const part of [email?.text, email?.html]) {
      expect(part).toContain("Xcelsior Compute Inc., 1 Test Street, Toronto ON M5V 0A1, Canada");
      expect(part).toContain("hello@headmesh.dev");
      expect(part).toContain("https://headmesh.dev/waitlist/unsubscribe/#token=");
      expect(part).toContain("https://headmesh.dev/waitlist/confirm/#token=");
      expect(part).not.toContain("ada@example.com");
    }
    expect(email?.headers["List-Unsubscribe-Post"]).toBe("List-Unsubscribe=One-Click");
    expect(email?.headers["List-Unsubscribe"]).toMatch(
      /^<https:\/\/headmesh\.dev\/api\/waitlist\/unsubscribe\?token=v1\./,
    );
  });

  it("answers repeat sign-ups identically without leaking membership or spamming", async () => {
    await signup(signupBody());
    const again = await signup(signupBody({ email: "ada@example.com" }));
    expect(again.status).toBe(202);
    expect(await again.json()).toEqual({ ok: true });
    expect((await rows()).results).toHaveLength(1);
    // Still inside the re-send window: no second email.
    expect(sentEmails).toHaveLength(1);

    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(Date.now() + RESEND_CONFIRMATION_AFTER_MS + 1000);
    await signup(signupBody());
    expect(sentEmails).toHaveLength(2);

    // Once confirmed, nothing more is sent.
    const { confirmToken } = tokensFromLastEmail();
    expect((await confirm(confirmToken)).status).toBe(200);
    vi.setSystemTime(Date.now() + RESEND_CONFIRMATION_AFTER_MS + 1000);
    const confirmedAgain = await signup(signupBody());
    expect(confirmedAgain.status).toBe(202);
    expect(sentEmails).toHaveLength(2);
  });

  it.each([
    [{ email: "not-an-email" }, "email"],
    [{ email: `${"a".repeat(250)}@example.com` }, "email"],
    [{ consent: false }, "consent"],
    [{ consent: "on" }, "consent"],
    [{ consent_version: "1999-01-01" }, "consent_version"],
    [{ use_case: "x".repeat(501) }, "use_case"],
  ])("rejects %j with a field error", async (overrides, field) => {
    const response = await signup(signupBody(overrides));
    expect(response.status).toBe(400);
    const body = (await response.json()) as { error: string; fields: Record<string, string> };
    expect(body.error).toBe("invalid_request");
    expect(body.fields[field]).toEqual(expect.any(String));
    expect(turnstileCalls).toHaveLength(0);
    expect((await rows()).results).toHaveLength(0);
  });

  it.each([[{ source: "Evil Source!" }], [{ utm: { utm_source: "x", not_utm: "y" } }], [{ turnstile_token: "" }]])(
    "rejects malformed tracking or token fields %j",
    async (overrides) => {
      expect((await signup(signupBody(overrides))).status).toBe(400);
    },
  );

  it("rejects non-JSON and oversized bodies", async () => {
    const form = new IncomingRequest("https://headmesh.dev/api/waitlist", {
      method: "POST",
      headers: { "content-type": "application/x-www-form-urlencoded" },
      body: "email=ada%40example.com",
    });
    expect((await signupFunction(pagesContext(form))).status).toBe(415);
    expect((await signup(signupBody({ use_case: "x".repeat(9000) }))).status).toBe(413);

    const broken = new IncomingRequest("https://headmesh.dev/api/waitlist", {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: "{not json",
    });
    expect((await signupFunction(pagesContext(broken))).status).toBe(400);
  });

  it("refuses sign-ups that fail Turnstile, and tokens minted for another action", async () => {
    turnstileOutcome = { success: false, "error-codes": ["invalid-input-response"] };
    const failed = await signup(signupBody());
    expect(failed.status).toBe(403);
    expect(await failed.json()).toEqual({ ok: false, error: "turnstile_failed" });

    turnstileOutcome = { success: true, action: "login" };
    expect((await signup(signupBody())).status).toBe(403);

    turnstileOutcome = "down";
    expect((await signup(signupBody())).status).toBe(503);

    expect((await rows()).results).toHaveLength(0);
    expect(sentEmails).toHaveLength(0);
  });

  it("rate-limits by hashed IP", async () => {
    for (let i = 0; i < SIGNUPS_PER_IP_PER_HOUR; i++) {
      expect((await signup(signupBody({ email: `person${i}@example.com` }))).status).toBe(202);
    }
    const limited = await signup(signupBody({ email: "one-too-many@example.com" }));
    expect(limited.status).toBe(429);
    expect(limited.headers.get("retry-after")).toBe("3600");
    // A different address isn't affected.
    expect(
      (await signup(signupBody({ email: "elsewhere@example.com" }), { "cf-connecting-ip": "198.51.100.1" })).status,
    ).toBe(202);
  });

  it("reports email delivery failures and lets the person retry", async () => {
    resendStatus = 500;
    const failed = await signup(signupBody());
    expect(failed.status).toBe(503);
    expect((await rows()).results[0]?.confirm_sent_at).toBeNull();

    resendStatus = 200;
    expect((await signup(signupBody())).status).toBe(202);
    expect(sentEmails).toHaveLength(1);
  });

  it("fails closed when CASL sender identification is missing", async () => {
    sender.postalAddress = "";
    try {
      const response = await signup(signupBody());
      expect(response.status).toBe(503);
      expect(turnstileCalls).toHaveLength(0);
      expect(sentEmails).toHaveLength(0);
    } finally {
      sender.postalAddress = "1 Test Street, Toronto ON M5V 0A1, Canada";
    }
  });

  it("logs links instead of emailing in preview deployments, using the request origin", async () => {
    const info = vi.spyOn(console, "info").mockImplementation(() => {});
    const previewEnv: Env = { ...env, EMAIL_DELIVERY: "log", SITE_URL: "", RESEND_API_KEY: "" };
    const request = new IncomingRequest("https://abc123.headmesh-marketing.pages.dev/api/waitlist", {
      method: "POST",
      headers: { "content-type": "application/json", "cf-connecting-ip": IP },
      body: JSON.stringify(signupBody()),
    });
    expect((await handleSignup(request, previewEnv)).status).toBe(202);
    expect(sentEmails).toHaveLength(0);
    const logged = info.mock.calls.map(([line]) => String(line)).join("\n");
    expect(logged).toContain("https://abc123.headmesh-marketing.pages.dev/waitlist/confirm/#token=v1.");
    expect(logged).not.toContain("ada@example.com");
  });
});

describe("POST /api/waitlist/confirm", () => {
  it("confirms once, idempotently", async () => {
    await signup(signupBody());
    const { confirmToken } = tokensFromLastEmail();

    const first = await confirm(confirmToken);
    expect(first.status).toBe(200);
    const confirmedAt = (await rows()).results[0]?.confirmed_at;
    expect(confirmedAt).toEqual(expect.any(String));

    expect((await confirm(confirmToken)).status).toBe(200);
    expect((await rows()).results[0]?.confirmed_at).toBe(confirmedAt);
  });

  it("rejects forged, expired and unsubscribe tokens", async () => {
    await signup(signupBody());
    const { confirmToken, unsubscribeToken } = tokensFromLastEmail();

    for (const token of [`${confirmToken}x`, unsubscribeToken, "v1.garbage"]) {
      const response = await confirm(token);
      expect(response.status).toBe(400);
      expect(await response.json()).toEqual({ ok: false, error: "invalid_token" });
    }

    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(Date.now() + 8 * 24 * 60 * 60 * 1000);
    expect((await confirm(confirmToken)).status).toBe(400);
    expect((await rows()).results[0]?.confirmed_at).toBeNull();
  });

  it("can't re-subscribe someone who has unsubscribed", async () => {
    await signup(signupBody());
    const { confirmToken, unsubscribeToken } = tokensFromLastEmail();
    await unsubscribe(unsubscribeToken);
    expect((await confirm(confirmToken)).status).toBe(400);
    expect((await rows()).results[0]?.confirmed_at).toBeNull();
  });
});

describe("POST /api/waitlist/unsubscribe", () => {
  it("unsubscribes from the page (JSON) and stays unsubscribed", async () => {
    await signup(signupBody());
    const { confirmToken, unsubscribeToken } = tokensFromLastEmail();
    await confirm(confirmToken);

    const response = await unsubscribe(unsubscribeToken);
    expect(response.status).toBe(200);
    const row = (await rows()).results[0];
    expect(row?.unsubscribed_at).toEqual(expect.any(String));

    // Twice is fine and keeps the original timestamp.
    await unsubscribe(unsubscribeToken);
    expect((await rows()).results[0]?.unsubscribed_at).toBe(row?.unsubscribed_at);
  });

  it("supports RFC 8058 one-click unsubscribe from mail providers", async () => {
    await signup(signupBody());
    const oneClick = sentEmails[0]?.headers["List-Unsubscribe"]?.slice(1, -1) ?? "";
    const request = new IncomingRequest(oneClick, {
      method: "POST",
      headers: { "content-type": "application/x-www-form-urlencoded" },
      body: "List-Unsubscribe=One-Click",
    });
    const response = await unsubscribeFunction(pagesContext(request));
    expect(response.status).toBe(200);
    expect((await rows()).results[0]?.unsubscribed_at).toEqual(expect.any(String));
  });

  it("rejects forged tokens", async () => {
    const response = await unsubscribe("v1.4f8e2a8c-3b2d-4c1e-9a7f-0123456789ab.AAAA");
    expect(response.status).toBe(400);
  });

  it("treats a new sign-up after unsubscribing as fresh consent that needs confirming again", async () => {
    await signup(signupBody());
    const first = tokensFromLastEmail();
    await confirm(first.confirmToken);
    await unsubscribe(first.unsubscribeToken);

    expect((await signup(signupBody({ use_case: "Changed my mind" }))).status).toBe(202);
    const row = (await rows()).results[0];
    expect(row).toMatchObject({ unsubscribed_at: null, confirmed_at: null, use_case: "Changed my mind" });
    expect(sentEmails).toHaveLength(2);

    const second = tokensFromLastEmail();
    expect((await confirm(second.confirmToken)).status).toBe(200);
    expect((await rows()).results[0]?.confirmed_at).toEqual(expect.any(String));
  });
});
