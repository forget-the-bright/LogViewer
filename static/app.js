// ===== LogViewer 前端逻辑 =====
(function () {
  "use strict";

  // ---------- 状态 ----------
  const state = {
    ws: null,
    wsIntendedClose: false,
    connected: false,
    running: false,
    stopping: false,
    paused: false,
    pausedBuffer: [],
    currentFile: "",
    selectedNode: null,
  };

  const MAX_PAUSED_BUFFER = 5000; // 暂停期间最多缓冲行数
  const HIGHLIGHT_ANSI = "\x1b[97;48;5;160m"; // 白字红底
  const RESET_ANSI = "\x1b[0m";

  // ---------- 工具 ----------
  const $ = (id) => document.getElementById(id);

  function toast(msg, type) {
    const t = $("toast");
    t.textContent = msg;
    t.className = "show " + (type || "");
    clearTimeout(t._timer);
    t._timer = setTimeout(() => (t.className = ""), 3000);
  }

  // 是否启用登录认证（由 /api/auth/status 决定）。未启用时所有 401 逻辑跳过。
  let authEnabled = false;

  function showLogin() {
    const mask = $("loginMask");
    if (mask) mask.classList.add("show");
    $("loginPass").value = "";
    setTimeout(() => $("loginUser").focus(), 30);
    // 断开 WS，避免未登录时的重连风暴
    state.wsIntendedClose = true;
    if (state.ws) { try { state.ws.close(); } catch (e) {} state.ws = null; }
    state.connected = false;
    state.running = false;
    setConnStatus("offline");
  }

  function hideLogin() {
    const mask = $("loginMask");
    if (mask) mask.classList.remove("show");
    $("loginError").textContent = "";
  }

  async function api(url, opts) {
    const r = await fetch(url, opts);
    if (r.status === 401 && authEnabled) {
      showLogin();
      throw new Error("未登录或会话已过期");
    }
    const ct = r.headers.get("content-type") || "";
    const data = ct.includes("json") ? await r.json().catch(() => ({})) : {};
    if (!r.ok) throw new Error(data.error || ("请求失败 " + r.status));
    return data;
  }

  // 当前选中的机器别名。阶段一固定 local；阶段二接入顶栏切换器后可变。
  let currentHost = "local";
  // 业务 API 都在 /api/h/:host 前缀下（目录/配置/导出）。
  function hapi(path) { return "/api/h/" + encodeURIComponent(currentHost) + path; }

  function setConnStatus(mode) {
    const el = $("connStatus");
    el.className = "status " + mode;
    const labels = { online: "已连接", offline: "未连接", running: "读取中", stopping: "停止中" };
    $("connText").textContent = labels[mode] || mode;
  }

  // ============ 主题 ============
  const XTERM_THEMES = {
    dark: { background: "#1e1e1e", foreground: "#d4d4d4", cursor: "#d4d4d4", selectionBackground: "#264f78" },
    light: { background: "#ffffff", foreground: "#1f2328", cursor: "#1f2328", selectionBackground: "#cfe4ff" },
  };
  function currentTheme() {
    return document.documentElement.getAttribute("data-theme") === "light" ? "light" : "dark";
  }
  function applyTheme(theme) {
    document.documentElement.setAttribute("data-theme", theme);
    const btn = $("themeToggle");
    if (btn) btn.textContent = theme === "light" ? "☀ 白天" : "🌙 黑夜";
    const t = XTERM_THEMES[theme];
    if (term) { try { term.options.theme = t; } catch (e) {} }
    if (gutter) {
      // 行号栏：背景略深/略浅，数字暗色，与主终端同字体
      try {
        gutter.options.theme = {
          background: theme === "light" ? "#eef0f3" : "#181818",
          foreground: theme === "light" ? "#9aa0a8" : "#6a6a6a",
          cursor: theme === "light" ? "#9aa0a8" : "#6a6a6a",
        };
      } catch (e) {}
    }
    try { localStorage.setItem("logviewer-theme", theme); } catch (e) {}
  }

  // ============ Xterm ============
  const FONT_SIZE = 13;
  const SCROLLBACK = 10000;
  const GUTTER_COLS = 7; // 行号栏宽度（字符列）
  let term = null;
  let gutter = null;
  let fitAddon = null;
  let gutterFit = null;
  let gutterMirroredRows = 0; // gutter 已镜像的主终端缓冲区行数（含换行续行）
  let logicalNo = 0;          // 逻辑行号（仅非续行递增）

  function makeTerm(opts) {
    return new Terminal(Object.assign({
      fontSize: FONT_SIZE,
      scrollback: SCROLLBACK,
      convertEol: false,
      fontFamily: 'Consolas, "Courier New", monospace',
      disableStdin: true,
    }, opts));
  }

  try {
    term = makeTerm({ cursorBlink: true, theme: XTERM_THEMES.dark });
    fitAddon = new FitAddon.FitAddon();
    term.loadAddon(fitAddon);
    term.open($("terminal"));

    // 行号栏：只读、无光标、不响应滚轮（由主终端滚动同步驱动）
    gutter = makeTerm({
      cursorBlink: false,
      cursorStyle: "bar",
      scrollback: SCROLLBACK,
      theme: { background: "#181818", foreground: "#6a6a6a", cursor: "#6a6a6a" },
    });
    gutterFit = new FitAddon.FitAddon();
    gutter.loadAddon(gutterFit);
    gutter.open($("gutter"));
    // 行号栏不处理鼠标滚轮/点击，让事件落到主终端
    $("gutter").style.pointerEvents = "none";

    fitTerm();
  } catch (e) {
    console.error("xterm init failed", e);
  }

  // 适配：主终端填满剩余空间；行号栏固定 GUTTER_COLS 列，行数与主终端一致。
  // 宽度变化会改变换行折行点，因此需要按新宽度重建行号栏。
  function fitTerm() {
    if (!fitAddon || !gutter) return;
    try {
      fitAddon.fit();
      gutter.resize(GUTTER_COLS, term.rows);
      rebuildGutter();
    } catch (e) { /* 容器尺寸为 0 时忽略 */ }
  }

  // 主终端滚动 -> 用绝对位置同步行号栏（两条缓冲区行数 1:1，不会累积误差）
  if (term) {
    term.onScroll(() => {
      if (!gutter) return;
      const target = term.buffer.active.viewportY;
      if (gutter.buffer.active.viewportY !== target) gutter.scrollToLine(target);
    });
  }

  // 判断某行是否为末尾的空光标行（不应编号）
  function isTrailingEmptyRow(buf, idx) {
    const line = buf.getLine(idx);
    if (!line) return false;
    return line.length === 0 && !line.isWrapped;
  }

  // 增量镜像主终端缓冲区：每个新增的缓冲区行对应 gutter 一行；
  // 关键：长行折出的"续行"（isWrapped）不递增逻辑行号，在 gutter 中留空，
  // 这样两条终端行数严格 1:1，滚动同步不会错位。
  function syncGutter() {
    if (!gutter || !term) return;
    if (!$("lineNumToggle") || !$("lineNumToggle").checked) return;
    const buf = term.buffer.active;
    let n = buf.length;
    // 末尾光标所在的空行不镜像（gutter 自己也会有一个对应的空光标行）
    if (n > 0 && isTrailingEmptyRow(buf, n - 1)) n--;
    let chunk = "";
    while (gutterMirroredRows < n) {
      const line = buf.getLine(gutterMirroredRows);
      const wrapped = !!(line && line.isWrapped);
      if (!wrapped) logicalNo++;
      const num = wrapped ? "" : String(logicalNo).padStart(GUTTER_COLS - 1);
      chunk += "\x1b[90m" + num + "\x1b[0m\r\n";
      gutterMirroredRows++;
    }
    if (chunk) gutter.write(chunk);
    // 绝对同步滚动位置
    const target = buf.viewportY;
    if (gutter.buffer.active.viewportY !== target) gutter.scrollToLine(target);
  }

  function resetGutter() {
    if (!gutter) return;
    gutterMirroredRows = 0;
    logicalNo = 0;
    gutter.reset();
  }

  // 从主终端当前缓冲区整行重建（切换显示、尺寸变化、重置后用）
  function rebuildGutter() {
    if (!gutter || !term) return;
    gutterMirroredRows = 0;
    logicalNo = 0;
    gutter.reset();
    syncGutter();
  }

  function setGutterVisible(visible) {
    const el = $("gutter");
    if (!el) return;
    el.style.display = visible ? "block" : "none";
    if (visible) {
      // 等浏览器完成布局后再 fit + 重建，否则隐藏期间尺寸为 0，
      // 重建出的行号栏行数/滚动位置会不对，滚动时无法跟随。
      requestAnimationFrame(() => {
        requestAnimationFrame(() => {
          fitTerm();
          rebuildGutter();
        });
      });
    } else {
      fitTerm();
    }
  }

  // 持续同步：每帧把行号栏的 viewportY 对齐到主终端。
  // 比起只在 onScroll 里同步，这种方式在"隐藏后重新显示""折叠面板恢复"等
  // 布局刚变化的场景下也能立刻对齐，不会出现行号不跟随滚动。
  function gutterSyncFrame() {
    if (gutter && term) {
      const show = $("lineNumToggle") && $("lineNumToggle").checked;
      if (show) {
        syncGutter();
      }
    }
    requestAnimationFrame(gutterSyncFrame);
  }
  requestAnimationFrame(gutterSyncFrame);

  // 容器尺寸变化（折叠配置、窗口缩放）时重新适配
  if (window.ResizeObserver && term) {
    const ro = new ResizeObserver(() => fitTerm());
    ro.observe($("terminal"));
    ro.observe($("termPanel"));
  }
  window.addEventListener("resize", fitTerm);
  window.__fitTerm = fitTerm;

  // 高亮：把一行内命中关键词的片段用 ANSI 包裹
  function colorizeLine(line, rules) {
    if (!rules || rules.length === 0) return line;
    const useRegex = $("useRegex").checked;
    const caseSensitive = $("caseSensitive").checked;
    const spans = [];
    for (const rule of rules) {
      if (!rule) continue;
      if (useRegex) {
        let re;
        try { re = new RegExp(rule, caseSensitive ? "g" : "gi"); } catch (e) { continue; }
        let m;
        while ((m = re.exec(line)) !== null) {
          spans.push([m.index, m.index + m[0].length]);
          if (m[0].length === 0) re.lastIndex++;
        }
      } else {
        const src = caseSensitive ? line : line.toLowerCase();
        const r = caseSensitive ? rule : rule.toLowerCase();
        let start = 0;
        while (true) {
          const i = src.indexOf(r, start);
          if (i < 0) break;
          spans.push([i, i + rule.length]);
          start = i + rule.length;
        }
      }
    }
    if (spans.length === 0) return line;
    spans.sort((a, b) => a[0] - b[0]);
    // 线性合并
    let merged = [];
    for (const s of spans) {
      const last = merged[merged.length - 1];
      if (last && s[0] <= last[1]) { if (s[1] > last[1]) last[1] = s[1]; }
      else merged.push([...s]);
    }
    let out = "";
    let pos = 0;
    for (const [a, b] of merged) {
      out += line.slice(pos, a) + HIGHLIGHT_ANSI + line.slice(a, b) + RESET_ANSI;
      pos = b;
    }
    out += line.slice(pos);
    return out;
  }

  function writeToTerminal(text, highlightRules) {
    if (!term) return;
    if (state.paused) {
      state.pausedBuffer.push(text);
      return;
    }
    // 逐行高亮（内容本身不含行号，行号由左侧 gutter 终端显示）
    // 注意：text 每行都以 '\n' 结尾（Windows 远端可能是 '\r\n'）。
    // split("\n") 后最后一个元素是空串，因此循环里对每个非末元素输出 "\r\n"
    // 已经覆盖了所有换行；绝不能再按 endsWith("\n") 额外补一个换行，
    // 否则追踪模式下每个刷新批次末尾都会多出一个空行。
    const lines = text.split("\n");
    let out = "";
    for (let i = 0; i < lines.length; i++) {
      const l = lines[i];
      const isLast = i === lines.length - 1;
      // 去掉 CRLF 残留的 '\r'，避免 "\r\r\n" 双回车。
      const clean = l.endsWith("\r") ? l.slice(0, -1) : l;
      if (clean !== "") out += colorizeLine(clean, highlightRules);
      if (!isLast) out += "\r\n";
    }
    term.write(out, () => syncGutter());
  }

  // ============ 目录树 ============
  const treeEl = $("tree");

  // 机器切换：拉取 /api/hosts 填充顶栏选择器。
  async function loadHosts() {
    const data = await api("/api/hosts");
    const sel = $("hostSelect");
    sel.innerHTML = "";
    (data.hosts || []).forEach((h) => {
      const opt = document.createElement("option");
      opt.value = h.name;
      const plat = h.platform || "未知";
      const status = h.online ? "" : "（离线）";
      opt.textContent = h.name + " [" + plat + "]" + status;
      sel.appendChild(opt);
    });
    sel.value = currentHost;
  }

  // 当前机器的命令能力（控制 GBK / 时间过滤是否可用）。
  let currentCaps = { hasTail: true, hasCat: true, hasGrep: true, hasAwk: true, hasIconv: true };

  async function loadCapabilities() {
    try {
      currentCaps = await api(hapi("/capabilities"));
    } catch (e) {
      currentCaps = { hasTail: true, hasCat: true, hasGrep: true, hasAwk: true, hasIconv: true };
    }
    applyCapabilities();
  }

  // 根据远端能力禁用/启用 GBK 编码选项和时间过滤控件。
  function applyCapabilities() {
    const enc = $("encoding");
    const gbkOpt = enc.querySelector('option[value="gbk"]');
    if (gbkOpt) {
      gbkOpt.disabled = !currentCaps.hasIconv;
      gbkOpt.textContent = currentCaps.hasIconv ? "GBK" : "GBK（远端缺少 iconv）";
    }
    if (!currentCaps.hasIconv && enc.value === "gbk") {
      enc.value = "utf-8";
    }
    const timeDisabled = !currentCaps.hasAwk;
    ["timeStart", "timeEnd", "timePrecision", "clearTimeBtn"].forEach((id) => {
      const el = $(id);
      if (el) el.disabled = timeDisabled;
    });
    if (timeDisabled) {
      if (fpStart) fpStart.clear();
      if (fpEnd) fpEnd.clear();
    }
  }

  // 切换机器：停掉当前 WS、清空目录树与终端、按新 host 重新加载。
  async function switchHost(name) {
    if (name === currentHost) return;
    currentHost = name;
    // 主动断开旧 WS（wsIntendedClose 阻止自动重连）
    state.wsIntendedClose = true;
    if (state.running || state.stopping) wsSend({ action: "stop" });
    if (state.ws) {
      try { state.ws.close(); } catch (e) {}
      state.ws = null;
    }
    state.connected = false;
    state.running = false;
    state.stopping = false;
    state.currentFile = "";
    setConnStatus("offline");
    treeEl.innerHTML = "";
    $("filePath").value = "";
    if (term) term.reset();
    resetGutter();
    try {
      await loadCapabilities();
      await loadRoots();
      await loadConfigList();
      const def = await api(hapi("/config/list"));
      if (def.default) {
        const cfg = await api(hapi("/config/get?name=" + encodeURIComponent(def.default)));
        fillForm(cfg);
      }
      // fillForm 可能选中 gbk，需在加载默认配置后再按能力回退禁用
      applyCapabilities();
    } catch (e) {
      const loginShown = $("loginMask") && $("loginMask").classList.contains("show");
      if (!loginShown) toast("切换机器失败: " + e.message, "error");
    } finally {
      state.wsIntendedClose = false;
      connectWS();
      updateButtons();
    }
  }

  async function loadRoots() {
    const data = await api(hapi("/dir/roots"));
    const sel = $("rootSelect");
    sel.innerHTML = "";
    data.roots.forEach((r, i) => {
      const opt = document.createElement("option");
      opt.value = r;
      opt.textContent = r;
      sel.appendChild(opt);
    });
    if (data.roots.length) {
      sel.value = data.roots[0];
      await loadTreeDir(data.roots[0], treeEl);
    }
  }

  async function loadTreeDir(path, container) {
    container.innerHTML = "";
    const data = await api(hapi("/dir/list?path=" + encodeURIComponent(path)));
    renderNodes(data.nodes, container);
  }

  function renderNodes(nodes, container) {
    for (const n of nodes) {
      const nodeEl = document.createElement("div");
      nodeEl.className = "tree-node";
      const item = document.createElement("div");
      item.className = "tree-item" + (n.isDir ? "" : " tree-file");
      item.dataset.path = n.path;
      item.dataset.isDir = n.isDir;
      item.dataset.name = n.name;

      const arrow = document.createElement("span");
      arrow.className = "arrow";
      arrow.textContent = n.isDir ? "▸" : "";
      const icon = document.createElement("span");
      icon.className = "icon";
      icon.innerHTML = n.isDir ? "&#128193;" : "&#128196;"; // 文件夹/文件
      const name = document.createElement("span");
      name.className = "name";
      name.textContent = n.name;
      name.title = n.name; // 长文件名省略号时 hover 看全名

      item.appendChild(arrow);
      item.appendChild(icon);
      item.appendChild(name);

      if (!n.isDir) {
        const size = document.createElement("span");
        size.className = "size";
        size.textContent = fmtSize(n.size);
        item.appendChild(size);
      }

      nodeEl.appendChild(item);

      const children = document.createElement("div");
      children.className = "tree-children";
      children.style.display = "none";
      nodeEl.appendChild(children);

      item.addEventListener("click", (e) => {
        e.stopPropagation();
        if (n.isDir) {
          toggleDir(item, arrow, children, n.path);
        } else {
          selectFile(item, n.path, n.name);
        }
      });

      container.appendChild(nodeEl);
    }
  }

  async function toggleDir(item, arrow, childrenEl, path) {
    const expanded = childrenEl.style.display !== "none";
    if (expanded) {
      childrenEl.style.display = "none";
      arrow.textContent = "▸";
      return;
    }
    arrow.textContent = "▾";
    if (!childrenEl.dataset.loaded) {
      try {
        const data = await api(hapi("/dir/list?path=" + encodeURIComponent(path)));
        renderNodes(data.nodes, childrenEl);
        childrenEl.dataset.loaded = "1";
      } catch (e) {
        toast(e.message, "error");
      }
    }
    childrenEl.style.display = "block";
  }

  function selectFile(item, path, name) {
    // 正在读取/跟踪时切换文件：必须先停止当前任务，避免"路径已变、后台仍在追旧文件"的错觉。
    // 点击同一个文件无需提示。
    if ((state.running || state.stopping) && path !== $("filePath").value) {
      const follow = $("followTail").value === "true";
      const verb = state.stopping ? "正在停止" : (follow ? "正在实时跟踪" : "正在读取");
      if (!confirm("当前" + verb + "日志文件，切换前需要先停止。\n是否停止并切换到「" + name + "」？")) {
        return;
      }
      wsSend({ action: "stop" });
      state.running = false;
      state.stopping = false;
      if (term) term.reset();
      resetGutter();
      setConnStatus("online");
      updateButtons();
      toast("已停止当前任务", "success");
    }
    if (state.selectedNode) state.selectedNode.classList.remove("selected");
    item.classList.add("selected");
    state.selectedNode = item;
    state.currentFile = path;
    $("filePath").value = path;
    toast("已选择文件: " + name, "success");
  }

  function fmtSize(bytes) {
    if (bytes >= 1024 * 1024) return (bytes / 1024 / 1024).toFixed(1) + "MB";
    if (bytes >= 1024) return (bytes / 1024).toFixed(1) + "KB";
    return bytes + "B";
  }

  // ============ 配置 ============

  // ---------- 时间控件：flatpickr（日期 + 时/分/秒，按粒度切换精度）----------
  let fpStart = null, fpEnd = null;

  // 粒度 -> flatpickr 配置
  function fpOptions(prec) {
    const base = {
      locale: "zh",
      dateFormat: "Y-m-d H:i:S",
      time_24hr: true,
      allowInput: false,
      disableMobile: true,
      enableTime: true,
    };
    if (prec === "day") {
      base.enableTime = false;
      base.dateFormat = "Y-m-d";
    } else if (prec === "hour") {
      base.dateFormat = "Y-m-d H:00:00";
    } else if (prec === "minute") {
      base.dateFormat = "Y-m-d H:i:00";
    }
    return base;
  }

  function initFp() {
    fpStart = flatpickr("#timeStart", fpOptions($("timePrecision").value));
    fpEnd = flatpickr("#timeEnd", fpOptions($("timePrecision").value));
  }

  // 当前值 -> "YYYY-MM-DD HH:MM:SS"（按粒度补齐，后端也会 snap 到边界）
  function fpToTime(fp, isEnd) {
    const s = (fp && fp.input.value) || "";
    if (!s) return "";
    let prec = $("timePrecision").value;
    let v = s.trim();
    if (prec === "day") return v + (isEnd ? " 23:59:59" : " 00:00:00");
    // 补齐被 dateFormat 隐藏的部分
    const parts = v.split(" ");
    if (parts.length < 2) { v += isEnd ? " 23:59:59" : " 00:00:00"; }
    else {
      const t = parts[1].split(":");
      while (t.length < 3) t.push(isEnd ? "59" : "00");
      // hour 粒度：分秒边界
      if (prec === "hour") { t[1] = isEnd ? "59" : "00"; t[2] = isEnd ? "59" : "00"; }
      if (prec === "minute") { t[2] = isEnd ? "59" : "00"; }
      v = parts[0] + " " + t.join(":");
    }
    return v;
  }

  function timeInputs() {
    return { prec: $("timePrecision").value, start: fpStart, end: fpEnd };
  }

  // 粒度切换：销毁重建 flatpickr 以改变可选精度，但保留已选时间（以 Date 对象保留）
  function applyTimePrecision() {
    if (!fpStart) return;
    const sd = fpStart.selectedDates[0] || null;
    const ed = fpEnd.selectedDates[0] || null;
    try { fpStart.destroy(); } catch (e) {}
    try { fpEnd.destroy(); } catch (e) {}
    const opts = fpOptions($("timePrecision").value);
    fpStart = flatpickr("#timeStart", opts);
    fpEnd = flatpickr("#timeEnd", opts);
    if (sd) try { fpStart.setDate(sd, false); } catch (e) {}
    if (ed) try { fpEnd.setDate(ed, false); } catch (e) {}
    // 重建后重新绑定变化监听
    fpStart.config.onChange = fpEnd.config.onChange = [refreshPreview];
  }
  function readLevels() {
    const out = [];
    document.querySelectorAll(".level-chk:checked").forEach((cb) => out.push(cb.value));
    return out;
  }
  function setLevels(levels) {
    document.querySelectorAll(".level-chk").forEach((cb) => {
      cb.checked = (levels || []).includes(cb.value);
    });
  }

  // 正则/普通模式联动：
  //   勾选正则 -> 显示构建器，隐藏文本框与反转
  //   取消勾选 -> 隐藏构建器，显示文本框与反转（反转仅普通模式生效）
  function applyRegexMode() {
    const on = $("useRegex").checked;
    $("configPanel").classList.toggle("regex-on", on);
    if (on) {
      // 切回正则时，把普通文本框的值带到"内容包含"
      const p = $("plainContains").value.trim();
      if (p && !$("contains").value.trim()) $("contains").value = p;
      // 正则模式反转不适用，强制取消
      $("invertMatch").checked = false;
    } else {
      // 切到普通模式，把内容包含带到文本框
      const c = $("contains").value.trim();
      if (c && !$("plainContains").value.trim()) $("plainContains").value = c;
    }
    refreshPreview();
  }

  function readForm() {
    const useRegex = $("useRegex").checked;
    const rule = {
      TimeStart: fpToTime(fpStart, false),
      TimeEnd: fpToTime(fpEnd, true),
      TimePrecision: $("timePrecision").value,
      Levels: useRegex ? readLevels() : [],
      // 正则模式用构建器"内容包含"；普通模式用文本框
      Content: useRegex ? $("contains").value.trim() : $("plainContains").value.trim(),
      Exclude: useRegex ? $("exclude").value.trim() : "",
      CustomRegex: useRegex ? $("customRegex").value.trim() : "",
    };
    return {
      ConfigName: $("cfgName").value.trim(),
      FollowTail: $("followTail").value === "true",
      ReadLinesLimit: $("limitEnable").checked ? (parseInt($("readLines").value, 10) || 0) : 0,
      Encoding: $("encoding").value,
      CaseSensitive: $("caseSensitive").checked,
      InvertMatch: useRegex ? false : $("invertMatch").checked,
      ContextBefore: parseInt($("contextBefore").value, 10) || 0,
      ContextAfter: parseInt($("contextAfter").value, 10) || 0,
      UseRegex: useRegex,
      FilterRule: rule,
      HighlightRules: $("highlightRules").value.split(/[,，\n]/).map((s) => s.trim()).filter(Boolean),
    };
  }

  function fillForm(cfg) {
    $("cfgName").value = cfg.ConfigName || "";
    $("followTail").value = cfg.FollowTail ? "true" : "false";
    const rule = cfg.FilterRule || {};
    $("timePrecision").value = rule.TimePrecision || "second";
    applyTimePrecision();
    // "YYYY-MM-DD HH:MM:SS" -> Date（本地时区）
    const parseLogDate = (s) => {
      if (!s) return null;
      s = s.replace(" ", "T");
      const d = new Date(s);
      return isNaN(d.getTime()) ? null : d;
    };
    if (fpStart) { const d = parseLogDate(rule.TimeStart); if (d) fpStart.setDate(d, false); else fpStart.clear(false); }
    if (fpEnd) { const d = parseLogDate(rule.TimeEnd); if (d) fpEnd.setDate(d, false); else fpEnd.clear(false); }
    setLevels(rule.Levels);
    $("contains").value = rule.Content || "";
    $("plainContains").value = rule.Content || "";
    $("exclude").value = rule.Exclude || "";
    $("customRegex").value = rule.CustomRegex || "";
    $("useRegex").checked = !!cfg.UseRegex;
    $("caseSensitive").checked = !!cfg.CaseSensitive;
    $("invertMatch").checked = !!cfg.InvertMatch;
    applyRegexMode();
    $("contextBefore").value = cfg.ContextBefore || 0;
    $("contextAfter").value = cfg.ContextAfter || 0;
    if (cfg.ReadLinesLimit && cfg.ReadLinesLimit > 0) {
      $("limitEnable").checked = true;
      $("readLines").value = cfg.ReadLinesLimit;
    } else {
      $("limitEnable").checked = false;
      $("readLines").value = "";
    }
    syncLimitEnable();
    $("encoding").value = cfg.Encoding || "utf-8";
    $("highlightRules").value = (cfg.HighlightRules || []).join(", ");
    refreshPreview();
  }

  // 拼装预览：时间范围由命令字符串比较处理，正则仅含级别+内容（短而清晰）
  let previewTimer = null;
  function refreshPreview() {
    clearTimeout(previewTimer);
    previewTimer = setTimeout(async () => {
      try {
        const data = await api(hapi("/config/preview"), {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ FilterRule: readForm().FilterRule, UseRegex: $("useRegex").checked }),
        });
        const el = $("regexPreview");
        const timePart = data.timeRange ? "时间: " + data.timeRange : "";
        const rePart = data.pattern ? "正则: " + data.pattern : "";
        el.textContent = [timePart, rePart].filter(Boolean).join("   |   ") || "(无过滤，读取全部)";
      } catch (e) {
        $("regexPreview").textContent = "预览失败: " + e.message;
      }
    }, 150);
  }

  // 行数输入与"限制行数"复选框联动
  function syncLimitEnable() {
    $("readLines").disabled = !$("limitEnable").checked;
  }

  // 开始/停止按钮联动：
  //  - 实时跟踪：开始=持续 tail，运行中可停止
  //  - 静态加载：开始=执行一次（执行中可中断），完成后自动恢复
  function updateButtons() {
    const follow = $("followTail").value === "true";
    const start = $("startBtn"), stop = $("stopBtn");
    if (state.stopping) {
      // 停止中：立即给视觉反馈，不阻塞其它操作
      start.disabled = true;
      stop.disabled = false;
      stop.textContent = "停止中...";
      stop.classList.add("loading");
      return;
    }
    stop.classList.remove("loading");
    if (state.running) {
      start.disabled = true;
      stop.disabled = false;
      stop.textContent = follow ? "停止跟踪" : "中断";
    } else {
      start.disabled = false;
      stop.disabled = true;
      start.textContent = follow ? "开始跟踪" : "开始查看";
    }
  }

  async function loadConfigList() {
    const data = await api(hapi("/config/list"));
    const sel = $("configSelect");
    sel.innerHTML = "";
    data.names.forEach((n) => {
      const opt = document.createElement("option");
      opt.value = n;
      opt.textContent = n;
      sel.appendChild(opt);
    });
    if (data.default && data.names.includes(data.default)) sel.value = data.default;
    return data;
  }

  async function loadSelectedConfig() {
    const name = $("configSelect").value;
    if (!name) return;
    const cfg = await api(hapi("/config/get?name=" + encodeURIComponent(name)));
    fillForm(cfg);
    toast("已加载配置: " + name, "success");
  }

  async function saveConfig(forceName) {
    const cfg = readForm();
    if (!cfg.ConfigName) {
      const propose = forceName || cfg.ConfigName || prompt("请输入配置名称：");
      if (!propose) return;
      cfg.ConfigName = propose;
    }
    await api(hapi("/config/save"), {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(cfg),
    });
    await loadConfigList();
    $("configSelect").value = cfg.ConfigName;
    toast("配置已保存: " + cfg.ConfigName, "success");
  }

  // ============ WebSocket ============
  function connectWS() {
    const proto = location.protocol === "https:" ? "wss" : "ws";
    const ws = new WebSocket(proto + "://" + location.host + "/ws?host=" + encodeURIComponent(currentHost));
    state.ws = ws;

    ws.onopen = () => {
      state.connected = true;
      setConnStatus(state.running ? "running" : "online");
    };
    ws.onmessage = (ev) => {
      let msg;
      try { msg = JSON.parse(ev.data); } catch (e) { return; }
      if (msg.type === "log") {
        writeToTerminal(msg.data, readForm().HighlightRules);
      } else if (msg.type === "error") {
        if (term) term.write("\x1b[91m[错误] " + msg.msg + "\x1b[0m\r\n");
        setConnStatus("online");
      } else if (msg.type === "status") {
        if (msg.status === "running") { state.running = true; state.stopping = false; setConnStatus("running"); }
        else if (msg.status === "stopped") { state.running = false; state.stopping = false; setConnStatus("online"); }
        else if (msg.status === "waiting") { toast("等待日志文件产生..."); }
        updateButtons();
      }
    };
    ws.onclose = () => {
      state.connected = false;
      state.running = false;
      state.stopping = false;
      setConnStatus("offline");
      updateButtons();
      // 登录遮罩显示时不自动重连（未授权，避免 2 秒一次的重连风暴）
      const loginShown = $("loginMask") && $("loginMask").classList.contains("show");
      if (!state.wsIntendedClose && !loginShown) {
        setTimeout(connectWS, 2000);
      }
    };
    ws.onerror = () => { ws.close(); };
  }

  function wsSend(obj) {
    if (state.ws && state.ws.readyState === WebSocket.OPEN) {
      state.ws.send(JSON.stringify(obj));
    }
  }

  function startView() {
    const file = $("filePath").value.trim();
    if (!file) { toast("请先在左侧选择日志文件", "error"); return; }
    // 修改过滤参数后重新开始：先停掉后台旧命令，清空控制台与缓冲，再用新配置启动
    if (state.running || state.stopping) wsSend({ action: "stop" });
    state.stopping = false;
    state.paused = false;
    state.pausedBuffer = [];
    $("pauseBtn").textContent = "暂停";
    if (term) term.reset();
    resetGutter();
    wsSend({ action: "start", filePath: file, config: readForm() });
  }

  function stopView() {
    // 立即进入"停止中"视觉态（非阻塞），不等服务端 stopped 回执，
    // 避免远程/本地查杀进程的几百毫秒让用户觉得"点了没反应"。
    state.stopping = true;
    setConnStatus("stopping");
    updateButtons();
    wsSend({ action: "stop" });
  }

  // ============ 导出 ============
  function triggerDownload(url) {
    const a = document.createElement("a");
    a.href = url;
    a.download = "";
    document.body.appendChild(a);
    a.click();
    a.remove();
  }

  // 导出遮罩：显示/更新进度，处理中禁用按钮
  let exporting = false;
  function fmtBytes(n) {
    if (n >= 1048576) return (n / 1048576).toFixed(1) + " MB";
    if (n >= 1024) return (n / 1024).toFixed(1) + " KB";
    return n + " B";
  }
  function showExport(title) {
    exporting = true;
    $("exportTitle").textContent = title;
    $("progressFill").style.width = "0%";
    $("exportPercent").textContent = "0%";
    $("exportStatus").textContent = "连接中...";
    $("exportMask").classList.add("show");
    $("exportRawBtn").disabled = true;
    $("exportFilterBtn").disabled = true;
  }
  function setExportProgress(received, total, done) {
    let pct;
    if (total && total > 0) {
      pct = Math.min(100, Math.round((received / total) * 100));
      $("exportStatus").textContent = done ? "完成" : (fmtBytes(received) + " / " + fmtBytes(total));
    } else {
      // 无 Content-Length（过滤导出通常如此）：不确定进度，用已接收字节做提示
      pct = done ? 100 : 0;
      $("exportStatus").textContent = done ? "完成" : ("已生成 " + fmtBytes(received) + " ...");
    }
    $("progressFill").style.width = pct + "%";
    $("exportPercent").textContent = pct + "%";
  }
  function hideExport() {
    exporting = false;
    $("exportMask").classList.remove("show");
    $("exportRawBtn").disabled = false;
    $("exportFilterBtn").disabled = false;
  }

  // 导出类 fetch 不走 api()，这里补 401 处理（会话过期时弹登录）。
  function authCheck(r) {
    if (r.status === 401 && authEnabled) {
      showLogin();
      throw new Error("未登录或会话已过期");
    }
    return r;
  }

  // 流式读取响应体，边下边显示进度（不依赖 Content-Length 也能显示已下载字节）
  async function streamDownload(response) {
    const total = parseInt(response.headers.get("Content-Length") || "0", 10);
    const reader = response.body.getReader();
    const chunks = [];
    let received = 0;
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      chunks.push(value);
      received += value.length;
      setExportProgress(received, total, false);
    }
    setExportProgress(received, total || received, true);
    return new Blob(chunks, { type: response.headers.get("Content-Type") || "application/octet-stream" });
  }

  function saveBlob(response, blob) {
    const url = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = url;
    const cd = response.headers.get("Content-Disposition") || "";
    const m = /filename="?([^"]+)"?/.exec(cd);
    if (m) a.download = decodeURIComponent(m[1]);
    document.body.appendChild(a);
    a.click();
    a.remove();
    setTimeout(() => URL.revokeObjectURL(url), 1000);
  }

  // 导出原始：GET 流式下载 + 进度（原始文件有 Content-Length，可显示真实进度）
  async function triggerRawDownload(file) {
    showExport("正在导出原始日志...");
    try {
      const r = authCheck(await fetch(hapi("/file/download/origin?path=" + encodeURIComponent(file))));
      if (!r.ok) {
        let msg = "导出失败 " + r.status;
        try { msg = (await r.json()).error || msg; } catch (e) {}
        throw new Error(msg);
      }
      const blob = await streamDownload(r);
      saveBlob(r, blob);
      setTimeout(hideExport, 400);
      toast("原始日志已导出", "success");
    } catch (e) {
      hideExport();
      toast("导出失败: " + e.message, "error");
    }
  }

  // 过滤导出：POST 当前表单配置，流式下载 + 字节进度。
  // 处理中禁用按钮，完成后恢复。
  async function triggerFilteredDownload(file) {
    if (exporting) return;
    showExport("正在按过滤条件导出（大文件可能需要一些时间）...");
    try {
      const r = authCheck(await fetch(
        hapi("/file/download/filter?path=" + encodeURIComponent(file)),
        {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify(readForm()),
        }
      ));
      if (!r.ok) {
        let msg = "导出失败 " + r.status;
        try { msg = (await r.json()).error || msg; } catch (e) {}
        throw new Error(msg);
      }
      const blob = await streamDownload(r);
      saveBlob(r, blob);
      setTimeout(hideExport, 400);
      toast("过滤日志已导出", "success");
    } catch (e) {
      hideExport();
      toast("导出失败: " + e.message, "error");
    }
  }

  // ============ 事件绑定 ============
  function bindEvents() {
    $("hostSelect").addEventListener("change", async (e) => {
      await switchHost(e.target.value);
    });
    $("refreshTreeBtn").addEventListener("click", async () => {
      const root = $("rootSelect").value;
      if (root) await loadTreeDir(root, treeEl);
    });
    $("rootSelect").addEventListener("change", async () => {
      const root = $("rootSelect").value;
      if (root) await loadTreeDir(root, treeEl);
    });

    $("loadCfgBtn").addEventListener("click", loadSelectedConfig);
    $("saveCfgBtn").addEventListener("click", () => saveConfig($("cfgName").value));
    $("saveAsCfgBtn").addEventListener("click", () => {
      const name = prompt("另存为配置名称：");
      if (name) { $("cfgName").value = name; saveConfig(name); }
    });
    $("delCfgBtn").addEventListener("click", async () => {
      const name = $("configSelect").value;
      if (!name) return;
      if (!confirm("删除配置「" + name + "」？")) return;
      await api(hapi("/config/delete"), {
        method: "POST", headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ name }),
      });
      await loadConfigList();
      toast("已删除", "success");
    });
    $("setDefaultCfgBtn").addEventListener("click", async () => {
      const name = $("configSelect").value;
      if (!name) return;
      await api(hapi("/config/setdefault"), {
        method: "POST", headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ name }),
      });
      toast("已设为默认", "success");
    });

    $("startBtn").addEventListener("click", () => { startView(); updateButtons(); });
    $("stopBtn").addEventListener("click", () => { stopView(); updateButtons(); });

    // 模式（跟踪/静态）切换：若正在运行先停掉，并刷新按钮文案/联动
    $("followTail").addEventListener("change", () => {
      if (state.running) {
        wsSend({ action: "stop" });
        state.running = false;
        setConnStatus("online");
        toast("已停止当前任务，可重新开始", "success");
      }
      updateButtons();
    });
    $("limitEnable").addEventListener("change", syncLimitEnable);

    // 配置面板折叠（点击折叠按钮或标题栏）——按钮带文字，醒目
    const togglePanel = () => {
      const panel = $("configPanel");
      panel.classList.toggle("collapsed");
      const collapsed = panel.classList.contains("collapsed");
      $("cfgToggle").innerHTML = collapsed ? "▸ 展开配置" : "▾ 收起配置";
      $("cfgToggle").title = collapsed ? "展开配置" : "折叠配置";
      setTimeout(fitTerm, 60);
    };
    $("cfgToggle").addEventListener("click", togglePanel);
    const cfgTitle = document.querySelector("#configPanel .cfg-quickbar .brand-mini");
    if (cfgTitle) cfgTitle.style.cursor = "pointer", cfgTitle.addEventListener("click", togglePanel);

    // 侧栏折叠 / 展开（带文字的醒目按钮）
    const setSidebar = (collapsed) => {
      $("treePanel").classList.toggle("collapsed", collapsed);
      $("sidebarReopen").classList.toggle("show", collapsed);
      $("sidebarCollapse").textContent = collapsed ? "▶ 展开" : "◀ 收起";
      setTimeout(fitTerm, 80);
    };
    $("sidebarCollapse").addEventListener("click", () => setSidebar(true));
    $("sidebarReopen").querySelector("button").addEventListener("click", () => setSidebar(false));

    // 主题切换（白天/黑夜）
    $("themeToggle").addEventListener("click", () => {
      applyTheme(currentTheme() === "dark" ? "light" : "dark");
    });

    // 时间粒度切换：重建 flatpickr
    $("timePrecision").addEventListener("change", () => {
      applyTimePrecision();
      refreshPreview();
    });
    $("clearTimeBtn").addEventListener("click", () => {
      if (fpStart) fpStart.clear();
      if (fpEnd) fpEnd.clear();
      refreshPreview();
    });

    // 正则/普通模式切换：联动显隐
    $("useRegex").addEventListener("change", applyRegexMode);
    // 过滤构建器：任何输入变化都刷新拼装预览
    ["contains", "exclude", "customRegex", "plainContains"]
      .forEach((id) => $(id).addEventListener("input", refreshPreview));
    document.querySelectorAll(".level-chk").forEach((cb) => cb.addEventListener("change", refreshPreview));

    $("pauseBtn").addEventListener("click", () => {
      state.paused = !state.paused;
      $("pauseBtn").textContent = state.paused ? "继续" : "暂停";
      if (!state.paused && state.pausedBuffer.length) {
        const rules = readForm().HighlightRules;
        writeToTerminal(state.pausedBuffer.join(""), rules);
        state.pausedBuffer = [];
      }
    });
    $("clearBtn").addEventListener("click", () => { if (term) term.reset(); resetGutter(); });
    $("lineNumToggle").addEventListener("change", (e) => {
      setGutterVisible(e.target.checked);
    });
    $("copyBtn").addEventListener("click", () => {
      const sel = term.getSelection();
      if (sel) { navigator.clipboard.writeText(sel).then(() => toast("已复制", "success")); }
      else toast("请先在终端中鼠标选中文本", "error");
    });
    $("exportRawBtn").addEventListener("click", () => {
      const file = $("filePath").value.trim();
      if (!file) { toast("请先选择日志文件", "error"); return; }
      triggerRawDownload(file);
    });
    $("exportFilterBtn").addEventListener("click", () => {
      const file = $("filePath").value.trim();
      if (!file) { toast("请先选择日志文件", "error"); return; }
      triggerFilteredDownload(file);
    });

    // 登录表单
    $("loginForm").addEventListener("submit", onLoginSubmit);
    // 登出
    $("logoutBtn").addEventListener("click", onLogout);

    initSplitter();
  }

  // ============ 拖拽侧栏 ============
  function initSplitter() {
    const panel = $("treePanel");
    const splitter = $("splitter");
    const MIN_W = 180, MAX_W = 600;
    const KEY = "logviewer-sidebar-width";

    // 恢复记忆宽度
    try {
      const saved = parseInt(localStorage.getItem(KEY), 10);
      if (!isNaN(saved) && saved >= MIN_W && saved <= MAX_W) {
        panel.style.width = saved + "px";
      }
    } catch (e) {}

    let dragging = false;
    splitter.addEventListener("mousedown", (e) => {
      if (panel.classList.contains("collapsed")) return;
      dragging = true;
      splitter.classList.add("dragging");
      panel.classList.add("no-transition");
      document.body.style.userSelect = "none";
      document.body.style.cursor = "col-resize";
      e.preventDefault();
    });
    document.addEventListener("mousemove", (e) => {
      if (!dragging) return;
      // 面板贴左，宽度直接取相对视口左边的距离
      let w = e.clientX;
      if (w < MIN_W) w = MIN_W;
      if (w > MAX_W) w = MAX_W;
      panel.style.width = w + "px";
    });
    const stopDrag = () => {
      if (!dragging) return;
      dragging = false;
      splitter.classList.remove("dragging");
      panel.classList.remove("no-transition");
      document.body.style.userSelect = "";
      document.body.style.cursor = "";
      const w = parseInt(panel.style.width, 10);
      if (!isNaN(w)) {
        try { localStorage.setItem(KEY, String(w)); } catch (e) {}
      }
      fitTerm(); // 宽度变化后让 xterm 重新适配
    };
    document.addEventListener("mouseup", stopDrag);
    document.addEventListener("mouseleave", stopDrag);
  }

  // ============ 登录 / 登出 ============
  async function onLoginSubmit(e) {
    e.preventDefault();
    const errEl = $("loginError");
    errEl.textContent = "";
    const username = $("loginUser").value.trim();
    const password = $("loginPass").value;
    if (!username || !password) { errEl.textContent = "请输入用户名和密码"; return; }
    try {
      const r = await fetch("/api/login", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ username, password }),
      });
      const data = r.headers.get("content-type")?.includes("json")
        ? await r.json().catch(() => ({})) : {};
      if (!r.ok) {
        errEl.textContent = data.error || ("登录失败 " + r.status);
        $("loginPass").value = "";
        return;
      }
      hideLogin();
      state.wsIntendedClose = false;
      await init();
    } catch (err) {
      errEl.textContent = "网络错误: " + err.message;
    }
  }

  async function onLogout() {
    try { await fetch("/api/logout", { method: "POST" }); } catch (e) {}
    // 登出后整页重载，回到鉴权判定流程
    window.location.reload();
  }

  // ============ 启动 ============
  let uiReady = false;

  // 一次性 UI 初始化（主题、flatpickr、事件绑定）。登录后重复 init 不会重复绑定。
  function setupUI() {
    if (uiReady) return;
    uiReady = true;
    let saved = "dark";
    try { saved = localStorage.getItem("logviewer-theme") || "dark"; } catch (e) {}
    applyTheme(saved);
    initFp();
    // flatpickr 变化时刷新预览
    fpStart.config.onChange.push(refreshPreview);
    fpEnd.config.onChange.push(refreshPreview);
    bindEvents();
    syncLimitEnable();
    updateButtons();
  }

  async function init() {
    setupUI();

    // 判定是否启用认证 / 当前是否已登录
    let status = { enabled: false, authed: false };
    try {
      const r = await fetch("/api/auth/status");
      if (r.ok) status = await r.json();
    } catch (e) {}
    authEnabled = !!status.enabled;
    const logoutBtn = $("logoutBtn");
    if (logoutBtn) logoutBtn.style.display = authEnabled ? "" : "none";
    if (authEnabled && !status.authed) {
      showLogin();
      return;
    }
    hideLogin();
    state.wsIntendedClose = false;

    try {
      await loadHosts();
      await loadCapabilities();
      await loadRoots();
      await loadConfigList();
      const def = await api(hapi("/config/list"));
      if (def.default) {
        const cfg = await api(hapi("/config/get?name=" + encodeURIComponent(def.default)));
        fillForm(cfg);
      }
      applyCapabilities();
    } catch (e) {
      // 401 已由 api() 弹出登录遮罩，这里不再重复 toast
      const loginShown = $("loginMask") && $("loginMask").classList.contains("show");
      if (!loginShown) toast("初始化失败: " + e.message, "error");
      return;
    }
    updateButtons();
    connectWS();
  }

  init();
})();