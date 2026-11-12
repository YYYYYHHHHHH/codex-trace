import assert from "node:assert/strict";
import { test } from "node:test";

import { MAX_BODY_BYTES, TRACES_PATH, basicPassword, handle, tokenAllowed } from "./worker.js";

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
  const bytes = new TextEncoder().encode(body);
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
    calls.push({ url, init, body: new TextDecoder().decode(init.body) });
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
  assert.equal(body, '{"resourceSpans":[]}');
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

test("does not spend rate limit budget on unauthenticated requests", async () => {
  const keys = [];
  const env = { ...ENV, RATE_LIMITER: { limit: async ({ key }) => (keys.push(key), { success: true }) } };

  const res = await handle(post("{}", { authorization: basic("x", "bad") }), env, fakeUpstream().impl);

  assert.equal(res.status, 401);
  assert.deepEqual(keys, []);
});

test("passes upstream errors through so the client can retry", async () => {
  const up = fakeUpstream(new Response("busy", { status: 503, headers: { "retry-after": "30" } }));

  const res = await handle(post("{}"), ENV, up.impl);

  assert.equal(res.status, 503);
  assert.equal(res.headers.get("retry-after"), "30");
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
