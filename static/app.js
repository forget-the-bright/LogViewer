// ===== LogViewer 前端逻辑 =====
(function () {
  "use strict";

  // ---------- 状态 ----------
  const state = {
    ws: null,
    wsGen: 0,            // WebSocket 代次：每次 connectWS 自增，用于回调守卫
    wsIntendedClose: false,
    reconnecting: false, // 服务器要求重连（主机热更）：抑制离线横幅、立即重连
    connected: false,
    running: false,
    stopping: false,
    waiting: false,      // follow 目标文件暂不存在，服务端等待中
    paused: false,
    pausedBuffer: [],
    pausedBufferChars: 0, // pausedBuffer 的总字符数
    pausedDropped: 0, // 暂停期间因缓冲超限被丢弃的字符数
    currentFile: "",
    selectedNode: null,
    activeConfig: null,   // 最近一次 start 使用的配置（断线重连自动恢复用）
    pendingResume: null,  // 连接建立后需要自动重发的 {filePath, config}
  };

  // 统一复位暂停相关状态：切换文件/主机、断线、停止、开始时都必须调用，
  // 否则暂停缓冲里的旧日志会在新视图继续输出、"继续"按钮状态也会错乱。
  function resetPauseState() {
    state.paused = false;
    state.pausedBuffer = [];
    state.pausedBufferChars = 0;
    state.pausedDropped = 0;
    const pb = $("pauseBtn");
    if (pb) pb.textContent = "暂停";
  }

  // WebSocket 指数退避重连
  const RECONNECT_BASE = 1000;
  const RECONNECT_MAX = 30000;
  let reconnectDelay = RECONNECT_BASE;
  let reconnectTimer = null;

  // 暂停期间最多缓冲的字符数。按平均每行 ~200 字符折算，约 5000 行的量级；
  // 超出后丢弃最旧的数据，避免长时间暂停 + 高频日志撑爆浏览器内存。
  const MAX_PAUSED_BUFFER_CHARS = 5000 * 200;
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

  // 包裹事件处理器中的 async 函数，捕获异常并 toast，避免 unhandled rejection。
  function safeRun(fn) {
    return function (...args) {
      try {
        const r = fn.apply(this, args);
        if (r && typeof r.then === "function") {
          r.catch((e) => {
            const loginShown = $("loginMask") && $("loginMask").classList.contains("show");
            if (!loginShown) toast((e && e.message) || String(e), "error");
          });
        }
      } catch (e) {
        toast((e && e.message) || String(e), "error");
      }
    };
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
    cancelReconnect();
    hideDisconnectBanner();
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
  let searchAddon = null;
  // 镜像游标：mirrorEndAbs 是"已镜像到的主终端绝对行号"（baseY+length 坐标系）。
  // 不能用单调递增的"已镜像行数"去和 buf.length 比——缓冲区超过 scrollback 后
  // 旧行被淘汰、buf.length 封顶，该计数会永久大于 length，导致行号栏彻底冻结。
  // baseY 会随淘汰前移，用它计算新增行数可正确跨越封顶。
  let mirrorEndAbs = 0;
  let logicalNo = 0;          // 逻辑行号（仅非续行递增）

  function makeTerm(opts) {
    return new Terminal(Object.assign({
      fontSize: FONT_SIZE,
      scrollback: SCROLLBACK,
      convertEol: false,
      fontFamily: 'Consolas, "Courier New", monospace',
      disableStdin: true,
      // search addon 的匹配高亮依赖 registerDecoration 装饰 API；
      // 此版本 xterm 将其列为 proposed API，不显式开启会抛
      // "You must set the allowProposedApi option to true"，
      // 导致 findNext 静默失败、Ctrl+F 搜索完全无效。
      allowProposedApi: true,
    }, opts));
  }

  try {
    term = makeTerm({ cursorBlink: true, theme: XTERM_THEMES.dark });
    fitAddon = new FitAddon.FitAddon();
    term.loadAddon(fitAddon);
    term.open($("terminal"));

    // 终端内搜索（xterm-addon-search）。UMD 挂在 window.SearchAddon，类为 .SearchAddon。
    if (window.SearchAddon && window.SearchAddon.SearchAddon) {
      searchAddon = new window.SearchAddon.SearchAddon();
      term.loadAddon(searchAddon);
    }

    // 行号栏：只读、无光标、不响应滚轮（由主终端滚动同步驱动）
    gutter = makeTerm({
      cursorBlink: false,
      cursorStyle: "bar",
      scrollback: SCROLLBACK,
      theme: { background: "#181818", foreground: "#6a6a6a", cursor: "#6a6a6a" },
    });
    // 行号栏宽度固定（GUTTER_COLS），高度由主终端 fit 后用 gutter.resize 同步，
    // 不需要 FitAddon（否则会按父容器拉伸宽度，与固定列宽冲突）。
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

  // 清空主终端、行号栏与搜索高亮（开始/停止/清空/切换主机时调用）。
  function resetTerminal() {
    if (term) term.reset();
    if (searchAddon) { try { searchAddon.clearDecorations(); } catch (e) {} }
    resetGutter();
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
  //
  // 用绝对行号（baseY 坐标系）而非单调计数来判断新增：缓冲区超出 scrollback
  // 后旧行被淘汰、buf.length 封顶、baseY 前移。若用"已镜像行数 < length"比较，
  // 一旦计数超过封顶的 length 就会永久停止镜像（行号栏冻结）。检测到淘汰时
  // 直接整栏重建，未淘汰时按绝对位置增量追加，两种情况都正确。
  function syncGutter() {
    if (!gutter || !term) return;
    if (!$("lineNumToggle") || !$("lineNumToggle").checked) return;
    const buf = term.buffer.active;
    let n = buf.length;
    // 末尾光标所在的空行不镜像（gutter 自己也会有一个对应的空光标行）
    if (n > 0 && isTrailingEmptyRow(buf, n - 1)) n--;

    const endAbs = buf.baseY + n; // 主终端最后一个有效行的绝对行号（exclusive）
    if (endAbs < mirrorEndAbs) {
      // 发生淘汰（baseY 前移到已镜像位置之前）：游标已失效，整栏重建。
      rebuildGutter();
      return;
    }
    if (endAbs === mirrorEndAbs) {
      // 无新增，仅同步滚动位置。
      const t = buf.viewportY;
      if (gutter.buffer.active.viewportY !== t) gutter.scrollToLine(t);
      return;
    }

    // 增量追加从 (mirrorEndAbs - baseY) 到 n 的缓冲区行。
    let chunk = "";
    let idx = mirrorEndAbs - buf.baseY;
    if (idx < 0) { rebuildGutter(); return; }
    while (idx < n) {
      const line = buf.getLine(idx);
      const wrapped = !!(line && line.isWrapped);
      if (!wrapped) logicalNo++;
      const num = wrapped ? "" : String(logicalNo).padStart(GUTTER_COLS - 1);
      chunk += "\x1b[90m" + num + "\x1b[0m\r\n";
      idx++;
    }
    mirrorEndAbs = endAbs;
    if (chunk) gutter.write(chunk);
    // 绝对同步滚动位置
    const target = buf.viewportY;
    if (gutter.buffer.active.viewportY !== target) gutter.scrollToLine(target);
  }

  function resetGutter() {
    if (!gutter) return;
    mirrorEndAbs = 0;
    logicalNo = 0;
    gutter.reset();
  }

  // 从主终端当前缓冲区整行重建（切换显示、尺寸变化、重置后、淘汰后用）。
  // 重建以"当前可视缓冲区第一行"为行号 1（重置后旧行已无意义），保证编号连续。
  function rebuildGutter() {
    if (!gutter || !term) return;
    mirrorEndAbs = 0;
    logicalNo = 0;
    gutter.reset();
    if (!$("lineNumToggle") || !$("lineNumToggle").checked) return;
    const buf = term.buffer.active;
    let n = buf.length;
    if (n > 0 && isTrailingEmptyRow(buf, n - 1)) n--;
    let chunk = "";
    for (let idx = 0; idx < n; idx++) {
      const line = buf.getLine(idx);
      const wrapped = !!(line && line.isWrapped);
      if (!wrapped) logicalNo++;
      const num = wrapped ? "" : String(logicalNo).padStart(GUTTER_COLS - 1);
      chunk += "\x1b[90m" + num + "\x1b[0m\r\n";
    }
    mirrorEndAbs = buf.baseY + n;
    if (chunk) gutter.write(chunk);
    const target = buf.viewportY;
    if (gutter.buffer.active.viewportY !== target) gutter.scrollToLine(target);
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
      state.pausedBufferChars += text.length;
      // 超出上限：从队首丢弃最旧的数据，直到回到上限以内（至少保留最后一批）。
      while (state.pausedBufferChars > MAX_PAUSED_BUFFER_CHARS && state.pausedBuffer.length > 1) {
        const dropped = state.pausedBuffer.shift();
        state.pausedBufferChars -= dropped.length;
        state.pausedDropped += dropped.length;
      }
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
    const prevHost = currentHost;
    sel.innerHTML = "";
    (data.hosts || []).forEach((h) => {
      const opt = document.createElement("option");
      opt.value = h.name;
      const plat = h.platform || "未知";
      const status = h.online ? "" : "（离线）";
      opt.textContent = h.name + " [" + plat + "]" + status;
      sel.appendChild(opt);
    });
    // 如果之前选中的主机已不存在，切回 local。
    // 用按 value 查找而非 querySelector 拼字符串：主机别名可能含引号等特殊字符，
    // 拼进属性选择器会造成选择器注入/语法错误。
    if (Array.from(sel.options).some((o) => o.value === prevHost)) {
      sel.value = prevHost;
    } else {
      sel.value = "local";
      if (prevHost !== "local") {
        await switchHost("local");
      }
    }
  }

  // 自动刷新机器列表（不打断当前操作，仅静默更新下拉框状态文字）
  let hostsTimer = null;
  let refreshingHosts = false;
  async function refreshHosts() {
    if (document.hidden) return;
    // 在途锁：若上一次刷新仍未返回（网络慢/轮询间隔短），跳过本次，
    // 避免并发请求的返回顺序错乱导致下拉框 DOM 互相覆盖。
    if (refreshingHosts) return;
    refreshingHosts = true;
    try {
      const data = await api("/api/hosts");
      const sel = $("hostSelect");
      const prevValue = sel.value;
      // 静默更新选项文本（在线/离线状态），不重建 DOM 以避免闪烁
      const hosts = data.hosts || [];
      const existing = {};
      Array.from(sel.options).forEach((o) => { existing[o.value] = o; });
      hosts.forEach((h) => {
        const plat = h.platform || "未知";
        const status = h.online ? "" : "（离线）";
        const text = h.name + " [" + plat + "]" + status;
        if (existing[h.name]) {
          existing[h.name].textContent = text;
          delete existing[h.name];
        } else {
          const opt = document.createElement("option");
          opt.value = h.name;
          opt.textContent = text;
          sel.appendChild(opt);
        }
      });
      // 删除已不存在的主机选项
      Object.values(existing).forEach((o) => o.remove());
      if (Array.from(sel.options).some((o) => o.value === prevValue)) {
        sel.value = prevValue;
      } else {
        sel.value = "local";
        if (prevValue !== "local") await switchHost("local");
      }
    } catch (e) {
      // 静默失败：自动刷新不应打扰用户
    } finally {
      refreshingHosts = false;
    }
  }

  // 热加载配置文件
  async function reloadConfig() {
    try {
      const data = await api("/api/reload", { method: "POST" });
      await loadHosts();
      // 如果当前主机仍存在，刷新其根目录和配置
      const sel = $("hostSelect");
      if (Array.from(sel.options).some((o) => o.value === currentHost)) {
        await loadCapabilities();
        await loadRoots();
        await loadConfigList();
      } else {
        await switchHost("local");
      }
      toast("配置已重载（共 " + ((data.hosts || []).length) + " 台机器）", "success");
    } catch (e) {
      toast("重载失败: " + e.message, "error");
    }
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

  // 根据远端能力禁用/启用 GBK/GB2312 编码选项和时间过滤控件。
  function applyCapabilities() {
    const enc = $("encoding");
    enc.querySelectorAll('option[value="gbk"],option[value="gb2312"]').forEach((opt) => {
      opt.disabled = !currentCaps.hasIconv;
      const label = opt.value.toUpperCase();
      opt.textContent = currentCaps.hasIconv ? label : label + "（远端缺少 iconv）";
    });
    if (!currentCaps.hasIconv && (enc.value === "gbk" || enc.value === "gb2312")) {
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
    // 主动断开旧 WS（wsIntendedClose 阻止自动重连），并取消任何挂起的重连定时器
    state.wsIntendedClose = true;
    cancelReconnect();
    hideDisconnectBanner();
    if (state.running || state.stopping) wsSend({ action: "stop" });
    if (state.ws) {
      try { state.ws.close(); } catch (e) {}
      state.ws = null;
    }
    state.connected = false;
    state.running = false;
    state.stopping = false;
    state.waiting = false;
    state.pendingResume = null;
    state.currentFile = "";
    state.activeConfig = null;
    setConnStatus("offline");
    resetPauseState();
    treeEl.innerHTML = "";
    $("filePath").value = "";
    resetTerminal();
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
    childrenEl.style.display = "block";
    // 防重复：加载中（dataset.loading）或已加载都不再发请求。
    // 否则用户在请求返回前连续点击会并发发起多次 /dir/list。
    if (childrenEl.dataset.loaded || childrenEl.dataset.loading) return;
    childrenEl.dataset.loading = "1";
    try {
      const data = await api(hapi("/dir/list?path=" + encodeURIComponent(path)));
      renderNodes(data.nodes, childrenEl);
      childrenEl.dataset.loaded = "1";
    } catch (e) {
      toast(e.message, "error");
      // 加载失败时折叠回去，允许重试
      childrenEl.style.display = "none";
      arrow.textContent = "▸";
    } finally {
      delete childrenEl.dataset.loading;
    }
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
      state.waiting = false;
      state.pendingResume = null;
      resetPauseState();
      resetTerminal();
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
  //   勾选正则 -> 显示构建器，隐藏纯文本框
  //   取消勾选 -> 隐藏构建器，显示纯文本框
  // 反转（grep -v / -NotMatch）在两种模式下都生效：后端对正则主模式同样支持 -vE。
  function applyRegexMode() {
    const on = $("useRegex").checked;
    $("configPanel").classList.toggle("regex-on", on);
    if (on) {
      // 切回正则时，把普通文本框的值带到"内容包含"
      const p = $("plainContains").value.trim();
      if (p && !$("contains").value.trim()) $("contains").value = p;
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
      InvertMatch: $("invertMatch").checked,
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
          body: JSON.stringify({
            FilterRule: readForm().FilterRule,
            UseRegex: $("useRegex").checked,
            CaseSensitive: $("caseSensitive").checked,
          }),
        });
        const el = $("regexPreview");
        const timePart = data.timeRange ? "时间: " + data.timeRange : "";
        const rePart = data.pattern ? "正则: " + data.pattern : "";
        el.textContent = [timePart, rePart].filter(Boolean).join("   |   ") || "(无过滤，读取全部)";
        // 正则校验错误：在预览区红字提示，并把对应输入框标红。
        const errEl = $("regexError");
        // 根据错误前缀定位到具体输入框（自定义正则优先级最高）。
        let badField = null;
        if ($("customRegex").value.trim()) badField = $("customRegex");
        else if (data.regexError && data.regexError.indexOf("排除") >= 0) badField = $("exclude");
        else if (data.regexError && data.regexError.indexOf("匹配") >= 0) badField = $("contains");
        document.querySelectorAll(".input-error").forEach((n) => n.classList.remove("input-error"));
        if (data.regexError) {
          if (errEl) {
            errEl.textContent = "⚠ " + data.regexError;
            errEl.style.display = "";
          }
          if (badField) badField.classList.add("input-error");
        } else if (errEl) {
          errEl.textContent = "";
          errEl.style.display = "none";
        }
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
    } else if (state.waiting) {
      // 等待目标文件产生：禁用"开始"避免重复提交，仍允许停止。
      start.disabled = true;
      stop.disabled = false;
      stop.textContent = "取消等待";
    } else {
      start.disabled = !state.connected;
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
  function showDisconnectBanner() {
    const b = $("disconnectBanner");
    if (b) b.classList.add("show");
  }
  function hideDisconnectBanner() {
    const b = $("disconnectBanner");
    if (b) b.classList.remove("show");
  }

  function cancelReconnect() {
    if (reconnectTimer) { clearTimeout(reconnectTimer); reconnectTimer = null; }
  }

  // probeAuthThenReconnect 在 WS 握手失败（从未 onopen）后探测认证状态：
  // 已登录说明是网络/服务暂时不可达，走正常退避重连；未登录说明会话过期，
  // 弹登录框并停止重连（登录成功后会主动 connectWS）。
  async function probeAuthThenReconnect(gen) {
    try {
      const r = await fetch("/api/auth/status", { cache: "no-store" });
      const data = r.ok ? await r.json().catch(() => ({})) : {};
      // 回调时连接可能已被 switchHost/新 connectWS 取代，守卫避免乱入。
      if (gen !== state.wsGen) return;
      if (data.enabled && data.authed === false) {
        showLogin();
        return;
      }
    } catch (e) {
      // auth/status 自身也不可达：按普通断线处理（可能服务整体没起来）。
      if (gen !== state.wsGen) return;
    }
    if (!state.wsIntendedClose) {
      const loginShown = $("loginMask") && $("loginMask").classList.contains("show");
      if (!loginShown) showDisconnectBanner();
    }
    scheduleReconnect();
  }

  function scheduleReconnect() {
    if (reconnectTimer) return;
    const loginShown = $("loginMask") && $("loginMask").classList.contains("show");
    if (state.wsIntendedClose || loginShown) return;
    reconnectTimer = setTimeout(() => {
      reconnectTimer = null;
      connectWS();
    }, reconnectDelay);
    reconnectDelay = Math.min(reconnectDelay * 2, RECONNECT_MAX);
  }

  function connectWS() {
    // 先关闭可能存在的旧连接，避免"立即重连"在旧连接仍 CONNECTING 时
    // 产生两个并存连接（onmessage 双写、onclose 互相覆盖状态）。
    if (state.ws) {
      try { state.ws.onopen = state.ws.onmessage = state.ws.onclose = state.ws.onerror = null; state.ws.close(); } catch (e) {}
      state.ws = null;
    }
    const gen = ++state.wsGen;
    const proto = location.protocol === "https:" ? "wss" : "ws";
    const ws = new WebSocket(proto + "://" + location.host + "/ws?host=" + encodeURIComponent(currentHost));
    state.ws = ws;
    // opened：本次连接是否曾成功握手。浏览器 WebSocket 拿不到 HTTP 401 状态码，
    // 只能用"从未 onopen"来区分"会话过期导致的握手失败"与"连接中途断开"。
    let opened = false;

    // 代次守卫：任何异步回调（open/message/close/error）触发时，若它已不是
    // 最新连接，就必须忽略，否则 switchHost 后旧连接的迟到 onclose 会误清
    // 新连接状态、误弹断线横幅、再触发一次多余重连。
    const stale = () => state.wsGen !== gen || state.ws !== ws;

    ws.onopen = () => {
      if (stale()) return;
      opened = true;
      state.connected = true;
      state.waiting = false;
      reconnectDelay = RECONNECT_BASE;
      hideDisconnectBanner();
      // 断线前若有正在跟踪的会话，重连后自动续跟，避免用户误以为日志仍在实时输出。
      if (state.pendingResume) {
        const resume = state.pendingResume;
        state.pendingResume = null;
        state.running = false;
        state.stopping = false;
        resetPauseState();
        setConnStatus("online");
        toast("连接已恢复，继续跟踪日志", "success");
        wsSend({ action: "start", filePath: resume.filePath, config: resume.config });
      } else {
        setConnStatus(state.running ? "running" : "online");
      }
      // 连接建立后刷新按钮态：空闲时需要启用"开始"按钮（setupUI 时因
      // 尚未连接将其禁用，而空闲连接不会收到 status 消息来再次触发刷新）。
      updateButtons();
    };
    ws.onmessage = (ev) => {
      if (stale()) return;
      let msg;
      try { msg = JSON.parse(ev.data); } catch (e) { return; }
      if (msg.type === "log") {
        writeToTerminal(msg.data, state.activeConfig ? state.activeConfig.HighlightRules : readForm().HighlightRules);
      } else if (msg.type === "reconnect") {
        // 服务器通知主机配置已热更：关闭当前连接，由 onclose 走重连流程
        // 绑定到新实例。reconnecting 标记抑制离线横幅，pendingResume 由
        // onclose 按运行状态保留，重连后自动恢复日志跟踪。
        state.reconnecting = true;
        try { state.ws.close(); } catch (e) {}
      } else if (msg.type === "error") {
        if (term) term.write("\x1b[91m[错误] " + msg.msg + "\x1b[0m\r\n");
        setConnStatus("online");
      } else if (msg.type === "status") {
        if (msg.status === "running") {
          state.running = true; state.stopping = false; state.waiting = false;
          setConnStatus("running");
        } else if (msg.status === "stopped") {
          state.running = false; state.stopping = false; state.waiting = false;
          state.pendingResume = null;
          resetPauseState();
          setConnStatus("online");
        } else if (msg.status === "waiting") {
          state.waiting = true;
          toast("等待日志文件产生...");
        }
        updateButtons();
      }
    };
    ws.onclose = () => {
      if (stale()) return;
      state.connected = false;
      state.waiting = false;
      // 正在读取/跟踪时断线：保留 pendingResume，重连成功后自动恢复；
      // 否则清空运行态，避免 UI 停在"读取中"。
      if (state.running || state.stopping) {
        if (state.currentFile && state.activeConfig) {
          state.pendingResume = { filePath: state.currentFile, config: state.activeConfig };
        }
      } else {
        state.pendingResume = null;
      }
      state.running = false;
      state.stopping = false;
      resetPauseState();
      setConnStatus("offline");
      updateButtons();
      // reconnecting=true 表示服务器主动要求重连（主机热更），抑制离线横幅，
      // 立即重连而不是走退避，避免用户看到闪断。
      if (state.reconnecting) {
        state.reconnecting = false;
        cancelReconnect();
        reconnectDelay = RECONNECT_BASE;
        connectWS();
        return;
      }
      // 从未 onopen 且启用认证：浏览器拿不到 WS 的 401 状态码（握手前被拒直接
      // close），此时探测一次 auth/status，区分"会话过期"与"服务暂不可达"。
      // 会话过期应弹登录框、停止重连风暴，而不是无限退避重连。
      if (!opened && authEnabled && !state.wsIntendedClose) {
        probeAuthThenReconnect(gen);
        return;
      }
      if (!state.wsIntendedClose) {
        const loginShown = $("loginMask") && $("loginMask").classList.contains("show");
        if (!loginShown) showDisconnectBanner();
      }
      scheduleReconnect();
    };
    ws.onerror = () => {
      if (stale()) return;
      if (term) term.write("\x1b[91m[连接错误] WebSocket 连接异常，正在尝试重连...\x1b[0m\r\n");
      ws.close();
    };
  }

  function wsSend(obj) {
    if (state.ws && state.ws.readyState === WebSocket.OPEN) {
      state.ws.send(JSON.stringify(obj));
      return true;
    }
    return false;
  }

  function startView() {
    const file = $("filePath").value.trim();
    if (!file) { toast("请先在左侧选择日志文件", "error"); return; }
    // 未连接时点开始：明确反馈并主动触发一次连接（不静默吞掉）。
    if (!state.connected) {
      toast("正在连接服务器，请稍后重试", "error");
      cancelReconnect();
      reconnectDelay = RECONNECT_BASE;
      connectWS();
      return;
    }
    // 修改过滤参数后重新开始：先停掉后台旧命令，清空控制台与缓冲，再用新配置启动
    if (state.running || state.stopping) wsSend({ action: "stop" });
    state.stopping = false;
    state.waiting = false;
    resetPauseState();
    resetTerminal();
    const cfg = readForm();
    state.currentFile = file;
    state.activeConfig = cfg;
    state.pendingResume = null;
    wsSend({ action: "start", filePath: file, config: cfg });
  }

  function stopView() {
    // 立即进入"停止中"视觉态（非阻塞），不等服务端 stopped 回执，
    // 避免远程/本地查杀进程的几百毫秒让用户觉得"点了没反应"。
    state.stopping = true;
    state.waiting = false;
    state.pendingResume = null;
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
  let exportAbort = null; // 当前导出的 AbortController，取消按钮用
  function fmtBytes(n) {
    if (n >= 1048576) return (n / 1048576).toFixed(1) + " MB";
    if (n >= 1024) return (n / 1024).toFixed(1) + " KB";
    return n + " B";
  }
  function showExport(title) {
    exporting = true;
    exportAbort = new AbortController();
    $("exportTitle").textContent = title;
    $("progressFill").style.width = "0%";
    $("exportPercent").textContent = "0%";
    $("exportStatus").textContent = "连接中...";
    $("exportMask").classList.add("show");
    $("exportRawBtn").disabled = true;
    $("exportFilterBtn").disabled = true;
  }
  function cancelExport() {
    if (exportAbort) {
      try { exportAbort.abort(); } catch (e) {}
    }
    $("exportStatus").textContent = "正在取消...";
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
    exportAbort = null;
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

  // 流式读取响应体，边下边显示进度（不依赖 Content-Length 也能显示已下载字节）。
  // signal 来自 exportAbort：取消时 reader.cancel() 会中断底层连接，
  // 服务端检测到客户端断开即 Kill 远端导出进程。
  async function streamDownload(response, signal) {
    const total = parseInt(response.headers.get("Content-Length") || "0", 10);
    const reader = response.body.getReader();
    if (signal) {
      signal.addEventListener("abort", () => {
        try { reader.cancel(); } catch (e) {}
      }, { once: true });
    }
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
      const r = authCheck(await fetch(hapi("/file/download/origin?path=" + encodeURIComponent(file)),
        { signal: exportAbort.signal }));
      if (!r.ok) {
        let msg = "导出失败 " + r.status;
        try { msg = (await r.json()).error || msg; } catch (e) {}
        throw new Error(msg);
      }
      const blob = await streamDownload(r, exportAbort.signal);
      saveBlob(r, blob);
      setTimeout(hideExport, 400);
      toast("原始日志已导出", "success");
    } catch (e) {
      if (e.name === "AbortError") { hideExport(); toast("已取消导出", ""); return; }
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
          signal: exportAbort.signal,
        }
      ));
      if (!r.ok) {
        let msg = "导出失败 " + r.status;
        try { msg = (await r.json()).error || msg; } catch (e) {}
        throw new Error(msg);
      }
      const blob = await streamDownload(r, exportAbort.signal);
      saveBlob(r, blob);
      setTimeout(hideExport, 400);
      toast("过滤日志已导出", "success");
    } catch (e) {
      if (e.name === "AbortError") { hideExport(); toast("已取消导出", ""); return; }
      hideExport();
      toast("导出失败: " + e.message, "error");
    }
  }

  // ============ 事件绑定 ============
  function bindEvents() {
    $("hostSelect").addEventListener("change", safeRun(async (e) => {
      await switchHost(e.target.value);
    }));
    $("refreshTreeBtn").addEventListener("click", safeRun(async () => {
      await refreshHosts();
      const root = $("rootSelect").value;
      if (root) await loadTreeDir(root, treeEl);
      toast("已刷新", "success");
    }));
    $("reloadCfgBtn").addEventListener("click", safeRun(reloadConfig));
    $("reconnectBtn").addEventListener("click", () => {
      cancelReconnect();
      reconnectDelay = RECONNECT_BASE;
      connectWS();
    });
    $("rootSelect").addEventListener("change", safeRun(async () => {
      const root = $("rootSelect").value;
      if (root) await loadTreeDir(root, treeEl);
    }));

    $("loadCfgBtn").addEventListener("click", safeRun(loadSelectedConfig));
    $("saveCfgBtn").addEventListener("click", safeRun(() => saveConfig($("cfgName").value)));
    $("saveAsCfgBtn").addEventListener("click", safeRun(() => {
      const name = prompt("另存为配置名称：");
      if (name) { $("cfgName").value = name; return saveConfig(name); }
    }));
    $("delCfgBtn").addEventListener("click", safeRun(async () => {
      const name = $("configSelect").value;
      if (!name) return;
      if (!confirm("删除配置「" + name + "」？")) return;
      await api(hapi("/config/delete"), {
        method: "POST", headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ name }),
      });
      await loadConfigList();
      toast("已删除", "success");
    }));
    $("setDefaultCfgBtn").addEventListener("click", safeRun(async () => {
      const name = $("configSelect").value;
      if (!name) return;
      await api(hapi("/config/setdefault"), {
        method: "POST", headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ name }),
      });
      toast("已设为默认", "success");
    }));
    $("renameCfgBtn").addEventListener("click", safeRun(async () => {
      const oldName = $("configSelect").value;
      if (!oldName) return;
      const newName = prompt("将配置「" + oldName + "」重命名为：", oldName);
      if (!newName || newName.trim() === "" || newName === oldName) return;
      await api(hapi("/config/rename"), {
        method: "POST", headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ old: oldName, new: newName.trim() }),
      });
      $("cfgName").value = newName.trim();
      await loadConfigList();
      toast("已重命名", "success");
    }));

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
    // 注意：initSplitter/拖拽会给 treePanel 写入内联 width，其优先级高于
    // 样式表的 .collapsed{width:0}，导致折叠无效。折叠时把内联 width 置 0，
    // 展开时恢复记忆宽度（或移除内联宽度回到 CSS 默认 260px）。
    let rememberedWidth = "";
    const setSidebar = (collapsed) => {
      const panel = $("treePanel");
      if (collapsed) {
        rememberedWidth = panel.style.width || rememberedWidth;
        panel.classList.add("collapsed");
        panel.style.width = "0px";
      } else {
        panel.classList.remove("collapsed");
        panel.style.width = rememberedWidth || "";
      }
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
      if (!state.paused) {
        if (state.pausedDropped > 0) {
          const approxLines = Math.round(state.pausedDropped / 120);
          if (term) term.write("\x1b[33m[提示] 暂停期间缓冲已达上限，丢弃了约 " + approxLines + " 行较早的日志\x1b[0m\r\n");
        }
        if (state.pausedBuffer.length) {
          const rules = readForm().HighlightRules;
          writeToTerminal(state.pausedBuffer.join(""), rules);
        }
        state.pausedBuffer = [];
        state.pausedBufferChars = 0;
        state.pausedDropped = 0;
      }
    });
    $("clearBtn").addEventListener("click", () => { resetTerminal(); });
    $("lineNumToggle").addEventListener("change", (e) => {
      setGutterVisible(e.target.checked);
    });
    $("copyBtn").addEventListener("click", safeRun(() => {
      const sel = term.getSelection();
      if (!sel) { toast("请先在终端中鼠标选中文本", "error"); return; }
      // navigator.clipboard 仅在安全上下文（HTTPS/localhost）下存在；
      // 非安全 HTTP 访问时为 undefined，直接调用会抛错。用 textarea 兜底。
      if (navigator.clipboard && navigator.clipboard.writeText) {
        return navigator.clipboard.writeText(sel).then(() => toast("已复制", "success"));
      }
      const ta = document.createElement("textarea");
      ta.value = sel;
      ta.style.position = "fixed";
      ta.style.opacity = "0";
      document.body.appendChild(ta);
      ta.select();
      try { document.execCommand("copy"); toast("已复制", "success"); }
      finally { ta.remove(); }
    }));
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
    $("exportCancelBtn").addEventListener("click", cancelExport);

    initTerminalSearch();

    // 登录表单
    $("loginForm").addEventListener("submit", onLoginSubmit);
    // 登出
    $("logoutBtn").addEventListener("click", onLogout);

    initSplitter();
  }

  // ============ 终端内搜索 ============
  function initTerminalSearch() {
    const bar = $("searchBar");
    const input = $("searchInput");
    const caseBtn = $("searchCase");
    const count = $("searchCount");
    if (!bar || !input || !searchAddon) return;

    let lastQuery = "";

    function opts() {
      return {
        caseSensitive: caseBtn.checked,
        // 整词/正则暂不暴露；decorations 让所有匹配带高亮
        decorations: {
          matchBackground: "#5a4a00",
          matchBorder: "#ffcc00",
          matchOverviewRuler: "#ffcc00",
          activeMatchBackground: "#8a6d00",
          activeMatchBorder: "#ffd84d",
          activeMatchColor: "#ffffff",
        },
      };
    }

    function showFound(found) {
      count.textContent = found ? "" : "无匹配";
      count.className = "search-count" + (found ? "" : " none");
    }

    function doFind(forward) {
      const q = input.value;
      if (!q) { count.textContent = ""; return; }
      // 查询变化时从头开始，避免从旧光标位置漏掉上方匹配
      if (q !== lastQuery) {
        searchAddon.clearDecorations?.();
        term.clearSelection();
        lastQuery = q;
      }
      let found = false;
      try {
        found = forward ? searchAddon.findNext(q, opts()) : searchAddon.findPrevious(q, opts());
      } catch (e) {
        // 非法正则（若启用 regex）等
        found = false;
      }
      showFound(found);
    }

    function open() {
      bar.style.display = "flex";
      const sel = term && term.getSelection();
      if (sel && sel.indexOf("\n") === -1) input.value = sel;
      input.focus();
      input.select();
      lastQuery = "";
      count.textContent = "";
    }

    function close() {
      bar.style.display = "none";
      searchAddon.clearDecorations?.();
      if (term) term.focus();
    }

    input.addEventListener("keydown", (e) => {
      if (e.key === "Enter") { e.preventDefault(); doFind(!e.shiftKey); }
      else if (e.key === "Escape") { e.preventDefault(); close(); }
    });
    // 输入变化时重置起点状态
    input.addEventListener("input", () => { lastQuery = ""; count.textContent = ""; });
    caseBtn.addEventListener("change", () => { lastQuery = ""; doFind(true); });

    $("searchNext").addEventListener("click", () => doFind(true));
    $("searchPrev").addEventListener("click", () => doFind(false));
    $("searchClose").addEventListener("click", close);

    // 全局 Ctrl+F / Cmd+F 唤起搜索
    document.addEventListener("keydown", (e) => {
      if ((e.ctrlKey || e.metaKey) && (e.key === "f" || e.key === "F")) {
        e.preventDefault();
        open();
      } else if (e.key === "Escape" && bar.style.display !== "none" && document.activeElement === input) {
        close();
      }
    });
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

    // 启动机器列表自动刷新（10 秒一次，标签页隐藏时跳过）
    if (!hostsTimer) {
      hostsTimer = setInterval(refreshHosts, 10000);
      document.addEventListener("visibilitychange", () => {
        if (!document.hidden) refreshHosts();
      });
    }
  }

  init();
})();