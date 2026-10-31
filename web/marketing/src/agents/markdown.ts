// Plain-Markdown views of the site for AI agents and other tools: /llms.txt (https://llmstxt.org), /llms-full.txt and
// a .md copy of every docs page. Pure functions, so they can be tested without a build.
import { LIMITS, REPO_URL, SITE_URL } from "../../shared/site.ts";

export type DocPage = {
  /** Content collection id: "docs" for the docs home, "docs/quickstart" for the others. */
  id: string;
  title: string;
  description: string;
  /** The page's MDX source, without frontmatter. */
  body: string;
};

/** The URL of a docs page's Markdown copy, e.g. https://headmesh.dev/docs/quickstart.md. */
export function markdownUrl(id: string): string {
  return `${SITE_URL}/${id}.md`;
}

/** The URL of the rendered docs page, e.g. https://headmesh.dev/docs/quickstart/. */
export function pageUrl(id: string): string {
  return `${SITE_URL}/${id}/`;
}

function absolute(href: string): string {
  return href.startsWith("/") ? `${SITE_URL}${href}` : href;
}

function attribute(tag: string, name: string): string {
  return new RegExp(`\\b${name}="([^"]*)"`).exec(tag)?.[1] ?? "";
}

/**
 * Turn a docs page's MDX into Markdown that reads well without a renderer: imports go, LinkCards become list items,
 * CardGrid wrappers disappear, Starlight asides (:::note[Title]) become blockquotes, and site-relative links become
 * absolute so the text still works when pasted somewhere else.
 */
export function mdxToMarkdown(body: string): string {
  let text = body.replace(/^import\s.+$\n?/gm, "");
  text = text.replace(/<\/?CardGrid\s*>\n?/g, "");
  text = text.replace(/^[ \t]*<LinkCard\b[\s\S]*?\/>/gm, (tag) => {
    const title = attribute(tag, "title");
    const href = absolute(attribute(tag, "href"));
    const description = attribute(tag, "description");
    return `- [${title}](${href})${description ? `: ${description}` : ""}`;
  });
  text = text.replace(
    /^:::(\w+)(?:\[([^\]]*)\])?\n([\s\S]*?)^:::\s*$/gm,
    (_m, kind: string, label = "", inner: string) => {
      const heading = label || kind.charAt(0).toUpperCase() + kind.slice(1);
      const lines = inner.trimEnd().split("\n");
      // The blank line after the quote keeps whatever follows from being folded into it.
      return `${[`> **${heading}.**`, ...lines.map((line) => (line ? `> ${line}` : ">"))].join("\n")}\n`;
    },
  );
  text = text.replace(/\]\((\/[^)\s]*)\)/g, (_m, href: string) => `](${absolute(href)})`);
  return `${text.replace(/\n{3,}/g, "\n\n").trim()}\n`;
}

/** One docs page as a standalone Markdown document. */
export function pageMarkdown(page: DocPage): string {
  return `# ${page.title}\n\n> ${page.description}\n\nSource: ${pageUrl(page.id)}\n\n${mdxToMarkdown(page.body)}`;
}

/** The docs sidebar's order (astro.config.mjs); pages not listed follow, by id. */
const READING_ORDER = ["docs", "docs/quickstart", "docs/concepts", "docs/self-hosting"];

/** Docs pages in reading order. */
export function sortPages(pages: DocPage[]): DocPage[] {
  const rank = (id: string) => {
    const i = READING_ORDER.indexOf(id);
    return i === -1 ? READING_ORDER.length : i;
  };
  return [...pages].sort((a, b) => rank(a.id) - rank(b.id) || a.id.localeCompare(b.id));
}

export const SUMMARY =
  "Headmesh is an open, self-hostable, multi-tenant control layer for private WireGuard meshes, built on Headscale " +
  "and the standard Tailscale clients. It gives teams, renters and AI agents time-limited access to exactly the " +
  "machines and ports they need, and gives cloud AI agents a sandbox on your own hardware.";

/**
 * The waitlist page with the form filled in. The values ride in the fragment, which browsers never send to the
 * server, and src/scripts/waitlist.ts clears it from the address bar once the form has them.
 */
export const WAITLIST_PREFILL_URL = `${SITE_URL}/waitlist/?ref=agent#email=<email>&use_case=<use case>`;

/**
 * What "Copy for LLM" puts on the clipboard: directions an AI agent can follow to get the person onto the waitlist.
 * The sign-up API needs a Turnstile token and the person's own consent, so the agent prefills the form and hands the
 * link back instead of calling the API.
 */
export const WAITLIST_PROMPT = `Help me join the Headmesh waitlist.

${SUMMARY}

The sign-up form has a spam check and a consent box I have to tick myself, so don't submit it or call its API for me.
Instead:

1. Ask me which email address to use, and what I'd use Headmesh for (optional, at most ${LIMITS.useCase} characters).
2. Build this link, percent-encoding both values. Leave out \`&use_case=…\` if I don't give one.
   ${WAITLIST_PREFILL_URL}
3. Give me the link. It opens the form already filled in; I tick the consent box and press "Join the waitlist".
4. Remind me to open the confirmation email and click its link. I'm not on the list until I confirm.

If I have questions first, ${SITE_URL}/llms.txt indexes the docs in plain Markdown and ${SITE_URL}/llms-full.txt has
all of them in one file.
`;

/** /llms.txt: a short, link-rich index in the llmstxt.org format. */
export function llmsTxt(pages: DocPage[]): string {
  const docs = sortPages(pages)
    .map((page) => `- [${page.title}](${markdownUrl(page.id)}): ${page.description}`)
    .join("\n");
  const blob = (path: string) => `${REPO_URL}/blob/main/${path}`;
  return `# Headmesh

> ${SUMMARY}

Headmesh is pre-release: there is no stable version yet. The CLI, agent sandbox and plugins are Apache-2.0; the
server and cell agent are AGPL-3.0. Every docs link below points at a plain-Markdown copy of the page.

## Docs

${docs}

## Use Headmesh from an AI agent

- Claude Code: \`claude plugin marketplace add headmesh-dev/headmesh --sparse .claude-plugin --sparse plugins\`, then \`claude plugin install headmesh@headmesh\`
- Codex: \`codex plugin marketplace add headmesh-dev/headmesh --sparse .agents/plugins --sparse plugins\`, then \`codex plugin add headmesh@headmesh\`
- [Plugins and marketplace](${blob("plugins/README.md")}): what the headmesh and xcelsior plugins do and how to configure them
- [Agent sandbox](${blob("sandbox/README.md")}): run cloud agents on your own hardware, reachable over your Headscale mesh

## Project

- [Source code](${REPO_URL})
- [Implementation plan](${blob("docs/plan.md")}): architecture, milestones and the security model
- [Architecture decision records](${REPO_URL}/tree/main/docs/adr)
- [Licensing](${blob("LICENSING.md")})
- [Security policy](${blob(".github/SECURITY.md")})

## Optional

- [All docs in one file](${SITE_URL}/llms-full.txt)
- [Waitlist](${SITE_URL}/waitlist/): to sign a person up, give them \`${WAITLIST_PREFILL_URL}\` with both values
  percent-encoded. The form opens filled in; they tick the consent box, submit, and confirm by email.
- [Privacy policy](${SITE_URL}/privacy/)
`;
}

/** Push every Markdown heading one level down, leaving fenced code (where `# ` starts a shell comment) alone. */
export function demoteHeadings(markdown: string): string {
  let fenced = false;
  return markdown
    .split("\n")
    .map((line) => {
      if (/^\s*(```|~~~)/.test(line)) fenced = !fenced;
      return !fenced && /^#{1,5} /.test(line) ? `#${line}` : line;
    })
    .join("\n");
}

/** /llms-full.txt: the summary and every docs page, in one Markdown document. */
export function llmsFullTxt(pages: DocPage[]): string {
  const sections = sortPages(pages).map((page) => demoteHeadings(pageMarkdown(page)));
  return `# Headmesh\n\n> ${SUMMARY}\n\n${sections.join("\n")}`;
}
