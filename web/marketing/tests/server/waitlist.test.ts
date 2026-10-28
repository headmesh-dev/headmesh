import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { handle } from "../../server/app.ts";
import { RESEND_CONFIRMATION_AFTER_MS, SIGNUPS_PER_IP_PER_HOUR } from "../../server/waitlist.ts";
import { CONSENT_VERSION } from "../../shared/site.ts";
import { createContext, IP, rows, signupBody, stubUpstreams, tokensFromLastEmail, type Upstreams } from "./helpers.ts";

// The committed SENDER has no postal address yet (it must be filled in before launch); tests supply one.
const sender = vi.hoisted(() => ({
  legalName: "Xcelsior Compute Inc.",
  postalAddress: "1 Test Street, Toronto ON M5V 0A1, Canada",
  email: "hello@headmesh.dev",
  website: "https://headmesh.dev",
}));
vi.mock(import("../../shared/site.ts"), async (importOriginal) => ({ ...(await importOriginal()), SENDER: sender }));

let upstreams: Upstreams;
let ctx: ReturnType<typeof createContext>;

beforeEach(() => {
  upstreams = stubUpstreams();
  ctx = createContext();
});

afterEach(() => {
  vi.restoreAllMocks();
  vi.useRealTimers();
  ctx.db.close();
});

function jsonRequest(path: string, body: unknown, headers: Record<string, string> = {}) {
  return new Request(`http://127.0.0.1${path}`, {
    method: "POST",
    headers: { "content-type": "application/json", "x-real-ip": IP, ...headers },
    body: JSON.stringify(body),
  });
}

const signup = (body: unknown, headers?: Record<string, string>) =>
  handle(jsonRequest("/api/waitlist", body, headers), ctx);
const confirm = (token: string) => handle(jsonRequest("/api/waitlist/confirm", { token }), ctx);
const unsubscribe = (token: string) => handle(jsonRequest("/api/waitlist/unsubscribe", { token }), ctx);

describe("POST /api/waitlist", () => {
  it("stores a normalised sign-up and sends a CASL-compliant double opt-in email", async () => {
    const response = await signup(signupBody());
    expect(response.status).toBe(202);
    expect(await response.json()).toEqual({ ok: true });
    expect(response.headers.get("cache-control")).toBe("no-store");

    const all = rows(ctx.db);
    expect(all).toHaveLength(1);
    const row = all[0];
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

    expect(upstreams.turnstileCalls).toEqual([
      expect.objectContaining({ secret: "test-turnstile-secret", response: "token-from-widget", remoteip: IP }),
    ]);

    expect(upstreams.sent).toHaveLength(1);
    const email = upstreams.sent[0];
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
    expect(rows(ctx.db)).toHaveLength(1);
    // Still inside the re-send window: no second email.
    expect(upstreams.sent).toHaveLength(1);

    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(Date.now() + RESEND_CONFIRMATION_AFTER_MS + 1000);
    await signup(signupBody());
    expect(upstreams.sent).toHaveLength(2);

    // Once confirmed, nothing more is sent.
    const { confirmToken } = tokensFromLastEmail(upstreams);
    expect((await confirm(confirmToken)).status).toBe(200);
    vi.setSystemTime(Date.now() + RESEND_CONFIRMATION_AFTER_MS + 1000);
    expect((await signup(signupBody())).status).toBe(202);
    expect(upstreams.sent).toHaveLength(2);
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
    expect(upstreams.turnstileCalls).toHaveLength(0);
    expect(rows(ctx.db)).toHaveLength(0);
  });

  it.each([[{ source: "Evil Source!" }], [{ utm: { utm_source: "x", not_utm: "y" } }], [{ turnstile_token: "" }]])(
    "rejects malformed tracking or token fields %j",
    async (overrides) => {
      expect((await signup(signupBody(overrides))).status).toBe(400);
    },
  );

  it("rejects non-JSON and oversized bodies", async () => {
    const form = new Request("http://127.0.0.1/api/waitlist", {
      method: "POST",
      headers: { "content-type": "application/x-www-form-urlencoded" },
      body: "email=ada%40example.com",
    });
    expect((await handle(form, ctx)).status).toBe(415);
    expect((await signup(signupBody({ use_case: "x".repeat(9000) }))).status).toBe(413);

    const broken = new Request("http://127.0.0.1/api/waitlist", {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: "{not json",
    });
    expect((await handle(broken, ctx)).status).toBe(400);
  });

  it("refuses sign-ups that fail Turnstile, and tokens minted for another action", async () => {
    upstreams.turnstile = { success: false, "error-codes": ["invalid-input-response"] };
    const failed = await signup(signupBody());
    expect(failed.status).toBe(403);
    expect(await failed.json()).toEqual({ ok: false, error: "turnstile_failed" });

    upstreams.turnstile = { success: true, action: "login" };
    expect((await signup(signupBody())).status).toBe(403);

    upstreams.turnstile = "down";
    expect((await signup(signupBody())).status).toBe(503);

    expect(rows(ctx.db)).toHaveLength(0);
    expect(upstreams.sent).toHaveLength(0);
  });

  it("rate-limits by hashed IP", async () => {
    for (let i = 0; i < SIGNUPS_PER_IP_PER_HOUR; i++) {
      expect((await signup(signupBody({ email: `person${i}@example.com` }))).status).toBe(202);
    }
    const limited = await signup(signupBody({ email: "one-too-many@example.com" }));
    expect(limited.status).toBe(429);
    expect(limited.headers.get("retry-after")).toBe("3600");
    // A different address isn't affected.
    const elsewhere = await signup(signupBody({ email: "elsewhere@example.com" }), { "x-real-ip": "198.51.100.1" });
    expect(elsewhere.status).toBe(202);
  });

  it("reports email delivery failures and lets the person retry", async () => {
    upstreams.resendStatus = 500;
    expect((await signup(signupBody())).status).toBe(503);
    expect(rows(ctx.db)[0]?.confirm_sent_at).toBeNull();

    upstreams.resendStatus = 200;
    expect((await signup(signupBody())).status).toBe(202);
    expect(upstreams.sent).toHaveLength(1);
  });

  it("fails closed when CASL sender identification is missing", async () => {
    sender.postalAddress = "";
    try {
      expect((await signup(signupBody())).status).toBe(503);
      expect(upstreams.turnstileCalls).toHaveLength(0);
      expect(upstreams.sent).toHaveLength(0);
    } finally {
      sender.postalAddress = "1 Test Street, Toronto ON M5V 0A1, Canada";
    }
  });

  it("logs links instead of emailing in the test environment", async () => {
    const info = vi.spyOn(console, "info").mockImplementation(() => {});
    ctx = createContext({ emailDelivery: "log", resendApiKey: undefined, siteUrl: "https://test.headmesh.dev" });
    expect((await signup(signupBody())).status).toBe(202);
    expect(upstreams.sent).toHaveLength(0);
    const logged = info.mock.calls.map(([line]) => String(line)).join("\n");
    expect(logged).toContain("https://test.headmesh.dev/waitlist/confirm/#token=v1.");
    expect(logged).not.toContain("ada@example.com");
  });
});

describe("POST /api/waitlist/confirm", () => {
  it("confirms once, idempotently", async () => {
    await signup(signupBody());
    const { confirmToken } = tokensFromLastEmail(upstreams);

    expect((await confirm(confirmToken)).status).toBe(200);
    const confirmedAt = rows(ctx.db)[0]?.confirmed_at;
    expect(confirmedAt).toEqual(expect.any(String));

    expect((await confirm(confirmToken)).status).toBe(200);
    expect(rows(ctx.db)[0]?.confirmed_at).toBe(confirmedAt);
  });

  it("rejects forged, expired and unsubscribe tokens", async () => {
    await signup(signupBody());
    const { confirmToken, unsubscribeToken } = tokensFromLastEmail(upstreams);

    for (const token of [`${confirmToken}x`, unsubscribeToken, "v1.garbage"]) {
      const response = await confirm(token);
      expect(response.status).toBe(400);
      expect(await response.json()).toEqual({ ok: false, error: "invalid_token" });
    }

    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(Date.now() + 8 * 24 * 60 * 60 * 1000);
    expect((await confirm(confirmToken)).status).toBe(400);
    expect(rows(ctx.db)[0]?.confirmed_at).toBeNull();
  });

  it("can't re-subscribe someone who has unsubscribed", async () => {
    await signup(signupBody());
    const { confirmToken, unsubscribeToken } = tokensFromLastEmail(upstreams);
    await unsubscribe(unsubscribeToken);
    expect((await confirm(confirmToken)).status).toBe(400);
    expect(rows(ctx.db)[0]?.confirmed_at).toBeNull();
  });
});

describe("POST /api/waitlist/unsubscribe", () => {
  it("unsubscribes from the page (JSON) and stays unsubscribed", async () => {
    await signup(signupBody());
    const { confirmToken, unsubscribeToken } = tokensFromLastEmail(upstreams);
    await confirm(confirmToken);

    expect((await unsubscribe(unsubscribeToken)).status).toBe(200);
    const unsubscribedAt = rows(ctx.db)[0]?.unsubscribed_at;
    expect(unsubscribedAt).toEqual(expect.any(String));

    // Twice is fine and keeps the original timestamp.
    expect((await unsubscribe(unsubscribeToken)).status).toBe(200);
    expect(rows(ctx.db)[0]?.unsubscribed_at).toBe(unsubscribedAt);
  });

  it("supports RFC 8058 one-click unsubscribe from mail providers", async () => {
    await signup(signupBody());
    const oneClick = upstreams.sent[0]?.headers["List-Unsubscribe"]?.slice(1, -1) ?? "";
    const { pathname, search } = new URL(oneClick);
    const request = new Request(`http://127.0.0.1${pathname}${search}`, {
      method: "POST",
      headers: { "content-type": "application/x-www-form-urlencoded" },
      body: "List-Unsubscribe=One-Click",
    });
    expect((await handle(request, ctx)).status).toBe(200);
    expect(rows(ctx.db)[0]?.unsubscribed_at).toEqual(expect.any(String));
  });

  it("rejects forged tokens", async () => {
    expect((await unsubscribe("v1.4f8e2a8c-3b2d-4c1e-9a7f-0123456789ab.AAAA")).status).toBe(400);
  });

  it("treats a new sign-up after unsubscribing as fresh consent that needs confirming again", async () => {
    await signup(signupBody());
    const first = tokensFromLastEmail(upstreams);
    await confirm(first.confirmToken);
    await unsubscribe(first.unsubscribeToken);

    expect((await signup(signupBody({ use_case: "Changed my mind" }))).status).toBe(202);
    expect(rows(ctx.db)[0]).toMatchObject({ unsubscribed_at: null, confirmed_at: null, use_case: "Changed my mind" });
    expect(upstreams.sent).toHaveLength(2);

    const second = tokensFromLastEmail(upstreams);
    expect((await confirm(second.confirmToken)).status).toBe(200);
    expect(rows(ctx.db)[0]?.confirmed_at).toEqual(expect.any(String));
  });
});

describe("routing", () => {
  it("answers health checks and rejects unknown paths and methods", async () => {
    expect((await handle(new Request("http://127.0.0.1/healthz"), ctx)).status).toBe(204);
    expect((await handle(new Request("http://127.0.0.1/readyz"), ctx)).status).toBe(204);
    expect((await handle(new Request("http://127.0.0.1/api/nope", { method: "POST" }), ctx)).status).toBe(404);
    const get = await handle(new Request("http://127.0.0.1/api/waitlist"), ctx);
    expect(get.status).toBe(405);
    expect(get.headers.get("allow")).toBe("POST");
  });

  it("reports not ready when the database is gone", async () => {
    vi.spyOn(console, "error").mockImplementation(() => {});
    ctx.db.close();
    expect((await handle(new Request("http://127.0.0.1/readyz"), ctx)).status).toBe(503);
    ctx = createContext();
  });
});
