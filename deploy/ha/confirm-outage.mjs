import { pathToFileURL } from "node:url";
import { setTimeout as sleep } from "node:timers/promises";
import { checkServices, services } from "./detector/health.mjs";

// Run from an independent network before any action against the primary.
export async function confirmOutage(probe = checkServices, pause = sleep) {
  let failed = Object.keys(services);
  for (let attempt = 0; attempt < 3; attempt++) {
    if (attempt) await pause(60_000);
    const observed = await probe();
    failed = failed.filter((service) => !observed[service]);
    if (!failed.length) return { confirmed: false, failed_services: [] };
  }
  return { confirmed: true, failed_services: failed };
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  const result = await confirmOutage();
  console.log(JSON.stringify(result));
  process.exitCode = result.confirmed ? 0 : 1;
}
