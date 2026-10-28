import { readdirSync, readFileSync } from "node:fs";
import { DatabaseSync } from "node:sqlite";

/** The migrations shipped with the service (web/marketing/migrations). */
export const MIGRATIONS_DIR = new URL("../migrations/", import.meta.url);

/**
 * Open the waitlist database. WAL lets the blue and green instances of one environment share the file during a
 * switch-over; busy_timeout makes the brief overlap wait instead of failing.
 */
export function openDatabase(path: string): DatabaseSync {
  const db = new DatabaseSync(path);
  db.exec("PRAGMA busy_timeout = 5000");
  db.exec("PRAGMA journal_mode = WAL");
  db.exec("PRAGMA synchronous = NORMAL");
  db.exec("PRAGMA foreign_keys = ON");
  return db;
}

/**
 * Apply every `NNNN_name.sql` in `dir` that hasn't run yet, each in its own transaction, in file-name order.
 * Safe to call from two processes at once: BEGIN IMMEDIATE serialises them and each re-checks inside the lock.
 * Migrations must be backward compatible (expand, then contract in a later release), because the previous colour
 * keeps serving until the switch.
 */
export function migrate(db: DatabaseSync, dir: URL = MIGRATIONS_DIR): string[] {
  db.exec(
    "CREATE TABLE IF NOT EXISTS schema_migrations (name TEXT PRIMARY KEY NOT NULL, applied_at TEXT NOT NULL) STRICT",
  );
  const files = readdirSync(dir)
    .filter((name) => /^\d{4}_[\w-]+\.sql$/.test(name))
    .sort();
  const isApplied = db.prepare("SELECT 1 FROM schema_migrations WHERE name = ?");
  const record = db.prepare("INSERT INTO schema_migrations (name, applied_at) VALUES (?, ?)");
  const applied: string[] = [];
  for (const name of files) {
    db.exec("BEGIN IMMEDIATE");
    try {
      if (!isApplied.get(name)) {
        db.exec(readFileSync(new URL(name, dir), "utf8"));
        record.run(name, new Date().toISOString());
        applied.push(name);
      }
      db.exec("COMMIT");
    } catch (error) {
      db.exec("ROLLBACK");
      throw new Error(`migration ${name} failed: ${error instanceof Error ? error.message : String(error)}`);
    }
  }
  return applied;
}
