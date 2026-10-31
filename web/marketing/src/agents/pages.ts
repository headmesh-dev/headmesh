import type { getCollection as GetCollection } from "astro:content";
import type { DocPage } from "./markdown.ts";

type DocEntry = { id: string; body?: string | undefined; data: { title: string; description?: string | undefined } };

/** One docs collection entry as a DocPage. */
export function toDocPage(entry: DocEntry): DocPage {
  return {
    id: entry.id,
    title: entry.data.title,
    description: entry.data.description ?? "",
    body: entry.body ?? "",
  };
}

/** Every docs page as a DocPage, with drafts and pages without a body left out. */
export async function docPages(getCollection: typeof GetCollection): Promise<DocPage[]> {
  const entries = await getCollection("docs", (entry) => !entry.data.draft);
  return entries.filter((entry) => entry.body).map(toDocPage);
}
