// Gate-only type shim; the real package supplies these at runtime (npm install).
declare module "@xterm/xterm" {
  export interface ITheme {
    background?: string;
    foreground?: string;
    cursor?: string;
    selectionBackground?: string;
    black?: string;
    red?: string;
    green?: string;
    yellow?: string;
    blue?: string;
    magenta?: string;
    cyan?: string;
    white?: string;
    brightBlack?: string;
    brightRed?: string;
    brightGreen?: string;
    brightYellow?: string;
    brightBlue?: string;
    brightMagenta?: string;
    brightCyan?: string;
    brightWhite?: string;
  }

  export interface ITerminalOptions {
    cursorBlink?: boolean;
    fontFamily?: string;
    fontSize?: number;
    lineHeight?: number;
    scrollback?: number;
    convertEol?: boolean;
    theme?: ITheme;
  }

  export interface IDisposable {
    dispose(): void;
  }

  export interface Terminal {
    open(parent: HTMLElement): void;
    onData(callback: (data: string) => void): IDisposable;
    write(data: string | Uint8Array): void;
    resize(columns: number, rows: number): void;
    reset(): void;
    focus(): void;
    dispose(): void;
  }

  export interface TerminalConstructor {
    new (options?: ITerminalOptions): Terminal;
  }
}
