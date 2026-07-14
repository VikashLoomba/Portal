export interface VersionInfo {
  version: string;
  gitSha: string;
  protoVersion: number;
}

export interface AgentStatus {
  pid: number;
  sha: string;
  kernel: string;
  bootId: string;
}

export interface PortStatus {
  port: number;
}

export interface ForwardStatus {
  name: string;
}

export interface ServiceStatus {
  loaded: boolean;
  stateLines: string[] | null;
}

export interface MasterStatus {
  up: boolean;
  pid: number;
  transport: string;
  detail: string;
}

export interface Health {
  lastDisconnectErr?: string;
  droppedNotifyCount: number;
  eventsSubscribers: number;
  reconcileCount: number;
}

export interface Status {
  version: VersionInfo;
  host: string;
  service: ServiceStatus;
  master: MasterStatus;
  agent: AgentStatus | null;
  ports: PortStatus[] | null;
  forwards: ForwardStatus[] | null;
  allowed: number[] | null;
  features: Record<string, boolean> | null;
  health: Health;
}

export interface Notify {
  title: string;
  body: string;
  subtitle: string;
  urgency: number;
  verified: boolean;
  source: string;
  sound: string;
  seq: number;
}

export interface Event {
  type: string;
  status?: Status | null;
  notify?: Notify | null;
}

export interface ErrorDetail {
  code: string;
  message: string;
}

export interface ErrorBody {
  error: ErrorDetail;
}

export interface SetupRequest {
  host?: string;
  force?: boolean;
}

export interface SetupEvent {
  step: string;
  status: string;
  line?: string;
  error?: ErrorDetail;
  report?: unknown;
}
