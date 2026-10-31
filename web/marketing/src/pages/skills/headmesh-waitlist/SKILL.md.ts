// The waitlist as an Agent Skill, at /skills/headmesh-waitlist/SKILL.md. "Copy for LLM" points agents here.
import type { APIRoute } from "astro";
import { waitlistSkill } from "../../../agents/markdown.ts";

export const GET: APIRoute = () =>
  new Response(waitlistSkill(), { headers: { "content-type": "text/markdown; charset=utf-8" } });
