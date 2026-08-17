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
    sessionID: "",        // 当前 follow 会话 ID（服务端断线宽限补齐用）
    lastSeq: 0,           // 最近一帧日志的序号（attach 时据此补发缺口）
  };

  // 统一复位暂停相关状态：切换文件/主机、断线、停止、开始时都必须调用，
  // 否则暂停缓冲里的旧日志会在新视图继续输出、"继续"按钮状态也会错乱。
  function resetPauseState() {
    state.paused = false;
    state.pausedBuffer = [];
    state.pausedBufferChars = 0;
    state.pausedDropped = 0;
    const pb = $("pauseBtn");
    if (pb) pb.textContent = t("pause");
  }

  // 复位一次查看任务的运行态（停止/切换文件/切换主机/断线本地复位时调用）。
  // 集中清理 running/stopping/waiting、follow 会话标识与暂停状态，避免各处
  // 漏清 sessionID/lastSeq 导致重连时错误 attach 到上一个会话。
  function resetRunState() {
    state.running = false;
    state.stopping = false;
    state.waiting = false;
    state.pendingResume = null;
    state.sessionID = "";
    state.lastSeq = 0;
    resetPauseState();
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

  // ============ 偏好设置（localStorage 持久化）============
  const PREFS_KEY = "logviewer-prefs";
  const DEFAULT_PREFS = {
    themeMode: "auto", // auto | light | dark
    lang: "",          // "" 表示按浏览器语言自动判定
    density: "comfortable", // compact | comfortable
    fontSize: 13,
    lineHeight: 1.2,
    scrollback: 10000,
  };

  function loadPrefs() {
    let p = {};
    try { p = JSON.parse(localStorage.getItem(PREFS_KEY) || "{}") || {}; } catch (e) { p = {}; }
    // 兼容旧键 logviewer-theme（明暗写死），无则默认 auto。
    if (!p.themeMode) {
      try {
        const oldTheme = localStorage.getItem("logviewer-theme");
        if (oldTheme === "light" || oldTheme === "dark") p.themeMode = oldTheme;
      } catch (e) {}
    }
    return Object.assign({}, DEFAULT_PREFS, p);
  }
  function savePrefs(patch) {
    const cur = loadPrefs();
    const next = Object.assign(cur, patch);
    try { localStorage.setItem(PREFS_KEY, JSON.stringify(next)); } catch (e) {}
    return next;
  }
  let prefs = loadPrefs();

  // ============ 国际化 ============
  function detectLang() {
    if (prefs.lang === "zh" || prefs.lang === "en") return prefs.lang;
    const nav = (navigator.language || "zh").toLowerCase();
    return nav.startsWith("zh") ? "zh" : "en";
  }
  function t(key, params) { return window.I18N.t(key, params); }
  function applyI18n() {
    window.I18N.setLang(prefs.lang || detectLang());
    document.documentElement.lang = window.I18N.lang === "en" ? "en" : "zh-CN";
    document.querySelectorAll("[data-i18n]").forEach((el) => {
      el.textContent = t(el.getAttribute("data-i18n"));
    });
    document.querySelectorAll("[data-i18n-placeholder]").forEach((el) => {
      el.placeholder = t(el.getAttribute("data-i18n-placeholder"));
    });
    document.querySelectorAll("[data-i18n-title]").forEach((el) => {
      el.title = t(el.getAttribute("data-i18n-title"));
    });
    const langSel = $("langSelect");
    if (langSel) langSel.value = window.I18N.lang;
  }

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
      throw new Error(t("toastSessionExpired"));
    }
    const ct = r.headers.get("content-type") || "";
    const data = ct.includes("json") ? await r.json().catch(() => ({})) : {};
    if (!r.ok) throw new Error(data.error || (t("requestFailed") + r.status));
    return data;
  }

  // 当前选中的机器别名。阶段一固定 local；阶段二接入顶栏切换器后可变。
  let currentHost = "local";
  // 业务 API 都在 /api/h/:host 前缀下（目录/配置/导出）。
  function hapi(path) { return "/api/h/" + encodeURIComponent(currentHost) + path; }

  function setConnStatus(mode) {
    const el = $("connStatus");
    el.className = "status " + mode;
    const labels = {
      online: t("connOnline"), offline: t("connOffline"),
      running: t("connRunning"), stopping: t("connStopping"),
    };
    $("connText").textContent = labels[mode] || mode;
  }

  // ============ 主题 ============
  const XTERM_THEMES = {
    dark: { background: "#1e1e1e", foreground: "#d4d4d4", cursor: "#d4d4d4", selectionBackground: "#264f78" },
    light: { background: "#ffffff", foreground: "#1f2328", cursor: "#1f2328", selectionBackground: "#cfe4ff" },
  };
  const mql = window.matchMedia ? window.matchMedia("(prefers-color-scheme: light)") : null;
  function systemTheme() { return mql && mql.matches ? "light" : "dark"; }
  function currentTheme() {
    return document.documentElement.getAttribute("data-theme") === "light" ? "light" : "dark";
  }
  // 解析 themeMode（auto/light/dark）到实际明暗。
  function resolveTheme(mode) {
    if (mode === "light" || mode === "dark") return mode;
    return systemTheme();
  }
  function setThemeMode(mode) {
    prefs = savePrefs({ themeMode: mode });
    applyTheme();
    updateThemeSeg();
  }
  function applyTheme() {
    const theme = resolveTheme(prefs.themeMode);
    document.documentElement.setAttribute("data-theme", theme);
    const btn = $("themeToggle");
    if (btn) {
      const icon = theme === "light" ? "☀" : "🌙";
      const label = prefs.themeMode === "auto" ? " " + t("settingThemeAuto") : "";
      btn.textContent = icon + label;
    }
    const xt = XTERM_THEMES[theme];
    if (term) { try { term.options.theme = xt; } catch (e) {} }
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
  }
  // 监听系统主题变化：仅 auto 模式下跟随。
  if (mql) {
    const onSys = () => { if (prefs.themeMode === "auto") applyTheme(); };
    if (mql.addEventListener) mql.addEventListener("change", onSys);
    else if (mql.addListener) mql.addListener(onSys);
  }

  // ============ Xterm ============
  // 字号/回溯行数均来自 prefs（可在设置里调整）；行高由密度决定。
  function fontSize() { return prefs.fontSize || 13; }
  function lineHeight() { return prefs.lineHeight || (prefs.density === "compact" ? 1.0 : 1.2); }
  function scrollback() { return prefs.scrollback || 10000; }
  const GUTTER_COLS = 7; // 行号栏宽度（字符列）
  let term = null;
  let gutter = null;
  let fitAddon = null;
  let searchAddon = null;
  let searchOpen = null; // initTerminalSearch 暴露的打开搜索函数（供全局快捷键调用）
  let searchResultLimit = 1000; // 与 searchAddon 的 highlightLimit 对齐（构造时设置）
  // 镜像游标：mirrorEndAbs 是"已镜像到的主终端绝对行号"（baseY+length 坐标系）。
  // 不能用单调递增的"已镜像行数"去和 buf.length 比——缓冲区超过 scrollback 后
  // 旧行被淘汰、buf.length 封顶，该计数会永久大于 length，导致行号栏彻底冻结。
  // baseY 会随淘汰前移，用它计算新增行数可正确跨越封顶。
  let mirrorEndAbs = 0;
  let logicalNo = 0;          // 逻辑行号（仅非续行递增）

  function makeTerm(opts) {
    return new Terminal(Object.assign({
      fontSize: fontSize(),
      lineHeight: lineHeight(),
      scrollback: scrollback(),
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

  // 应用字号/行高（密度）到两个终端。字号变化会改变折行点，fit 后重建行号栏。
  function applyTerminalPrefs() {
    const fs = fontSize(), lh = lineHeight();
    if (term) {
      try { term.options.fontSize = fs; term.options.lineHeight = lh; } catch (e) {}
    }
    if (gutter) {
      try { gutter.options.fontSize = fs; gutter.options.lineHeight = lh; } catch (e) {}
    }
    document.documentElement.style.setProperty("--term-font-size", fs + "px");
    requestAnimationFrame(() => fitTerm());
  }

  try {
    term = makeTerm({ cursorBlink: true, theme: XTERM_THEMES.dark });
    fitAddon = new FitAddon.FitAddon();
    term.loadAddon(fitAddon);
    term.open($("terminal"));

    // 终端内搜索（xterm-addon-search）。UMD 挂在 window.SearchAddon，类为 .SearchAddon。
    // highlightLimit：装饰高亮（及 onDidChangeResults 的 resultCount）最多计算多少个匹配。
    // 超出后计数显示为 "N+"，避免超长缓冲区里全量装饰拖慢渲染。
    const SEARCH_HIGHLIGHT_LIMIT = 5000;
    if (window.SearchAddon && window.SearchAddon.SearchAddon) {
      searchAddon = new window.SearchAddon.SearchAddon({ highlightLimit: SEARCH_HIGHLIGHT_LIMIT });
      term.loadAddon(searchAddon);
      searchResultLimit = SEARCH_HIGHLIGHT_LIMIT;
    }

    // 关键：把浏览器/UI 快捷键从 xterm 的按键处理中“放行”。
    //
    // 根因（已从 vendor/xterm.js 源码确认，非猜测）：xterm 在【捕获阶段】监听其
    // 隐藏 textarea 的 keydown。对 Ctrl+F、Ctrl+C 这类组合，evaluateKeyboardEvent
    // 会产生 cancel=true 的终端输入键，_keyDown 随即调用 cancel(e,true) ->
    // stopPropagation()+preventDefault()：
    //   - Ctrl+F 的事件在冒泡到我们的 document 监听前被杀死，所以日志区按 Ctrl+F
    //     无法唤起搜索；
    //   - Ctrl+C 被解析为 ETX 并 preventDefault，取消了浏览器原生 copy 命令，
    //     xterm 自己注册的 copy 事件处理器（负责写入 selectionText）因此不触发，
    //     表现为“选中后 Ctrl+C 复制不了”。
    //
    // attachCustomKeyEventHandler 返回 false 会让 xterm 在执行任何 stopPropagation/
    // preventDefault 之前提前退出，事件得以正常冒泡给全局处理器 / 浏览器原生行为。
    term.attachCustomKeyEventHandler((e) => {
      if (e.type !== "keydown") return true;
      const mod = e.ctrlKey || e.metaKey;
      if (mod) {
        const key = (e.key || "").toLowerCase();
        // Ctrl/Cmd+F：交给全局搜索
        if (key === "f") return false;
        // Ctrl/Cmd+C：有选区时交给浏览器原生复制（触发 xterm copyHandler 写选区）
        if (key === "c" && term && term.hasSelection && term.hasSelection()) return false;
        // Ctrl/Cmd+Shift+P：命令面板
        if (key === "p" && e.shiftKey) return false;
        return true;
      }
      // 无修饰键的全局快捷键（g/G/PgUp/PgDn/t/s//?等）：xterm 在【捕获阶段】对这些
      // 键执行 cancel(stopPropagation+preventDefault)，会在事件冒泡到 document 的
      // initShortcuts 之前截停它们。返回 false 让 xterm 提前退出、不取消事件，
      // 从而冒泡给全局处理器。仅在"非输入"上下文生效（终端只读，其隐藏 textarea
      // 不算 typing；真实输入框聚焦时不会走到这个 handler）。
      if (isBareShortcutKey(e)) return false;
      return true;
    });

    // 行号栏：只读、无光标、不响应滚轮（由主终端滚动同步驱动）
    gutter = makeTerm({
      cursorBlink: false,
      cursorStyle: "bar",
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

  // 高亮：把一行内命中关键词的片段用 ANSI 包裹。
  // useRegex/caseSensitive 由调用方随本次查看会话的配置快照传入，不能逐行读实时
  // DOM——日志持续输出期间用户切换复选框会让同屏各行按不同规则高亮，出现错配。
  function colorizeLine(line, rules, useRegex, caseSensitive) {
    if (!rules || rules.length === 0) return line;
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

  // 取本次查看会话的高亮上下文（规则 + 正则/大小写标志）。
  // follow 运行中用 activeConfig 快照：用户改表单不影响正在输出的日志；
  // 静态/尚未启动时回退到当前表单。逐行读实时 DOM 会导致同屏高亮错配。
  function highlightContext() {
    const cfg = state.activeConfig || readForm();
    return {
      rules: cfg.HighlightRules,
      useRegex: cfg.UseRegex,
      caseSensitive: cfg.CaseSensitive,
    };
  }

  function writeToTerminal(text, highlightRules, useRegex, caseSensitive) {
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
      if (clean !== "") out += colorizeLine(clean, highlightRules, useRegex, caseSensitive);
      if (!isLast) out += "\r\n";
    }
    term.write(out, () => syncGutter());
  }

  // ============ 目录树 ============
  const treeEl = $("tree");

  // 主机在顶栏下拉中的显示文本。
  // displayName 优先；本机（h.local）在显示名后追加 "-local" 后缀以区分远程主机。
  function hostDisplayLabel(h) {
    let label = h.displayName || h.name;
    if (h.local && label !== "local" && !label.endsWith("-local")) {
      label += "-local";
    }
    const plat = h.platform || t("unknown");
    const status = h.online ? "" : t("offline");
    return label + " [" + plat + "]" + status;
  }

  // 机器切换：拉取 /api/hosts 填充顶栏选择器。
  async function loadHosts() {
    const data = await api("/api/hosts");
    const sel = $("hostSelect");
    const prevHost = currentHost;
    sel.innerHTML = "";
    (data.hosts || []).forEach((h) => {
      const opt = document.createElement("option");
      opt.value = h.name;
      opt.textContent = hostDisplayLabel(h);
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
        const text = hostDisplayLabel(h);
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
      toast(t("toastConfigReloaded", { n: (data.hosts || []).length }), "success");
    } catch (e) {
      toast(t("toastReloadFailed", { msg: e.message }), "error");
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
      opt.textContent = currentCaps.hasIconv ? label : label + t("encodingMissingIconv");
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
      // 清空时间后同步刷新预览，否则预览区仍显示已被清除的旧时间范围，
      // 与实际禁用的时间控件状态不一致。
      refreshPreview();
    }
  }

  // 切换机器：停掉当前 WS、清空目录树与终端、按新 host 重新加载。
  // hostSwitchGen 是代次令牌：用户快速连切两台主机时，第一次 switchHost 的
  // 异步加载（loadCapabilities/loadRoots/loadConfigList）可能在第二次之后才
  // 返回，若<[PLHD79_never_used_51bce0c785ca2f68081bfa7d91973934]>fillForm 旧主机配置覆盖新主机表单。每次进入自增 gen，
  // 每个 await 之后校验 gen 是否仍为本次——若已被新切换取代则直接放弃后续操作。
  let hostSwitchGen = 0;
  async function switchHost(name) {
    if (name === currentHost) return;
    const gen = ++hostSwitchGen;
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
    resetRunState();
    state.currentFile = "";
    state.activeConfig = null;
    setConnStatus("offline");
    treeEl.innerHTML = "";
    $("filePath").value = "";
    resetTerminal();
    try {
      await loadCapabilities();
      if (gen !== hostSwitchGen) return; // 用户在加载期间又切了主机
      await loadRoots();
      if (gen !== hostSwitchGen) return;
      await loadConfigList();
      if (gen !== hostSwitchGen) return;
      const def = await api(hapi("/config/list"));
      if (gen !== hostSwitchGen) return;
      if (def.default) {
        const cfg = await api(hapi("/config/get?name=" + encodeURIComponent(def.default)));
        if (gen !== hostSwitchGen) return;
        fillForm(cfg);
      }
      // fillForm 可能选中 gbk，需在加载默认配置后再按能力回退禁用
      applyCapabilities();
    } catch (e) {
      if (gen !== hostSwitchGen) return;
      const loginShown = $("loginMask") && $("loginMask").classList.contains("show");
      if (!loginShown) toast(t("toastSwitchFailed", { msg: e.message }), "error");
    } finally {
      if (gen !== hostSwitchGen) return;
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
      const verbKey = state.stopping ? "verbStopping" : (follow ? "verbFollow" : "verbStatic");
      if (!confirm(t("confirmSwitchFile", { verb: t(verbKey), name }))) {
        return;
      }
      wsSend({ action: "stop" });
      resetRunState();
      resetTerminal();
      setConnStatus("online");
      updateButtons();
      toast(t("toastStoppedTask"), "success");
    }
    if (state.selectedNode) state.selectedNode.classList.remove("selected");
    item.classList.add("selected");
    state.selectedNode = item;
    state.currentFile = path;
    $("filePath").value = path;
    toast(t("toastFileSelected", { name }), "success");
    // 移动端选完文件自动收起全屏侧栏。
    try { document.dispatchEvent(new Event("fileSelected")); } catch (e) {}
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
        const timePart = data.timeRange ? t("previewTime") + data.timeRange : "";
        const rePart = data.pattern ? t("previewRegex") + data.pattern : "";
        el.textContent = [timePart, rePart].filter(Boolean).join("   |   ") || t("previewEmpty");
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
        $("regexPreview").textContent = t("previewFailed") + e.message;
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
      stop.textContent = t("stopping");
      stop.classList.add("loading");
      return;
    }
    stop.classList.remove("loading");
    if (state.running) {
      start.disabled = true;
      stop.disabled = false;
      stop.textContent = follow ? t("stopFollow") : t("stopView");
    } else if (state.waiting) {
      // 等待目标文件产生：禁用"开始"避免重复提交，仍允许停止。
      start.disabled = true;
      stop.disabled = false;
      stop.textContent = t("cancelWait");
    } else {
      start.disabled = !state.connected;
      stop.disabled = true;
      start.textContent = follow ? t("startFollow") : t("startView");
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
    toast(t("toastConfigLoaded", { name }), "success");
  }

  async function saveConfig(forceName) {
    const cfg = readForm();
    if (!cfg.ConfigName) {
      const propose = forceName || cfg.ConfigName || prompt(t("promptConfigName"));
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
    toast(t("toastConfigSaved", { name: cfg.ConfigName }), "success");
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
        toast(t("connRecovered"), "success");
        // 优先 attach 到服务端仍存活的 follow 会话（断线宽限期内），按 lastSeq
        // 补发缺口日志；若会话已失效（attachSession 内部），由服务端回退到全新 start。
        if (state.sessionID) {
          wsSend({
            action: "attach",
            sessionID: state.sessionID,
            lastSeq: state.lastSeq || 0,
            filePath: resume.filePath,
            config: resume.config,
          });
        } else {
          wsSend({ action: "start", filePath: resume.filePath, config: resume.config });
        }
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
        // 记录服务端序号，断线重连时据此请求缺口补发。
        if (typeof msg.seq === "number") state.lastSeq = msg.seq;
        {
          const hc = highlightContext();
          writeToTerminal(msg.data, hc.rules, hc.useRegex, hc.caseSensitive);
        }
        scheduleFab();
      } else if (msg.type === "reconnect") {
        // 服务器通知主机配置已热更：关闭当前连接，由 onclose 走重连流程
        // 绑定到新实例。reconnecting 标记抑制离线横幅，pendingResume 由
        // onclose 按运行状态保留，重连后自动恢复日志跟踪。
        state.reconnecting = true;
        try { state.ws.close(); } catch (e) {}
      } else if (msg.type === "error") {
        if (term) term.write("\x1b[91m" + t("termError") + msg.msg + "\x1b[0m\r\n");
        setConnStatus("online");
      } else if (msg.type === "notice") {
        // 日志轮转/截断/断线缺口等非错误事件：终端内写一行提示，同时显示可关闭通知条。
        let key;
        if (msg.kind === "truncate") key = "noticeTruncate";
        else if (msg.kind === "gap") key = "noticeGap";
        else key = "noticeRotate";
        const text = t(key);
        if (term) term.write("\x1b[33m" + text + "\x1b[0m\r\n");
        showNotice(text);
      } else if (msg.type === "status") {
        if (msg.status === "running") {
          state.running = true; state.stopping = false; state.waiting = false;
          // follow 会话返回 sessionID：断线重连据此 attach 补发缺口。
          if (msg.sessionID) {
            state.sessionID = msg.sessionID;
            if (typeof msg.seq === "number") state.lastSeq = msg.seq;
          }
          setConnStatus("running");
        } else if (msg.status === "stopped") {
          state.running = false; state.stopping = false; state.waiting = false;
          state.pendingResume = null;
          state.sessionID = "";
          state.lastSeq = 0;
          resetPauseState();
          setConnStatus("online");
        } else if (msg.status === "waiting") {
          state.waiting = true;
          toast(t("toastWaiting"));
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
      if (term) term.write("\x1b[91m" + t("wsReconnecting") + "\x1b[0m\r\n");
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
    if (!file) { toast(t("toastChooseFileLeft"), "error"); return; }
    // 未连接时点开始：明确反馈并主动触发一次连接（不静默吞掉）。
    if (!state.connected) {
      toast(t("toastConnecting"), "error");
      cancelReconnect();
      reconnectDelay = RECONNECT_BASE;
      connectWS();
      return;
    }
    // 修改过滤参数后重新开始：先停掉后台旧命令，清空控制台与缓冲，再用新配置启动。
    // 清掉上一代 follow 会话标识：start 会拿到全新 sessionID，旧 ID/序号残留
    // 可能让重连逻辑误 attach 到已销毁会话。
    if (state.running || state.stopping) wsSend({ action: "stop" });
    state.stopping = false;
    state.waiting = false;
    state.sessionID = "";
    state.lastSeq = 0;
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
    if (!wsSend({ action: "stop" })) {
      // 连接已断开：服务端无从收到 stop，本连接也没有在管进程，直接本地复位，
      // 否则界面会永久卡在"停止中"。断线期间的 follow 会话由服务端宽限期管理，
      // pendingResume 已置空，重连后不会自动恢复。
      resetRunState();
      setConnStatus("online");
      updateButtons();
    }
  }

  // ============ 导出 ============
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
    $("exportStatus").textContent = t("exportConnecting");
    $("exportMask").classList.add("show");
    $("exportRawBtn").disabled = true;
    $("exportFilterBtn").disabled = true;
  }
  function cancelExport() {
    if (exportAbort) {
      try { exportAbort.abort(); } catch (e) {}
    }
    $("exportStatus").textContent = t("exportCanceling");
  }
  function setExportProgress(received, total, done) {
    let pct;
    if (total && total > 0) {
      pct = Math.min(100, Math.round((received / total) * 100));
      $("exportStatus").textContent = done ? t("exportDone") : (fmtBytes(received) + " / " + fmtBytes(total));
    } else {
      // 无 Content-Length（过滤导出通常如此）：不确定进度，用已接收字节做提示
      pct = done ? 100 : 0;
      $("exportStatus").textContent = done ? t("exportDone") : (t("exportGenerating") + fmtBytes(received) + " ...");
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
      throw new Error(t("toastSessionExpired"));
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
    // 优先取 RFC 5987 的 filename*=UTF-8''<percent-encoded>（含中文等非 ASCII 文件名）；
    // 缺失时回退到 ASCII filename="..."。服务端两者都会发，但顺序/存在性不可假设。
    const cd = response.headers.get("Content-Disposition") || "";
    let name = "";
    const star = /filename\*=UTF-8''([^;]+)/i.exec(cd);
    if (star && star[1]) {
      name = decodeURIComponent(star[1].trim().replace(/^"|"$/g, ""));
    } else {
      const m = /filename="?([^";]+)"?/i.exec(cd);
      if (m) name = decodeURIComponent(m[1].trim());
    }
    if (name) a.download = name;
    document.body.appendChild(a);
    a.click();
    a.remove();
    setTimeout(() => URL.revokeObjectURL(url), 1000);
  }

  // 导出原始：GET 流式下载 + 进度（原始文件有 Content-Length，可显示真实进度）
  async function triggerRawDownload(file) {
    if (exporting) return;
    showExport(t("exportingRaw"));
    try {
      const r = authCheck(await fetch(hapi("/file/download/origin?path=" + encodeURIComponent(file)),
        { signal: exportAbort.signal }));
      if (!r.ok) {
        let msg = t("exportFailedCode") + r.status;
        try { msg = (await r.json()).error || msg; } catch (e) {}
        throw new Error(msg);
      }
      const blob = await streamDownload(r, exportAbort.signal);
      saveBlob(r, blob);
      setTimeout(hideExport, 400);
      toast(t("exportRawDone"), "success");
    } catch (e) {
      if (e.name === "AbortError") { hideExport(); toast(t("exportCanceled"), ""); return; }
      hideExport();
      toast(t("exportFailed", { msg: e.message }), "error");
    }
  }

  // 过滤导出：POST 当前表单配置，流式下载 + 字节进度。
  // 处理中禁用按钮，完成后恢复。
  async function triggerFilteredDownload(file) {
    if (exporting) return;
    showExport(t("exportingFilter"));
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
        let msg = t("exportFailedCode") + r.status;
        try { msg = (await r.json()).error || msg; } catch (e) {}
        throw new Error(msg);
      }
      const blob = await streamDownload(r, exportAbort.signal);
      saveBlob(r, blob);
      setTimeout(hideExport, 400);
      toast(t("exportFilterDone"), "success");
    } catch (e) {
      if (e.name === "AbortError") { hideExport(); toast(t("exportCanceled"), ""); return; }
      hideExport();
      toast(t("exportFailed", { msg: e.message }), "error");
    }
  }

  // ============ 悬浮滚动按钮 / 通知条 ============
  // FAB 显隐由主终端滚动位置驱动：顶部隐藏"到顶部"，底部隐藏"到底部"，
  // 中间两者都显示（符合用户预期，且"到底部"在跟踪态上滚时同时充当"继续跟踪"）。
  let fabRaf = 0;
  function updateFab() {
    const fab = $("scrollFab");
    if (!fab || !term) return;
    const buf = term.buffer.active;
    const top = buf.viewportY;
    const bottom = buf.baseY + buf.length - term.rows;
    const showTop = top > 0;
    const showBottom = top < bottom - 1;
    if (!showTop && !showBottom) { fab.style.display = "none"; return; }
    fab.style.display = "flex";
    $("fabTop").style.display = showTop ? "" : "none";
    $("fabBottom").style.display = showBottom ? "" : "none";
  }
  function scheduleFab() {
    if (fabRaf) return;
    fabRaf = requestAnimationFrame(() => { fabRaf = 0; updateFab(); });
  }
  function scrollTop() { if (term) { term.scrollToTop(); scheduleFab(); } }
  function scrollBottom() { if (term) { term.scrollToBottom(); scheduleFab(); } }

  // 通知条：日志轮转/截断等非错误事件，显示在终端工具栏上方，可手动关闭。
  function showNotice(text) {
    const bar = $("noticeBar");
    if (!bar) return;
    $("noticeText").textContent = text;
    bar.style.display = "flex";
  }
  function hideNotice() {
    const bar = $("noticeBar");
    if (bar) bar.style.display = "none";
  }

  // ============ 焦点判定与全局快捷键 ============
  // 输入框/文本域/下拉/contentEditable 内按键时保留浏览器原生行为（输入、复制、
  // 查找等），不触发全局单键快捷键；仅 Ctrl/Cmd 组合与 Esc 放行给对应处理器。
  function isTyping() {
    const el = document.activeElement;
    if (!el) return false;
    const tag = el.tagName;
    if (tag === "INPUT" || tag === "SELECT") return true;
    if (tag === "TEXTAREA") {
      // xterm 的隐藏 textarea（.xterm-helper-textarea）在终端聚焦时持有焦点，
      // 但它是 disableStdin 的只读辅助元素，不接收真实文本输入。不能把它当作
      // "正在输入"，否则 g/G/PgUp/PgDn 等快捷键会被守卫误拦、全部失效。
      if (el.classList && el.classList.contains("xterm-helper-textarea")) return false;
      return true;
    }
    if (el.isContentEditable) return true;
    return false;
  }
  function openSearch() {
    // 复用 initTerminalSearch 暴露的 open()（挂在模块级 searchOpen）。
    if (typeof searchOpen === "function") searchOpen();
  }
  function toggleFollowMode() {
    const sel = $("followTail");
    sel.value = sel.value === "true" ? "false" : "true";
    sel.dispatchEvent(new Event("change"));
  }
  function toggleStartStop() {
    if (state.running || state.stopping) { stopView(); }
    else { startView(); }
    updateButtons();
  }
  function cycleTheme() {
    // 三态循环：auto -> light -> dark -> auto。按钮点击的便捷入口。
    const order = ["auto", "light", "dark"];
    const next = order[(order.indexOf(prefs.themeMode) + 1) % order.length];
    setThemeMode(next);
    const label = next === "auto" ? t("settingThemeAuto")
      : next === "light" ? t("settingThemeLight") : t("settingThemeDark");
    toast(label);
  }
  // isBareShortcutKey 判断一个无 Ctrl/Cmd/Meta 修饰的 keydown 是否属于全局快捷键。
  // 供 xterm 的 attachCustomKeyEventHandler 使用：仅对这些键返回 false 放行，
  // 其余键（方向键、Home/End 等）仍交 xterm 处理，避免越权篡改终端行为。
  function isBareShortcutKey(e) {
    if (e.ctrlKey || e.metaKey || e.altKey) return false;
    switch (e.key) {
      case "/": case "?":
      case "t": case "T":
      case "s": case "S":
      case "g": case "G":
      case "PageUp": case "PageDown":
      case "[": case "]":
        return true;
      default:
        return false;
    }
  }

  function initShortcuts() {
    document.addEventListener("keydown", (e) => {
      // Ctrl/Cmd 组合：除 Ctrl+Shift+P（命令面板）与 Ctrl+F（搜索）外，一律交给
      // 浏览器/各控件原生处理，避免吞掉复制、粘贴、全选、刷新等。
      const mod = e.ctrlKey || e.metaKey;
      if (mod) {
        const key = (e.key || "").toLowerCase();
        if (key === "f") { e.preventDefault(); openSearch(); return; }
        if (key === "p" && e.shiftKey) { e.preventDefault(); openPalette(); return; }
        return;
      }
      // Esc 关闭最上层浮层（任何焦点下都生效）。
      if (e.key === "Escape") {
        if (closeTopOverlay()) return;
        return;
      }
      if (isTyping()) return;

      let handled = true;
      switch (e.key) {
        case "/": openSearch(); break;
        case "t": case "T": toggleFollowMode(); break;
        case "s": case "S": toggleStartStop(); break;
        case "g": scrollTop(); break;
        case "G": scrollBottom(); break;
        case "PageUp": if (term) term.scrollPages(-1); scheduleFab(); break;
        case "PageDown": if (term) term.scrollPages(1); scheduleFab(); break;
        case "?": openOverlay("shortcutsHelp"); break;
        case "[": case "]": /* 标签页功能预留 */ break;
        default: handled = false;
      }
      if (handled) e.preventDefault();
    });
  }

  // ============ 浮层管理 ============
  const OVERLAY_IDS = ["shortcutsHelp", "commandPalette", "settingsDrawer"];
  function isOverlayOpen(id) {
    const el = $(id);
    return !!(el && el.classList.contains("show"));
  }
  function openOverlay(id) {
    // 同一时刻只显示一个浮层；打开命令面板/帮助前关闭其它。
    OVERLAY_IDS.forEach((x) => { if (x !== id) $(x).classList.remove("show"); });
    $(id).classList.add("show");
  }
  function closeOverlay(id) { $(id).classList.remove("show"); }
  function closeTopOverlay() {
    for (const id of OVERLAY_IDS) {
      if (isOverlayOpen(id)) { closeOverlay(id); return true; }
    }
    return false;
  }
  function bindOverlayClose(id) {
    const mask = $(id);
    const closeBtn = mask.querySelector(".overlay-close");
    if (closeBtn) closeBtn.addEventListener("click", () => closeOverlay(id));
    // 点击遮罩空白处关闭
    mask.addEventListener("mousedown", (e) => {
      if (e.target === mask) closeOverlay(id);
    });
  }

  // ============ 命令面板 ============
  // 命令注册表：title 用于展示与模糊匹配，run 执行动作。
  let commands = [];
  let paletteIndex = 0;
  let paletteFiltered = [];
  function fuzzyScore(query, text) {
    // 子序列匹配：query 的每个字符按序出现在 text 中得分；连续/开头命中加权。
    if (!query) return 1;
    const q = query.toLowerCase();
    const s = text.toLowerCase();
    let qi = 0, score = 0, streak = 0, prevIdx = -1;
    for (let i = 0; i < s.length && qi < q.length; i++) {
      if (s[i] === q[qi]) {
        streak = (prevIdx === i - 1) ? streak + 1 : 1;
        score += 1 + streak;
        if (i === 0) score += 2;
        prevIdx = i;
        qi++;
      }
    }
    return qi === q.length ? score : 0;
  }
  function renderPalette() {
    const list = $("paletteList");
    const q = $("paletteInput").value.trim();
    paletteFiltered = commands
      .map((c) => ({ c, score: fuzzyScore(q, c.title) }))
      .filter((x) => x.score > 0)
      .sort((a, b) => b.score - a.score)
      .map((x) => x.c);
    if (paletteIndex >= paletteFiltered.length) paletteIndex = 0;
    list.innerHTML = "";
    if (!paletteFiltered.length) {
      const li = document.createElement("li");
      li.className = "palette-empty";
      li.textContent = t("paletteEmpty");
      list.appendChild(li);
      return;
    }
    paletteFiltered.forEach((c, i) => {
      const li = document.createElement("li");
      li.textContent = c.title;
      if (i === paletteIndex) li.className = "active";
      li.addEventListener("mousedown", (e) => { e.preventDefault(); paletteIndex = i; runPalette(); });
      li.addEventListener("mouseenter", () => {
        paletteIndex = i;
        list.querySelectorAll("li").forEach((el, j) => el.classList.toggle("active", j === i));
      });
      list.appendChild(li);
    });
  }
  function runPalette() {
    const c = paletteFiltered[paletteIndex];
    if (!c) return;
    closeOverlay("commandPalette");
    try { c.run(); } catch (e) { toast(t("toastInitFailed", { msg: e.message }), "error"); }
  }
  function openPalette() {
    openOverlay("commandPalette");
    paletteIndex = 0;
    const input = $("paletteInput");
    input.value = "";
    renderPalette();
    setTimeout(() => input.focus(), 0);
  }
  function initPalette() {
    const input = $("paletteInput");
    input.addEventListener("input", () => { paletteIndex = 0; renderPalette(); });
    input.addEventListener("keydown", (e) => {
      if (e.key === "ArrowDown") {
        e.preventDefault();
        if (paletteFiltered.length) paletteIndex = (paletteIndex + 1) % paletteFiltered.length;
        renderPalette();
      } else if (e.key === "ArrowUp") {
        e.preventDefault();
        if (paletteFiltered.length) paletteIndex = (paletteIndex - 1 + paletteFiltered.length) % paletteFiltered.length;
        renderPalette();
      } else if (e.key === "Enter") {
        e.preventDefault();
        runPalette();
      } else if (e.key === "Escape") {
        e.preventDefault();
        closeOverlay("commandPalette");
      }
    });
  }
  function buildCommands() {
    commands = [
      { id: "start", title: t("cmdStart"), run: toggleStartStop },
      { id: "clear", title: t("cmdClear"), run: () => resetTerminal() },
      { id: "copy", title: t("cmdCopy"), run: () => $("copyBtn").click() },
      { id: "exportRaw", title: t("cmdExportRaw"), run: () => $("exportRawBtn").click() },
      { id: "exportFilter", title: t("cmdExportFilter"), run: () => $("exportFilterBtn").click() },
      { id: "theme", title: t("cmdToggleTheme"), run: cycleTheme },
      { id: "follow", title: t("cmdToggleFollow"), run: toggleFollowMode },
      { id: "top", title: t("cmdTop"), run: scrollTop },
      { id: "bottom", title: t("cmdBottom"), run: scrollBottom },
      { id: "reload", title: t("cmdReload"), run: safeRun(reloadConfig) },
      { id: "shortcuts", title: t("cmdShortcuts"), run: () => openOverlay("shortcutsHelp") },
      { id: "settings", title: t("cmdSettings"), run: openSettings },
      { id: "lang", title: t("cmdLanguage"), run: () => setLang(prefs.lang === "en" ? "zh" : "en") },
    ];
  }

  // ============ 设置抽屉 ============
  function updateThemeSeg() {
    const seg = $("themeSeg");
    if (!seg) return;
    seg.querySelectorAll("button").forEach((b) => {
      b.classList.toggle("active", b.getAttribute("data-theme") === prefs.themeMode);
    });
  }
  function updateLangSeg() {
    const seg = $("langSeg");
    if (!seg) return;
    const lang = window.I18N.lang || detectLang();
    seg.querySelectorAll("button").forEach((b) => {
      b.classList.toggle("active", b.getAttribute("data-lang") === lang);
    });
    const sel = $("langSelect");
    if (sel) sel.value = lang;
  }
  function updateDensitySeg() {
    const seg = $("densitySeg");
    if (!seg) return;
    seg.querySelectorAll("button").forEach((b) => {
      b.classList.toggle("active", b.getAttribute("data-density") === prefs.density);
    });
  }
  function setLang(lang) {
    if (lang !== "zh" && lang !== "en") lang = detectLang();
    prefs = savePrefs({ lang: lang });
    applyI18n();
    updateLangSeg();
    // 语言切换后，命令标题、按钮文案等动态文本需要重建。
    buildCommands();
    updateButtons();
    const pb = $("pauseBtn");
    if (pb) pb.textContent = state.paused ? t("resume") : t("pause");
  }
  function openSettings() {
    openOverlay("settingsDrawer");
    $("fontSizeRange").value = fontSize();
    $("lineHeightRange").value = lineHeight();
    $("fontSizeVal").textContent = fontSize();
    $("lineHeightVal").textContent = Number(lineHeight()).toFixed(2);
    $("scrollbackSelect").value = String(scrollback());
    updateThemeSeg();
    updateLangSeg();
    updateDensitySeg();
  }
  function initSettings() {
    $("themeSeg").addEventListener("click", (e) => {
      const btn = e.target.closest("button[data-theme]");
      if (btn) setThemeMode(btn.getAttribute("data-theme"));
    });
    $("langSeg").addEventListener("click", (e) => {
      const btn = e.target.closest("button[data-lang]");
      if (btn) setLang(btn.getAttribute("data-lang"));
    });
    $("densitySeg").addEventListener("click", (e) => {
      const btn = e.target.closest("button[data-density]");
      if (!btn) return;
      prefs = savePrefs({ density: btn.getAttribute("data-density") });
      updateDensitySeg();
      applyTerminalPrefs();
    });
    const fsRange = $("fontSizeRange");
    fsRange.addEventListener("input", () => {
      const v = parseInt(fsRange.value, 10);
      $("fontSizeVal").textContent = v;
      prefs = savePrefs({ fontSize: v });
      applyTerminalPrefs();
    });
    const lhRange = $("lineHeightRange");
    lhRange.addEventListener("input", () => {
      const v = parseFloat(lhRange.value);
      $("lineHeightVal").textContent = v.toFixed(2);
      prefs = savePrefs({ lineHeight: v });
      applyTerminalPrefs();
    });
    // xterm 无法在运行中改 scrollback（只能在构造时设定）。保存并提示下次生效，
    // 不偷偷重建终端以免打断正在跟踪的日志。
    $("scrollbackSelect").addEventListener("change", (e) => {
      prefs = savePrefs({ scrollback: parseInt(e.target.value, 10) });
      toast(t("toastScrollbackHint"));
    });
  }

  // ============ 移动端 ============
  function initMobile() {
    const hamburger = $("hamburgerBtn");
    const panel = $("treePanel");
    if (!hamburger || !panel) return;
    hamburger.addEventListener("click", () => {
      panel.classList.toggle("mobile-open");
    });
    // 选中文件/目录后自动收起移动端侧栏
    document.addEventListener("fileSelected", () => {
      if (window.matchMedia && window.matchMedia("(max-width: 768px)").matches) {
        panel.classList.remove("mobile-open");
      }
    });
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
      toast(t("toastRefreshed"), "success");
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
      const name = prompt(t("promptSaveAsName"));
      if (name) { $("cfgName").value = name; return saveConfig(name); }
    }));
    $("delCfgBtn").addEventListener("click", safeRun(async () => {
      const name = $("configSelect").value;
      if (!name) return;
      if (!confirm(t("confirmDeleteCfg", { name }))) return;
      await api(hapi("/config/delete"), {
        method: "POST", headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ name }),
      });
      await loadConfigList();
      toast(t("toastDeleted"), "success");
    }));
    $("setDefaultCfgBtn").addEventListener("click", safeRun(async () => {
      const name = $("configSelect").value;
      if (!name) return;
      await api(hapi("/config/setdefault"), {
        method: "POST", headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ name }),
      });
      toast(t("toastSetDefault"), "success");
    }));
    $("renameCfgBtn").addEventListener("click", safeRun(async () => {
      const oldName = $("configSelect").value;
      if (!oldName) return;
      const newName = prompt(t("promptRename", { old: oldName }), oldName);
      if (!newName || newName.trim() === "" || newName === oldName) return;
      await api(hapi("/config/rename"), {
        method: "POST", headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ old: oldName, new: newName.trim() }),
      });
      $("cfgName").value = newName.trim();
      await loadConfigList();
      // loadConfigList 重建了下拉选项，需显式选中新名，否则选中态会回落，
      // 用户看到的"当前配置"与实际 cfgName 输入框不一致。
      $("configSelect").value = newName.trim();
      toast(t("toastRenamed"), "success");
    }));

    $("startBtn").addEventListener("click", () => { startView(); updateButtons(); });
    $("stopBtn").addEventListener("click", () => { stopView(); updateButtons(); });

    // 模式（跟踪/静态）切换：若正在运行先停掉，并刷新按钮文案/联动。
    // 必须走完整运行态复位：旧 follow 会话的 sessionID/lastSeq、暂停缓冲都要清掉，
    // 否则新模式 start 后重连可能错误 attach 到上一个会话，继续追旧文件/旧配置。
    $("followTail").addEventListener("change", () => {
      if (state.running || state.stopping) {
        wsSend({ action: "stop" });
        resetRunState();
        setConnStatus("online");
        toast(t("toastTaskStoppedRestart"), "success");
      }
      updateButtons();
    });
    $("limitEnable").addEventListener("change", syncLimitEnable);

    // 配置面板折叠（点击折叠按钮或标题栏）——按钮带文字，醒目
    const togglePanel = () => {
      const panel = $("configPanel");
      panel.classList.toggle("collapsed");
      const collapsed = panel.classList.contains("collapsed");
      $("cfgToggle").textContent = collapsed ? t("cfgExpand") : t("cfgCollapse");
      $("cfgToggle").title = collapsed ? t("cfgExpandTitle") : t("cfgCollapseTitle");
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
      $("sidebarCollapse").textContent = collapsed ? t("expandSidebar") : t("collapseSidebar");
      setTimeout(fitTerm, 80);
    };
    const sidebarCollapseBtn = $("sidebarCollapse");
    sidebarCollapseBtn.addEventListener("click", () => setSidebar(true));
    $("sidebarReopen").querySelector("button").addEventListener("click", () => setSidebar(false));

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
      $("pauseBtn").textContent = state.paused ? t("resume") : t("pause");
      if (!state.paused) {
        if (state.pausedDropped > 0) {
          const approxLines = Math.round(state.pausedDropped / 120);
          if (term) term.write("\x1b[33m" + t("toastPausedDropped", { n: approxLines }) + "\x1b[0m\r\n");
        }
        if (state.pausedBuffer.length) {
          const hc = highlightContext();
          writeToTerminal(state.pausedBuffer.join(""), hc.rules, hc.useRegex, hc.caseSensitive);
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
      if (!term) return; // 终端未初始化（初始化失败或已销毁）
      const sel = term.getSelection();
      if (!sel) { toast(t("toastNoSelection"), "error"); return; }
      // navigator.clipboard 仅在安全上下文（HTTPS/localhost）下存在；
      // 非安全 HTTP 访问时为 undefined，直接调用会抛错。用 textarea 兜底。
      if (navigator.clipboard && navigator.clipboard.writeText) {
        return navigator.clipboard.writeText(sel).then(() => toast(t("toastCopied"), "success"));
      }
      const ta = document.createElement("textarea");
      ta.value = sel;
      ta.style.position = "fixed";
      ta.style.opacity = "0";
      document.body.appendChild(ta);
      ta.select();
      try { document.execCommand("copy"); toast(t("toastCopied"), "success"); }
      finally { ta.remove(); }
    }));
    $("exportRawBtn").addEventListener("click", () => {
      const file = $("filePath").value.trim();
      if (!file) { toast(t("toastChooseFile"), "error"); return; }
      triggerRawDownload(file);
    });
    $("exportFilterBtn").addEventListener("click", () => {
      const file = $("filePath").value.trim();
      if (!file) { toast(t("toastChooseFile"), "error"); return; }
      triggerFilteredDownload(file);
    });
    $("exportCancelBtn").addEventListener("click", cancelExport);

    // 悬浮滚动按钮
    $("fabTop").addEventListener("click", scrollTop);
    $("fabBottom").addEventListener("click", scrollBottom);
    $("noticeClose").addEventListener("click", hideNotice);

    // 顶栏：主题三态循环、设置抽屉、语言下拉
    $("themeToggle").addEventListener("click", cycleTheme);
    $("settingsBtn").addEventListener("click", openSettings);
    // 右上角两组独立弹窗：帮助（快捷键面板）、控制面板（命令面板）。
    // 二者经 openOverlay 互斥但各自独立打开/关闭，状态互不干扰。
    const helpBtn = $("helpBtn");
    if (helpBtn) helpBtn.addEventListener("click", () => openOverlay("shortcutsHelp"));
    const controlPanelBtn = $("controlPanelBtn");
    if (controlPanelBtn) controlPanelBtn.addEventListener("click", openPalette);
    $("langSelect").addEventListener("change", (e) => setLang(e.target.value));

    // 浮层关闭
    bindOverlayClose("shortcutsHelp");
    bindOverlayClose("commandPalette");
    bindOverlayClose("settingsDrawer");
    initPalette();
    initSettings();
    initMobile();
    buildCommands();

    initTerminalSearch();

    // 登录表单
    $("loginForm").addEventListener("submit", onLoginSubmit);
    // 登出
    $("logoutBtn").addEventListener("click", onLogout);

    initSplitter();
    initShortcuts();

    // 滚动同步 FAB 显隐
    if (term) term.onScroll(scheduleFab);
    window.addEventListener("resize", scheduleFab);
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

    // 当前查询对应的匹配总数（来自 addon 的 onDidChangeResults 事件，含已装饰匹配数）。
    // resultIndex 为当前选中项的 0 基下标；-1 表示无选中。
    let currentResultCount = 0;
    let currentResultIndex = -1;

    function renderCount() {
      if (!input.value) {
        count.textContent = "";
        count.className = "search-count";
        return;
      }
      if (currentResultCount === 0) {
        count.textContent = t("searchNoMatch");
        count.className = "search-count none";
        return;
      }
      // 达到装饰上限时总数可能更多，如实标注 "N+"。
      const total = currentResultCount >= searchResultLimit
        ? currentResultCount + "+"
        : String(currentResultCount);
      const pos = currentResultIndex >= 0 ? (currentResultIndex + 1) : 0;
      count.textContent = t("searchCount", { pos, total });
      count.className = "search-count";
    }

    // 订阅 addon 的官方结果事件：每次 findNext/findPrevious 后回传
    // {resultIndex, resultCount}，是显示【当前/总数】的权威数据源，不靠返回值猜测。
    if (searchAddon.onDidChangeResults) {
      searchAddon.onDidChangeResults((r) => {
        currentResultCount = r.resultCount || 0;
        currentResultIndex = typeof r.resultIndex === "number" ? r.resultIndex : -1;
        renderCount();
      });
    }

    function doFind(forward) {
      const q = input.value;
      if (!q) { currentResultCount = 0; currentResultIndex = -1; renderCount(); return; }
      // 查询变化时从头开始，避免从旧光标位置漏掉上方匹配
      if (q !== lastQuery) {
        searchAddon.clearDecorations?.();
        term.clearSelection();
        lastQuery = q;
        currentResultCount = 0;
        currentResultIndex = -1;
      }
      try {
        const found = forward ? searchAddon.findNext(q, opts()) : searchAddon.findPrevious(q, opts());
        // 事件通常已携带结果；若 addon 版本未触发事件，用返回值兜底。
        if (!searchAddon.onDidChangeResults) {
          currentResultCount = found ? 1 : 0;
          currentResultIndex = found ? 0 : -1;
          renderCount();
        }
      } catch (e) {
        // 非法正则（若启用 regex）等
        currentResultCount = 0;
        currentResultIndex = -1;
        renderCount();
      }
    }

    function open() {
      bar.style.display = "flex";
      const sel = term && term.getSelection();
      if (sel && sel.indexOf("\n") === -1) input.value = sel;
      input.focus();
      input.select();
      lastQuery = "";
      currentResultCount = 0;
      currentResultIndex = -1;
      // 打开时不立即显示"无匹配"，等用户按 Enter/上下钮触发查找后再出结果。
      count.textContent = "";
      count.className = "search-count";
    }
    // 暴露给全局快捷键模块（initShortcuts 统一处理 Ctrl+F 与 /）。
    searchOpen = open;

    function close() {
      bar.style.display = "none";
      searchAddon.clearDecorations?.();
      if (term) term.focus();
    }

    input.addEventListener("keydown", (e) => {
      if (e.key === "Enter") { e.preventDefault(); doFind(!e.shiftKey); }
      else if (e.key === "Escape") { e.preventDefault(); close(); }
    });
    // 输入变化时重置起点状态；尚未执行查找前不显示"无匹配/计数"。
    input.addEventListener("input", () => {
      lastQuery = "";
      currentResultCount = 0;
      currentResultIndex = -1;
      count.textContent = "";
      count.className = "search-count";
    });
    caseBtn.addEventListener("change", () => { lastQuery = ""; doFind(true); });

    $("searchNext").addEventListener("click", () => doFind(true));
    $("searchPrev").addEventListener("click", () => doFind(false));
    $("searchClose").addEventListener("click", close);
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
    if (!username || !password) { errEl.textContent = t("userPassRequired"); return; }
    try {
      const r = await fetch("/api/login", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ username, password }),
      });
      const data = r.headers.get("content-type")?.includes("json")
        ? await r.json().catch(() => ({})) : {};
      if (!r.ok) {
        errEl.textContent = data.error || (t("loginFailed") + r.status);
        $("loginPass").value = "";
        return;
      }
      hideLogin();
      state.wsIntendedClose = false;
      await init();
    } catch (err) {
      errEl.textContent = t("netError") + err.message;
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
    applyI18n();
    applyTheme();
    // 初始化各设置分段控件的高亮态。
    updateThemeSeg();
    updateLangSeg();
    updateDensitySeg();
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
      if (!loginShown) toast(t("toastInitFailed", { msg: e.message }), "error");
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