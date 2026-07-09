export type {
  AgentStatus,
  ErrorBody,
  ErrorDetail,
  Event,
  ForwardStatus,
  Health,
  MasterStatus,
  Notify,
  PortStatus,
  ServiceStatus,
  Status,
  VersionInfo,
} from "./dto.ts";

export { ApiError, PortalClient, apiErrorFromStatusBody, createClient } from "./http.ts";
export type { PortalRequestOptions } from "./http.ts";

export { events } from "./events.ts";

export {
  ExecStreamError,
  ExecStreamExit,
  ExecStreamStderr,
  ExecStreamStdin,
  ExecStreamStdout,
  ExecStreamWinch,
  decode,
  decodeExecFrame,
  encodeExecFrame,
} from "./cbor.ts";
export type { CborMap, CborValue, ExecFrame, ExecFrameInit } from "./cbor.ts";

export {
  GUID,
  MaxPayload,
  OpBinary,
  OpClose,
  OpContinuation,
  OpPing,
  OpPong,
  OpText,
  WebSocketFrameReader,
  acceptKey,
  writeClose,
  writeFrame,
  writePing,
  writePong,
} from "./wsframe.ts";
export type { Frame, Opcode } from "./wsframe.ts";

export { PTY_UNSUPPORTED_MESSAGE, PtyUnsupportedError, exec, runExec } from "./exec.ts";
export type { ByteChunk, ByteSink, ExecOptions, ExecPtyOptions, ExecResult, ExecSession, StdinSource, WinchSize, WinchSource } from "./exec.ts";

export { EXPECTED_PROTO_VERSION, checkProtoVersion } from "./version.ts";
export type { ProtoVersionCheck, VersionClient } from "./version.ts";
