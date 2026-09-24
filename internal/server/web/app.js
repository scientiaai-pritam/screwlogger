/* ScrewLogger admin UI — self-contained, same-origin, session-cookie auth.
 * Talks only to /admin/api/*; never /api/v1/*. */
(function () {
  "use strict";

  var MAX_GAP = 30; // protocol.MaxHeartbeatGap — dwell cap, seconds
  var PALETTE = [
    "#2563eb", "#7c3aed", "#0d9488", "#d97706", "#dc2626",
    "#0891b2", "#65a30d", "#db2777", "#4338ca", "#059669"
  ];

  // Chart instances, so we can destroy before re-rendering.
  var charts = {};

  /* ---------- tiny DOM helpers ---------- */
  function $(sel) { return document.querySelector(sel); }
  function on(sel, evt, fn) { var n = $(sel); if (n) n.addEventListener(evt, fn); }
  function esc(s) {
    return String(s == null ? "" : s).replace(/[&<>"']/g, function (c) {
      return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c];
    });
  }

  function toast(msg, isErr) {
    var t = $("#toast");
    t.textContent = msg;
    t.className = "toast" + (isErr ? " err" : "");
    t.hidden = false;
    clearTimeout(t._timer);
    t._timer = setTimeout(function () { t.hidden = true; }, 3200);
  }

  /* ---------- API ---------- */
  function api(path, opts) {
    opts = opts || {};
    var init = {
      method: opts.method || "GET",
      credentials: "same-origin",
      headers: {}
    };
    if (opts.body !== undefined) {
      init.headers["Content-Type"] = "application/json";
      init.body = JSON.stringify(opts.body);
    }
    return fetch(path, init).then(function (res) {
      var ct = res.headers.get("Content-Type") || "";
      var isJson = ct.indexOf("application/json") !== -1;
      return (isJson ? res.json() : res.text().catch(function () { return ""; }))
        .then(function (data) { return { ok: res.ok, status: res.status, data: data }; });
    }).catch(function (err) {
      toast("Network error: " + err.message, true);
      return { ok: false, status: 0, data: { error: err.message } };
    });
  }

  // True when the response carried a valid session; on 401, drop back to login.
  function authed(res) {
    if (res.status === 401) {
      showLogin();
      return false;
    }
    if (!res.ok) {
      var msg = (res.data && res.data.error) ? res.data.error : ("HTTP " + res.status);
      toast(msg, true);
      return false;
    }
    return true;
  }

  /* ---------- view switching ---------- */
  function showLogin() {
    $("#app-view").hidden = true;
    $("#login-view").hidden = false;
    $("#login-error").hidden = true;
  }

  function showApp() {
    $("#login-view").hidden = true;
    $("#app-view").hidden = false;
  }

  function switchPanel(name) {
    ["devices", "rules", "keys", "dashboards"].forEach(function (p) {
      $("#panel-" + p).hidden = (p !== name);
    });
    document.querySelectorAll(".nav-btn").forEach(function (b) {
      b.classList.toggle("active", b.getAttribute("data-panel") === name);
    });
    if (name === "devices") loadDevices();
    if (name === "rules") loadRules();
    if (name === "keys") loadKeys();
    if (name === "dashboards") loadDashboards();
  }

  /* ---------- bootstrap / auth ---------- */
  function bootstrap() {
    api("/admin/api/devices").then(function (res) {
      if (res.status === 401) { showLogin(); return; }
      showApp();
      loadDevices();
    });
  }

  function doLogin(evt) {
    evt.preventDefault();
    var err = $("#login-error");
    err.hidden = true;
    api("/admin/login", { method: "POST", body: { password: $("#password").value } })
      .then(function (res) {
        if (res.status === 401) {
          err.textContent = "Invalid password";
          err.hidden = false;
          return;
        }
        if (!res.ok) { err.textContent = "Login failed"; err.hidden = false; return; }
        $("#password").value = "";
        showApp();
        loadDevices();
      });
  }

  function doLogout() {
    api("/admin/logout", { method: "POST" }).then(function () { showLogin(); });
  }

  /* ---------- one-time secret modal ---------- */
  function showSecret(title, secret) {
    $("#modal-title").textContent = title;
    $("#modal-secret").value = secret;
    $("#modal").hidden = false;
  }
  function hideModal() { $("#modal").hidden = true; }

  /* ---------- formatting ---------- */
  function fmtTs(sec) {
    if (sec == null) return "—";
    return new Date(sec * 1000).toLocaleString();
  }
  function fmtDuration(sec) {
    if (sec == null || sec < 0) return "—";
    sec = Math.round(sec);
    var h = Math.floor(sec / 3600), m = Math.floor((sec % 3600) / 60), s = sec % 60;
    if (h > 0) return h + "h " + m + "m";
    if (m > 0) return m + "m " + s + "s";
    return s + "s";
  }
  function statusBadge(status) {
    return '<span class="badge ' + esc(status) + '">' + esc(status) + "</span>";
  }

  /* ---------- Devices ---------- */
  function loadDevices() {
    api("/admin/api/devices").then(function (res) {
      if (!authed(res)) return;
      var tbody = $("#devices-table tbody");
      tbody.textContent = "";
      var devices = res.data || [];
      if (!devices.length) {
        var tr = document.createElement("tr");
        var td = document.createElement("td");
        td.colSpan = 4;
        td.className = "empty";
        td.textContent = "No devices enrolled yet.";
        tr.appendChild(td);
        tbody.appendChild(tr);
        return;
      }
      devices.forEach(function (d) {
        var tr = document.createElement("tr");
        tr.innerHTML =
          "<td>" + esc(d.name) + "</td>" +
          "<td>" + esc(fmtTs(d.last_seen)) + "</td>" +
          "<td>" + statusBadge(d.status) + "</td>" +
          '<td class="actions"></td>';
        var btn = document.createElement("button");
        btn.className = "danger";
        btn.textContent = "Revoke";
        btn.disabled = d.status === "revoked";
        btn.addEventListener("click", function () { revokeDevice(d.id, d.name); });
        tr.querySelector("td.actions").appendChild(btn);
        tbody.appendChild(tr);
      });
    });
  }

  function revokeDevice(id, name) {
    if (!confirm("Revoke device \"" + name + "\"? Its token will stop working.")) return;
    api("/admin/api/devices/" + encodeURIComponent(id) + "/revoke", { method: "POST" })
      .then(function (res) {
        if (!authed(res)) return;
        toast("Device revoked");
        loadDevices();
      });
  }

  function enrollDevice(evt) {
    evt.preventDefault();
    var name = $("#enroll-name").value.trim();
    if (!name) return;
    api("/admin/api/devices", { method: "POST", body: { name: name } }).then(function (res) {
      if (!authed(res)) return;
      $("#enroll-name").value = "";
      loadDevices();
      showSecret("Device token for " + name, res.data.token);
    });
  }

  /* ---------- Rules ---------- */
  function loadRules() {
    api("/admin/api/rules").then(function (res) {
      if (!authed(res)) return;
      var tbody = $("#rules-table tbody");
      tbody.textContent = "";
      var rules = res.data || [];
      if (!rules.length) {
        var tr = document.createElement("tr");
        var td = document.createElement("td");
        td.colSpan = 4;
        td.className = "empty";
        td.textContent = "No rules yet.";
        tr.appendChild(td);
        tbody.appendChild(tr);
        return;
      }
      rules.forEach(function (r) {
        var tr = document.createElement("tr");
        tr.innerHTML =
          "<td><code>" + esc(r.pattern) + "</code></td>" +
          "<td>" + esc(r.category) + "</td>" +
          "<td>" + esc(fmtTs(r.updated_at)) + "</td>" +
          '<td class="actions"></td>';
        var edit = document.createElement("button");
        edit.textContent = "Edit";
        edit.addEventListener("click", function () { fillRuleForm(r); });
        var del = document.createElement("button");
        del.className = "danger";
        del.textContent = "Delete";
        del.addEventListener("click", function () { deleteRule(r.pattern); });
        var cell = tr.querySelector("td.actions");
        cell.appendChild(edit);
        cell.appendChild(del);
        tbody.appendChild(tr);
      });
    });
  }

  function fillRuleForm(r) {
    $("#rule-pattern").value = r.pattern;
    $("#rule-category").value = r.category;
    $("#rule-apply").checked = false;
    $("#rule-cancel").hidden = false;
  }

  function resetRuleForm() {
    $("#rule-pattern").value = "";
    $("#rule-category").value = "";
    $("#rule-apply").checked = false;
    $("#rule-cancel").hidden = true;
  }

  function saveRule(evt) {
    evt.preventDefault();
    var body = {
      pattern: $("#rule-pattern").value.trim(),
      category: $("#rule-category").value.trim(),
      apply_to_history: $("#rule-apply").checked
    };
    if (!body.pattern || !body.category) return;
    api("/admin/api/rules", { method: "PUT", body: body }).then(function (res) {
      if (!authed(res)) return;
      resetRuleForm();
      var extra = res.data && res.data.recategorized ? " (" + res.data.recategorized + " heartbeats re-mapped)" : "";
      toast("Rule saved" + extra);
      loadRules();
    });
  }

  function deleteRule(pattern) {
    if (!confirm("Delete rule \"" + pattern + "\"?")) return;
    api("/admin/api/rules", { method: "DELETE", body: { pattern: pattern } }).then(function (res) {
      if (!authed(res)) return;
      toast("Rule deleted");
      loadRules();
    });
  }

  /* ---------- API keys ---------- */
  function loadKeys() {
    api("/admin/api/keys").then(function (res) {
      if (!authed(res)) return;
      var tbody = $("#keys-table tbody");
      tbody.textContent = "";
      var keys = res.data || [];
      if (!keys.length) {
        var tr = document.createElement("tr");
        var td = document.createElement("td");
        td.colSpan = 5;
        td.className = "empty";
        td.textContent = "No API keys yet.";
        tr.appendChild(td);
        tbody.appendChild(tr);
        return;
      }
      keys.forEach(function (k) {
        var revoked = k.revoked_at != null;
        var tr = document.createElement("tr");
        tr.innerHTML =
          "<td>" + esc(k.label) + "</td>" +
          "<td>" + esc(fmtTs(k.created_at)) + "</td>" +
          "<td>" + (revoked ? esc(fmtTs(k.revoked_at)) : "—") + "</td>" +
          "<td><code>" + esc(k.hash_prefix) + "</code></td>" +
          '<td class="actions"></td>';
        var btn = document.createElement("button");
        btn.className = "danger";
        btn.textContent = "Revoke";
        btn.disabled = revoked;
        btn.addEventListener("click", function () { revokeKey(k.hash_prefix); });
        tr.querySelector("td.actions").appendChild(btn);
        tbody.appendChild(tr);
      });
    });
  }

  function createKey(evt) {
    evt.preventDefault();
    var label = $("#key-label").value.trim();
    if (!label) return;
    api("/admin/api/keys", { method: "POST", body: { label: label } }).then(function (res) {
      if (!authed(res)) return;
      $("#key-label").value = "";
      loadKeys();
      showSecret("API key for " + label, res.data.key);
    });
  }

  function revokeKey(prefix) {
    if (!confirm("Revoke API key " + prefix + "…? Clients using it will be rejected.")) return;
    api("/admin/api/keys/" + encodeURIComponent(prefix) + "/revoke", { method: "POST" }).then(function (res) {
      if (!authed(res)) return;
      toast("API key revoked");
      loadKeys();
    });
  }

  /* ---------- Dashboards ---------- */
  function rangeWindow(preset) {
    var to = Math.floor(Date.now() / 1000);
    var from;
    if (preset === "today") {
      var d = new Date();
      d.setHours(0, 0, 0, 0);
      from = Math.floor(d.getTime() / 1000);
    } else if (preset === "30d") {
      from = to - 30 * 24 * 3600;
    } else { // 7d
      from = to - 7 * 24 * 3600;
    }
    return { from: from, to: to };
  }

  function populateDeviceSelect(devices) {
    var sel = $("#device-select");
    var prev = sel.value;
    sel.textContent = "";
    var all = document.createElement("option");
    all.value = "";
    all.textContent = "All devices";
    sel.appendChild(all);
    (devices || []).forEach(function (d) {
      var o = document.createElement("option");
      o.value = d.id;
      o.textContent = d.name + (d.status === "revoked" ? " (revoked)" : "");
      sel.appendChild(o);
    });
    if (prev !== null && prev !== undefined) {
      // keep previous selection when possible
      var match = Array.prototype.find.call(sel.options, function (o) { return o.value === prev; });
      if (match) sel.value = prev;
    }
  }

  function loadDashboards() {
    // Populate the device selector once per visit, then render charts.
    api("/admin/api/devices").then(function (res) {
      if (!authed(res)) return;
      populateDeviceSelect(res.data || []);
      renderDashboards();
    });
  }

  function renderDashboards() {
    var w = rangeWindow($("#range-select").value);
    var device = $("#device-select").value || "";
    var q = "from=" + w.from + "&to=" + w.to + (device ? "&device=" + encodeURIComponent(device) : "");

    var jobs = [
      api("/admin/api/usage?group_by=category&" + q),
      api("/admin/api/usage?group_by=app&" + q),
      api("/admin/api/active-ratio?" + q),
      api("/admin/api/events?limit=10000&" + q)
    ];
    Promise.all(jobs).then(function (rs) {
      if (!authed(rs[0])) return;
      renderBar("box-category", "chart-category", "category", rs[0].data);
      renderBar("box-app", "chart-app", "app", rs[1].data);
      renderRatio(rs[2].data);
      renderHourly(rs[3].data, w.to);
    });
  }

  function colorFor(i) { return PALETTE[i % PALETTE.length]; }

  function ensureChart() {
    if (typeof Chart === "undefined") {
      toast("Chart.js failed to load", true);
      return false;
    }
    return true;
  }

  function destroyChart(canvasId) {
    if (charts[canvasId]) { charts[canvasId].destroy(); delete charts[canvasId]; }
  }

  // Reset a chart box to either an empty message or a fresh canvas.
  function prepareBox(boxId, canvasId, isEmpty) {
    var box = document.getElementById(boxId);
    if (isEmpty) {
      box.innerHTML = '<div class="chart-empty">No data for this range</div>';
      return null;
    }
    box.innerHTML = '<canvas id="' + canvasId + '"></canvas>';
    return document.getElementById(canvasId);
  }

  function renderBar(boxId, canvasId, title, data) {
    destroyChart(canvasId);
    if (!ensureChart()) return;
    var buckets = (data && data.buckets) || [];
    var canvas = prepareBox(boxId, canvasId, !buckets.length);
    if (!canvas) return;
    charts[canvasId] = new Chart(canvas, {
      type: "bar",
      data: {
        labels: buckets.map(function (b) { return b.key; }),
        datasets: [{
          label: title + " dwell",
          data: buckets.map(function (b) { return b.seconds; }),
          backgroundColor: buckets.map(function (_, i) { return colorFor(i); })
        }]
      },
      options: {
        responsive: true,
        maintainAspectRatio: false,
        plugins: { legend: { display: false } },
        scales: {
          x: { ticks: { autoSkip: true, maxRotation: 45, minRotation: 0 } },
          y: { beginAtZero: true, title: { display: true, text: "seconds" } }
        },
        tooltip: { callbacks: { label: function (c) { return " " + fmtDuration(c.parsed.y); } } }
      }
    });
  }

  function renderRatio(data) {
    destroyChart("chart-ratio");
    if (!ensureChart()) return;
    var total = data ? data.total_seconds : 0;
    var canvas = prepareBox("box-ratio", "chart-ratio", !total);
    if (!canvas) return;
    charts["chart-ratio"] = new Chart(canvas, {
      type: "doughnut",
      data: {
        labels: ["Active", "Idle"],
        datasets: [{
          data: [data.active_seconds, data.idle_seconds],
          backgroundColor: ["#16a34a", "#d97706"],
          borderWidth: 0
        }]
      },
      options: {
        responsive: true,
        maintainAspectRatio: false,
        cutout: "62%",
        plugins: {
          legend: { position: "bottom" },
          tooltip: { callbacks: { label: function (c) { return " " + c.label + ": " + fmtDuration(c.parsed); } } }
        }
      }
    });
  }

  // Bucket raw heartbeats by hour into active/idle dwell seconds (gap-capped at
  // MAX_GAP, mirroring the server dwell model). For "All devices" the events are
  // interleaved, so the gap is an approximation — acceptable for a timeline.
  function bucketByHour(events, to) {
    var buckets = {}; // hourKey -> {active: 0, idle: 0}
    function hourKey(ts) { return Math.floor(ts / 3600) * 3600; }
    for (var i = 0; i < events.length; i++) {
      var ev = events[i];
      var next = events[i + 1];
      var gap;
      if (next) {
        gap = Math.min(next.ts - ev.ts, MAX_GAP);
        if (gap < 0) gap = 0;
      } else {
        gap = Math.min(to - ev.ts, MAX_GAP);
        if (gap < 0) gap = 0;
      }
      var key = hourKey(ev.ts);
      if (!buckets[key]) buckets[key] = { active: 0, idle: 0 };
      if (ev.active) buckets[key].active += gap; else buckets[key].idle += gap;
    }
    return buckets;
  }

  function hourLabel(ts) {
    var d = new Date(ts * 1000);
    var mm = ("0" + (d.getMonth() + 1)).slice(-2);
    var dd = ("0" + d.getDate()).slice(-2);
    var hh = ("0" + d.getHours()).slice(-2);
    return mm + "-" + dd + " " + hh + ":00";
  }

  function renderHourly(data, to) {
    destroyChart("chart-hourly");
    if (!ensureChart()) return;
    var events = (data && data.events) || [];
    var canvas = prepareBox("box-hourly", "chart-hourly", !events.length);
    if (!canvas) return;

    var buckets = bucketByHour(events, to);
    var keys = Object.keys(buckets).map(Number).sort(function (a, b) { return a - b; });
    var labels = keys.map(hourLabel);
    var active = keys.map(function (k) { return buckets[k].active; });
    var idle = keys.map(function (k) { return buckets[k].idle; });

    charts["chart-hourly"] = new Chart(canvas, {
      type: "line",
      data: {
        labels: labels,
        datasets: [
          { label: "Active", data: active, borderColor: "#16a34a", backgroundColor: "rgba(22,163,74,0.12)", fill: true, tension: 0.3, pointRadius: 0 },
          { label: "Idle", data: idle, borderColor: "#d97706", backgroundColor: "rgba(217,119,6,0.10)", fill: true, tension: 0.3, pointRadius: 0 }
        ]
      },
      options: {
        responsive: true,
        maintainAspectRatio: false,
        plugins: { legend: { position: "bottom" } },
        scales: {
          x: { ticks: { maxTicksLimit: 12, maxRotation: 0 } },
          y: { beginAtZero: true, title: { display: true, text: "seconds / hour" } }
        },
        tooltip: { callbacks: { label: function (c) { return " " + c.dataset.label + ": " + fmtDuration(c.parsed.y); } } }
      }
    });
  }

  /* ---------- wire up ---------- */
  on("#login-form", "submit", doLogin);
  on("#logout", "click", doLogout);
  on("#enroll-form", "submit", enrollDevice);
  on("#rule-form", "submit", saveRule);
  on("#rule-cancel", "click", resetRuleForm);
  on("#key-form", "submit", createKey);
  on("#device-select", "change", renderDashboards);
  on("#range-select", "change", renderDashboards);
  on("#refresh-dash", "click", renderDashboards);
  on("#modal-close", "click", hideModal);
  on("#modal-copy", "click", function () {
    var input = $("#modal-secret");
    input.select();
    if (navigator.clipboard) {
      navigator.clipboard.writeText(input.value).then(function () { toast("Copied to clipboard"); });
    } else {
      document.execCommand("copy");
      toast("Copied to clipboard");
    }
  });

  document.querySelectorAll(".nav-btn").forEach(function (b) {
    b.addEventListener("click", function () { switchPanel(b.getAttribute("data-panel")); });
  });

  bootstrap();
})();
