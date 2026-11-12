// codex-trace 上报网关（Cloudflare Worker）。
//
// 客户端只内置网关 token，真实的 Langfuse key 只保存在 Worker secret 中。
// 网关只放行 OTLP traces 写入，token 泄露后无法读取或删除 Langfuse 中的数据。
// 注意：Langfuse 按 trace / span id 合并写入，知道某个 id 的人可以覆盖该条数据；
// 客户端的 id 由 Codex 随机生成的 turn / tool id 派生，且 token 无法读出已有 id。
//
// 环境变量：
//   LANGFUSE_HOST         Langfuse 地址（vars）
//   LANGFUSE_PUBLIC_KEY   Langfuse public key（secret）
//   LANGFUSE_SECRET_KEY   Langfuse secret key（secret）
//   INGEST_TOKENS         允许的网关 token，逗号分隔；轮换时新旧 token 可并存（secret）
//   RATE_LIMITER          按客户端 IP 限速的 ratelimit 绑定（可选）

export const TRACES_PATH = "/api/public/otel/v1/traces";
export const MAX_BODY_BYTES = 5 * 1024 * 1024;
// 解压后的上限，防止小体积 gzip 炸弹在 Langfuse 侧展开成巨量数据。正常批次（100 个 span、单字段 10KB）远小于此值。
export const MAX_DECODED_BYTES = 32 * 1024 * 1024;

// 只转发这些请求头，其余（Cookie、CF-* 等）一律丢弃。
const FORWARD_HEADERS = ["content-type", "content-encoding", "user-agent"];

const encoder = new TextEncoder();

function reply(status, message, headers = {}) {
  return new Response(message + "\n", {
    status,
    headers: { "content-type": "text/plain; charset=utf-8", ...headers },
  });
}

// basicPassword 返回 Basic Auth 中的密码；客户端把网关 token 放在 secret_key 位置。
export function basicPassword(header) {
  const match = /^Basic\s+([A-Za-z0-9+/=]+)$/.exec(header ?? "");
  if (!match) return null;
  let decoded;
  try {
    decoded = atob(match[1]);
  } catch {
    return null;
  }
  const sep = decoded.indexOf(":");
  return sep < 0 ? null : decoded.slice(sep + 1);
}

async function digest(value) {
  return new Uint8Array(await crypto.subtle.digest("SHA-256", encoder.encode(value)));
}

// equalConstantTime 先哈希成等长摘要再比较，避免按长度或前缀泄露 token。
async function equalConstantTime(a, b) {
  const [x, y] = await Promise.all([digest(a), digest(b)]);
  if (crypto.subtle.timingSafeEqual) return crypto.subtle.timingSafeEqual(x, y);
  let diff = 0;
  for (let i = 0; i < x.length; i++) diff |= x[i] ^ y[i];
  return diff === 0;
}

// tokenAllowed 检查密码是否命中任一允许的 token；逐个比较完，不提前返回。
export async function tokenAllowed(password, tokens) {
  const allowed = (tokens ?? "")
    .split(",")
    .map((t) => t.trim())
    .filter(Boolean);
  if (password == null || allowed.length === 0) return false;
  const results = await Promise.all(allowed.map((t) => equalConstantTime(password, t)));
  return results.some(Boolean);
}

// decodedSize 返回请求体解压后的字节数；超过上限时返回 Infinity，gzip 损坏时抛出异常。
export async function decodedSize(body, encoding) {
  if (encoding === "identity") return body.byteLength;
  const reader = new Blob([body]).stream().pipeThrough(new DecompressionStream("gzip")).getReader();
  let total = 0;
  for (;;) {
    const { done, value } = await reader.read();
    if (done) return total;
    total += value.byteLength;
    if (total > MAX_DECODED_BYTES) {
      await reader.cancel();
      return Infinity;
    }
  }
}

// contentEncoding 只接受 gzip 或不压缩，其余返回 null。
function contentEncoding(request) {
  const value = (request.headers.get("content-encoding") ?? "").trim().toLowerCase();
  if (value === "" || value === "identity") return "identity";
  return value === "gzip" ? "gzip" : null;
}

function configured(env) {
  return Boolean(env.LANGFUSE_HOST && env.LANGFUSE_PUBLIC_KEY && env.LANGFUSE_SECRET_KEY && env.INGEST_TOKENS);
}

async function rateLimited(request, env) {
  if (!env.RATE_LIMITER) return false;
  const ip = request.headers.get("cf-connecting-ip") ?? "unknown";
  const { success } = await env.RATE_LIMITER.limit({ key: ip });
  return !success;
}

function upstreamHeaders(request, env) {
  const headers = new Headers();
  for (const name of FORWARD_HEADERS) {
    const value = request.headers.get(name);
    if (value) headers.set(name, value);
  }
  headers.set("authorization", "Basic " + btoa(`${env.LANGFUSE_PUBLIC_KEY}:${env.LANGFUSE_SECRET_KEY}`));
  return headers;
}

// handle 处理一次请求；fetchImpl 便于测试替换上游。
export async function handle(request, env, fetchImpl = fetch) {
  const url = new URL(request.url);
  if (url.pathname !== TRACES_PATH) return reply(404, "not found");
  if (request.method !== "POST") return reply(405, "method not allowed", { allow: "POST" });

  if (!configured(env)) {
    console.error("codex-trace gateway: missing LANGFUSE_HOST / LANGFUSE_PUBLIC_KEY / LANGFUSE_SECRET_KEY / INGEST_TOKENS");
    return reply(500, "gateway not configured");
  }
  // 先限速再鉴权：错误 token 的请求同样消耗配额，避免被用来无限制地消耗 Worker 资源。
  if (await rateLimited(request, env)) return reply(429, "rate limited", { "retry-after": "60" });
  if (!(await tokenAllowed(basicPassword(request.headers.get("authorization")), env.INGEST_TOKENS))) {
    return reply(401, "invalid ingest token", { "www-authenticate": 'Basic realm="codex-trace"' });
  }
  const encoding = contentEncoding(request);
  if (!encoding) return reply(415, "unsupported content-encoding");

  const raw = request.headers.get("content-length");
  const length = Number(raw);
  if (raw == null || raw === "" || !Number.isSafeInteger(length) || length < 0) {
    return reply(411, "content-length required");
  }
  if (length > MAX_BODY_BYTES) return reply(413, "payload too large");

  // 先读入内存：长度已受上限约束，且上游拿到的是定长请求体，不依赖分块传输。
  const body = await request.arrayBuffer();
  if (body.byteLength > MAX_BODY_BYTES) return reply(413, "payload too large");
  let size;
  try {
    size = await decodedSize(body, encoding);
  } catch {
    return reply(400, "invalid gzip body");
  }
  if (size > MAX_DECODED_BYTES) return reply(413, "decoded payload too large");

  const target = env.LANGFUSE_HOST.replace(/\/+$/, "") + TRACES_PATH;
  let upstream;
  try {
    upstream = await fetchImpl(target, {
      method: "POST",
      headers: upstreamHeaders(request, env),
      body,
      redirect: "manual",
    });
  } catch (err) {
    console.error("codex-trace gateway: upstream fetch failed", String(err));
    return reply(502, "upstream unavailable");
  }

  const retryAfter = upstream.headers.get("retry-after");
  if (!upstream.ok) {
    // 上游错误详情只写入 Worker 日志，不回传给客户端，避免泄露内部信息。
    const detail = (await upstream.text().catch(() => "")).slice(0, 500);
    console.error("codex-trace gateway: upstream error", upstream.status, detail);
    return reply(upstream.status, `upstream error ${upstream.status}`, retryAfter ? { "retry-after": retryAfter } : {});
  }
  const headers = new Headers();
  const type = upstream.headers.get("content-type");
  if (type) headers.set("content-type", type);
  return new Response(upstream.body, { status: upstream.status, headers });
}

export default {
  fetch: (request, env) => handle(request, env),
};
