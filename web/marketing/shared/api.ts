// Wire contract between the waitlist pages and the waitlist service under /api/waitlist.

export type ApiErrorCode =
  | "invalid_request"
  | "unsupported_media_type"
  | "payload_too_large"
  | "turnstile_failed"
  | "rate_limited"
  | "invalid_token"
  | "unavailable";

export type FieldErrors = Partial<Record<"email" | "use_case" | "consent" | "consent_version", string>>;

export type ApiResponse = { ok: true } | { ok: false; error: ApiErrorCode; fields?: FieldErrors };

export interface SignupRequest {
  email: string;
  use_case?: string;
  consent: true;
  consent_version: string;
  turnstile_token: string;
  source?: string;
  utm?: Partial<Record<string, string>>;
}

export interface TokenRequest {
  token: string;
}
