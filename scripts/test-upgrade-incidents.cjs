// Uses actual v1.5 and candidate binaries with a synthetic loopback target.
const assert = require("node:assert/strict");
const { execFileSync } = require("node:child_process");
const { createHash } = require("node:crypto");
const { once } = require("node:events");
const fs = require("node:fs/promises");
const http = require("node:http");
const os = require("node:os");
const path = require("node:path");
const { DatabaseSync } = require("node:sqlite");
const { setTimeout: delay } = require("node:timers/promises");
const { launchServer, stopChild } = require("./launch-server.cjs");

async function main() {
  const args = process.argv.slice(2);
  if (args.length !== 2) throw Error("Pass the actual v1.5 binary and the candidate binary");
  const [old, candidate] = args.map((value) => path.resolve(value));
  const dir = await fs.mkdtemp(path.join(os.tmpdir(), "pulse-incident-upgrade-"));
  const source = path.join(dir, "v15.db");
  const upgraded = path.join(dir, "v16.db");
  const archive = path.join(dir, "before.zip");
  const incidentArchive = path.join(dir, "incidents.zip");
  const restored = path.join(dir, "restored.db");
  const rollback = path.join(dir, "rollback.db");
  let status = 503;
  let app;
  const upstream = http.createServer((req, res) => res.writeHead(status).end("synthetic"));
  const start = async (exe, db) => {
    app = await launchServer(exe, ["serve", "-addr", "127.0.0.1:0", "-db", db]);
  };
  const stop = async () => {
    await stopChild(app?.child);
    app = undefined;
  };
  const api = async (route, method = "GET", data) => {
    const response = await fetch(app.url + route, {
      method, body: data && JSON.stringify(data), headers: { "Content-Type": "application/json" },
      signal: AbortSignal.timeout(3000),
    });
    assert(response.ok, method + " " + route + ": " + response.status);
    return response.json();
  };
  const waitFor = async (predicate) => {
    for (let attempt = 0; attempt < 160; attempt++) {
      const value = await predicate();
      if (value) return value;
      await delay(50);
    }
    throw Error("Timed out waiting for the synthetic check");
  };
  const maintain = (...options) => JSON.parse(execFileSync(candidate, ["maintain", ...options], {
    encoding: "utf8", windowsHide: true, timeout: 130000,
  }));
  const schema = (file) => {
    const db = new DatabaseSync(file, { readOnly: true });
    try { return db.prepare("PRAGMA user_version").get().user_version; }
    finally { db.close(); }
  };
  const hash = async (file) => createHash("sha256").update(await fs.readFile(file)).digest("hex");

  try {
    upstream.listen(0, "127.0.0.1");
    await once(upstream, "listening");
    const input = {
      name: "incident upgrade fixture", url: "http://127.0.0.1:" + upstream.address().port,
      enabled: true, interval_seconds: 60, timeout_seconds: 1,
    };
    await start(old, source);
    const target = await api("/api/targets", "POST", input);
    const endpoint = "/api/targets/" + target.id;
    const idle = () => waitFor(async () => {
      const [current] = await api("/api/targets");
      return current.latest && !current.running;
    });
    await idle();
    await api(endpoint + "/check", "POST");
    const oldEvents = await waitFor(async () => {
      const events = await api("/api/events");
      return events.length === 1 && events;
    });
    const original = (await api(endpoint + "/checks")).items;
    await api(endpoint, "PUT", { ...input, enabled: false });
    await stop();
    assert.equal(schema(source), 5);
    const originalHash = await hash(source);
    const before = maintain("backup", "-db", source, "-out", archive);
    assert.equal(before.schema_version, 5);
    assert.equal(before.counts.alert_events, 1);
    assert(!Object.hasOwn(before.counts, "incidents"));
    maintain("verify", "-in", archive);
    maintain("restore", "-in", archive, "-db", upgraded);
    await start(candidate, upgraded);
    assert.deepEqual((await api("/api/incidents")).items, []);
    assert.deepEqual(await api("/api/events"), oldEvents);
    assert.deepEqual((await api(endpoint + "/checks")).items, original);
    await api(endpoint, "PUT", input);
    await waitFor(async () => (await api(endpoint + "/checks")).items.length === original.length + 1);
    await idle();
    await api(endpoint + "/check", "POST");
    const active = await waitFor(async () => (await api("/api/incidents?status=active")).items[0]);
    assert.equal(active.failure_count, 2);
    assert(active.started_at <= active.confirmed_at && active.first_failure_check_id < active.last_failure_check_id);
    status = 200;
    await idle();
    await api(endpoint + "/check", "POST");
    const recovered = await waitFor(async () => (await api("/api/incidents?status=recovered")).items[0]);
    assert.equal(recovered.id, active.id);
    assert(recovered.recovery_duration_ms >= 0);
    await api(endpoint, "PUT", { ...input, enabled: false });
    await stop();
    assert.equal(schema(upgraded), 6);
    const after = maintain("backup", "-db", upgraded, "-out", incidentArchive);
    assert.equal(after.counts.incidents, 1);
    maintain("verify", "-in", incidentArchive);
    maintain("restore", "-in", incidentArchive, "-db", restored);
    await start(candidate, restored);
    assert.deepEqual((await api("/api/incidents")).items, [recovered]);
    await stop();
    await assert.rejects(start(old, upgraded), /版本|version/i);
    maintain("restore", "-in", archive, "-db", rollback);
    assert.equal(schema(rollback), 5);
    await start(old, rollback);
    assert.deepEqual(await api("/api/events"), oldEvents);
    assert.deepEqual((await api(endpoint + "/checks")).items, original);
    assert.equal(await hash(source), originalHash);
    console.log(JSON.stringify({
      passed: true, old_schema: 5, new_schema: 6, no_backfill: true, old_events_and_checks_preserved: true,
      incident_backup_restored: true, old_binary_refused_upgraded: true, old_backup_rollback_verified: true,
      old_binary_sha256: await hash(old), new_binary_sha256: await hash(candidate),
    }));
  } finally {
    await stop();
    await new Promise((resolve) => upstream.close(resolve));
    await fs.rm(dir, { recursive: true, force: true });
  }
}

main().catch((error) => { console.error(error); process.exitCode = 1; });
