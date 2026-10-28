// A plain-Markdown copy of every docs page: /docs.md, /docs/quickstart.md and so on. Starlight pages link to theirs
// with <link rel="alternate" type="text/markdown"> (src/components/StarlightHead.astro).

import { getCollection } from "astro:content";
import type { APIRoute, GetStaticPaths } from "astro";
import { type DocPage, pageMarkdown } from "../agents/markdown.ts";
import { docPages } from "../agents/pages.ts";

export const getStaticPaths = (async () =>
  (await docPages(getCollection)).map((page) => ({ params: { slug: page.id }, props: page }))) satisfies GetStaticPaths;

export const GET: APIRoute<DocPage> = ({ props }) =>
  new Response(pageMarkdown(props), { headers: { "content-type": "text/markdown; charset=utf-8" } });
