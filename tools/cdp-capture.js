const fs = require("fs");
const path = require("path");

const wsURL = process.argv[2];
if (!wsURL) {
  console.error("usage: node tools/cdp-capture.js <devtools-page-ws-url>");
  process.exit(2);
}

const captureDir = path.join(process.cwd(), "captures");
fs.mkdirSync(captureDir, { recursive: true });
const captureFile = path.join(
  captureDir,
  `icloud-capture-${new Date().toISOString().replace(/[:.]/g, "-")}.jsonl`,
);

function redact(value) {
  if (value == null) return value;
  let text = String(value);
  text = text.replace(
    /(authorization|cookie|x-apple-[^:=\s]+|scnt|session|token|secret|password)(["']?\s*[:=]\s*["']?)[^"'&\s,}]+/gi,
    "$1$2[REDACTED]",
  );
  text = text.replace(/([?&](?:ck|token|session|code|auth|scnt|key)=)[^&]+/gi, "$1[REDACTED]");
  return text;
}

function safe(value) {
  return JSON.parse(JSON.stringify(value, (key, val) => {
    if (/cookie|authorization|token|secret|password|scnt/i.test(key)) return "[REDACTED]";
    if (typeof val === "string") return redact(val);
    return val;
  }));
}

function write(event) {
  fs.appendFileSync(
    captureFile,
    `${JSON.stringify(safe({ ...event, ts: new Date().toISOString() }))}\n`,
  );
}

let id = 0;
const pending = new Map();
const sock = new WebSocket(wsURL);

function send(method, params = {}) {
  sock.send(JSON.stringify({ id: ++id, method, params }));
  return id;
}

function interesting(url) {
  return /icloud|apple|idmsa/i.test(url || "");
}

sock.onopen = () => {
  send("Network.enable", {
    maxTotalBufferSize: 10000000,
    maxResourceBufferSize: 5000000,
    maxPostDataSize: 2000000,
  });
  send("Page.enable");
  console.log(`CAPTURE_FILE=${captureFile}`);
  console.log("现在可以在比特浏览器里登录并手动创建隐私邮箱；按 Ctrl+C 停止抓包。");
};

sock.onmessage = (event) => {
  let message;
  try {
    message = JSON.parse(event.data);
  } catch {
    return;
  }

  if (message.method === "Network.requestWillBeSent") {
    const req = message.params.request || {};
    pending.set(message.params.requestId, { url: req.url, method: req.method });
    if (interesting(req.url)) {
      write({
        event: "request",
        requestId: message.params.requestId,
        method: req.method,
        url: req.url,
        postData: req.postData,
        headers: req.headers,
        type: message.params.type,
        initiator: message.params.initiator?.type,
      });
    }
    return;
  }

  if (message.method === "Network.responseReceived") {
    const res = message.params.response || {};
    const prev = pending.get(message.params.requestId) || {};
    if (interesting(res.url || prev.url)) {
      write({
        event: "response",
        requestId: message.params.requestId,
        status: res.status,
        statusText: res.statusText,
        url: res.url || prev.url,
        mimeType: res.mimeType,
        headers: res.headers,
      });
    }
    return;
  }

  if (message.method === "Network.loadingFinished") {
    const prev = pending.get(message.params.requestId);
    if (prev && interesting(prev.url)) {
      send("Network.getResponseBody", { requestId: message.params.requestId });
    }
    return;
  }

  if (message.id && message.result && Object.prototype.hasOwnProperty.call(message.result, "body")) {
    write({
      event: "body",
      body: message.result.base64Encoded ? "[BASE64_BODY]" : redact((message.result.body || "").slice(0, 5000)),
    });
  }
};

sock.onerror = (event) => {
  console.error("capture websocket error", event.message || event);
};

sock.onclose = () => {
  console.log("capture closed");
};

setInterval(() => {}, 1000);

