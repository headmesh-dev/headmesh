// Theme toggle for the marketing pages. Uses Starlight's storage key and values ("light", "dark" or "" for
// "follow the system") so the choice carries over to /docs and back.
const STORAGE_KEY = "starlight-theme";

type Theme = "light" | "dark";

function storedTheme(): Theme | null {
  try {
    const value = localStorage.getItem(STORAGE_KEY);
    return value === "light" || value === "dark" ? value : null;
  } catch {
    return null;
  }
}

function systemTheme(): Theme {
  return matchMedia("(prefers-color-scheme: light)").matches ? "light" : "dark";
}

function apply(theme: Theme): void {
  document.documentElement.dataset.theme = theme;
  const next = theme === "dark" ? "light" : "dark";
  for (const button of document.querySelectorAll<HTMLButtonElement>("[data-theme-toggle]")) {
    button.setAttribute("aria-label", `Switch to ${next} theme`);
  }
}

export function initThemeToggle(): void {
  apply(storedTheme() ?? systemTheme());

  for (const button of document.querySelectorAll<HTMLButtonElement>("[data-theme-toggle]")) {
    button.addEventListener("click", () => {
      const next: Theme = document.documentElement.dataset.theme === "dark" ? "light" : "dark";
      try {
        // Picking the theme the system already uses means "follow the system" from now on.
        localStorage.setItem(STORAGE_KEY, next === systemTheme() ? "" : next);
      } catch {
        // Storage can be unavailable (private mode, blocked cookies); the toggle still works for this page.
      }
      apply(next);
    });
  }

  matchMedia("(prefers-color-scheme: light)").addEventListener("change", () => {
    if (storedTheme() === null) apply(systemTheme());
  });
}
