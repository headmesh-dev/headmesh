import { SENDER } from "../shared/site.ts";
import type { Config } from "./config.ts";
import { hashIp, signToken, verifyToken } from "./crypto.ts";
import { fail, HttpError, log, ok, readJson, readText } from "./http.ts";
import { sendWithResend } from "./resend.ts";
import { fieldErrors, signupSchema, tokenSchema } from "./schema.ts";
import type { WaitlistStore } from "./store.ts";
import { verifyTurnstile } from "./turnstile.ts";

/** What the handlers need: settings and storage. */
export interface WaitlistContext {
  config: Config;
  store: WaitlistStore;
}

/** Sign-ups accepted from one IP address per hour before it gets 429s. */
export const SIGNUPS_PER_IP_PER_HOUR = 10;
/** A repeat sign-up for an unconfirmed address re-sends the confirmation at most this often. */
export const RESEND_CONFIRMATION_AFTER_MS = 10 * 60 * 1000;

const nowIso = () => new Date().toISOString();
const nowSeconds = () => Math.floor(Date.now() / 1000);

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
export function handleSignup(request: Request, { config, store }: WaitlistContext): Promise<Response> {
  return handle("waitlist.signup", async () => {
    const parsed = signupSchema.safeParse(await readJson(request));
    if (!parsed.success) return fail("invalid_request", fieldErrors(parsed.error));
    const signup = parsed.data;

    const delivery = config.emailDelivery;
    if (delivery === "resend" && (!SENDER.postalAddress || !config.resendApiKey || !config.confirmationTemplateId)) {
      // Fail closed: never send a consent request that CASL wouldn't accept.
      log("error", "waitlist.signup.misconfigured", {
        missing: !SENDER.postalAddress
          ? "SENDER.postalAddress"
          : !config.resendApiKey
            ? "RESEND_API_KEY"
            : "RESEND_CONFIRMATION_TEMPLATE_ID",
      });
      return fail("unavailable");
    }

    const ip = request.headers.get(config.clientIpHeader);
    const turnstile = await verifyTurnstile(config.turnstileSecretKey, signup.turnstile_token, ip, crypto.randomUUID());
    if (!turnstile.ok) {
      log("warn", "waitlist.signup.turnstile", { reason: turnstile.reason, codes: turnstile.codes });
      return fail(turnstile.reason === "unavailable" ? "unavailable" : "turnstile_failed");
    }

    const ipHash = ip ? await hashIp(config.waitlistSecret, ip) : null;
    const hourAgo = new Date(Date.now() - 60 * 60 * 1000).toISOString();
    if (ipHash && store.recentSignups(ipHash, hourAgo) >= SIGNUPS_PER_IP_PER_HOUR) {
      log("warn", "waitlist.signup.rate_limited");
      return fail("rate_limited", undefined, { "retry-after": "3600" });
    }

    const row = store.upsert(signup, ipHash, nowIso());
    const confirmed = row.confirmed_at !== null && row.unsubscribed_at === null;
    const recentlySent =
      row.confirm_sent_at !== null && Date.now() - Date.parse(row.confirm_sent_at) < RESEND_CONFIRMATION_AFTER_MS;

    // Every accepted request gets the same answer, so the endpoint can't be used to learn who's on the list.
    if (confirmed || recentlySent) return ok(202);

    const origin = config.siteUrl;
    const confirmationAttempt = Math.floor(Date.now() / RESEND_CONFIRMATION_AFTER_MS);
    // Stable token timestamps keep retries in the same idempotency window byte-identical.
    const issuedAt = Math.floor((confirmationAttempt * RESEND_CONFIRMATION_AFTER_MS) / 1000);
    const [confirmToken, unsubscribeToken] = await Promise.all([
      signToken(config.waitlistSecret, "confirm", row.id, issuedAt),
      signToken(config.waitlistSecret, "unsubscribe", row.id, issuedAt),
    ]);
    const links = {
      confirm: `${origin}/waitlist/confirm/#token=${confirmToken}`,
      unsubscribe: `${origin}/waitlist/unsubscribe/#token=${unsubscribeToken}`,
      site: origin,
    };

    if (delivery === "log") {
      log("info", "waitlist.signup.email_logged", { links });
    } else {
      if (!config.resendApiKey || !config.confirmationTemplateId) return fail("unavailable");
      const sent = await sendWithResend(config.resendApiKey, {
        from: config.emailFrom,
        to: signup.email,
        ...(config.emailReplyTo && { replyTo: config.emailReplyTo }),
        templateId: config.confirmationTemplateId,
        variables: { CONFIRM_URL: links.confirm, UNSUBSCRIBE_URL: links.unsubscribe, SITE_URL: links.site },
        oneClickUnsubscribeUrl: `${origin}/api/waitlist/unsubscribe?token=${encodeURIComponent(unsubscribeToken)}`,
        idempotencyKey: `waitlist-confirm/${row.id}/${confirmationAttempt}`,
      });
      if (!sent.ok) {
        log("error", "waitlist.signup.email_failed", { status: sent.status });
        return fail("unavailable");
      }
      log("info", "waitlist.signup.email_sent", { emailId: sent.id });
    }

    store.markConfirmSent(row.id, nowIso());
    log("info", "waitlist.signup.accepted", { source: signup.source ?? null });
    return ok(202);
  });
}

/** POST /api/waitlist/confirm  { token } */
export function handleConfirm(request: Request, { config, store }: WaitlistContext): Promise<Response> {
  return handle("waitlist.confirm", async () => {
    const parsed = tokenSchema.safeParse(await readJson(request));
    if (!parsed.success) return fail("invalid_token");
    const id = await verifyToken(config.waitlistSecret, "confirm", parsed.data.token, nowSeconds());
    if (!id) return fail("invalid_token");

    if (!store.confirm(id, nowIso())) return fail("invalid_token");
    log("info", "waitlist.confirm.accepted");
    return ok();
  });
}

/**
 * POST /api/waitlist/unsubscribe
 * Accepts the token as JSON ({ token }, from the unsubscribe page) or in the query string with any body
 * (RFC 8058 one-click: mail providers POST "List-Unsubscribe=One-Click" as a form).
 */
export function handleUnsubscribe(request: Request, { config, store }: WaitlistContext): Promise<Response> {
  return handle("waitlist.unsubscribe", async () => {
    let token = new URL(request.url).searchParams.get("token");
    if (!token) {
      const parsed = tokenSchema.safeParse(await readJson(request));
      if (!parsed.success) return fail("invalid_token");
      token = parsed.data.token;
    } else {
      await readText(request);
    }

    const id = await verifyToken(config.waitlistSecret, "unsubscribe", token, nowSeconds());
    if (!id) return fail("invalid_token");

    store.unsubscribe(id, nowIso());
    log("info", "waitlist.unsubscribe.accepted");
    return ok();
  });
}
