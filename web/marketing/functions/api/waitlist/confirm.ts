import type { Env } from "../../../server/env";
import { handleConfirm } from "../../../server/waitlist";

/** POST /api/waitlist/confirm: second step of the double opt-in. */
export const onRequestPost: PagesFunction<Env> = ({ request, env }) => handleConfirm(request, env);
