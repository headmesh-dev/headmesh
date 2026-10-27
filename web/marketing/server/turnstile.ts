import { TURNSTILE_ACTION } from "../shared/site";

const SITEVERIFY_URL = "https://challenges.cloudflare.com/turnstile/v0/siteverify";

interface SiteverifyResponse {
  success: boolean;
  "error-codes"?: string[];
  action?: string;
  hostname?: string;
}

export type TurnstileResult = { ok: true } | { ok: false; reason: "rejected" | "unavailable"; codes: string[] };

/** Server-side Turnstile check (https://developers.cloudflare.com/turnstile/get-started/server-side-validation/). */
export async function verifyTurnstile(
  secret: string,
  token: string,
  remoteIp: string | null,
  idempotencyKey: string,
): Promise<TurnstileResult> {
  let response: Response;
  try {
    response = await fetch(SITEVERIFY_URL, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({
        secret,
        response: token,
        idempotency_key: idempotencyKey,
        ...(remoteIp && { remoteip: remoteIp }),
      }),
    });
  } catch {
    return { ok: false, reason: "unavailable", codes: ["fetch-failed"] };
  }
  if (!response.ok) return { ok: false, reason: "unavailable", codes: [`http-${response.status}`] };

  const result = (await response.json()) as SiteverifyResponse;
  if (!result.success) return { ok: false, reason: "rejected", codes: result["error-codes"] ?? [] };
  // The widget sets data-action="waitlist". Tokens minted for another action on the same site key don't count.
  // (Cloudflare's test secret keys return an empty action, which local and preview environments rely on.)
  if (result.action && result.action !== TURNSTILE_ACTION) {
    return { ok: false, reason: "rejected", codes: ["action-mismatch"] };
  }
  return { ok: true };
}
