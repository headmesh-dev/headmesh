cat > Header.astro <<'EOF'
---
import { REPO_URL } from "../../shared/site";
import GitHubIcon from "./GitHubIcon.astro";
import Logo from "./Logo.astro";
import ThemeToggle from "./ThemeToggle.astro";
import Wordmark from "./Wordmark.astro";

const path = Astro.url.pathname;
---

<header class="sticky top-0 z-40 border-b border-line/70 bg-bg/80 backdrop-blur-md supports-[backdrop-filter]:bg-bg/65">
  <div class="container-page flex h-16 items-center justify-between gap-4">
    <a href="/" class="group flex items-center gap-2.5 text-fg" aria-label="Headmesh home">
      <Logo class="size-8 transition-transform duration-300 group-hover:-translate-y-px" />
      <Wordmark class="h-[19px] w-auto" />
    </a>
    <nav aria-label="Main" class="flex items-center gap-1 sm:gap-2">
      <a
        href="/docs/"
        class="rounded-md px-2.5 py-2 text-sm font-medium text-muted transition-colors hover:text-fg"
        aria-current={path.startsWith("/docs") ? "page" : undefined}
      >
        Docs
      </a>
      <a
        href={REPO_URL}
        class="hidden items-center gap-1.5 rounded-md px-2.5 py-2 text-sm font-medium text-muted transition-colors hover:text-fg sm:inline-flex"
      >
        <GitHubIcon class="size-4" />
        GitHub
      </a>
      <a
        href="/waitlist/"
        class="btn btn-primary ml-1 min-h-9 px-3.5 py-2 text-sm whitespace-nowrap"
        aria-current={path.startsWith("/waitlist") ? "page" : undefined}
      >
        <span class="sm:hidden">Waitlist</span>
        <span class="hidden sm:inline">Join the waitlist</span>
      </a>
      <ThemeToggle />
    </nav>
  </div>
</header>
EOF
cat > Footer.astro <<'EOF'
---
import { CONTACT_EMAIL, REPO_URL, SENDER, XCELSIOR_URL } from "../../shared/site";
import Logo from "./Logo.astro";
import Wordmark from "./Wordmark.astro";

const year = new Date().getUTCFullYear();
---

<footer class="mt-28 border-t border-line">
  <div class="container-page grid gap-10 py-12 sm:grid-cols-[1.2fr_1fr] sm:items-start">
    <div class="max-w-sm">
      <a href="/" class="inline-flex items-center gap-2.5 text-fg" aria-label="Headmesh home">
        <Logo class="size-7" />
        <Wordmark class="h-[17px] w-auto" />
      </a>
      <p class="mt-4 font-display text-xl font-bold tracking-tight">Share machines, not your network.</p>
      <p class="mt-2 text-sm leading-relaxed text-muted">
        Open, self-hostable, multi-tenant networking built on Headscale and WireGuard. An
        <a class="text-fg underline-offset-4 hover:underline" href={XCELSIOR_URL}>Xcelsior Compute</a> project.
      </p>
    </div>
    <nav aria-label="Footer" class="grid grid-cols-2 gap-x-12 gap-y-2.5 text-sm sm:grid-cols-3 sm:justify-self-end">
      <a class="text-muted hover:text-fg" href="/docs/">Docs</a>
      <a class="text-muted hover:text-fg" href={REPO_URL}>GitHub</a>
      <a class="text-muted hover:text-fg" href="/waitlist/">Waitlist</a>
      <a class="text-muted hover:text-fg" href="/privacy/">Privacy</a>
      <a class="text-muted hover:text-fg" href="/terms/">Terms</a>
      <a class="text-muted hover:text-fg" href="/.well-known/security.txt">Security</a>
      <a class="col-span-2 text-muted hover:text-fg sm:col-span-3" href={`mailto:${CONTACT_EMAIL}`}>{CONTACT_EMAIL}</a>
    </nav>
  </div>
  <div class="container-page border-t border-line/60 py-6 text-xs leading-relaxed text-muted">
    <p>
      © {year} {SENDER.legalName} Headmesh is not affiliated with Tailscale Inc. or the Headscale project.
      WireGuard is a registered trademark of Jason A. Donenfeld.
    </p>
  </div>
</footer>
EOF
echo done
