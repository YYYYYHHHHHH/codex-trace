import assert from "node:assert/strict";
import { test } from "node:test";

import { gzipSync } from "node:zlib";

import { MAX_BODY_BYTES, MAX_DECODED_BYTES, TRACES_PATH, basicPassword, decodedSize, handle, tokenAllowed } from "./worker.js";

const ENV = {
  LANGFUSE_HOST: "https://langfuse.example.com/",
  LANGFUSE_PUBLIC_KEY: "pk-lf-real",
  LANGFUSE_SECRET_KEY: "sk-lf-real",
  INGEST_TOKENS: "old-token, new-token",
};

const GATEWAY = "https://ingest.example.com";

function basic(user, password) {
  return "Basic " + btoa(`${user}:${password}`);
}

function post(body, headers = {}, path = TRACES_PATH) {
  const bytes = typeof body === "string" ? new Uint8Array(gzipSync(body)) : body;
  return new Request(GATEWAY + path, {
    method: "POST",
    body: bytes,
    headers: {
      authorization: basic("codex-trace", "new-token"),
      "content-type": "application/json",
      "content-encoding": "gzip",
      "content-length": String(bytes.byteLength),
      "user-agent": "codex-trace/0.2.0",
      cookie: "session=leak",
      ...headers,
    },
  });
}

// fakeUpstream 记录转发出去的请求，并返回给定响应。
function fakeUpstream(response = new Response('{"partialSuccess":{}}', { status: 200 })) {
  const calls = [];
  const impl = async (url, init) => {
    calls.push({ url, init, body: new Uint8Array(init.body) });
    return response;
  };
  return { calls, impl };
}

test("forwards traces with the real Langfuse credentials", async () => {
  const up = fakeUpstream(
    new Response('{"partialSuccess":{}}', { status: 200, headers: { "content-type": "application/json", "set-cookie": "x=y" } }),
  );

  const res = await handle(post('{"resourceSpans":[]}'), ENV, up.impl);

  assert.equal(res.status, 200);
  assert.equal(await res.text(), '{"partialSuccess":{}}');
  assert.equal(res.headers.get("set-cookie"), null);
  assert.equal(up.calls.length, 1);
  const { url, init, body } = up.calls[0];
  assert.equal(url, "https://langfuse.example.com" + TRACES_PATH);
  assert.equal(init.method, "POST");
  assert.equal(init.redirect, "manual");
  assert.deepEqual(body, new Uint8Array(gzipSync('{"resourceSpans":[]}')));
  assert.equal(init.headers.get("authorization"), basic("pk-lf-real", "sk-lf-real"));
  assert.equal(init.headers.get("content-encoding"), "gzip");
  assert.equal(init.headers.get("content-type"), "application/json");
  assert.equal(init.headers.get("user-agent"), "codex-trace/0.2.0");
  assert.equal(init.headers.get("cookie"), null);
});

test("accepts every configured token so rotation can overlap", async () => {
  for (const token of ["old-token", "new-token"]) {
    const up = fakeUpstream();
    const res = await handle(post("{}", { authorization: basic("any", token) }), ENV, up.impl);
    assert.equal(res.status, 200, token);
  }
});

test("rejects requests without a valid token before touching upstream", async () => {
  const cases = {
    missing: { authorization: "" },
    wrong: { authorization: basic("codex-trace", "nope") },
    "prefix of a token": { authorization: basic("codex-trace", "new-") },
    "real langfuse key": { authorization: basic("pk-lf-real", "sk-lf-real") },
    bearer: { authorization: "Bearer new-token" },
  };
  for (const [name, headers] of Object.entries(cases)) {
    const up = fakeUpstream();
    const res = await handle(post("{}", headers), ENV, up.impl);
    assert.equal(res.status, 401, name);
    assert.equal(up.calls.length, 0, name);
  }
});

test("only exposes the traces ingestion endpoint", async () => {
  const up = fakeUpstream();
  const auth = { authorization: basic("codex-trace", "new-token") };
  const cases = [
    [new Request(GATEWAY + "/api/public/v2/observations", { headers: auth }), 404],
    [new Request(GATEWAY + "/api/public/traces/abc", { method: "DELETE", headers: auth }), 404],
    [new Request(GATEWAY + TRACES_PATH + "/../../v2/observations", { headers: auth }), 404],
    [new Request(GATEWAY + TRACES_PATH, { headers: auth }), 405],
    [new Request(GATEWAY + "/", { headers: auth }), 404],
  ];
  for (const [req, status] of cases) {
    const res = await handle(req, ENV, up.impl);
    assert.equal(res.status, status, `${req.method} ${req.url}`);
  }
  assert.equal(up.calls.length, 0);
});

test("enforces the body size limit", async () => {
  const up = fakeUpstream();

  const declared = await handle(post("{}", { "content-length": String(MAX_BODY_BYTES + 1) }), ENV, up.impl);
  assert.equal(declared.status, 413);

  const invalid = await handle(post("{}", { "content-length": "abc" }), ENV, up.impl);
  assert.equal(invalid.status, 411);

  assert.equal(up.calls.length, 0);
});

test("rejects gzip bombs and unsupported encodings before touching upstream", async () => {
  const up = fakeUpstream();

  const bomb = new Uint8Array(gzipSync(Buffer.alloc(MAX_DECODED_BYTES + 1)));
  assert.ok(bomb.byteLength < MAX_BODY_BYTES);
  assert.equal((await handle(post(bomb), ENV, up.impl)).status, 413);

  const corrupt = await handle(post(new Uint8Array([1, 2, 3, 4])), ENV, up.impl);
  assert.equal(corrupt.status, 400);

  const br = await handle(post("{}", { "content-encoding": "br" }), ENV, up.impl);
  assert.equal(br.status, 415);

  assert.equal(up.calls.length, 0);
});

test("accepts uncompressed bodies", async () => {
  const up = fakeUpstream();
  const raw = new TextEncoder().encode("{}");
  const res = await handle(post(raw, { "content-encoding": "" }), ENV, up.impl);
  assert.equal(res.status, 200);
  assert.equal(up.calls[0].init.headers.get("content-encoding"), null);
});

test("decodedSize counts decompressed bytes", async () => {
  assert.equal(await decodedSize(new Uint8Array(gzipSync("hello")), "gzip"), 5);
  assert.equal(await decodedSize(new Uint8Array(7), "identity"), 7);
});

test("returns 429 with Retry-After when the client IP is rate limited", async () => {
  const up = fakeUpstream();
  const keys = [];
  const env = { ...ENV, RATE_LIMITER: { limit: async ({ key }) => (keys.push(key), { success: false }) } };

  const res = await handle(post("{}", { "cf-connecting-ip": "203.0.113.7" }), env, up.impl);

  assert.equal(res.status, 429);
  assert.equal(res.headers.get("retry-after"), "60");
  assert.deepEqual(keys, ["203.0.113.7"]);
  assert.equal(up.calls.length, 0);
});

test("rate limits before checking the token", async () => {
  const env = { ...ENV, RATE_LIMITER: { limit: async () => ({ success: false }) } };

  const res = await handle(post("{}", { authorization: basic("x", "bad") }), env, fakeUpstream().impl);

  assert.equal(res.status, 429);
});

test("keeps upstream error status but hides its body", async () => {
  const up = fakeUpstream(new Response("db at 10.0.0.5 is busy", { status: 503, headers: { "retry-after": "30" } }));
  const errors = [];
  const original = console.error;
  console.error = (...args) => errors.push(args.join(" "));
  try {
    const res = await handle(post("{}"), ENV, up.impl);

    assert.equal(res.status, 503);
    assert.equal(res.headers.get("retry-after"), "30");
    assert.equal((await res.text()).includes("10.0.0.5"), false);
    assert.ok(errors.some((e) => e.includes("10.0.0.5")), "upstream detail should be logged");
  } finally {
    console.error = original;
  }
});

test("maps upstream network failures to 502", async () => {
  const res = await handle(post("{}"), ENV, async () => {
    throw new Error("connection reset");
  });
  assert.equal(res.status, 502);
});

test("fails closed when secrets are missing", async () => {
  for (const key of Object.keys(ENV)) {
    const env = { ...ENV, [key]: "" };
    const up = fakeUpstream();
    const res = await handle(post("{}"), env, up.impl);
    assert.equal(res.status, 500, key);
    assert.equal(up.calls.length, 0, key);
  }
});

test("basicPassword parses Basic credentials", () => {
  assert.equal(basicPassword(basic("u", "p:with:colons")), "p:with:colons");
  assert.equal(basicPassword(basic("u", "")), "");
  assert.equal(basicPassword("Basic " + btoa("no-colon")), null);
  assert.equal(basicPassword("Basic !!!"), null);
  assert.equal(basicPassword(null), null);
});

test("tokenAllowed ignores blank entries", async () => {
  assert.equal(await tokenAllowed("", " , ,"), false);
  assert.equal(await tokenAllowed("a", ""), false);
  assert.equal(await tokenAllowed("a", undefined), false);
  assert.equal(await tokenAllowed("b", "a, b ,"), true);
});
