const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");

// 加载实际app.js，只替换DOM容器和fetch边界；验证真实保存/刷新/暂停函数。
for (const refreshFails of [false, true]) {
  test(`旧刷新不能回滚保存；后续刷新失败=${refreshFails}`, async () => {
    let database = {
      id: 1,
      name: "目标",
      url: "http://local/old",
      interval_seconds: 60,
      timeout_seconds: 5,
      enabled: true,
      stats: { samples: 0, successes: 0, average_ms: 0, availability: 0 },
      latest: null,
    };
    let releaseOld,
      reads = 0;
    const elements = new Map();
    function element() {
      return {
        listeners: {},
        classList: { add() {}, remove() {}, toggle() {} },
        addEventListener(name, fn) {
          this.listeners[name] = fn;
        },
        replaceChildren() {},
        append() {},
        close() {},
        showModal() {},
        setAttribute() {},
        reset() {},
      };
    }
    const document = {
      hidden: false,
      getElementById(id) {
        if (!elements.has(id)) elements.set(id, element());
        return elements.get(id);
      },
      querySelectorAll() {
        return [];
      },
      createElement: element,
      createElementNS: element,
      createTextNode: element,
    };
    const response = (value) => ({
      ok: true,
      status: 200,
      json: async () => JSON.parse(JSON.stringify(value)),
    });
    const context = vm.createContext({
      document,
      AbortSignal,
      console,
      setInterval() {},
      setTimeout() {
        return 0;
      },
      clearTimeout() {},
      fetch: async (url, options = {}) => {
        if (url === "/api/status")
          return response({ running: 0, concurrency: 4 });
        if (url === "/api/targets") {
          reads++;
          if (reads === 1) {
            const old = JSON.parse(JSON.stringify(database));
            return new Promise((resolve) => {
              releaseOld = () => resolve(response([old]));
            });
          }
          if (refreshFails) throw new Error("模拟刷新断网");
          return response([database]);
        }
        if (url === "/api/targets/1" && options.method === "PUT") {
          database = { ...database, ...JSON.parse(options.body) };
          return response(database);
        }
        throw new Error("unexpected request " + url);
      },
    });
    vm.runInContext(
      fs.readFileSync(path.join(__dirname, "web/app.js"), "utf8"),
      context,
    );
    vm.runInContext("render=()=>{}; state.editing=1", context);
    await new Promise((resolve) => setImmediate(resolve));
    assert.equal(typeof releaseOld, "function");
    const fields = {
      name: { value: "新名称" },
      url: { value: "http://local/new" },
      interval_seconds: { value: "30" },
      timeout_seconds: { value: "5" },
      enabled: { checked: true },
    };
    const form = document.getElementById("target-form");
    form.elements = { namedItem: (name) => fields[name] };
    const saving = form.listeners.submit({
      preventDefault() {},
      currentTarget: form,
    });
    await new Promise((resolve) => setImmediate(resolve));
    assert.equal(database.url, "http://local/new");
    releaseOld();
    await saving;
    await new Promise((resolve) => setImmediate(resolve));
    assert.equal(
      vm.runInContext("state.targets[0]?.url", context),
      "http://local/new",
    );
    assert.equal(vm.runInContext("state.mutating", context), false);
    await vm.runInContext("toggle(state.targets[0])", context);
    assert.equal(database.url, "http://local/new");
    assert.equal(database.name, "新名称");
    assert.equal(database.enabled, false);
  });
}
