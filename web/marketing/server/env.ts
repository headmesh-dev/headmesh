/** Bindings and settings for the waitlist Functions. Declared in wrangler.toml (vars, D1) or as Pages secrets. */
export interface Env {
  DB: D1Database;
  /** Secret: Turnstile widget secret key. */
  TURNSTILE_SECRET_KEY: string;
  /** Secret: Resend API key with "sending access" to mail.headmesh.dev only. */
  RESEND_API_KEY?: string;
  /** Secret: at least 32 random characters. Keys the confirm/unsubscribe token HMACs and the IP hash. */
  WAITLIST_SECRET: string;
  /** Origin used in email links. Empty means "the origin the request came in on" (preview deployments). */
  SITE_URL?: string;
  /** RFC 5322 From header, on the Resend-verified domain. */
  EMAIL_FROM: string;
  EMAIL_REPLY_TO?: string;
  /** "resend" sends real email; "log" writes the links to the Function log instead (previews, local dev). */
  EMAIL_DELIVERY?: "resend" | "log";
}
