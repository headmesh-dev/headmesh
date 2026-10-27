const RESEND_URL = "https://api.resend.com/emails";

export interface OutgoingEmail {
  from: string;
  to: string;
  replyTo?: string;
  subject: string;
  text: string;
  html: string;
  /** RFC 8058 one-click unsubscribe endpoint (POST). */
  oneClickUnsubscribeUrl: string;
  /** Resend deduplicates sends with the same key for 24 hours. */
  idempotencyKey: string;
}

/** Send one email through Resend's HTTP API. Returns false on any failure; callers decide what to tell the user. */
export async function sendWithResend(apiKey: string, email: OutgoingEmail): Promise<{ ok: boolean; status: number }> {
  try {
    const response = await fetch(RESEND_URL, {
      method: "POST",
      headers: {
        authorization: `Bearer ${apiKey}`,
        "content-type": "application/json",
        "idempotency-key": email.idempotencyKey,
      },
      body: JSON.stringify({
        from: email.from,
        to: [email.to],
        ...(email.replyTo && { reply_to: email.replyTo }),
        subject: email.subject,
        text: email.text,
        html: email.html,
        headers: {
          "List-Unsubscribe": `<${email.oneClickUnsubscribeUrl}>`,
          "List-Unsubscribe-Post": "List-Unsubscribe=One-Click",
        },
      }),
    });
    return { ok: response.ok, status: response.status };
  } catch {
    return { ok: false, status: 0 };
  }
}
