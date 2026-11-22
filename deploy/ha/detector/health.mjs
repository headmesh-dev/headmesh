export const services = Object.freeze({
  cell: { url: "https://hs1.headmesh.dev/health", status: 200 },
  api: { url: "https://api.headmesh.dev/readyz", status: 204 },
});

export async function checkServices(fetcher = fetch) {
  const results = await Promise.all(Object.entries(services).map(async ([name, { url, status }]) => {
    try {
      const response = await fetcher(url, {
        cache: "no-store",
        redirect: "manual",
        signal: AbortSignal.timeout(5000),
      });
      await response.body?.cancel();
      return [name, response.status === status];
    } catch {
      return [name, false];
    }
  }));
  return Object.fromEntries(results);
}
