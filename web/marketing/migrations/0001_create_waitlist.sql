-- Migration number: 0001 	 2026-10-27
-- Headmesh waitlist (D1). Timestamps are ISO 8601 UTC strings, e.g. 2026-10-27T14:03:00.000Z.
-- Mailable audience: confirmed_at IS NOT NULL AND unsubscribed_at IS NULL.

CREATE TABLE waitlist (
  id              TEXT PRIMARY KEY NOT NULL,              -- random UUID; the only identifier that appears in links
  email           TEXT NOT NULL UNIQUE,                   -- normalised (trimmed, NFC, lowercase, punycode domain)
  use_case        TEXT,                                   -- optional "what would you use it for"
  source          TEXT,                                   -- ?ref=, utm_source or referring hostname
  utm             TEXT,                                   -- JSON object of utm_* parameters
  consent_version TEXT NOT NULL,                          -- version of the consent wording shown (shared/site.ts)
  ip_hash         TEXT,                                   -- keyed HMAC of the IP, for rate limiting only
  created_at      TEXT NOT NULL,
  confirmed_at    TEXT,                                   -- double opt-in completed
  unsubscribed_at TEXT,                                   -- consent withdrawn
  confirm_sent_at TEXT,                                   -- last confirmation email, throttles re-sends
  CHECK (length(email) BETWEEN 3 AND 254),
  CHECK (use_case IS NULL OR length(use_case) <= 500),
  CHECK (source IS NULL OR length(source) <= 64),
  CHECK (utm IS NULL OR json_valid(utm))
) STRICT;

CREATE INDEX waitlist_ip_hash_created_at ON waitlist (ip_hash, created_at);
CREATE INDEX waitlist_created_at ON waitlist (created_at);
