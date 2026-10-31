import { describe, expect, it } from "vitest";
import {
  type DocPage,
  demoteHeadings,
  llmsFullTxt,
  llmsTxt,
  markdownUrl,
  mdxToMarkdown,
  pageMarkdown,
  sortPages,
  WAITLIST_PREFILL_URL,
  WAITLIST_PROMPT,
} from "../../src/agents/markdown.ts";
import { prefillFromFragment } from "../../src/scripts/waitlist.ts";

const page = (id: string, title = id): DocPage => ({ id, title, description: `About ${title}.`, body: "Text.\n" });

describe("mdxToMarkdown", () => {
  it("turns Starlight components into plain Markdown", () => {
    const body = [
      'import { LinkCard, CardGrid } from "@astrojs/starlight/components";',
      "",
      ":::note[Early development]",
      "Read the [repository](https://github.com/headmesh-dev/headmesh).",
      ":::",
      "<CardGrid>",
      '  <LinkCard title="Quickstart" href="/docs/quickstart/" description="Get started." />',
      "  <LinkCard",
      '    title="Repo"',
      '    href="https://github.com/headmesh-dev/headmesh"',
      "  />",
      "</CardGrid>",
    ].join("\n");
    expect(mdxToMarkdown(body)).toBe(
      [
        "> **Early development.**",
        "> Read the [repository](https://github.com/headmesh-dev/headmesh).",
        "",
        "- [Quickstart](https://headmesh.dev/docs/quickstart/): Get started.",
        "- [Repo](https://github.com/headmesh-dev/headmesh)",
        "",
      ].join("\n"),
    );
  });

  it("makes site-relative links absolute and leaves others alone", () => {
    expect(mdxToMarkdown("[Join](/waitlist/) or [read](https://example.com/x).")).toBe(
      "[Join](https://headmesh.dev/waitlist/) or [read](https://example.com/x).\n",
    );
  });

  it("names an aside after its kind when it has no title", () => {
    expect(mdxToMarkdown(":::caution\nCareful.\n:::")).toBe("> **Caution.**\n> Careful.\n");
  });
});

describe("pages", () => {
  it("follow the docs sidebar's reading order", () => {
    const ids = sortPages([page("docs/self-hosting"), page("docs/zzz"), page("docs/concepts"), page("docs")]).map(
      (p) => p.id,
    );
    expect(ids).toEqual(["docs", "docs/concepts", "docs/self-hosting", "docs/zzz"]);
  });

  it("get a standalone Markdown copy with a link back to the page", () => {
    expect(pageMarkdown(page("docs/quickstart", "Quickstart"))).toBe(
      "# Quickstart\n\n> About Quickstart.\n\nSource: https://headmesh.dev/docs/quickstart/\n\nText.\n",
    );
    expect(markdownUrl("docs/quickstart")).toBe("https://headmesh.dev/docs/quickstart.md");
  });
});

describe("llms.txt", () => {
  const text = llmsTxt([page("docs/concepts", "Concepts"), page("docs", "Overview")]);

  it("follows the llmstxt.org shape: one H1, a summary quote, then H2 sections of links", () => {
    const lines = text.split("\n");
    expect(lines[0]).toBe("# Headmesh");
    expect(lines[2]).toMatch(/^> Headmesh is an open, self-hostable/);
    expect(lines.filter((line) => line.startsWith("# "))).toHaveLength(1);
    expect(text).toContain("## Docs\n\n- [Overview](https://headmesh.dev/docs.md): About Overview.");
    expect(text).toContain("## Optional");
  });

  it("tells agents how to install the plugins in Claude Code and Codex", () => {
    expect(text).toContain("claude plugin install headmesh@headmesh");
    expect(text).toContain("codex plugin add headmesh@headmesh");
  });

  it("tells agents how to send a person to a prefilled waitlist form", () => {
    expect(text).toContain(WAITLIST_PREFILL_URL);
  });
});

describe("waitlist directions for agents", () => {
  it("has the agent prefill the form and leave consent and submitting to the person", () => {
    expect(WAITLIST_PROMPT).toMatch(/^Help me join the Headmesh waitlist\.\n/);
    expect(WAITLIST_PROMPT).toContain("https://headmesh.dev/waitlist/?ref=agent#email=<email>&use_case=<use case>");
    expect(WAITLIST_PROMPT).toContain("don't submit it or call its API for me");
    expect(WAITLIST_PROMPT).toContain("I tick the consent box");
    expect(WAITLIST_PROMPT).toContain("at most 500 characters");
  });

  it("round-trips through the link an agent builds", () => {
    const email = "ada+hm@example.com";
    const useCase = "Share 2 GPUs & a NAS with renters";
    const link = WAITLIST_PREFILL_URL.replace("<email>", encodeURIComponent(email)).replace(
      "<use case>",
      encodeURIComponent(useCase),
    );
    expect(prefillFromFragment(new URL(link).hash)).toEqual({ email, useCase });
  });

  it("repairs an unencoded + in the email, trims, and enforces the form's limits", () => {
    expect(prefillFromFragment("#email= ada+hm@example.com &use_case=")).toEqual({ email: "ada+hm@example.com" });
    expect(prefillFromFragment(`#use_case=${"x".repeat(600)}`).useCase).toHaveLength(500);
    expect(prefillFromFragment("")).toEqual({});
  });
});

describe("llms-full.txt", () => {
  it("nests every page under the site title without touching code blocks", () => {
    const docs: DocPage = {
      id: "docs/quickstart",
      title: "Quickstart",
      description: "Start.",
      body: "## Install\n\n```sh\n# a shell comment\ncurl -fsSL https://get.headmesh.dev | sh\n```\n",
    };
    const full = llmsFullTxt([docs]);
    expect(full).toMatch(/^# Headmesh\n/);
    expect(full).toContain("\n## Quickstart\n");
    expect(full).toContain("\n### Install\n");
    expect(full).toContain("\n# a shell comment\n");
  });

  it("demotes headings only outside fences", () => {
    expect(demoteHeadings("# A\n```\n# b\n```\n## C")).toBe("## A\n```\n# b\n```\n### C");
  });
});
