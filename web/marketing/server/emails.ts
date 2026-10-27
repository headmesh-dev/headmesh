import { SENDER } from "../shared/site";

export interface ConfirmationEmail {
  subject: string;
  text: string;
  html: string;
}

const escapeHtml = (value: string) =>
  value.replaceAll("&", "&amp;").replaceAll("<", "&lt;").replaceAll(">", "&gt;").replaceAll('"', "&quot;");

/**
 * The double opt-in email. It is itself a commercial electronic message under CASL, so it identifies the sender
 * (name, mailing address, contact email) and carries a working unsubscribe link.
 */
export function confirmationEmail(links: { confirm: string; unsubscribe: string; site: string }): ConfirmationEmail {
  const sender = `${SENDER.legalName}, ${SENDER.postalAddress}`;
  const subject = "Confirm your spot on the Headmesh waitlist";
  const text = [
    "Hi,",
    "",
    "Thanks for joining the Headmesh waitlist. Please confirm your email address:",
    "",
    links.confirm,
    "",
    "The link works for 7 days. If you didn't sign up, ignore this email and you won't hear from us again.",
    "",
    "--",
    `Headmesh (${links.site})`,
    `Sent by ${sender}. Contact: ${SENDER.email}`,
    `Unsubscribe: ${links.unsubscribe}`,
    "",
  ].join("\n");

  const e = escapeHtml;
  const html = `<!doctype html>
<html lang="en">
<head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>${e(subject)}</title></head>
<body style="margin:0;padding:0;background:#f5f7f9;font-family:-apple-system,'Segoe UI',Inter,Helvetica,Arial,sans-serif;color:#0d1520">
  <table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="background:#f5f7f9;padding:32px 16px">
    <tr><td align="center">
      <table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="max-width:520px;background:#ffffff;border:1px solid #dde3e9;border-radius:12px">
        <tr><td style="padding:32px 32px 8px;font-size:18px;font-weight:700">Headmesh</td></tr>
        <tr><td style="padding:8px 32px;font-size:16px;line-height:1.6">
          <p style="margin:0 0 16px">Thanks for joining the Headmesh waitlist. Please confirm your email address.</p>
          <p style="margin:24px 0">
            <a href="${e(links.confirm)}" style="display:inline-block;background:#0d7068;color:#ffffff;text-decoration:none;font-weight:600;padding:12px 20px;border-radius:8px">Confirm my email</a>
          </p>
          <p style="margin:0 0 16px;font-size:14px;color:#4a5664">The link works for 7 days. If you didn't sign up, ignore this email and you won't hear from us again.</p>
        </td></tr>
        <tr><td style="padding:16px 32px 32px;font-size:12px;line-height:1.6;color:#4a5664;border-top:1px solid #dde3e9">
          Sent by ${e(sender)}. Contact: <a href="mailto:${e(SENDER.email)}" style="color:#4a5664">${e(SENDER.email)}</a><br>
          <a href="${e(links.unsubscribe)}" style="color:#4a5664">Unsubscribe</a> · <a href="${e(links.site)}/privacy/" style="color:#4a5664">Privacy policy</a>
        </td></tr>
      </table>
    </td></tr>
  </table>
</body>
</html>`;

  return { subject, text, html };
}
