import type { APIRoute, GetStaticPaths } from "astro";
import homeCard from "../../../brand-reference/headmesh-process/og-card-grant-1200x630.png?inline";
import { type OgCard, renderOgImage } from "../../og/render";

const cards: Record<string, OgCard> = {
  home: {
    title: "Share machines, not your network.",
    subtitle: "Secure access for teams, renters and AI agents to exactly the machines they need, and nothing else.",
  },
  waitlist: {
    title: "Join the Headmesh waitlist",
    subtitle: "Early access to the open, self-hostable, multi-tenant mesh built on Headscale and WireGuard.",
  },
  docs: {
    title: "Headmesh documentation",
    subtitle: "Workspaces, devices, rules, grants, cells and agent sandboxes.",
  },
  privacy: { title: "Privacy policy", subtitle: "What headmesh.dev collects, why, and what you can do about it." },
  terms: { title: "Terms of use", subtitle: "The terms for using the headmesh.dev website and waitlist." },
};

export const getStaticPaths = (() =>
  Object.entries(cards).map(([slug, card]) => ({ params: { slug }, props: card }))) satisfies GetStaticPaths;

export const GET: APIRoute<OgCard> = async ({ params, props }) => {
  if (params.slug === "home") {
    const bytes = Buffer.from(homeCard.slice(homeCard.indexOf(",") + 1), "base64");
    return new Response(bytes, { headers: { "content-type": "image/png" } });
  }
  return new Response(await renderOgImage(props), { headers: { "content-type": "image/png" } });
};
