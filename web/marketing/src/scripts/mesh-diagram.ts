// The hero diagram's hover and tap interaction. A lane is "previewed" while a mouse is over it (or its chip has
// keyboard focus) and "pinned" by a click or tap, so touch screens, which have no hover, still get every caption.
export function initMeshDiagram(root: HTMLElement): void {
  const lanes = [...root.querySelectorAll<SVGGElement>(".mesh-lane")];
  const chips = [...root.querySelectorAll<HTMLButtonElement>(".mesh-chip")];
  const captions = [...root.querySelectorAll<HTMLElement>("[data-caption]")];
  let pinned = "";
  let preview = "";

  const render = () => {
    const active = preview || pinned;
    if (active) root.dataset.active = active;
    else delete root.dataset.active;
    for (const lane of lanes) lane.classList.toggle("is-active", lane.dataset.lane === active);
    for (const chip of chips) {
      chip.classList.toggle("is-active", chip.dataset.lane === active);
      chip.setAttribute("aria-pressed", String(chip.dataset.lane === pinned));
    }
    for (const caption of captions) caption.classList.toggle("is-shown", caption.dataset.caption === active);
  };

  for (const target of [...lanes, ...chips]) {
    const lane = target.dataset.lane ?? "";
    target.addEventListener("pointerenter", (event) => {
      if ((event as PointerEvent).pointerType !== "mouse") return;
      preview = lane;
      render();
    });
    target.addEventListener("pointerleave", () => {
      preview = "";
      render();
    });
    target.addEventListener("click", () => {
      pinned = pinned === lane ? "" : lane;
      preview = "";
      render();
    });
  }
  for (const chip of chips) {
    chip.addEventListener("focus", () => {
      if (!chip.matches(":focus-visible")) return;
      preview = chip.dataset.lane ?? "";
      render();
    });
    chip.addEventListener("blur", () => {
      preview = "";
      render();
    });
  }
  // Tapping or clicking anywhere else on the page lets go of a pinned lane.
  document.addEventListener("click", (event) => {
    if (!pinned || root.contains(event.target as Node)) return;
    pinned = "";
    render();
  });
}
