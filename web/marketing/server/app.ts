import type { IncomingMessage, ServerResponse } from "node:http";
import { fail, json, log, MAX_BODY_BYTES } from "./http.ts";
import { handleConfirm, handleSignup, handleUnsubscribe, type WaitlistContext } from "./waitlist.ts";

type Handler = (request: Request, ctx: WaitlistContext) => Promise<Response>;

const ROUTES: Record<string, Handler> = {
  "/api/waitlist": handleSignup,
  "/api/waitlist/confirm": handleConfirm,
  "/api/waitlist/unsubscribe": handleUnsubscribe,
};

const empty = (status: number, headers: Record<string, string> = {}) =>
  new Response(null, { status, headers: { "cache-control": "no-store", ...headers } });

/** Route one request. Pure Web Request in, Response out, so tests can call it without a socket. */
export async function handle(request: Request, ctx: WaitlistContext): Promise<Response> {
  const { pathname } = new URL(request.url);

  if (pathname === "/healthz") return empty(204);
  if (pathname === "/readyz") {
    try {
      ctx.store.ping();
      return empty(204);
    } catch (error) {
      log("error", "waitlist.readyz.failed", { message: error instanceof Error ? error.message : String(error) });
      return empty(503);
    }
  }

  const route = ROUTES[pathname];
  if (!route) return json(404, { ok: false, error: "invalid_request" });
  if (request.method !== "POST") return empty(405, { allow: "POST" });
  return route(request, ctx);
}

/**
 * Buffer at most MAX_BODY_BYTES. Anything beyond that is drained and dropped (nginx already caps bodies at 16 KiB),
 * so a client can't make the service hold an unbounded body in memory.
 */
async function readBody(req: IncomingMessage): Promise<Buffer | "too_large"> {
  const chunks: Buffer[] = [];
  let size = 0;
  for await (const chunk of req as AsyncIterable<Buffer>) {
    size += chunk.byteLength;
    if (size <= MAX_BODY_BYTES) chunks.push(chunk);
  }
  return size > MAX_BODY_BYTES ? "too_large" : Buffer.concat(chunks);
}

function toRequest(req: IncomingMessage, body: Buffer): Request {
  const headers = new Headers();
  for (const [name, values] of Object.entries(req.headersDistinct)) {
    for (const value of values ?? []) headers.append(name, value);
  }
  const method = req.method ?? "GET";
  return new Request(`http://${req.headers.host ?? "localhost"}${req.url ?? "/"}`, {
    method,
    headers,
    ...(method !== "GET" && method !== "HEAD" && { body: new Uint8Array(body) }),
  });
}

async function send(res: ServerResponse, response: Response): Promise<void> {
  const headers: Record<string, string> = {};
  response.headers.forEach((value, name) => {
    headers[name] = value;
  });
  res.writeHead(response.status, headers);
  res.end(Buffer.from(await response.arrayBuffer()));
}

/** node:http request listener for the service. */
export function createListener(ctx: WaitlistContext) {
  return async (req: IncomingMessage, res: ServerResponse): Promise<void> => {
    try {
      const body = await readBody(req);
      if (body === "too_large") {
        await send(res, fail("payload_too_large"));
        return;
      }
      await send(res, await handle(toRequest(req, body), ctx));
    } catch (error) {
      log("error", "waitlist.request.failed", { message: error instanceof Error ? error.message : String(error) });
      if (!res.headersSent) await send(res, fail("unavailable"));
      else res.destroy();
    }
  };
}
