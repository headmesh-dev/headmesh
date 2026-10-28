// Signed, opaque tokens for the confirm and unsubscribe links, and a keyed hash for IP addresses. All keys are
// derived from WAITLIST_SECRET with HKDF, one per purpose, so a token minted for one purpose never verifies for
// another and the IP hash can't be used to forge tokens.
import type { webcrypto } from "node:crypto";

type CryptoKey = webcrypto.CryptoKey;

export type TokenPurpose = "confirm" | "unsubscribe";
type KeyPurpose = TokenPurpose | "ip-hash";

const MIN_SECRET_LENGTH = 32;
const HKDF_SALT = new TextEncoder().encode("headmesh-waitlist/v1");
const VERSION = "v1";

/** How long a confirmation link stays valid. */
export const CONFIRM_TOKEN_TTL_SECONDS = 7 * 24 * 60 * 60;

const keyCache = new Map<string, Promise<CryptoKey>>();

function hmacKey(secret: string, purpose: KeyPurpose): Promise<CryptoKey> {
  if (secret.length < MIN_SECRET_LENGTH) {
    return Promise.reject(new Error(`WAITLIST_SECRET must be at least ${MIN_SECRET_LENGTH} characters`));
  }
  const cacheKey = `${purpose}\u0000${secret}`;
  let key = keyCache.get(cacheKey);
  if (!key) {
    key = (async () => {
      const ikm = await crypto.subtle.importKey("raw", new TextEncoder().encode(secret), "HKDF", false, ["deriveKey"]);
      return crypto.subtle.deriveKey(
        { name: "HKDF", hash: "SHA-256", salt: HKDF_SALT, info: new TextEncoder().encode(purpose) },
        ikm,
        { name: "HMAC", hash: "SHA-256", length: 256 },
        false,
        ["sign", "verify"],
      );
    })();
    keyCache.set(cacheKey, key);
  }
  return key;
}

function base64url(bytes: ArrayBuffer): string {
  let binary = "";
  for (const byte of new Uint8Array(bytes)) binary += String.fromCharCode(byte);
  return btoa(binary).replaceAll("+", "-").replaceAll("/", "_").replace(/=+$/, "");
}

function fromBase64url(text: string): Uint8Array<ArrayBuffer> | null {
  if (!/^[A-Za-z0-9_-]+$/.test(text)) return null;
  const binary = atob(text.replaceAll("-", "+").replaceAll("_", "/"));
  return Uint8Array.from(binary, (c) => c.charCodeAt(0));
}

const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;

/**
 * Token layout: `v1.<row id>[.<expiry, unix seconds>].<base64url HMAC>`. Confirm tokens expire; unsubscribe tokens
 * don't, because CASL requires the unsubscribe link to keep working for at least 60 days after each email.
 * Tokens carry the row id, never the email address.
 */
export async function signToken(
  secret: string,
  purpose: TokenPurpose,
  id: string,
  nowSeconds: number,
): Promise<string> {
  const payload =
    purpose === "confirm" ? `${VERSION}.${id}.${nowSeconds + CONFIRM_TOKEN_TTL_SECONDS}` : `${VERSION}.${id}`;
  const signature = await crypto.subtle.sign("HMAC", await hmacKey(secret, purpose), new TextEncoder().encode(payload));
  return `${payload}.${base64url(signature)}`;
}

/** Returns the row id if the token is authentic, unexpired and minted for `purpose`; otherwise null. */
export async function verifyToken(
  secret: string,
  purpose: TokenPurpose,
  token: string,
  nowSeconds: number,
): Promise<string | null> {
  if (token.length > 256) return null;
  const parts = token.split(".");
  const expected = purpose === "confirm" ? 4 : 3;
  if (parts.length !== expected || parts[0] !== VERSION) return null;

  const id = parts[1] ?? "";
  if (!UUID_RE.test(id)) return null;
  if (purpose === "confirm") {
    const expires = Number(parts[2]);
    if (!Number.isSafeInteger(expires) || expires < nowSeconds) return null;
  }

  const signature = fromBase64url(parts.at(-1) ?? "");
  if (!signature) return null;
  const payload = parts.slice(0, -1).join(".");
  const valid = await crypto.subtle.verify(
    "HMAC",
    await hmacKey(secret, purpose),
    signature,
    new TextEncoder().encode(payload),
  );
  return valid ? id : null;
}

/** Keyed hash of an IP address: stable enough for rate limiting, useless without the secret. */
export async function hashIp(secret: string, ip: string): Promise<string> {
  const mac = await crypto.subtle.sign("HMAC", await hmacKey(secret, "ip-hash"), new TextEncoder().encode(ip));
  return base64url(mac).slice(0, 32);
}
