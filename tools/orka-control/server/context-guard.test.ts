import assert from "node:assert/strict";
import test from "node:test";
import { ContextGuard } from "../src/lib/context-guard.js";

test("context guard rejects stale crawls after a cluster switch", () => {
  const guard = new ContextGuard();
  guard.select("cluster-a");
  const crawlA = guard.beginCrawl("cluster-a");
  guard.select("cluster-b");
  const crawlB = guard.beginCrawl("cluster-b");
  assert.equal(guard.accepts(crawlA), false);
  assert.equal(guard.accepts(crawlB), true);
});

test("context guard rejects late operations from a prior cluster", () => {
  const guard = new ContextGuard();
  guard.select("cluster-a");
  assert.equal(guard.isCurrent("cluster-a"), true);
  guard.select("cluster-b");
  assert.equal(guard.isCurrent("cluster-a"), false);
  assert.equal(guard.isCurrent("cluster-b"), true);
});
