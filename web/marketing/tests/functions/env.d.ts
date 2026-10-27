// Bindings the tests add on top of wrangler.toml (see vitest.config.ts).
declare namespace Cloudflare {
  interface Env {
    TEST_MIGRATIONS: import("cloudflare:test").D1Migration[];
    TURNSTILE_SECRET_KEY: string;
    RESEND_API_KEY: string;
    WAITLIST_SECRET: string;
  }
}
