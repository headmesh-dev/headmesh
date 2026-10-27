import type { Env } from "../../server/env";
import { handleSignup } from "../../server/waitlist";

/** POST /api/waitlist: validate, verify Turnstile, store the sign-up and send the double opt-in email. */
export const onRequestPost: PagesFunction<Env> = ({ request, env }) => handleSignup(request, env);
