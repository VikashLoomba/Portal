export interface SseRecord {
  event: string;
  data: string;
}

export async function readSse(
  response: Response,
  onRecord: (record: SseRecord) => void,
): Promise<void> {
  if (!response.ok) {
    throw new Error(await responseError(response));
  }
  if (response.body === null) {
    throw new Error("stream response has no body");
  }

  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  let pending = "";
  for (;;) {
    const chunk = await reader.read();
    pending += decoder.decode(chunk.value, { stream: !chunk.done });
    let boundary = pending.indexOf("\n\n");
    while (boundary >= 0) {
      const block = pending.slice(0, boundary);
      pending = pending.slice(boundary + 2);
      const record = parseBlock(block);
      if (record !== null) {
        onRecord(record);
      }
      boundary = pending.indexOf("\n\n");
    }
    if (chunk.done) {
      return;
    }
  }
}

async function responseError(response: Response): Promise<string> {
  const text = await response.text();
  if (text === "") {
    return `request failed (${response.status})`;
  }
  try {
    const value: unknown = JSON.parse(text);
    if (
      typeof value === "object" && value !== null && "error" in value &&
      typeof value.error === "string"
    ) {
      return value.error;
    }
  } catch {
    // Plain-text failures are already suitable for display.
  }
  return text;
}

function parseBlock(block: string): SseRecord | null {
  let event = "message";
  const data: string[] = [];
  for (const line of block.split("\n")) {
    if (line.startsWith("event:")) {
      event = line.slice(6).trimStart();
    } else if (line.startsWith("data:")) {
      data.push(line.slice(5).trimStart());
    }
  }
  return data.length === 0 ? null : { event, data: data.join("\n") };
}
