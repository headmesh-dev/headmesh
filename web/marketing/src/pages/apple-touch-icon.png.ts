import type { APIRoute } from "astro";
import { renderTouchIcon } from "../og/render";

export const GET: APIRoute = () => new Response(renderTouchIcon(), { headers: { "content-type": "image/png" } });
