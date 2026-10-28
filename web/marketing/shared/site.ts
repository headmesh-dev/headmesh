// Facts shared by the static site (build time) and the waitlist service (runtime). Keep this module free of
// DOM- and Node-specific APIs so both sides can import it.

export const SITE_URL = "https://headmesh.dev";
export const REPO_URL = "https://github.com/headmesh-dev/headmesh";
export const CONTACT_EMAIL = "hello@headmesh.dev";
/** Matches SECURITY.md at the repository root. */
export const SECURITY_EMAIL = "security@headmesh.dev";
export const XCELSIOR_URL = "https://xcelsior.ca";

/**
 * Sender identification for CASL (S.C. 2010, c. 23, s. 6(2)): every consent request and every email must
 * name the sender and give a mailing address plus one other contact method.
 *
 * `postalAddress` must be filled in before launch. Production builds fail while it is empty (see
 * `astro.config.mjs`), and the waitlist service refuses sign-ups rather than send non-compliant email.
 */
export interface Sender {
  readonly legalName: string;
  readonly postalAddress: string;
  readonly email: string;
  readonly website: string;
}

export const SENDER: Sender = {
  legalName: "Xcelsior Compute Inc.",
  postalAddress: "240 St George St, London, ON N6A 3A7, Canada",
  email: CONTACT_EMAIL,
  website: SITE_URL,
};

/**
 * Version of the consent wording shown next to the waitlist checkbox. Change the version whenever the wording
 * changes so each stored consent record points at the exact text the person agreed to.
 */
export const CONSENT_VERSION = "2026-10-27";

export const CONSENT_TEXT =
  "Yes, email me about Headmesh: early-access invitations and occasional product updates. " +
  "I can unsubscribe at any time.";

/** Turnstile action name. The Function rejects tokens minted for any other action. */
export const TURNSTILE_ACTION = "waitlist";

/** Limits enforced by both the form (maxlength) and the service (zod). */
export const LIMITS = {
  email: 254,
  useCase: 500,
  source: 64,
  utmValue: 128,
} as const;

export const UTM_KEYS = ["utm_source", "utm_medium", "utm_campaign", "utm_term", "utm_content"] as const;
