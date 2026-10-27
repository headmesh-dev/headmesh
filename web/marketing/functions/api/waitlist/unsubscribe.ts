import type { Env } from "../../../server/env";
import { handleUnsubscribe } from "../../../server/waitlist";

/** POST /api/waitlist/unsubscribe: from the unsubscribe page, or RFC 8058 one-click from a mail provider. */
export const onRequestPost: PagesFunction<Env> = ({ request, env }) => handleUnsubscribe(request, env);
