import { z } from "zod";

/**
 * Runtime settings for the waitlist service, read from the environment. On the server they come from the systemd
 * unit's EnvironmentFile (deploy/env/<env>/waitlist.env, rendered by deploy/deploy.sh); locally from `.env`.
 */
export interface Config {
  /** Interface to listen on. nginx is the only client, so the default is loopback. */
  host: string;
  port: number;
  /** SQLite database file. Both blue and green instances of an environment share it. */
  dbPath: string;
  /** Public origin used in email links, e.g. https://headmesh.dev. */
  siteUrl: string;
  /** RFC 5322 From header on the Resend-verified domain. */
  emailFrom: string;
  emailReplyTo: string | undefined;
  /** "resend" sends real email; "log" writes the links to the journal instead (test environment, local dev). */
  emailDelivery: "resend" | "log";
  turnstileSecretKey: string;
  /** Resend API key restricted to sending from mail.headmesh.dev. Required when emailDelivery is "resend". */
  resendApiKey: string | undefined;
  /** Published Resend template ID or alias used for waitlist confirmations. Required with real delivery. */
  confirmationTemplateId: string | undefined;
  /** At least 32 random characters. Keys the token HMACs and the IP hash; rotating it voids outstanding links. */
  waitlistSecret: string;
  /** Header nginx puts the client address in. Trusted because the service only listens behind nginx. */
  clientIpHeader: string;
}

const optional = z
  .string()
  .optional()
  .transform((value) => value?.trim() || undefined);

const schema = z
  .object({
    WAITLIST_HOST: z.string().min(1).default("127.0.0.1"),
    WAITLIST_PORT: z.coerce.number().int().min(1).max(65_535).default(8787),
    WAITLIST_DB_PATH: z.string().min(1),
    SITE_URL: z.url({ protocol: /^https?$/ }).transform((value) => value.replace(/\/+$/, "")),
    EMAIL_FROM: z.string().min(3),
    EMAIL_REPLY_TO: optional,
    EMAIL_DELIVERY: z.enum(["resend", "log"]).default("resend"),
    TURNSTILE_SECRET_KEY: z.string().min(1),
    RESEND_API_KEY: optional,
    RESEND_CONFIRMATION_TEMPLATE_ID: optional,
    WAITLIST_SECRET: z.string().min(32, "WAITLIST_SECRET must be at least 32 characters"),
    CLIENT_IP_HEADER: z.string().min(1).default("x-real-ip"),
  })
  .refine((env) => env.EMAIL_DELIVERY === "log" || env.RESEND_API_KEY, {
    message: "RESEND_API_KEY is required when EMAIL_DELIVERY is resend",
    path: ["RESEND_API_KEY"],
  })
  .refine((env) => env.EMAIL_DELIVERY === "log" || env.RESEND_CONFIRMATION_TEMPLATE_ID, {
    message: "RESEND_CONFIRMATION_TEMPLATE_ID is required when EMAIL_DELIVERY is resend",
    path: ["RESEND_CONFIRMATION_TEMPLATE_ID"],
  });

export function loadConfig(env: NodeJS.ProcessEnv = process.env): Config {
  const parsed = schema.safeParse(env);
  if (!parsed.success) {
    const problems = parsed.error.issues.map((issue) => `${issue.path.join(".")}: ${issue.message}`);
    throw new Error(`Invalid waitlist configuration:\n- ${problems.join("\n- ")}`);
  }
  const e = parsed.data;
  return {
    host: e.WAITLIST_HOST,
    port: e.WAITLIST_PORT,
    dbPath: e.WAITLIST_DB_PATH,
    siteUrl: e.SITE_URL,
    emailFrom: e.EMAIL_FROM,
    emailReplyTo: e.EMAIL_REPLY_TO,
    emailDelivery: e.EMAIL_DELIVERY,
    turnstileSecretKey: e.TURNSTILE_SECRET_KEY,
    resendApiKey: e.RESEND_API_KEY,
    confirmationTemplateId: e.RESEND_CONFIRMATION_TEMPLATE_ID,
    waitlistSecret: e.WAITLIST_SECRET,
    clientIpHeader: e.CLIENT_IP_HEADER.toLowerCase(),
  };
}
