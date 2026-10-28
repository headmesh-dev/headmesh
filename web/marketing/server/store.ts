import type { DatabaseSync, StatementSync } from "node:sqlite";
import type { Signup } from "./schema.ts";

export interface WaitlistRow {
  id: string;
  confirmed_at: string | null;
  unsubscribed_at: string | null;
  confirm_sent_at: string | null;
}

/** All SQL the waitlist runs, against the schema in migrations/. */
export class WaitlistStore {
  readonly #ping: StatementSync;
  readonly #recent: StatementSync;
  readonly #upsert: StatementSync;
  readonly #confirmSent: StatementSync;
  readonly #confirm: StatementSync;
  readonly #unsubscribe: StatementSync;

  constructor(db: DatabaseSync) {
    this.#ping = db.prepare("SELECT 1");
    this.#recent = db.prepare("SELECT COUNT(*) AS n FROM waitlist WHERE ip_hash = ? AND created_at > ?");
    // Someone who unsubscribed and signs up again gives fresh consent: their row goes back to unconfirmed.
    this.#upsert = db.prepare(
      `INSERT INTO waitlist (id, email, use_case, source, utm, consent_version, ip_hash, created_at)
       VALUES (?, ?, ?, ?, ?, ?, ?, ?)
       ON CONFLICT (email) DO UPDATE SET
         use_case = coalesce(excluded.use_case, waitlist.use_case),
         consent_version = CASE WHEN waitlist.confirmed_at IS NULL OR waitlist.unsubscribed_at IS NOT NULL
                                THEN excluded.consent_version ELSE waitlist.consent_version END,
         confirmed_at = CASE WHEN waitlist.unsubscribed_at IS NOT NULL THEN NULL ELSE waitlist.confirmed_at END,
         confirm_sent_at = CASE WHEN waitlist.unsubscribed_at IS NOT NULL THEN NULL ELSE waitlist.confirm_sent_at END,
         unsubscribed_at = NULL
       RETURNING id, confirmed_at, unsubscribed_at, confirm_sent_at`,
    );
    this.#confirmSent = db.prepare("UPDATE waitlist SET confirm_sent_at = ? WHERE id = ?");
    // Deleted or unsubscribed rows can't be confirmed by an old link.
    this.#confirm = db.prepare(
      `UPDATE waitlist SET confirmed_at = coalesce(confirmed_at, ?)
       WHERE id = ? AND unsubscribed_at IS NULL
       RETURNING id`,
    );
    this.#unsubscribe = db.prepare("UPDATE waitlist SET unsubscribed_at = coalesce(unsubscribed_at, ?) WHERE id = ?");
  }

  /** Throws if the database can't answer; used by /readyz. */
  ping(): void {
    this.#ping.get();
  }

  recentSignups(ipHash: string, sinceIso: string): number {
    const row = this.#recent.get(ipHash, sinceIso) as { n: number } | undefined;
    return row?.n ?? 0;
  }

  upsert(signup: Signup, ipHash: string | null, nowIso: string): WaitlistRow {
    const utm = signup.utm && Object.keys(signup.utm).length > 0 ? JSON.stringify(signup.utm) : null;
    const row = this.#upsert.get(
      crypto.randomUUID(),
      signup.email,
      signup.use_case ?? null,
      signup.source ?? null,
      utm,
      signup.consent_version,
      ipHash,
      nowIso,
    ) as WaitlistRow | undefined;
    if (!row) throw new Error("upsert returned no row");
    return row;
  }

  markConfirmSent(id: string, nowIso: string): void {
    this.#confirmSent.run(nowIso, id);
  }

  /** True if the row exists and is still subscribed (confirming twice is fine). */
  confirm(id: string, nowIso: string): boolean {
    return this.#confirm.get(nowIso, id) !== undefined;
  }

  /** Idempotent: unsubscribing twice, or after the row was deleted, is still a success. */
  unsubscribe(id: string, nowIso: string): void {
    this.#unsubscribe.run(nowIso, id);
  }
}
