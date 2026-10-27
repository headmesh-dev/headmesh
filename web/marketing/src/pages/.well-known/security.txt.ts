import type { APIRoute } from "astro";
import { REPO_URL, SECURITY_EMAIL, SITE_URL } from "../../../shared/site";

// RFC 9116. Generated at build time so Expires always sits just under a year after the latest deploy; the site is
// redeployed far more often than that.
const EXPIRES_AFTER_DAYS = 330;

export const GET: APIRoute = () => {
  const expires = new Date(Date.now() + EXPIRES_AFTER_DAYS * 86_400_000);
  expires.setUTCHours(0, 0, 0, 0);
  const body = [
    `Contact: ${REPO_URL}/security/advisories/new`,
    `Contact: mailto:${SECURITY_EMAIL}`,
    `Expires: ${expires.toISOString().replace(".000Z", "Z")}`,
    `Policy: ${REPO_URL}/security/policy`,
    "Preferred-Languages: en",
    `Canonical: ${SITE_URL}/.well-known/security.txt`,
    "",
  ].join("\n");
  return new Response(body, { headers: { "content-type": "text/plain; charset=utf-8" } });
};
