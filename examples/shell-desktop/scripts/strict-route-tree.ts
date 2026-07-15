const routeTree = new URL("../src/routeTree.gen.ts", import.meta.url);
const source = await Deno.readTextFile(routeTree);
const unsafeRouteCast = /}\s+as\s+a(?:ny)\)/g;
const matches = source.match(unsafeRouteCast)?.length ?? 0;

if (matches !== 0 && matches !== 3) {
  throw new Error(
    `expected zero or three unsafe route casts, found ${matches}`,
  );
}

await Deno.writeTextFile(routeTree, source.replaceAll(unsafeRouteCast, "})"));
