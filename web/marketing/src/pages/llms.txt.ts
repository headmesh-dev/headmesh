import { getCollection } from "astro:content";
import type { APIRoute } from "astro";
import { llmsTxt } from "../agents/markdown.ts";
import { docPages } from "../agents/pages.ts";

export const GET: APIRoute = async () =>
  new Response(llmsTxt(await docPages(getCollection)), { headers: { "content-type": "text/plain; charset=utf-8" } });
