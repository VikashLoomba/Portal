import type { VersionInfo } from "./dto.ts";
import type { PortalRequestOptions } from "./http.ts";

export const EXPECTED_PROTO_VERSION = 4;

export interface VersionClient {
  version(options?: PortalRequestOptions): Promise<VersionInfo>;
}

export interface ProtoVersionCheck {
  expected: number;
  actual: number;
  version: VersionInfo;
}

export async function checkProtoVersion(client: VersionClient, options: PortalRequestOptions = {}): Promise<ProtoVersionCheck> {
  const version = await client.version(options);
  if (version.protoVersion !== EXPECTED_PROTO_VERSION) {
    throw new Error(`portal protocol version mismatch: client expects ${EXPECTED_PROTO_VERSION}, daemon reports ${version.protoVersion}`);
  }
  return {
    expected: EXPECTED_PROTO_VERSION,
    actual: version.protoVersion,
    version,
  };
}
