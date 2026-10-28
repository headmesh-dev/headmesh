import type { ApiErrorCode, ApiResponse, FieldErrors } from "../shared/api.ts";

/** Largest request body any waitlist endpoint accepts. */
export const MAX_BODY_BYTES = 8 * 1024;

// _headers only covers static assets, so Function responses carry their own hardening.
const BASE_HEADERS = {
  "content-type": "application/json; charset=utf-8",
  "cache-control": "no-store",
  "x-content-type-options": "nosniff",
  "referrer-policy": "no-referrer",
  "content-security-policy": "default-src 'none'; frame-ancestors 'none'",
};

export function json(status: number, body: ApiResponse, headers: Record<string, string> = {}): Response {
  return new Response(JSON.stringify(body), { status, headers: { ...BASE_HEADERS, ...headers } });
}

export const ok = (status = 200) => json(status, { ok: true });

const STATUS: Record<ApiErrorCode, number> = {
  invalid_request: 400,
  invalid_token: 400,
  turnstile_failed: 403,
  payload_too_large: 413,
  unsupported_media_type: 415,
  rate_limited: 429,
  unavailable: 503,
};

export function fail(error: ApiErrorCode, fields?: FieldErrors, headers?: Record<string, string>): Response {
  return json(STATUS[error], fields ? { ok: false, error, fields } : { ok: false, error }, headers);
}

export class HttpError extends Error {
  readonly response: Response;

  constructor(response: Response) {
    super(`HTTP ${response.status}`);
    this.response = response;
  }
}

/** Read a small JSON body, or throw an HttpError with the right 4xx response. */
export async function readJson(request: Request): Promise<unknown> {
  const type = request.headers.get("content-type")?.split(";")[0]?.trim().toLowerCase();
  if (type !== "application/json") throw new HttpError(fail("unsupported_media_type"));
  const text = await readText(request);
  try {
    return JSON.parse(text);
  } catch {
    throw new HttpError(fail("invalid_request"));
  }
}

/** Read a request body as text without accepting more than MAX_BODY_BYTES. */
export async function readText(request: Request): Promise<string> {
  const declared = Number(request.headers.get("content-length") ?? "0");
  if (declared > MAX_BODY_BYTES) throw new HttpError(fail("payload_too_large"));
  const text = await request.text();
  if (new TextEncoder().encode(text).byteLength > MAX_BODY_BYTES) throw new HttpError(fail("payload_too_large"));
  return text;
}

/** Structured log line. Never pass email addresses or tokens. */
export function log(level: "info" | "warn" | "error", event: string, data: Record<string, unknown> = {}): void {
  console[level](JSON.stringify({ level, event, ...data }));
}
