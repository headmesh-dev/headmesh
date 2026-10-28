import { z } from "zod";
import type { FieldErrors } from "../shared/api.ts";
import { CONSENT_VERSION, LIMITS, UTM_KEYS } from "../shared/site.ts";
import { normalizeEmail } from "./email.ts";

export const signupSchema = z.object({
  email: z
    .string("Enter your email address.")
    .max(LIMITS.email, "That email address is too long.")
    .transform((value, ctx) => {
      const normalized = normalizeEmail(value);
      if (normalized === null) {
        ctx.addIssue({ code: "custom", message: "Enter a valid email address." });
        return z.NEVER;
      }
      return normalized;
    })
    .pipe(z.email("Enter a valid email address.").max(LIMITS.email, "That email address is too long.")),
  use_case: z
    .string()
    .trim()
    .max(LIMITS.useCase, `Keep it under ${LIMITS.useCase} characters.`)
    .optional()
    .transform((value) => value || undefined),
  consent: z.literal(true, "Tick the box to agree to receive emails from us."),
  consent_version: z.literal(CONSENT_VERSION, "The consent wording has changed. Reload the page and try again."),
  turnstile_token: z.string().min(1).max(2048),
  source: z
    .string()
    .regex(new RegExp(`^[a-z0-9._-]{1,${LIMITS.source}}$`))
    .optional(),
  utm: z.partialRecord(z.enum(UTM_KEYS), z.string().trim().min(1).max(LIMITS.utmValue)).optional(),
});

export type Signup = z.output<typeof signupSchema>;

export const tokenSchema = z.object({ token: z.string().min(1).max(256) });

/** First message per form field, in the shape the waitlist page renders. */
export function fieldErrors(error: z.ZodError): FieldErrors {
  const fields: FieldErrors = {};
  for (const [field, messages] of Object.entries(z.flattenError(error).fieldErrors)) {
    if (field === "email" || field === "use_case" || field === "consent" || field === "consent_version") {
      const message = (messages as string[] | undefined)?.[0];
      if (message) fields[field] = message;
    }
  }
  return fields;
}
