// Waitlist service for headmesh.dev. nginx serves the static site and proxies /api/waitlist* here.
//
//   node server/main.ts          (Node 24 runs the TypeScript directly; settings come from the environment)
//
// See server/config.ts for the variables. On the server it runs as headmesh-waitlist@<env>-<colour>.service.
import { createServer } from "node:http";
import { createListener } from "./app.ts";
import { loadConfig } from "./config.ts";
import { migrate, openDatabase } from "./db.ts";
import { log } from "./http.ts";
import { WaitlistStore } from "./store.ts";

const SHUTDOWN_GRACE_MS = 10_000;

const config = loadConfig();
const db = openDatabase(config.dbPath);
const applied = migrate(db);
if (applied.length > 0) log("info", "waitlist.migrated", { applied });

const server = createServer(createListener({ config, store: new WaitlistStore(db) }));
server.headersTimeout = 10_000;
server.requestTimeout = 15_000;
server.keepAliveTimeout = 5_000;

server.listen(config.port, config.host, () => {
  log("info", "waitlist.listening", { host: config.host, port: config.port, delivery: config.emailDelivery });
});

// systemd sends SIGTERM when the old colour is retired: finish in-flight requests, then close the database.
let stopping = false;
function shutdown(signal: string): void {
  if (stopping) return;
  stopping = true;
  log("info", "waitlist.stopping", { signal });
  const force = setTimeout(() => {
    log("warn", "waitlist.shutdown_forced");
    process.exit(1);
  }, SHUTDOWN_GRACE_MS);
  force.unref();
  server.close(() => {
    db.close();
    process.exit(0);
  });
  server.closeIdleConnections();
}
process.on("SIGTERM", () => shutdown("SIGTERM"));
process.on("SIGINT", () => shutdown("SIGINT"));
