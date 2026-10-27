import { SENDER } from "../shared/site";
import { hashIp, signToken, verifyToken } from "./crypto";
import { confirmationEmail } from "./emails";
import type { Env } from "./env";
import { fail, HttpError, log, ok, readJson, readText } from "./http";
import { sendWithResend } from "./resend";
import { fieldErrors, type Signup, signupSchema, tokenSchema } from "./schema";
import { verifyTurnstile } from "./turnstile";

/** Sign-ups accepted from one IP address per hour before it gets 429s. */
export const SIGNUPS_PER_IP_PER_HOUR = 10;
/** A repeat sign-up for an unconfirmed address re-sends the confirmation at most this often. */
export const RESEND_CONFIRMATION_AFTER_MS = 10 * 60 * 1000;

interface Row {
  id: string;
  confirmed_at: string | null;
  unsubscribed_at: string | null;
  confirm_sent_at: string | null;
}

const nowIso = () => new Date().toISOString();
const nowSeconds = () => Math.floor(Date.now() / 1000);

function siteOrigin(request: Request, env: Env): string {
  return (env.SITE_URL || new URL(request.url).origin).replace(/\/$/, "");
}

/** Wrap a handler so thrown HttpErrors become their responses and anything else is a logged 503. */
async function handle(event: string, run: () => Promise<Response>): Promise<Response> {
  try {
    return await run();
  } catch (error) {
    if (error instanceof HttpError) return error.response;
    log("error", `${event}.failed`, { message: error instanceof Error ? error.message : String(error) });
    return fail("unavailable");
  }
}

/** POST /api/waitlist */
export function handleSignup(request: Request, env: Env): Promise<Response> {
  return handle("waitlist.signup", async () => {
    const parsed = signupSchema.safeParse(await readJson(request));
    if (!parsed.success) return fail("invalid_request", fieldErrors(parsed.error));
    const signup = parsed.data;

    const delivery = env.EMAIL_DELIVERY ?? "resend";
    if (delivery === "resend" && (!SENDER.postalAddress || !env.RESEND_API_KEY)) {
      // Fail closed: never send a consent request that CASL wouldn't accept.
      log("error", "waitlist.signup.misconfigured", {
        missing: !SENDER.postalAddress ? "SENDER.postalAddress" : "RESEND_API_KEY",
      });
      return fail("unavailable");
    }

    const ip = request.headers.get("cf-connecting-ip");
    const turnstile = await verifyTurnstile(env.TURNSTILE_SECRET_KEY, signup.turnstile_token, ip, crypto.randomUUID());
    if (!turnstile.ok) {
      log("warn", "waitlist.signup.turnstile", { reason: turnstile.reason, codes: turnstile.codes });
      return fail(turnstile.reason === "unavailable" ? "unavailable" : "turnstile_failed");
    }

    const ipHash = ip ? await hashIp(env.WAITLIST_SECRET, ip) : null;
    if (ipHash && (await recentSignups(env.DB, ipHash)) >= SIGNUPS_PER_IP_PER_HOUR) {
      log("warn", "waitlist.signup.rate_limited");
      return fail("rate_limited", undefined, { "retry-after": "3600" });
    }

    const row = await upsert(env.DB, signup, ipHash);
    const confirmed = row.confirmed_at !== null && row.unsubscribed_at === null;
    const recentlySent =
      row.confirm_sent_at !== null && Date.now() - Date.parse(row.confirm_sent_at) < RESEND_CONFIRMATION_AFTER_MS;

    // Every accepted request gets the same answer, so the endpoint can't be used to learn who's on the list.
    if (confirmed || recentlySent) return ok(202);

    const origin = siteOrigin(request, env);
    const [confirmToken, unsubscribeToken] = await Promise.all([
      signToken(env.WAITLIST_SECRET, "confirm", row.id, nowSeconds()),
      signToken(env.WAITLIST_SECRET, "unsubscribe", row.id, nowSeconds()),
    ]);
    const links = {
      confirm: `${origin}/waitlist/confirm/#token=${confirmToken}`,
      unsubscribe: `${origin}/waitlist/unsubscribe/#token=${unsubscribeToken}`,
      site: origin,
    };

    if (delivery === "log") {
      log("info", "waitlist.signup.email_logged", { links });
    } else {
      const email = confirmationEmail(links);
      const sent = await sendWithResend(env.RESEND_API_KEY ?? "", {
        from: env.EMAIL_FROM,
        to: signup.email,
        ...(env.EMAIL_REPLY_TO && { replyTo: env.EMAIL_REPLY_TO }),
        ...email,
        oneClickUnsubscribeUrl: `${origin}/api/waitlist/unsubscribe?token=${encodeURIComponent(unsubscribeToken)}`,
        idempotencyKey: `waitlist-confirm/${row.id}/${Math.floor(Date.now() / RESEND_CONFIRMATION_AFTER_MS)}`,
      });
      if (!sent.ok) {
        log("error", "waitlist.signup.email_failed", { status: sent.status });
        return fail("unavailable");
      }
    }

    await env.DB.prepare("UPDATE waitlist SET confirm_sent_at = ?1 WHERE id = ?2").bind(nowIso(), row.id).run();
    log("info", "waitlist.signup.accepted", { source: signup.source ?? null });
    return ok(202);
  });
}

async function recentSignups(db: D1Database, ipHash: string): Promise<number> {
  const since = new Date(Date.now() - 60 * 60 * 1000).toISOString();
  const result = await db
    .prepare("SELECT COUNT(*) AS n FROM waitlist WHERE ip_hash = ?1 AND created_at > ?2")
    .bind(ipHash, since)
    .first<{ n: number }>();
  return result?.n ?? 0;
}

/**
 * Insert a new sign-up, or refresh an existing one. Someone who unsubscribed and signs up again is giving fresh
 * consent: their row goes back to unconfirmed and they get a new confirmation email.
 */
async function upsert(db: D1Database, signup: Signup, ipHash: string | null): Promise<Row> {
  const now = nowIso();
  const utm = signup.utm && Object.keys(signup.utm).length > 0 ? JSON.stringify(signup.utm) : null;
  const row = await db
    .prepare(
      `INSERT INTO waitlist (id, email, use_case, source, utm, consent_version, ip_hash, created_at)
       VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8)
       ON CONFLICT (email) DO UPDATE SET
         use_case = coalesce(excluded.use_case, waitlist.use_case),
         consent_version = CASE WHEN waitlist.confirmed_at IS NULL OR waitlist.unsubscribed_at IS NOT NULL
                                THEN excluded.consent_version ELSE waitlist.consent_version END,
         confirmed_at = CASE WHEN waitlist.unsubscribed_at IS NOT NULL THEN NULL ELSE waitlist.confirmed_at END,
         confirm_sent_at = CASE WHEN waitlist.unsubscribed_at IS NOT NULL THEN NULL ELSE waitlist.confirm_sent_at END,
         unsubscribed_at = NULL
       RETURNING id, confirmed_at, unsubscribed_at, confirm_sent_at`,
    )
    .bind(
      crypto.randomUUID(),
      signup.email,
      signup.use_case ?? null,
      signup.source ?? null,
      utm,
      signup.consent_version,
      ipHash,
      now,
    )
    .first<Row>();
  if (!row) throw new Error("upsert returned no row");
  return row;
}

/** POST /api/waitlist/confirm  { token } */
export function handleConfirm(request: Request, env: Env): Promise<Response> {
  return handle("waitlist.confirm", async () => {
    const parsed = tokenSchema.safeParse(await readJson(request));
    if (!parsed.success) return fail("invalid_token");
    const id = await verifyToken(env.WAITLIST_SECRET, "confirm", parsed.data.token, nowSeconds());
    if (!id) return fail("invalid_token");

    const row = await env.DB.prepare(
      `UPDATE waitlist SET confirmed_at = coalesce(confirmed_at, ?1)
       WHERE id = ?2 AND unsubscribed_at IS NULL
       RETURNING id`,
    )
      .bind(nowIso(), id)
      .first<{ id: string }>();
    // Deleted or unsubscribed rows can't be confirmed by an old link.
    if (!row) return fail("invalid_token");
    log("info", "waitlist.confirm.accepted");
    return ok();
  });
}

/**
 * POST /api/waitlist/unsubscribe
 * Accepts the token as JSON ({ token }, from the unsubscribe page) or in the query string with any body
 * (RFC 8058 one-click: mail providers POST "List-Unsubscribe=One-Click" as a form).
 */
export function handleUnsubscribe(request: Request, env: Env): Promise<Response> {
  return handle("waitlist.unsubscribe", async () => {
    let token = new URL(request.url).searchParams.get("token");
    if (!token) {
      const parsed = tokenSchema.safeParse(await readJson(request));
      if (!parsed.success) return fail("invalid_token");
      token = parsed.data.token;
    } else {
      await readText(request);
    }

    const id = await verifyToken(env.WAITLIST_SECRET, "unsubscribe", token, nowSeconds());
    if (!id) return fail("invalid_token");

    // Idempotent: unsubscribing twice, or after the row was deleted, is still a success.
    await env.DB.prepare("UPDATE waitlist SET unsubscribed_at = coalesce(unsubscribed_at, ?1) WHERE id = ?2")
      .bind(nowIso(), id)
      .run();
    log("info", "waitlist.unsubscribe.accepted");
    return ok();
  });
}
