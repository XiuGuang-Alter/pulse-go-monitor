"use strict";
const $ = (id) => document.getElementById(id);
const state = {
  targets: [],
  filter: "all",
  search: "",
  editing: null,
  deleting: null,
  historyId: null,
  historyToken: 0,
  historyRows: [],
  cursor: 0,
  refreshing: false,
  mutating: false,
  historyBusy: false,
  ruleRows: [],
};
const kinds = {
  ok: "正常",
  http_error: "HTTP 异常",
  timeout: "超时",
  request_error: "连接失败",
  invalid_input: "输入无效",
  assertion_failed: "断言失败",
  body_too_large: "正文超限",
};
let toastTimer;
let refreshSerial = Promise.resolve();
let refreshGeneration = 0;
function toast(message) {
  $("toast").textContent = message;
  $("toast").hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => ($("toast").hidden = true), 3500);
}
function notice(id, message = "") {
  const node = $(id);
  node.textContent = message;
  node.hidden = !message;
}
async function api(path, options = {}) {
  const response = await fetch(path, {
    ...options,
    headers: {
      ...(options.body ? { "Content-Type": "application/json" } : {}),
      ...options.headers,
    },
    signal: options.signal ?? AbortSignal.timeout(12000),
  });
  if (!response.ok) {
    let message = "请求失败（" + response.status + "）";
    try {
      const body = await response.json();
      message = body.error || message;
    } catch {}
    throw new Error(message);
  }
  return response.status === 204 ? null : response.json();
}
function el(tag, className, text) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text !== undefined) node.textContent = text;
  return node;
}
function button(text, action, className = "action") {
  const node = el("button", className, text);
  node.type = "button";
  node.disabled = state.mutating;
  node.addEventListener("click", action);
  return node;
}
function date(ms) {
  return ms
    ? new Date(ms).toLocaleString("zh-CN", {
        month: "2-digit",
        day: "2-digit",
        hour: "2-digit",
        minute: "2-digit",
        second: "2-digit",
        hour12: false,
      })
    : "尚未检查";
}
function ms(value) {
  return (
    Number(value).toLocaleString("zh-CN", { maximumFractionDigits: 1 }) + " ms"
  );
}
function badge(text, kind) {
  return el("span", "badge " + kind, text);
}
function targetState(target) {
  if (target.running) return ["检查中", "running"];
  if (!target.enabled) return ["已暂停", ""];
  if (!target.latest) return ["待检查", ""];
  return [
    kinds[target.latest.kind] || "未知",
    target.latest.kind === "ok" ? "ok" : "error",
  ];
}
function matches(target) {
  if (
    state.search &&
    !`${target.name} ${target.url}`
      .toLowerCase()
      .includes(state.search.toLowerCase())
  )
    return false;
  return (
    state.filter === "all" ||
    (state.filter === "paused" && !target.enabled) ||
    (state.filter === "healthy" &&
      target.enabled &&
      target.latest?.kind === "ok") ||
    (state.filter === "error" &&
      target.enabled &&
      target.latest &&
      target.latest.kind !== "ok")
  );
}
function render() {
  const targets = state.targets,
    enabled = targets.filter((t) => t.enabled),
    healthy = enabled.filter((t) => t.latest?.kind === "ok"),
    failed = enabled.filter((t) => t.latest && t.latest.kind !== "ok");
  $("metric-total").textContent = targets.length;
  $("metric-enabled").textContent =
    `${enabled.length} 个已启用 · ${targets.length - enabled.length} 个已暂停`;
  $("metric-healthy").textContent = healthy.length;
  $("metric-errors").textContent = failed.length;
  $("nav-errors").textContent = failed.length;
  const samples = targets.reduce((n, t) => n + t.stats.samples, 0),
    total = targets.reduce(
      (n, t) => n + t.stats.average_ms * t.stats.samples,
      0,
    );
  $("metric-latency").textContent = samples ? ms(total / samples) : "—";
  const shown = targets.filter(matches);
  $("target-count").textContent = shown.length;
  $("targets").replaceChildren();
  for (const target of shown) {
    const row = el("tr");
    const name = el("td"),
      title = button(target.name, () => openHistory(target), "target-title");
    title.title = "查看检查历史";
    name.append(title, el("span", "target-url", target.url));
    name.lastChild.title = target.url;
    row.append(name);
    const status = el("td");
    status.dataset.label = "当前状态";
    status.append(badge(...targetState(target)));
    row.append(status);
    const latency = el("td");
    latency.dataset.label = "最近响应";
    latency.append(
      el("span", "number", target.latest ? ms(target.latest.duration_ms) : "—"),
      el(
        "span",
        "cell-sub",
        `每 ${target.interval_seconds} 秒 · 超时 ${target.timeout_seconds} 秒`,
      ),
    );
    row.append(latency);
    const availability = el("td");
    availability.dataset.label = "样本可用率";
    availability.append(
      el(
        "span",
        "number",
        target.stats.samples ? target.stats.availability.toFixed(1) + "%" : "—",
      ),
      el("span", "cell-sub", `${target.stats.samples} 条保留记录`),
    );
    row.append(availability);
    const checked = el("td");
    checked.dataset.label = "最近检查";
    checked.append(
      el(
        "span",
        "number",
        target.latest ? date(target.latest.checked_at) : "尚未检查",
      ),
      el("span", "cell-sub", target.enabled ? "自动监测中" : "自动监测已暂停"),
    );
    row.append(checked);
    const actionsCell = el("td"),
      actions = el("div", "actions");
    const check = button(target.running ? "检查中" : "立即检查", () =>
      checkNow(target),
    );
    check.disabled = target.running || state.mutating;
    actions.append(
      check,
      button("历史", () => openHistory(target)),
      button("编辑", () => openForm(target)),
      button(target.enabled ? "暂停" : "恢复", () => toggle(target)),
      button("删除", () => openDelete(target), "action delete"),
    );
    actionsCell.append(actions);
    row.append(actionsCell);
    $("targets").append(row);
  }
  $("empty-state").hidden = shown.length !== 0;
  $("empty-title").textContent = targets.length
    ? "没有匹配的监测目标"
    : "还没有监测目标";
  $("empty-description").textContent = targets.length
    ? "试试其他关键词或切换状态筛选。"
    : "添加一个网址，开始记录它的每一次响应。";
  $("empty-add").hidden = targets.length > 0;
  document
    .querySelectorAll("[data-filter]")
    .forEach((node) =>
      node.classList.toggle("active", node.dataset.filter === state.filter),
    );
}
function refresh() {
  const generation = ++refreshGeneration;
  const task = refreshSerial.then(async () => {
    if (generation !== refreshGeneration) return;
    state.refreshing = true;
    $("refresh").disabled = true;
    try {
      const [targets, status] = await Promise.all([
        api("/api/targets"),
        api("/api/status"),
      ]);
      if (generation !== refreshGeneration) return;
      state.targets = targets;
      render();
      $("connection").classList.remove("offline");
      $("connection").replaceChildren(
        el("i", "status-dot"),
        document.createTextNode(
          `监测服务运行中 · ${status.running}/${status.concurrency}`,
        ),
      );
      notice("global-error");
      $("updated-at").textContent =
        "更新于 " + new Date().toLocaleTimeString("zh-CN", { hour12: false });
      if (state.historyId && !targets.some((t) => t.id === state.historyId))
        $("history-dialog").close();
    } catch (error) {
      if (generation !== refreshGeneration) return;
      notice(
        "global-error",
        "连接监测服务失败：" +
          error.message +
          "。已有数据保留，页面将自动重试。",
      );
      $("connection").classList.add("offline");
      $("connection").textContent = "连接中断";
    } finally {
      if (generation === refreshGeneration) {
        state.refreshing = false;
        $("refresh").disabled = false;
      }
    }
  });
  refreshSerial = task.catch(() => {});
  return task;
}
function beginMutation() {
  if (state.mutating) return false;
  state.mutating = true;
  render();
  return true;
}
function endMutation() {
  state.mutating = false;
  render();
}
function applySavedTarget(saved) {
  const previous = state.targets.find((target) => target.id === saved.id);
  const updated = {
    ...saved,
    latest: previous?.latest ?? saved.latest,
    stats: previous?.stats ?? saved.stats,
    running: previous?.running ?? false,
  };
  state.targets = previous
    ? state.targets.map((target) => (target.id === saved.id ? updated : target))
    : [updated, ...state.targets];
  render();
}
function input(target) {
  return {
    name: target.name,
    url: target.url,
    interval_seconds: target.interval_seconds,
    timeout_seconds: target.timeout_seconds,
    enabled: target.enabled,
  };
}
function openForm(target = null) {
  if (state.mutating) return;
  state.editing = target?.id ?? null;
  const form = $("target-form");
  form.reset();
  $("form-title").textContent = target ? "编辑监测目标" : "添加监测目标";
  const values = target
    ? input(target)
    : {
        name: "",
        url: "",
        interval_seconds: 60,
        timeout_seconds: 5,
        enabled: true,
      };
  for (const [key, value] of Object.entries(values)) {
    const field = form.elements.namedItem(key);
    if (key === "enabled") field.checked = value;
    else field.value = value;
  }
  $("rule-statuses").value=(target?.rules?.expected_statuses??[]).join(', ');
  const contains=encodedRuleString(target?.rules?.body_contains??'');
  state.containsEncoded=contains.encoded;
  $("rule-contains").value=contains.text;
  $("rule-contains").title=contains.encoded?'特殊字符使用JSON字符串编辑，请保留外层双引号和转义符':'';
  $("rule-encoding-note").textContent=contains.encoded?'正文含控制字符，当前以JSON字符串显示；请保留外层双引号和转义符。':'';
  state.ruleRows=(target?.rules?.json_equals??[]).map((rule,i)=>{const p=encodedRuleString(rule.pointer);return {pointer:p.text,pointerEncoded:p.encoded,expected:target.rule_values_json?.[i]??JSON.stringify(rule.equals)};});
  renderRuleRows();
  notice("form-error");
  $("target-dialog").showModal();
}
$("target-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  if (!beginMutation()) return;
  const form = event.currentTarget,
    fields = form.elements,
    data = {
      name: fields.namedItem("name").value,
      url: fields.namedItem("url").value,
      interval_seconds: Number(fields.namedItem("interval_seconds").value),
      timeout_seconds: Number(fields.namedItem("timeout_seconds").value),
      enabled: fields.namedItem("enabled").checked,
    };
  $("save-target").disabled = true;
  notice("form-error");
  try {
    const rules=rulePayload();
    const body=JSON.stringify(data).slice(0,-1)+',"rules":'+rules+'}';
    const saved = await api(
      state.editing ? `/api/targets/${state.editing}` : "/api/targets",
      { method: state.editing ? "PUT" : "POST", body },
    );
    applySavedTarget(saved);
    $("target-dialog").close();
    toast("监测目标已保存");
    await refresh();
  } catch (error) {
    notice("form-error", error.message);
  } finally {
    $("save-target").disabled = false;
    endMutation();
  }
});
function encodedRuleString(value){return /[\u0000-\u001f\u007f]/.test(value)?{text:JSON.stringify(value),encoded:true}:{text:value,encoded:false};}
function decodeRuleString(value,encoded){if(!encoded)return value;let parsed;try{parsed=JSON.parse(value);}catch{throw Error('含控制字符的规则应填写有效JSON字符串');}if(typeof parsed!=='string')throw Error('含控制字符的规则应填写JSON字符串');return parsed;}
function captureRuleRows(){return state.ruleRows.map(row=>({pointer:row.pointerInput?.value??row.pointer,pointerEncoded:row.pointerEncoded,expected:row.valueInput?.value??row.expected}));}
function renderRuleRows(){
  $("json-rule-rows").replaceChildren();
  state.ruleRows.forEach((rule,index)=>{
    const row=el('div','rule-row');
    const pointerLabel=el('label','','JSON路径 '+(index+1));const pointer=el('input');pointer.value=rule.pointer;pointer.setAttribute('aria-label','JSON路径 '+(index+1));pointerLabel.append(pointer);if(rule.pointerEncoded)pointerLabel.append(el('small','','控制字符：使用带双引号的JSON字符串编辑'));
    const valueLabel=el('label','','期望值 '+(index+1));const value=el('input');value.value=rule.expected;value.setAttribute('aria-label','期望值 '+(index+1));valueLabel.append(value);
    const remove=el('button','button subtle','移除规则 '+(index+1));remove.type='button';remove.addEventListener('click',()=>{state.ruleRows=captureRuleRows().filter((_,i)=>i!==index);renderRuleRows();});
    rule.pointerInput=pointer;rule.valueInput=value;row.append(pointerLabel,valueLabel,remove);$("json-rule-rows").append(row);
  });
  $("add-json-rule").disabled=state.ruleRows.length>=5;
}
$("add-json-rule").addEventListener('click',()=>{if(state.ruleRows.length>=5)return;state.ruleRows=captureRuleRows();state.ruleRows.push({pointer:'',expected:'null'});renderRuleRows();});
function rulePayload(){
  const raw=($("rule-statuses").value??'').trim();
  const codes=raw?raw.split(/[,，\s]+/):[];
  if(codes.length>10||codes.some(code=>!/^\d{3}$/.test(code)||Number(code)<200||Number(code)>599))throw Error('状态码应为200–599，最多10个');
  const assertions=captureRuleRows().map((rule,index)=>{
    let value;try{value=JSON.parse(rule.expected);}catch{throw Error('JSON规则'+(index+1)+'：期望值应为有效JSON标量');}
    if(value!==null&&typeof value==='object')throw Error('JSON规则'+(index+1)+'：暂不支持对象或数组作为期望值');
    return '{"pointer":'+JSON.stringify(decodeRuleString(rule.pointer,rule.pointerEncoded))+',"equals":'+rule.expected+'}';
  });
  // Keep the original numeric tokens; JSON.parse/stringify would round large integers.
  return '{"expected_statuses":'+JSON.stringify(codes.map(Number))+',"body_contains":'+JSON.stringify(decodeRuleString($("rule-contains").value??'',state.containsEncoded))+',"json_equals":['+assertions.join(',')+']}';
}
async function checkNow(target) {
  if (state.mutating) return;
  try {
    await api(`/api/targets/${target.id}/check`, { method: "POST" });
    toast("检查已开始");
    await refresh();
    setTimeout(refresh, 1200);
  } catch (error) {
    toast(error.message);
  }
}
async function toggle(target) {
  if (!beginMutation()) return;
  try {
    const saved = await api(`/api/targets/${target.id}`, {
      method: "PUT",
      body: JSON.stringify({ ...input(target), enabled: !target.enabled }),
    });
    applySavedTarget(saved);
    toast(
      target.enabled ? "自动监测已暂停；在途检查仍可完成" : "自动监测已恢复",
    );
    await refresh();
  } catch (error) {
    toast(error.message);
  } finally {
    endMutation();
  }
}
function openDelete(target) {
  if (state.mutating) return;
  state.deleting = target;
  $("delete-description").textContent =
    `“${target.name}”及其全部历史记录将被删除，此操作无法撤销。`;
  notice("delete-error");
  $("delete-dialog").showModal();
}
$("confirm-delete").addEventListener("click", async () => {
  const target = state.deleting;
  if (!target) return;
  if (!beginMutation()) return;
  $("confirm-delete").disabled = true;
  try {
    await api(`/api/targets/${target.id}`, { method: "DELETE" });
    state.targets = state.targets.filter((item) => item.id !== target.id);
    render();
    $("delete-dialog").close();
    toast("目标及历史已删除");
    await refresh();
  } catch (error) {
    notice("delete-error", error.message);
  } finally {
    $("confirm-delete").disabled = false;
    endMutation();
  }
});
function openHistory(target) {
  state.historyId = target.id;
  state.historyToken++;
  state.historyRows = [];
  state.cursor = 0;
  state.historyBusy = false;
  $("history-title").textContent = target.name;
  $("history-url").textContent = target.url;
  $("history-rows").replaceChildren();
  $("history-chart").replaceChildren();
  $("history-empty").hidden = true;
  $("history-more").hidden = true;
  $("daily-rows").replaceChildren();$("daily-note").textContent='正在读取日汇总…';
  notice("history-error");
  $("history-dialog").showModal();
  loadHistory(false);
}
async function loadHistory(more) {
  if (state.historyBusy || !state.historyId) return;
  state.historyBusy = true;
  const token = state.historyToken,
    id = state.historyId;
  $("history-refresh").disabled = true;
  $("history-more").disabled = true;
  notice("history-error");
  try {
    const [result,daily] = await Promise.all([api(
      `/api/targets/${id}/checks?limit=50${more ? "&before_id=" + state.cursor : ""}`,
    ),more?Promise.resolve(null):api(`/api/targets/${id}/daily?days=30`)]);
    if (token !== state.historyToken) return;
    state.historyRows = more
      ? [...state.historyRows, ...result.items]
      : result.items;
    state.cursor = result.next_before_id;
    renderHistory();
    if(daily)renderDaily(daily);
    $("history-more").hidden = !state.cursor;
  } catch (error) {
    if (token === state.historyToken) notice("history-error", error.message);
  } finally {
    if (token === state.historyToken) {
      state.historyBusy = false;
      $("history-refresh").disabled = false;
      $("history-more").disabled = false;
    }
  }
}
function renderHistory() {
  $("history-rows").replaceChildren();
  $("history-empty").hidden = state.historyRows.length !== 0;
  for (const item of state.historyRows) {
    const row = el("tr");
    row.append(el("td", "", date(item.checked_at)));
    const status = el("td");
    status.append(
      badge(kinds[item.kind] || item.kind, item.kind === "ok" ? "ok" : "error"),
    );
    row.append(
      status,
      el("td", "number", item.status_code || "—"),
      el("td", "number", ms(item.duration_ms)),
      el("td", "number", item.total_duration_ms==null?'—':ms(item.total_duration_ms)),
    );
    const detail = el("td", "history-detail", item.detail);
    detail.append(el("small", "", item.input_url));
    detail.append(el("small", "", item.config_version==null?'首版HTTP判定':('配置版本 '+item.config_version+' · 按当次规则判定')));
    if (item.final_url !== item.input_url && item.final_url)
      detail.append(el("small", "", "最终地址：" + item.final_url));
    row.append(detail);
    $("history-rows").append(row);
  }
  const chart = $("history-chart");
  chart.replaceChildren();
  const points = state.historyRows.slice(0, 30).reverse();
  if (!points.length) return;
  const max = Math.max(...points.map((r) => r.duration_ms), 1),
    coords = points.map((r, i) => [
      points.length === 1 ? 400 : 20 + (i * 760) / (points.length - 1),
      120 - (r.duration_ms / max) * 95,
    ]);
  const svg = (name, attributes) => {
    const node = document.createElementNS("http://www.w3.org/2000/svg", name);
    for (const [key, value] of Object.entries(attributes))
      node.setAttribute(key, value);
    return node;
  };
  chart.append(
    svg("line", { x1: 20, x2: 780, y1: 121, y2: 121, stroke: "#dfe8ed" }),
    svg("polyline", {
      points: coords.map((p) => p.join(",")).join(" "),
      fill: "none",
      stroke: "#5aa99e",
      "stroke-width": 2,
    }),
  );
  points.forEach((r, i) => {
    const dot = svg("circle", {
      cx: coords[i][0],
      cy: coords[i][1],
      r: 4,
      fill: r.kind === "ok" ? "#0b927c" : "#d28358",
    });
    const title = svg("title", {});
    title.textContent = `${date(r.checked_at)} · ${ms(r.duration_ms)} · ${kinds[r.kind] || r.kind}`;
    dot.append(title);
    chart.append(dot);
  });
}
function renderDaily(daily){
  $("daily-note").textContent='UTC自然日；'+new Date(daily.started_at).toLocaleString('zh-CN')+'开始汇总采集。按各次检查当时规则统计；无样本不代表健康，样本通过率不是全天在线率。';
  $("daily-rows").replaceChildren();
  for(const bucket of [...daily.buckets].reverse()){const row=el('tr');const notes=[bucket.samples?'':'无样本',bucket.in_progress?'进行中':'',bucket.migration_partial?'迁移前历史不完整':''].filter(Boolean).join(' · ');row.append(el('td','',bucket.date),el('td','number',String(bucket.samples)),el('td','number',bucket.availability==null?'—':bucket.availability.toFixed(1)+'%'),el('td','number',bucket.average_ms==null?'—':ms(bucket.average_ms)),el('td','number',bucket.max_ms==null?'—':ms(bucket.max_ms)),el('td','',notes));$("daily-rows").append(row);}
}
$("history-dialog").addEventListener("close", () => {
  state.historyId = null;
  state.historyToken++;
  state.historyBusy = false;
});
$("history-refresh").addEventListener("click", () => loadHistory(false));
$("history-more").addEventListener("click", () => loadHistory(true));
$("add-target").addEventListener("click", () => openForm());
$("empty-add").addEventListener("click", () => openForm());
$("refresh").addEventListener("click", refresh);
$("help-open").addEventListener("click", () => $("help-dialog").showModal());
$("search").addEventListener("input", (event) => {
  state.search = event.target.value;
  render();
});
$("filter").addEventListener("change", (event) => {
  state.filter = event.target.value;
  render();
});
document.querySelectorAll("[data-filter]").forEach((node) =>
  node.addEventListener("click", () => {
    state.filter = node.dataset.filter;
    $("filter").value = state.filter;
    render();
  }),
);
document
  .querySelectorAll("[data-close]")
  .forEach((node) =>
    node.addEventListener("click", () => $(node.dataset.close).close()),
  );
refresh();
setInterval(() => {
  if (!document.hidden) refresh();
}, 5000);
