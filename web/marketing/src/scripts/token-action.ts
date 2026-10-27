import type { ApiResponse } from "../../shared/api";

// Confirmation and unsubscribe links carry their token in the URL fragment, which browsers never send to the
// server or in Referer headers. The page posts it only when the person clicks the button, so link scanners that
// prefetch URLs in email can't confirm or unsubscribe anyone.
export function initTokenAction(): void {
  const root = document.querySelector<HTMLElement>("[data-token-action]");
  const button = root?.querySelector<HTMLButtonElement>("button");
  const status = root?.querySelector<HTMLElement>("[data-status]");
  const endpoint = root?.dataset.endpoint;
  if (!root || !button || !status || !endpoint) return;

  const token = new URLSearchParams(location.hash.slice(1)).get("token");
  if (!token) {
    status.textContent = root.dataset.missing ?? "";
    return;
  }
  // Drop the token from the address bar and history now that it's in memory.
  history.replaceState(null, "", location.pathname);
  button.disabled = false;

  button.addEventListener("click", async () => {
    button.disabled = true;
    button.setAttribute("aria-busy", "true");
    status.textContent = "";
    try {
      const response = await fetch(endpoint, {
        method: "POST",
        headers: { "content-type": "application/json", accept: "application/json" },
        body: JSON.stringify({ token }),
      });
      const result = (await response.json().catch(() => ({ ok: false, error: "unavailable" }))) as ApiResponse;
      if (result.ok) {
        button.hidden = true;
        status.textContent = root.dataset.success ?? "";
        return;
      }
      status.textContent = (result.error === "invalid_token" ? root.dataset.invalid : root.dataset.failed) ?? "";
      button.disabled = result.error === "invalid_token";
    } catch {
      status.textContent = root.dataset.failed ?? "";
      button.disabled = false;
    } finally {
      button.removeAttribute("aria-busy");
    }
  });
}
