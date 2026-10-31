// The "Copy for LLM" split button (src/components/CopyForLlm.astro). The main button copies the first option and
// the chevron opens a menu of all of them, with the keys of the WAI-ARIA menu button pattern.

const FEEDBACK_MS = 2000;

export function initCopyForLlm(): void {
  for (const root of document.querySelectorAll<HTMLElement>("[data-llm-copy]")) init(root);
}

function init(root: HTMLElement): void {
  const main = root.querySelector<HTMLButtonElement>(".llm-copy-main");
  const label = root.querySelector<HTMLElement>("[data-copy-label]");
  const toggle = root.querySelector<HTMLButtonElement>("[aria-haspopup=menu]");
  const menu = root.querySelector<HTMLElement>("[role=menu]");
  const status = root.querySelector<HTMLElement>("[data-copy-status]");
  if (!main || !label || !toggle || !menu || !status) return;

  const items = [...menu.querySelectorAll<HTMLElement>("[role=menuitem]")];
  const resting = label.textContent ?? "";
  let timer: number | undefined;

  const feedback = (copied: boolean) => {
    root.classList.toggle("is-copied", copied);
    label.textContent = copied ? "Copied" : "Couldn't copy";
    status.textContent = copied ? "Copied to the clipboard." : "Couldn't copy. The browser blocked clipboard access.";
    window.clearTimeout(timer);
    timer = window.setTimeout(() => {
      root.classList.remove("is-copied");
      label.textContent = resting;
      status.textContent = "";
    }, FEEDBACK_MS);
  };

  const copy = async (option: string) => {
    const text = root.querySelector<HTMLTemplateElement>(`template[data-copy-source="${option}"]`)?.content.textContent;
    if (!text) return;
    try {
      await navigator.clipboard.writeText(text);
      feedback(true);
    } catch {
      feedback(false);
    }
  };

  const open = (focus: "first" | "last") => {
    menu.hidden = false;
    toggle.setAttribute("aria-expanded", "true");
    // Line up with the edge the page asked for, unless the menu would then run off the screen.
    const end = root.dataset.align === "end";
    root.classList.toggle("is-end", end);
    const rect = menu.getBoundingClientRect();
    if (!end && rect.right > document.documentElement.clientWidth - 8) root.classList.add("is-end");
    if (end && rect.left < 8) root.classList.remove("is-end");
    (focus === "first" ? items[0] : items.at(-1))?.focus();
  };

  const close = (refocus: boolean) => {
    if (menu.hidden) return;
    menu.hidden = true;
    toggle.setAttribute("aria-expanded", "false");
    if (refocus) toggle.focus();
  };

  main.addEventListener("click", () => {
    close(false);
    void copy(main.dataset.copyOption ?? "0");
  });

  toggle.addEventListener("click", () => (menu.hidden ? open("first") : close(true)));
  toggle.addEventListener("keydown", (event) => {
    if (event.key !== "ArrowDown" && event.key !== "ArrowUp") return;
    event.preventDefault();
    open(event.key === "ArrowDown" ? "first" : "last");
  });

  menu.addEventListener("keydown", (event) => {
    const current = items.indexOf(document.activeElement as HTMLElement);
    const next = { ArrowDown: current + 1, ArrowUp: current - 1, Home: 0, End: items.length - 1 }[event.key];
    if (next !== undefined) {
      event.preventDefault();
      items[(next + items.length) % items.length]?.focus();
    } else if (event.key === "Escape") {
      event.preventDefault();
      close(true);
    } else if (event.key === "Tab") {
      close(false);
    }
  });

  for (const item of items) {
    item.addEventListener("click", () => {
      const option = item.dataset.copyOption;
      if (option !== undefined) void copy(option);
      close(true);
    });
  }

  document.addEventListener("click", (event) => {
    if (!root.contains(event.target as Node)) close(false);
  });
}
