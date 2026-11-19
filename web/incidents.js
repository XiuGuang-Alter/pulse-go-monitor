"use strict";

(() => {
  const dialog = $("incidents-dialog");
  const detailDialog = $("incident-detail-dialog");
  const detailTable = $("incident-detail-table");
  const detailRows = $("incident-detail-rows");
  const detailMore = $("incident-detail-more");
  const detailRefresh = $("incident-detail-refresh");
  const target = $("incident-target");
  const status = $("incident-status");
  const more = $("incident-more");
  const update = $("incident-refresh");
  const pageSize = 50;
  const reasons = new Map([
    ["paused", "目标已暂停"],
    ["configuration_changed", "配置已修改"],
    ["restarted", "监测进程重新启动"],
    ["stopped", "监测已停止"],
  ]);
  let items = [];
  let hasMore = false;
  let viewingOlder = false;
  let request = null;
  let detailRequest = null;
  let detailTrigger = null;
  let snapshot = null;
  let shown = 0;

  function render() {
    const body = $("incident-rows");
    const focusedID = document.activeElement?.dataset.incidentId;
    body.replaceChildren(...items.map((item) => {
      const row = el("tr");
      const name = el("td", "event-name", item.target_name);
      name.append(el("small", "", "配置版本 " + item.config_version));
      const inspect = el("button", "action incident-inspect", "查看检查");
      inspect.dataset.incidentId = String(item.id);
      inspect.addEventListener("click", () => openDetail(inspect));
      name.append(inspect);
      const start = el("td", "", date(item.started_at));
      const category = Object.hasOwn(categories, item.error_category) ? categories[item.error_category] : "未知分类";
      start.append(el("small", "", "确认 " + date(item.confirmed_at) + " · " + category));
      const last = el("td", "", date(item.last_failure_at));
      last.append(el("small", "", item.failure_count + " 次失败"));
      const outcome = el("td");
      const span = el("td");
      if (item.status === "recovered") {
        outcome.append(badge("已恢复", "ok"), el("small", "", date(item.recovered_at)));
        span.textContent = item.recovery_duration_ms === null
          ? "不可确定"
          : "约 " + duration(item.recovery_duration_ms) + "（采样估算）";
      } else {
        const interrupted = item.status === "interrupted";
        outcome.append(badge(interrupted ? "监测中断，未确认恢复" : "尚未观察到恢复", interrupted ? "paused" : "error"));
        if (interrupted) outcome.append(el("small", "", (reasons.get(item.interrupt_reason) || "监测中断") + " · " + date(item.closed_at)));
        span.textContent = item.observed_duration_ms === null ? "不可确定" : "已观测 " + duration(item.observed_duration_ms);
      }
      row.append(name, start, last, outcome, span);
      return row;
    }));
    if (focusedID) {
      const next = body.querySelector('[data-incident-id="' + CSS.escape(focusedID) + '"]');
      (next || body.closest(".history-scroll")).focus({ preventScroll: true });
    }
  }

  function openDetail(trigger) {
    cancelRequest();
    update.disabled = false;
    more.disabled = !hasMore;
    detailTrigger = trigger;
    detailDialog.showModal();
    detailDialog.scrollTop = 0;
    loadDetail();
  }

  function showMoreChecks() {
    if (!snapshot) return;
    const batch = snapshot.items.slice(shown, shown + pageSize);
    const rows = batch.map((check) => {
      const row = el("tr");
      const ok = check.kind === "ok";
      const result = el("td");
      result.append(badge(ok ? "通过" : "失败", ok ? "ok" : "error"));
      const explanation = el("td", "history-detail");
      if (!ok) {
        const label = Object.hasOwn(categories, check.error_category) ? categories[check.error_category] : "未知分类";
        explanation.append(el("strong", "", label), el("br"));
      }
      explanation.append(document.createTextNode(check.detail));
      row.append(el("td", "", date(check.checked_at)), result,
        el("td", "", check.status_code || "未收到状态码"), explanation);
      return row;
    });
    detailRows.append(...rows);
    if (shown && rows.length) {
      rows[0].tabIndex = -1;
      rows[0].focus();
    }
    shown += rows.length;
    detailMore.hidden = shown >= snapshot.items.length;
    const { total, retained, cleared } = snapshot;
    $("incident-detail-count").textContent = "共 " + total + " 次检查，仍可查看 " + retained + " 次，已清理 " + cleared + " 次" +
      (shown < retained ? " · 当前展示 " + shown + " 次" : "");
  }

  async function loadDetail() {
    detailRequest?.abort();
    const pending = new AbortController();
    detailRequest = pending;
    const id = detailTrigger.dataset.incidentId;
    detailRefresh.disabled = true;
    snapshot = null;
    shown = 0;
    detailRows.replaceChildren();
    detailTable.hidden = true;
    detailMore.hidden = true;
    $("incident-detail-target").textContent = "";
    $("incident-detail-times").textContent = "";
    $("incident-detail-count").textContent = "正在读取检查…";
    notice("incident-detail-error");
    try {
      const detail = await api("/api/incidents/" + id + "/checks", {
        signal: AbortSignal.any([pending.signal, AbortSignal.timeout(12000)]),
      });
      if (detailRequest !== pending || !detailDialog.open) return;
      snapshot = detail;
      const timeline = ["首次失败 " + date(detail.started_at), "确认 " + date(detail.confirmed_at),
        "最后失败 " + date(detail.last_failure_at)];
      let stateLabel = "尚未观察到恢复";
      if (detail.status === "recovered") {
        stateLabel = "已恢复";
        timeline.push("恢复 " + date(detail.recovered_at));
      } else if (detail.status === "interrupted") {
        stateLabel = "监测中断，未确认恢复";
        timeline.push("中断 " + date(detail.closed_at));
      }
      $("incident-detail-target").textContent = detail.target_name + " · 配置版本 " + detail.config_version + " · " + stateLabel;
      $("incident-detail-times").textContent = timeline.join(" · ");
      detailTable.hidden = !detail.items.length;
      detailTable.scrollTop = 0;
      showMoreChecks();
    } catch (error) {
      if (detailRequest !== pending || !detailDialog.open) return;
      $("incident-detail-count").textContent = "";
      notice("incident-detail-error", error.message);
    } finally {
      if (detailRequest === pending) {
        detailRequest = null;
        detailRefresh.disabled = false;
      }
    }
  }

  function duration(ms) {
    if (ms < 1000) return ms + " 毫秒";
    let seconds = Math.floor(ms / 1000);
    const units = [[86400, "天"], [3600, "小时"], [60, "分"], [1, "秒"]];
    const parts = [];
    for (const [unit, label] of units) {
      const value = Math.floor(seconds / unit);
      if (value) parts.push(value + label);
      seconds %= unit;
    }
    return parts.join(" ");
  }

  function cancelRequest() {
    request?.abort();
    request = null;
  }

  function changeFilter() {
    cancelRequest();
    items = [];
    hasMore = false;
    viewingOlder = false;
    render();
    $("incident-note").textContent = "";
    load();
  }

  async function load(append = false) {
    if (!dialog.open || detailDialog.open || request || (append && !hasMore)) return;
    const pending = new AbortController();
    request = pending;
    update.disabled = true;
    more.disabled = true;
    notice("incident-error");
    $("incident-empty").hidden = true;
    const query = new URLSearchParams({ limit: String(pageSize + 1), target_id: target.value, status: status.value });
    if (append) query.set("before_id", String(items.at(-1).id));
    try {
      const response = await api("/api/incidents?" + query, {
        signal: AbortSignal.any([pending.signal, AbortSignal.timeout(12000)]),
      });
      if (request !== pending || !dialog.open) return;
      const page = response.items.slice(0, pageSize);
      items = append ? items.concat(page) : page;
      hasMore = response.items.length > pageSize;
      viewingOlder = append;
      render();
      $("incident-empty").hidden = items.length !== 0;
      const mode = viewingOlder ? "查看更早记录" : "首页每 5 秒更新";
      const message = "已加载 " + items.length + " 条 · " + mode;
      if ($("incident-note").textContent !== message) $("incident-note").textContent = message;
    } catch (error) {
      if (request === pending && dialog.open) notice("incident-error", error.message);
    } finally {
      if (request === pending) {
        request = null;
        update.disabled = false;
        more.disabled = !hasMore;
      }
    }
  }

  $("incidents-open").addEventListener("click", () => {
    const options = [new Option("全部目标", "0")];
    for (const item of state.targets) options.push(new Option(item.name, String(item.id)));
    target.replaceChildren(...options);
    status.value = "";
    $("incident-note").textContent = "";
    dialog.showModal();
    changeFilter();
  });
  dialog.addEventListener("close", () => {
    if (!dialog.open) cancelRequest();
  });
  detailDialog.addEventListener("close", () => {
    if (detailDialog.open) return;
    detailRequest?.abort();
    detailRequest = null;
    snapshot = null;
    if (dialog.open) detailTrigger?.focus({ preventScroll: true });
    detailTrigger = null;
  });
  detailRefresh.addEventListener("click", loadDetail);
  detailMore.addEventListener("click", showMoreChecks);
  target.addEventListener("change", changeFilter);
  status.addEventListener("change", changeFilter);
  update.addEventListener("click", () => load());
  more.addEventListener("click", () => load(true));
  setInterval(() => {
    if (!document.hidden && !viewingOlder) load();
  }, 5000);
})();
