import type { ApiResponse, FieldErrors, SignupRequest } from "../../shared/api";
import { LIMITS, UTM_KEYS } from "../../shared/site";

declare global {
  interface Window {
    turnstile?: { reset(widget?: string | HTMLElement): void };
  }
}

const MESSAGES: Record<string, string> = {
  turnstile_failed: "The spam check didn't pass. Please try again.",
  rate_limited: "Too many attempts from your network. Please wait a few minutes and try again.",
  unavailable: "We couldn't add you right now. Please try again in a few minutes.",
  network: "We couldn't reach the server. Check your connection and try again.",
  spam_check_pending: "The spam check is still loading. Give it a moment, then try again.",
  form_outdated: "Something in the form is out of date. Reload the page and try again.",
};

/** Where the visitor came from: an explicit ?ref=, the UTM source, or the referring site's hostname. */
function detectSource(params: URLSearchParams): string | undefined {
  const raw = params.get("ref") ?? params.get("utm_source") ?? referrerHost();
  const cleaned = raw
    ?.toLowerCase()
    .replace(/[^a-z0-9._-]/g, "")
    .slice(0, LIMITS.source);
  return cleaned || undefined;
}

function referrerHost(): string | null {
  try {
    const host = document.referrer ? new URL(document.referrer).hostname : "";
    return host && host !== location.hostname ? host : null;
  } catch {
    return null;
  }
}

function collectUtm(params: URLSearchParams): Record<string, string> | undefined {
  const utm: Record<string, string> = {};
  for (const key of UTM_KEYS) {
    const value = params.get(key)?.trim();
    if (value) utm[key] = value.slice(0, LIMITS.utmValue);
  }
  return Object.keys(utm).length > 0 ? utm : undefined;
}

/**
 * Values an AI agent put in the link (#email=…&use_case=…, see WAITLIST_PREFILL_URL), cut to the form's limits.
 * An unencoded "+" in the email arrives as a space, and an email never contains one, so it's put back.
 */
export function prefillFromFragment(hash: string): { email?: string; useCase?: string } {
  const params = new URLSearchParams(hash.replace(/^#/, ""));
  const email = params.get("email")?.trim().replaceAll(" ", "+").slice(0, LIMITS.email);
  const useCase = params.get("use_case")?.trim().slice(0, LIMITS.useCase);
  return { ...(email && { email }), ...(useCase && { useCase }) };
}

export function initWaitlistForm(): void {
  const form = document.querySelector<HTMLFormElement>("[data-waitlist-form]");
  const success = document.querySelector<HTMLElement>("[data-waitlist-success]");
  const status = form?.querySelector<HTMLElement>("[data-form-status]");
  const button = form?.querySelector<HTMLButtonElement>("button[type=submit]");
  if (!form || !success || !status || !button) return;

  const params = new URLSearchParams(location.search);
  const source = detectSource(params);
  const utm = collectUtm(params);
  const prefill = prefillFromFragment(location.hash);
  if (prefill.email || prefill.useCase) {
    const emailField = form.elements.namedItem("email");
    const useCaseField = form.elements.namedItem("use_case");
    if (prefill.email && emailField instanceof HTMLInputElement) emailField.value = prefill.email;
    if (prefill.useCase && useCaseField instanceof HTMLTextAreaElement) useCaseField.value = prefill.useCase;
    // The email is in the form now; keep it out of the address bar and history.
    history.replaceState(null, "", location.pathname + location.search);
  }
  button.disabled = false;

  const showFieldErrors = (fields: FieldErrors = {}) => {
    for (const el of form.querySelectorAll<HTMLElement>("[data-error-for]")) {
      const name = el.dataset.errorFor as keyof FieldErrors;
      const message = fields[name];
      el.textContent = message ?? "";
      el.classList.toggle("hidden", !message);
      const field = form.elements.namedItem(name);
      if (field instanceof HTMLElement) field.setAttribute("aria-invalid", message ? "true" : "false");
    }
  };

  form.addEventListener("submit", async (event) => {
    event.preventDefault();
    status.textContent = "";
    showFieldErrors();
    if (!form.reportValidity()) return;

    const data = new FormData(form);
    const turnstileToken = String(data.get("cf-turnstile-response") ?? "");
    if (!turnstileToken) {
      status.textContent = MESSAGES.spam_check_pending ?? "";
      return;
    }
    const email = String(data.get("email") ?? "").trim();
    const useCase = String(data.get("use_case") ?? "").trim();
    const body: SignupRequest = {
      email,
      consent: true,
      consent_version: String(data.get("consent_version") ?? ""),
      turnstile_token: turnstileToken,
      ...(useCase && { use_case: useCase }),
      ...(source && { source }),
      ...(utm && { utm }),
    };

    button.disabled = true;
    button.setAttribute("aria-busy", "true");
    try {
      const response = await fetch(form.action, {
        method: "POST",
        headers: { "content-type": "application/json", accept: "application/json" },
        body: JSON.stringify(body),
      });
      const result = (await response.json().catch(() => ({ ok: false, error: "unavailable" }))) as ApiResponse;
      if (result.ok) {
        form.hidden = true;
        success.classList.remove("hidden");
        const emailSlot = success.querySelector("[data-success-email]");
        if (emailSlot) emailSlot.textContent = email;
        success.focus();
        return;
      }
      if (result.error === "invalid_request") {
        const { consent_version: outdated, ...visible } = result.fields ?? {};
        showFieldErrors(visible);
        status.textContent =
          outdated || Object.keys(visible).length === 0
            ? (MESSAGES.form_outdated ?? "")
            : "Please fix the highlighted fields.";
      } else {
        status.textContent = MESSAGES[result.error] ?? MESSAGES.unavailable ?? "";
      }
    } catch {
      status.textContent = MESSAGES.network ?? "";
    } finally {
      button.disabled = false;
      button.removeAttribute("aria-busy");
      // Turnstile tokens are single-use, so every attempt needs a fresh one.
      window.turnstile?.reset();
    }
  });
}
