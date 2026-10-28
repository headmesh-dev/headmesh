import type { getCollection as GetCollection } from "astro:content";
import type { DocPage } from "./markdown.ts";

/** Every docs page as a DocPage, with drafts and pages without a body left out. */
export async function docPages(getCollection: typeof GetCollection): Promise<DocPage[]> {
  const entries = await getCollection("docs", (entry) => !entry.data.draft);
  return entries
    .filter((entry) => entry.body)
    .map((entry) => ({
      id: entry.id,
      title: entry.data.title,
      description: entry.data.description ?? "",
      body: entry.body ?? "",
    }));
}
