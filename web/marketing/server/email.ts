/**
 * Normalise an email address so the same mailbox always maps to the same row: trim, Unicode NFC, lowercase, and
 * IDNA (punycode) for the domain. Provider-specific rewriting (Gmail dots, "+tags") is deliberately not applied:
 * people use those addresses on purpose and expect mail to arrive there.
 *
 * Returns null when the input can't be an address at all; full syntax validation happens in the schema.
 */
export function normalizeEmail(input: string): string | null {
  const trimmed = input.trim().normalize("NFC");
  const at = trimmed.lastIndexOf("@");
  if (at < 1 || at === trimmed.length - 1) return null;

  const local = trimmed.slice(0, at).toLowerCase();
  const rawDomain = trimmed
    .slice(at + 1)
    .toLowerCase()
    .replace(/\.$/, "");
  if (!rawDomain || /[\s/?#@:[\]\\]/.test(rawDomain)) return null;

  let domain: string;
  try {
    domain = new URL(`http://${rawDomain}`).hostname;
  } catch {
    return null;
  }
  return `${local}@${domain}`;
}
