export interface ExecStartRequest {
  argv: string[];
  term: string;
  rows: number;
  cols: number;
}

export interface ExecSize {
  rows: number;
  cols: number;
}

export function normalizeExecStart(value: unknown): ExecStartRequest | null {
  if (!isRecord(value)) {
    return null;
  }
  const argv = Array.isArray(value.argv)
    ? value.argv.filter((item): item is string => typeof item === "string")
      .slice(0, 32)
    : [];
  const term = typeof value.term === "string" && value.term.length > 0 &&
      value.term.length < 64
    ? value.term
    : "xterm-256color";
  return {
    argv,
    term,
    rows: normalizeDimension(value.rows, 6, 200, 24),
    cols: normalizeDimension(value.cols, 20, 500, 80),
  };
}

export function normalizeExecSize(value: unknown): ExecSize | null {
  if (!isRecord(value)) {
    return null;
  }
  return {
    rows: normalizeDimension(value.rows, 6, 200, 24),
    cols: normalizeDimension(value.cols, 20, 500, 80),
  };
}

export function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null;
}

function normalizeDimension(
  value: unknown,
  min: number,
  max: number,
  fallback: number,
): number {
  if (typeof value !== "number" || !Number.isInteger(value)) {
    return fallback;
  }
  return Math.max(min, Math.min(max, value));
}
