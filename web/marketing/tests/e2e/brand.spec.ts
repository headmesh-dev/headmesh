import { cspViolations, expect, test } from "./fixtures";

for (const colorScheme of ["light", "dark"] as const) {
  test(`favicons preserve transparent gaps in ${colorScheme} mode`, async ({ page, request, problems }) => {
    await page.emulateMedia({ colorScheme });
    for (const path of ["/", "/docs/"]) {
      await page.goto(path);
      const samples = await page.evaluate(async () => {
        const links = [...document.querySelectorAll<HTMLLinkElement>('link[rel~="icon"]')];
        const icon = links.filter((link) => !link.media || matchMedia(link.media).matches).at(-1);
        if (!icon) throw new Error("No favicon for this colour scheme");
        const image = new Image();
        image.src = icon.href;
        await image.decode();
        const canvas = document.createElement("canvas");
        canvas.width = canvas.height = 512;
        const context = canvas.getContext("2d");
        if (!context) throw new Error("No 2D canvas context");
        context.drawImage(image, 0, 0, 512, 512);
        const pixel = (x: number, y: number) => [...context.getImageData(x, y, 1, 1).data];
        return {
          path: new URL(icon.href).pathname,
          version: new URL(icon.href).searchParams.get("v"),
          gaps: [pixel(189, 340)[3], pixel(323, 340)[3], pixel(256, 150)[3]],
          leg: pixel(122, 340),
        };
      });
      expect(samples.path).toBe(colorScheme === "dark" ? "/favicon-dark.svg" : "/favicon.svg");
      expect(samples.version).toBe("transparent-1");
      expect(samples.gaps).toEqual([0, 0, 0]);
      expect(samples.leg).toEqual(colorScheme === "dark" ? [242, 246, 249, 255] : [7, 11, 18, 255]);
      expect(await cspViolations(page)).toEqual([]);
    }
    const ico = await request.get("/favicon.ico?v=transparent-1");
    expect(ico.status()).toBe(200);
    const bytes = await ico.body();
    expect(bytes.readUInt16LE(2)).toBe(1);
    expect(bytes.readUInt16LE(4)).toBe(3);
    expect(problems.console).toEqual([]);
  });
}
