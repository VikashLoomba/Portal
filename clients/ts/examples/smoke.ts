import { createClient, events, runExec } from "../src/index.ts";

const socketPath = process.argv[2] ?? process.env.PORTAL_SOCKET ?? process.env.PORTAL_APISOCK;

if (socketPath === undefined || socketPath === "") {
  throw new Error("usage: node examples/smoke.ts <api-socket-path>");
}

const client = createClient(socketPath);
const status = await client.status();
const portCount = status.ports?.length ?? 0;
const forwardCount = status.forwards?.length ?? 0;
const agent = status.agent === null ? "none" : `${status.agent.pid}:${status.agent.sha}`;
console.log(`status host=${status.host} master=${status.master.up} agent=${agent} ports=${portCount} forwards=${forwardCount}`);

const stream = events(socketPath);
const first = await stream.next();
if (first.done === true) {
  throw new Error("events stream ended before first line");
}
console.log(`event type=${first.value.type}`);
await stream.return(undefined);

const chunks: Uint8Array[] = [];
const result = await runExec(socketPath, {
  argv: ["echo", "ts-smoke"],
  stdout(chunk) {
    chunks.push(chunk);
  },
});
const output = Buffer.concat(chunks).toString("utf8").trimEnd();
console.log(`exec code=${result.code} output=${JSON.stringify(output)}`);
