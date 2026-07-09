"use strict";

const { contextBridge, ipcRenderer } = require("electron");

contextBridge.exposeInMainWorld("portal", {
  config() {
    return ipcRenderer.invoke("portal:config");
  },
  onStatus(callback) {
    return subscribe("portal:status", callback);
  },
  onStatusError(callback) {
    return subscribe("portal:status:error", callback);
  },
  onEvent(callback) {
    return subscribe("portal:event", callback);
  },
  onEventError(callback) {
    return subscribe("portal:event:error", callback);
  },
  startExec(request) {
    return ipcRenderer.invoke("portal:exec:start", normalizeExecRequest(request));
  },
  writeExec(id, data) {
    ipcRenderer.send("portal:exec:stdin", {
      id: String(id),
      data: typeof data === "string" ? data : "",
    });
  },
  resizeExec(id, rows, cols) {
    ipcRenderer.send("portal:exec:resize", {
      id: String(id),
      rows: normalizeInteger(rows),
      cols: normalizeInteger(cols),
    });
  },
  closeExec(id) {
    ipcRenderer.send("portal:exec:close", { id: String(id) });
  },
  onExecData(callback) {
    return subscribe("portal:exec:data", callback);
  },
  onExecExit(callback) {
    return subscribe("portal:exec:exit", callback);
  },
  onExecError(callback) {
    return subscribe("portal:exec:error", callback);
  },
});

function subscribe(channel, callback) {
  if (typeof callback !== "function") {
    throw new TypeError("callback must be a function");
  }
  const listener = (_event, payload) => {
    callback(payload);
  };
  ipcRenderer.on(channel, listener);
  return () => {
    ipcRenderer.removeListener(channel, listener);
  };
}

function normalizeExecRequest(request) {
  if (request === null || typeof request !== "object") {
    return { argv: [], rows: 24, cols: 80, term: "xterm-256color" };
  }
  return {
    argv: Array.isArray(request.argv) ? request.argv.filter((item) => typeof item === "string") : [],
    rows: normalizeInteger(request.rows),
    cols: normalizeInteger(request.cols),
    term: typeof request.term === "string" ? request.term : "xterm-256color",
  };
}

function normalizeInteger(value) {
  return Number.isInteger(value) ? value : 0;
}
