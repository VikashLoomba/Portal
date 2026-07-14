import { basename, dirname, join, resolve } from "node:path";

export type BinarySourceKind = "override" | "packaged" | "development";

export interface BinarySource {
  kind: BinarySourceKind;
  path: string;
}

export interface BinarySourceOptions {
  env: Record<string, string>;
  packaged: boolean;
  moduleDir: string;
  cwd: string;
}

type Exists = (path: string) => boolean | Promise<boolean>;

export async function selectBinarySource(
  options: BinarySourceOptions,
  exists: Exists,
): Promise<BinarySource> {
  const override = options.env.PORTAL_BIN;
  if (override !== undefined && override !== "") {
    if (!await exists(override)) {
      throw new Error(`PORTAL_BIN does not exist: ${override}`);
    }
    return { kind: "override", path: override };
  }

  if (options.packaged) {
    for (const candidate of packagedCandidates(options.moduleDir)) {
      if (await exists(candidate)) {
        return { kind: "packaged", path: candidate };
      }
    }
    throw new Error(
      "packaged portal resource is missing; package with --include ../../portal",
    );
  }

  const development = resolve(options.cwd, "../../portal");
  if (!await exists(development)) {
    throw new Error(
      `development portal binary is missing: ${development}; run make portal`,
    );
  }
  return { kind: "development", path: development };
}

export async function resolvePortalBinary(configDir: string): Promise<string> {
  const source = await selectBinarySource({
    env: Deno.env.toObject(),
    packaged: isPackagedRuntime(),
    moduleDir: import.meta.dirname ?? Deno.cwd(),
    cwd: Deno.cwd(),
  }, fileExists);
  if (source.kind !== "packaged") {
    return source.path;
  }
  return await extractPackagedBinary(source.path, join(configDir, "bin"));
}

export function isPackagedRuntime(execPath: string = Deno.execPath()): boolean {
  return basename(execPath) !== "deno";
}

function packagedCandidates(moduleDir: string): string[] {
  const candidates: string[] = [];
  let current = moduleDir;
  for (let depth = 0; depth < 8; depth += 1) {
    candidates.push(join(current, "portal"));
    const parent = dirname(current);
    if (parent === current) {
      break;
    }
    current = parent;
  }
  return candidates;
}

async function extractPackagedBinary(
  resourcePath: string,
  cacheDir: string,
): Promise<string> {
  const bytes = await Deno.readFile(resourcePath);
  const digest = new Uint8Array(await crypto.subtle.digest("SHA-256", bytes));
  const hash = Array.from(
    digest.slice(0, 12),
    (value) => value.toString(16).padStart(2, "0"),
  ).join("");
  const destination = join(cacheDir, `portal-${hash}`);
  await Deno.mkdir(cacheDir, { recursive: true, mode: 0o700 });
  if (await fileExists(destination)) {
    await Deno.chmod(destination, 0o755);
    return destination;
  }

  const temporary = join(cacheDir, `.portal-${crypto.randomUUID()}.tmp`);
  try {
    await Deno.writeFile(temporary, bytes, { createNew: true, mode: 0o700 });
    await Deno.chmod(temporary, 0o755);
    try {
      await Deno.rename(temporary, destination);
    } catch (error) {
      if (!(error instanceof Deno.errors.AlreadyExists)) {
        throw error;
      }
    }
  } finally {
    await Deno.remove(temporary).catch(() => {});
  }
  await Deno.chmod(destination, 0o755);
  return destination;
}

async function fileExists(path: string): Promise<boolean> {
  try {
    return (await Deno.stat(path)).isFile;
  } catch (error) {
    if (error instanceof Deno.errors.NotFound) {
      return false;
    }
    throw error;
  }
}
