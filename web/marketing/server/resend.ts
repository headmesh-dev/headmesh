import { setTimeout as wait } from "node:timers/promises";
import { Resend } from "resend";

export interface OutgoingEmail {
  from: string;
  to: string;
  replyTo?: string;
  templateId: string;
  variables: { CONFIRM_URL: string; UNSUBSCRIBE_URL: string; SITE_URL: string };
  /** RFC 8058 one-click unsubscribe endpoint (POST). */
  oneClickUnsubscribeUrl: string;
  /** Resend deduplicates sends with the same key for 24 hours. */
  idempotencyKey: string;
}

/** Send a published template, retrying transient failures with the same payload and idempotency key. */
export async function sendWithResend(
  apiKey: string,
  email: OutgoingEmail,
): Promise<{ ok: true; status: number; id: string } | { ok: false; status: number }> {
  for (let attempt = 0; attempt < 3; attempt += 1) {
    let status = 0;
    let retryAfter: string | undefined;
    try {
      const { data, error, headers } = await new Resend(apiKey).emails.send(
        {
          from: email.from,
          to: [email.to],
          ...(email.replyTo && { replyTo: email.replyTo }),
          template: { id: email.templateId, variables: email.variables },
          headers: {
            "List-Unsubscribe": `<${email.oneClickUnsubscribeUrl}>`,
            "List-Unsubscribe-Post": "List-Unsubscribe=One-Click",
          },
        },
        { idempotencyKey: email.idempotencyKey, signal: AbortSignal.timeout(5000) },
      );
      if (!error) {
        if (!data?.id) return { ok: false, status: 0 };
        return { ok: true, status: 200, id: data.id };
      }
      status = error.statusCode ?? 0;
      retryAfter = headers?.["retry-after"];
    } catch {
      // Network failures may be retried; the idempotency key prevents a second accepted send.
    }
    if (attempt === 2 || (status !== 0 && status !== 429 && (status < 500 || status > 599))) {
      return { ok: false, status };
    }
    const seconds = retryAfter ? Number(retryAfter) : Number.NaN;
    const deadline = retryAfter ? Date.parse(retryAfter) : Number.NaN;
    const delay = Number.isFinite(seconds)
      ? seconds * 1000
      : Number.isFinite(deadline)
        ? Math.max(0, deadline - Date.now())
        : 500 * 2 ** attempt;
    // Longer provider backoffs are left to a later signup attempt; never retry before Retry-After.
    if (delay > 5000) return { ok: false, status };
    await wait(Math.max(0, delay));
  }
  return { ok: false, status: 0 };
}
