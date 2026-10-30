/* NetBootX 管理界面逻辑
 * 原生 JavaScript，无框架、无外部请求。
 * 通过 REST 拉取初始数据，用 SSE（/api/events/stream）接收实时增量。
 */
(function () {
  'use strict';

  /* ---------- 常量 ---------- */

  var POLL_INTERVAL = 10000;   // SSE 断开时的兜底轮询间隔
  var MAX_LOG_ROWS = 600;      // 日志面板最多保留的行数
  var TOAST_MS = 3600;         // 提示条停留时间

  var API = {
    status: '/api/status',
    start: '/api/start',
    stop: '/api/stop',
    isos: '/api/isos',
    rescan: '/api/isos/rescan',
    upload: '/api/isos/upload',
    clients: '/api/clients',
    events: '/api/events',
    stream: '/api/events/stream',
    settings: '/api/settings',
    interfaces: '/api/interfaces',
    preview: '/api/preview/',
    version: '/api/version'
  };

  var STATE_TEXT = {
    dhcp: '等待引导',
    tftp: '下载引导文件',
    menu: '选择菜单',
    booting: '加载内核',
    install: '安装中',
    failed: '启动失败'
  };

  var FAMILY_TEXT = {
    windows: 'Windows',
    winpe: 'WinPE',
    linux: 'Linux',
    esxi: 'ESXi',
    bsd: 'BSD',
    other: '其他'
  };

  /* ---------- 运行时状态 ---------- */

  var state = {
    status: null,
    settings: null,
    isos: [],
    clients: [],
    interfaces: [],
    events: [],
    defaultISO: '',
    version: '',
    filter: '',
    autoScroll: true,
    busy: false,
    live: false,
    logCount: 0,
    modalScript: ''
  };

  var pollTimer = null;
  var es = null;

  /* ---------- 小工具 ---------- */

  function $(id) { return document.getElementById(id); }

  function el(tag, cls, text) {
    var node = document.createElement(tag);
    if (cls) { node.className = cls; }
    if (text !== undefined && text !== null) { node.textContent = String(text); }
    return node;
  }

  var SVG_NS = 'http://www.w3.org/2000/svg';

  function icon(id, cls) {
    var svg = document.createElementNS(SVG_NS, 'svg');
    svg.setAttribute('class', cls || 'icon');
    svg.setAttribute('aria-hidden', 'true');
    var use = document.createElementNS(SVG_NS, 'use');
    use.setAttribute('href', '#' + id);
    svg.appendChild(use);
    return svg;
  }

  function escapeHTML(s) {
    return String(s)
      .replace(/&/g, '&amp;')
      .replace(/</g, '&lt;')
      .replace(/>/g, '&gt;')
      .replace(/"/g, '&quot;')
      .replace(/'/g, '&#39;');
  }

  function fmtSize(bytes) {
    var n = Number(bytes) || 0;
    if (n < 1024) { return n + ' B'; }
    var units = ['KB', 'MB', 'GB', 'TB'];
    var v = n;
    for (var i = 0; i < units.length; i++) {
      v = v / 1024;
      if (v < 1024 || i === units.length - 1) {
        return v.toFixed(v >= 100 ? 0 : 1) + ' ' + units[i];
      }
    }
    return n + ' B';
  }

  function pad2(n) { return (n < 10 ? '0' : '') + n; }

  function fmtClock(d) {
    var t = d instanceof Date ? d : new Date(d);
    if (isNaN(t.getTime())) { return '--:--:--'; }
    return pad2(t.getHours()) + ':' + pad2(t.getMinutes()) + ':' + pad2(t.getSeconds());
  }

  function fmtAgo(d) {
    var t = d instanceof Date ? d : new Date(d);
    if (isNaN(t.getTime())) { return '—'; }
    var sec = Math.floor((Date.now() - t.getTime()) / 1000);
    if (sec < 0) { sec = 0; }
    if (sec < 10) { return '刚刚'; }
    if (sec < 60) { return sec + ' 秒前'; }
    if (sec < 3600) { return Math.floor(sec / 60) + ' 分钟前'; }
    if (sec < 86400) { return Math.floor(sec / 3600) + ' 小时前'; }
    return Math.floor(sec / 86400) + ' 天前';
  }

  function fmtUptime(sec) {
    var s = Math.max(0, Math.floor(Number(sec) || 0));
    if (s < 60) { return s + ' 秒'; }
    var m = Math.floor(s / 60);
    if (m < 60) { return m + ' 分钟'; }
    var h = Math.floor(m / 60);
    var rm = m % 60;
    if (h < 24) { return h + ' 小时 ' + rm + ' 分'; }
    return Math.floor(h / 24) + ' 天 ' + (h % 24) + ' 小时';
  }

  function isRunning() {
    return !!(state.status && state.status.running);
  }

  /* ---------- 提示条 ---------- */

  function toast(kind, text) {
    var box = $('toasts');
    if (!box) { return; }
    var node = el('div', 'toast');
    node.setAttribute('data-kind', kind || 'info');
    var iconId = kind === 'ok' ? 'i-check' : (kind === 'error' ? 'i-warn' : (kind === 'warn' ? 'i-warn' : 'i-arrow'));
    node.appendChild(icon(iconId));
    node.appendChild(el('div', 'toast-text', text));
    box.appendChild(node);
    window.setTimeout(function () {
      node.classList.add('is-out');
      window.setTimeout(function () {
        if (node.parentNode) { node.parentNode.removeChild(node); }
      }, 220);
    }, TOAST_MS);
  }

  /* ---------- API ---------- */

  function api(path, options) {
    var opt = options || {};
    var init = { method: opt.method || 'GET', headers: {} };
    if (opt.body !== undefined) {
      init.headers['Content-Type'] = 'application/json';
      init.body = JSON.stringify(opt.body);
    }
    return fetch(path, init).then(function (res) {
      return res.text().then(function (text) {
        var body = null;
        if (text) {
          try { body = JSON.parse(text); } catch (e) { body = null; }
        }
        if (!res.ok) {
          var msg = body && body.error ? body.error : ('HTTP ' + res.status + ' ' + res.statusText);
          throw new Error(msg);
        }
        if (!body) { throw new Error('服务器返回了非 JSON 内容'); }
        if (!body.ok) { throw new Error(body.error || '请求失败'); }
        return body.data;
      });
    });
  }

  /* ---------- 状态栏渲染 ---------- */

  function setConn(stateName, text) {
    var box = $('connState');
    if (!box) { return; }
    box.setAttribute('data-state', stateName);
    $('connText').textContent = text;
  }

  function renderStatus() {
    var st = state.status || {};
    var running = !!st.running;
    var pill = $('statusPill');

    if (state.busy) {
      pill.setAttribute('data-state', 'busy');
      $('statusText').textContent = '处理中…';
    } else if (running) {
      pill.setAttribute('data-state', 'running');
      $('statusText').textContent = '运行中';
    } else {
      pill.setAttribute('data-state', 'stopped');
      $('statusText').textContent = '已停止';
    }

    var btn = $('powerBtn');
    btn.setAttribute('data-state', state.busy ? 'busy' : (running ? 'running' : 'stopped'));
    btn.disabled = !!state.busy;
    btn.querySelector('.btn-power-text').textContent = state.busy ? '处理中…' : (running ? '停止服务' : '启动服务');
    $('powerHint').textContent = running
      ? ('服务运行中 · 已运行 ' + fmtUptime(st.uptimeSec))
      : '服务已停止，点击启动 PXE 服务';

    $('metaHost').textContent = st.hostname ? (st.hostname + ' · ' + (st.os || '') + '/' + (st.arch || '')) : '—';
    var ports = [];
    if (st.httpPort) { ports.push('HTTP ' + st.httpPort); }
    if (st.tftpPort) { ports.push('TFTP ' + st.tftpPort); }
    if (st.dhcpMode) { ports.push('DHCP ' + st.dhcpMode); }
    $('metaPorts').textContent = ports.length ? ports.join(' · ') : '—';
    $('metaUptime').textContent = running ? ('↑ ' + fmtUptime(st.uptimeSec)) : '未运行';
    $('versionBadge').textContent = 'v' + (state.version || st.version || '—');

    document.title = 'NetBootX · ' + (running ? '运行中' : '已停止') + ' · 网络引导服务器';
  }

  /* ---------- 网卡 / 配置 ---------- */

  function renderInterfaces() {
    var sel = $('ifaceSelect');
    var list = state.interfaces || [];
    var current = state.settings || {};
    sel.innerHTML = '';

    if (!list.length) {
      var opt = el('option', null, '未检测到可用网卡');
      opt.value = '';
      sel.appendChild(opt);
      return;
    }

    var matched = false;
    for (var i = 0; i < list.length; i++) {
      var nic = list[i];
      var ips = Array.isArray(nic.ips) && nic.ips.length ? nic.ips : (nic.ip ? [nic.ip] : []);
      if (!ips.length) {
        var emptyOpt = el('option', null, nic.name + '（无 IPv4）');
        emptyOpt.value = nic.name + '|';
        emptyOpt.disabled = true;
        sel.appendChild(emptyOpt);
        continue;
      }
      for (var j = 0; j < ips.length; j++) {
        var value = nic.name + '|' + ips[j];
        var label = nic.name + ' · ' + ips[j];
        if (nic.loopback) { label += '（回环）'; }
        if (!nic.up) { label += '（未启用）'; }
        var o = el('option', null, label);
        o.value = value;
        sel.appendChild(o);
        if (current.interface === nic.name && current.serverIP === ips[j]) {
          o.selected = true;
          matched = true;
        }
      }
    }

    if (!matched) {
      // 配置里的 IP 不在枚举结果里（比如手工填的），补一个选项显示出来。
      if (current.serverIP) {
        var label = current.interface
          ? ('当前配置 · ' + current.interface + ' · ' + current.serverIP)
          : ('当前配置 · ' + current.serverIP + '（自动）');
        var extra = el('option', null, label);
        extra.value = (current.interface || '') + '|' + current.serverIP;
        extra.selected = true;
        sel.appendChild(extra);
      } else {
        sel.selectedIndex = 0;
      }
    }
  }

  function renderSettingsForm() {
    var s = state.settings;
    if (!s) { return; }
    var rs = $('rangeStart');
    var re = $('rangeEnd');
    if (document.activeElement !== rs) { rs.value = s.rangeStart || ''; }
    if (document.activeElement !== re) { re.value = s.rangeEnd || ''; }
    var mode = $('dhcpMode');
    if (document.activeElement !== mode) { mode.value = s.dhcpMode || 'proxy'; }
  }

  function saveSettings(patch, okText) {
    return api('/api/settings', { method: 'POST', body: patch }).then(function (data) {
      state.settings = data.settings;
      renderSettingsForm();
      renderInterfaces();
      if (okText) { toast('ok', okText); }
      return data;
    }).catch(function (err) {
      toast('error', '保存配置失败：' + err.message);
      renderSettingsForm();
      throw err;
    });
  }

  /* ---------- ISO 列表 ---------- */

  function isoVisible(iso) {
    if (!state.filter) { return true; }
    var q = state.filter.toLowerCase();
    return (iso.name || '').toLowerCase().indexOf(q) >= 0 ||
      (iso.osName || '').toLowerCase().indexOf(q) >= 0 ||
      (iso.relPath || '').toLowerCase().indexOf(q) >= 0;
  }

  function renderISOs() {
    var box = $('isoList');
    var list = (state.isos || []).filter(isoVisible);
    box.innerHTML = '';
    $('isoCount').textContent = String((state.isos || []).length);

    if (!state.isos || !state.isos.length) {
      box.appendChild(emptyState('ok', '还没有 ISO 镜像',
        '把 .iso 文件放进 iso/ 目录，或点右上角「上传 ISO」。'));
      return;
    }
    if (!list.length) {
      box.appendChild(emptyState('ok', '没有匹配的镜像', '换个关键词试试。'));
      return;
    }

    for (var i = 0; i < list.length; i++) {
      box.appendChild(isoItem(list[i]));
    }
  }

  function isoItem(iso) {
    var row = el('div', 'iso-item');
    row.setAttribute('data-family', iso.family || 'other');
    if (iso['default']) { row.classList.add('is-default'); }

    var box = el('div', 'iso-icon');
    box.appendChild(icon('i-disc'));
    row.appendChild(box);

    var main = el('div', 'iso-main');

    var title = el('div', 'iso-title');
    title.appendChild(el('span', 'name', iso.name || '(未命名)'));
    if (iso.osName) { title.appendChild(el('span', 'os', iso.osName)); }
    main.appendChild(title);

    var sub = el('div', 'iso-sub');
    sub.appendChild(el('span', null, iso.sizeText || fmtSize(iso.size)));
    sub.appendChild(el('span', 'sep', '·'));
    sub.appendChild(el('span', null, FAMILY_TEXT[iso.family] || iso.family || '其他'));
    if (iso.archText) {
      sub.appendChild(el('span', 'sep', '·'));
      sub.appendChild(el('span', null, iso.archText));
    }
    if (iso.boot && iso.boot.method) {
      sub.appendChild(el('span', 'sep', '·'));
      sub.appendChild(el('span', null, iso.boot.method));
    }
    if (iso['default']) {
      sub.appendChild(tag('tag-default', '默认'));
    }
    if (!iso.bootable) {
      sub.appendChild(tag('tag-warn', '不可引导'));
    }
    if (iso.error) {
      var warn = tag('tag-err', '探测异常');
      warn.title = iso.error;
      sub.appendChild(warn);
    }
    main.appendChild(sub);
    row.appendChild(main);

    var actions = el('div', 'iso-actions');

    var previewBtn = el('button', 'btn btn-tiny');
    previewBtn.type = 'button';
    previewBtn.title = '预览该镜像生成的 iPXE 脚本';
    previewBtn.appendChild(icon('i-eye'));
    previewBtn.appendChild(el('span', null, '预览'));
    previewBtn.addEventListener('click', function () { openPreview(iso); });
    actions.appendChild(previewBtn);

    if (!iso['default']) {
      var defBtn = el('button', 'btn btn-tiny');
      defBtn.type = 'button';
      defBtn.title = '设为默认启动项';
      defBtn.appendChild(icon('i-check'));
      defBtn.addEventListener('click', function () {
        defBtn.disabled = true;
        setDefaultISO(iso);
      });
      actions.appendChild(defBtn);
    }

    row.appendChild(actions);
    return row;
  }

  function tag(cls, text) {
    var node = el('span', 'tag ' + cls, text);
    return node;
  }

  function setDefaultISO(iso) {
    // 优先走专门的接口（不重启服务）；后端没实现时退回保存配置。
    api('/api/isos/default', { method: 'POST', body: { id: iso.id } }).then(function () {
      state.defaultISO = iso.id;
      markDefault(iso.id);
      toast('ok', '已把「' + iso.name + '」设为默认启动项');
    }).catch(function (err) {
      if (/404|未知接口/.test(err.message)) {
        return saveSettings({ defaultISO: iso.id }, '已把「' + iso.name + '」设为默认启动项');
      }
      toast('error', '设置默认项失败：' + err.message);
      throw err;
    }).catch(function () {
      renderISOs();
    });
  }

  function markDefault(id) {
    for (var i = 0; i < state.isos.length; i++) {
      state.isos[i]['default'] = state.isos[i].id === id;
    }
    renderISOs();
  }

  function emptyState(kind, title, hint) {
    var node = el('div', 'empty');
    node.setAttribute('data-kind', kind || 'ok');
    node.appendChild(el('strong', null, title));
    if (hint) { node.appendChild(el('div', 'empty-hint', hint)); }
    return node;
  }

  /* ---------- 客户机列表 ---------- */

  function renderClients() {
    var box = $('clientList');
    var list = state.clients || [];
    box.innerHTML = '';
    $('clientCount').textContent = String(list.length);

    if (!list.length) {
      box.appendChild(emptyState('ok', '还没有客户机', '客户机开机并走 PXE 网络启动后，会出现在这里。'));
      return;
    }

    for (var i = 0; i < list.length; i++) {
      box.appendChild(clientItem(list[i]));
    }
  }

  function clientItem(c) {
    var row = el('div', 'client-item');
    row.setAttribute('data-state', c.state || 'dhcp');

    var main = el('div', 'client-main');
    var top = el('div', 'client-top');
    top.appendChild(el('span', 'client-name', c.hostname || c.mac || '未知客户机'));
    if (c.ip) { top.appendChild(el('span', 'client-ip', c.ip)); }
    main.appendChild(top);

    var sub = el('div', 'client-sub');
    sub.appendChild(el('span', null, c.mac || '—'));
    if (c.arch) {
      sub.appendChild(el('span', 'sep', '·'));
      sub.appendChild(el('span', 'arch', archText(c.arch)));
    }
    if (c.selected) {
      var iso = findISO(c.selected);
      sub.appendChild(el('span', 'sep', '·'));
      sub.appendChild(el('span', null, iso ? iso.name : c.selected));
    }
    if (c.bootCount > 1) {
      sub.appendChild(el('span', 'sep', '·'));
      sub.appendChild(el('span', null, '第 ' + c.bootCount + ' 次'));
    }
    main.appendChild(sub);
    row.appendChild(main);

    var side = el('div', 'client-side');
    var chip = el('span', 'state-chip');
    chip.setAttribute('data-state', c.state || 'dhcp');
    chip.appendChild(el('span', 'dot'));
    chip.appendChild(el('span', null, STATE_TEXT[c.state] || c.state || '未知'));
    side.appendChild(chip);
    side.appendChild(el('span', 'client-time', fmtAgo(c.lastSeen)));
    row.appendChild(side);

    return row;
  }

  function archText(arch) {
    var map = {
      bios: 'Legacy BIOS',
      uefi32: 'UEFI IA32',
      uefi64: 'UEFI x64',
      uefi64http: 'UEFI x64 HTTP',
      arm64: 'UEFI ARM64',
      arm64http: 'UEFI ARM64 HTTP',
      loongarch64: 'UEFI LoongArch64'
    };
    return map[arch] || arch;
  }

  function findISO(id) {
    for (var i = 0; i < state.isos.length; i++) {
      if (state.isos[i].id === id) { return state.isos[i]; }
    }
    return null;
  }

  /* ---------- 日志 ---------- */

  // renderLogs 只负责把 state.events 渲染到 DOM。
  // reset=true 表示「先清空 DOM 再重画」，它**不会**动 state.events——
  // 数据由调用方管理，否则「先塞数据再渲染」的调用方会被自己清空。
  function renderLogs(reset) {
    var box = $('logList');
    if (reset) {
      box.innerHTML = '';
    }
    var events = state.events || [];
    if (!events.length) {
      box.appendChild(emptyState('ok', '暂无日志', '服务运行后，DHCP / TFTP / HTTP 的访问记录会显示在这里。'));
      $('logCount').textContent = '0';
      return;
    }

    box.innerHTML = '';
    for (var i = 0; i < events.length; i++) {
      box.appendChild(logRow(events[i]));
    }
    $('logCount').textContent = String(events.length);
    if (state.autoScroll) { box.scrollTop = box.scrollHeight; }
  }

  function appendEvents(list) {
    if (!list || !list.length) { return; }
    var box = $('logList');
    if (!state.events.length) { box.innerHTML = ''; }

    for (var i = 0; i < list.length; i++) {
      var ev = list[i];
      state.events.push(ev);
      box.appendChild(logRow(ev));
    }
    while (state.events.length > MAX_LOG_ROWS) {
      state.events.shift();
      if (box.firstChild) { box.removeChild(box.firstChild); }
    }
    $('logCount').textContent = String(state.events.length);
    if (state.autoScroll) { box.scrollTop = box.scrollHeight; }
  }

  function logRow(ev) {
    var row = el('div', 'log-row');
    row.setAttribute('data-level', ev.level || 'info');
    row.appendChild(el('span', 'log-time', fmtClock(ev.time)));
    var kind = el('span', 'log-kind', (ev.kind || 'log').toUpperCase());
    kind.setAttribute('data-kind', ev.kind || 'system');
    row.appendChild(kind);
    var msg = el('span', 'log-msg');
    msg.textContent = ev.msg || '';
    if (ev.mac) {
      msg.appendChild(document.createTextNode(' '));
      msg.appendChild(el('span', 'mac', '[' + ev.mac + ']'));
    }
    row.appendChild(msg);
    return row;
  }

  /* ---------- 预览弹窗 ---------- */

  function openPreview(iso) {
    var modal = $('modal');
    $('modalTitle').textContent = 'iPXE 启动脚本预览';
    $('modalSub').textContent = (iso.osName || iso.name || '') + ' · ' + (iso.name || '');
    $('modalMeta').innerHTML = '';
    $('modalMeta').appendChild(tag('tag-accent', FAMILY_TEXT[iso.family] || '其他'));
    $('modalMeta').appendChild(tag('tag-idle', fmtSize(iso.size)));
    if (iso.boot && iso.boot.method) {
      $('modalMeta').appendChild(tag('tag-idle', iso.boot.method));
    }
    var code = $('modalCode').querySelector('code') || $('modalCode');
    code.textContent = '正在生成脚本…';
    $('modalHint').textContent = '';
    $('modalCode').scrollTop = 0;
    modal.hidden = false;
    document.body.style.overflow = 'hidden';
    $('modalClose').focus();

    var url = '/api/preview/' + encodeURIComponent(iso.id);
    var mac = state.clients && state.clients.length ? state.clients[0].mac : '';
    if (mac) { url += '?mac=' + encodeURIComponent(mac); }

    api(url).then(function (data) {
      state.modalScript = data.script || '';
      code.innerHTML = highlightScript(state.modalScript);
      $('modalHint').textContent = data.source === 'pxe'
        ? '由 PXE 服务真实生成，与实际下发给客户机的脚本一致。'
        : '由 Web 层按 ISO 探测结果生成的近似脚本（PXE 层未提供 Preview 接口）。';
    }).catch(function (err) {
      code.textContent = '生成失败：' + err.message;
      state.modalScript = '';
      $('modalHint').textContent = '';
    });
  }

  function closeModal() {
    var modal = $('modal');
    if (modal.hidden) { return; }
    modal.hidden = true;
    document.body.style.overflow = '';
  }

  function highlightScript(text) {
    var lines = String(text).split('\n');
    var out = [];
    for (var i = 0; i < lines.length; i++) {
      var line = lines[i];
      var esc = escapeHTML(line);
      if (/^\s*#/.test(line)) {
        var cls = /^\s*#!ipxe/.test(line) ? 'c-directive' : 'c-comment';
        out.push('<span class="' + cls + '">' + esc + '</span>');
        continue;
      }
      esc = esc.replace(/^(\s*)([A-Za-z!][\w-]*)/, function (m, sp, cmd) {
        return sp + '<span class="c-cmd">' + cmd + '</span>';
      });
      esc = esc.replace(/(https?:\/\/[^\s]+)/g, '<span class="c-url">$1</span>');
      out.push(esc);
    }
    return out.join('\n');
  }

  function copyScript() {
    var text = state.modalScript;
    if (!text) {
      toast('warn', '脚本还没生成好');
      return;
    }
    function done() { toast('ok', '脚本已复制到剪贴板'); }
    function fallback() {
      var ta = document.createElement('textarea');
      ta.value = text;
      ta.style.position = 'fixed';
      ta.style.opacity = '0';
      document.body.appendChild(ta);
      ta.select();
      try {
        document.execCommand('copy');
        done();
      } catch (e) {
        toast('error', '复制失败，请手动选择文本');
      }
      document.body.removeChild(ta);
    }
    if (navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(text).then(done).catch(fallback);
    } else {
      fallback();
    }
  }

  /* ---------- 数据加载 ---------- */

  function applySnapshot(snap) {
    if (!snap) { return; }
    if (snap.status) { state.status = snap.status; }
    if (snap.settings) { state.settings = snap.settings; }
    if (snap.clients) { state.clients = snap.clients; }
    if (snap.isos) { state.isos = snap.isos; }
    if (snap.events) { state.events = snap.events; renderLogs(true); }
    if (snap.interfaces) { state.interfaces = snap.interfaces; }
    if (snap.defaultISO !== undefined) { state.defaultISO = snap.defaultISO; }
    if (snap.version) { state.version = snap.version; }

    renderStatus();
    renderSettingsForm();
    renderInterfaces();
    renderISOs();
    renderClients();
    if (!snap.events) { renderLogs(true); } // 保留已有日志，只重画
  }

  function loadInitial() {
    api('/api/status').then(function (data) {
      state.status = data.status;
      state.version = data.version || state.version;
      state.defaultISO = data.defaultISO || '';
      if (data.settings) {
        // /api/status 里只带精简配置，完整配置另外拉。
        state.settings = Object.assign({}, state.settings || {}, data.settings);
      }
      renderStatus();
    }).catch(function (err) {
      $('statusPill').setAttribute('data-state', 'error');
      $('statusText').textContent = '连接失败';
      toast('error', '无法连接服务端：' + err.message);
    });

    api('/api/settings').then(function (s) {
      state.settings = s;
      renderSettingsForm();
      renderInterfaces();
    }).catch(function () { /* 配置拉取失败不阻塞界面 */ });

    api('/api/interfaces').then(function (data) {
      state.interfaces = data.interfaces || [];
      renderInterfaces();
    }).catch(function () { /* 忽略 */ });

    api('/api/isos').then(function (data) {
      state.isos = data.isos || [];
      state.defaultISO = data.defaultISO || state.defaultISO;
      renderISOs();
    }).catch(function (err) {
      $('isoList').innerHTML = '';
      $('isoList').appendChild(emptyState('error', '加载镜像失败', err.message));
    });

    api('/api/clients').then(function (data) {
      state.clients = data.clients || [];
      renderClients();
    }).catch(function (err) {
      $('clientList').innerHTML = '';
      $('clientList').appendChild(emptyState('error', '加载客户机失败', err.message));
    });

    api('/api/events?limit=200').then(function (data) {
      state.events = data.events || [];
      renderLogs(true);
    }).catch(function (err) {
      $('logList').innerHTML = '';
      $('logList').appendChild(emptyState('error', '加载日志失败', err.message));
    });

    api('/api/version').then(function (v) {
      state.version = v.version || state.version;
      renderStatus();
    }).catch(function () { /* 忽略 */ });
  }

  /* ---------- SSE ---------- */

  function connectSSE() {
    if (!window.EventSource) {
      setConn('lost', '浏览器不支持实时推送');
      startPolling();
      return;
    }
    if (es) { es.close(); }

    setConn('connecting', '实时连接中');
    es = new EventSource('/api/events/stream');

    es.addEventListener('open', function () {
      state.live = true;
      setConn('live', '实时已连接');
      stopPolling();
    });

    es.addEventListener('snapshot', function (e) {
      try { applySnapshot(JSON.parse(e.data)); } catch (err) { /* 忽略坏消息 */ }
    });

    es.addEventListener('status', function (e) {
      try {
        state.status = JSON.parse(e.data);
        renderStatus();
      } catch (err) { /* 忽略 */ }
    });

    es.addEventListener('clients', function (e) {
      try {
        state.clients = JSON.parse(e.data) || [];
        renderClients();
      } catch (err) { /* 忽略 */ }
    });

    es.addEventListener('events', function (e) {
      try { appendEvents(JSON.parse(e.data)); } catch (err) { /* 忽略 */ }
    });

    es.addEventListener('error', function () {
      state.live = false;
      setConn('lost', '实时连接断开，重连中…');
      startPolling();
    });
  }

  function startPolling() {
    if (pollTimer) { return; }
    pollTimer = window.setInterval(function () {
      api('/api/status').then(function (data) {
        state.status = data.status;
        renderStatus();
      }).catch(function () { /* 保持现状 */ });
      api('/api/clients').then(function (data) {
        state.clients = data.clients || [];
        renderClients();
      }).catch(function () { /* 保持现状 */ });
      api('/api/events?limit=200').then(function (data) {
        state.events = data.events || [];
        renderLogs(true);
      }).catch(function () { /* 保持现状 */ });
    }, POLL_INTERVAL);
  }

  function stopPolling() {
    if (pollTimer) {
      window.clearInterval(pollTimer);
      pollTimer = null;
    }
  }

  /* ---------- 操作 ---------- */

  function togglePower() {
    if (state.busy) { return; }
    var running = isRunning();
    state.busy = true;
    renderStatus();

    var action = running ? 'stop' : 'start';
    api(running ? API.stop : API.start, { method: 'POST' }).then(function (data) {
      state.status = data.status;
      if (data.defaultISO !== undefined) { state.defaultISO = data.defaultISO; }
      toast('ok', running ? 'PXE 服务已停止' : 'PXE 服务已启动');
    }).catch(function (err) {
      toast('error', (running ? '停止' : '启动') + '失败：' + err.message);
    }).then(function () {
      state.busy = false;
      renderStatus();
    });
  }

  function rescanISOs() {
    var btn = $('rescanBtn');
    btn.disabled = true;
    api('/api/isos/rescan', { method: 'POST' }).then(function (data) {
      state.isos = data.isos || [];
      state.defaultISO = data.defaultISO || state.defaultISO;
      renderISOs();
      toast('ok', '扫描完成，共 ' + (data.rescanCount || state.isos.length) + ' 个镜像');
    }).catch(function (err) {
      toast('error', '扫描失败：' + err.message);
    }).then(function () {
      btn.disabled = false;
    });
  }

  function uploadFiles(files) {
    if (!files || !files.length) { return; }
    var fd = new FormData();
    for (var i = 0; i < files.length; i++) {
      fd.append('files', files[i], files[i].name);
    }

    var btn = $('uploadBtn');
    btn.disabled = true;
    var label = btn.lastChild;
    if (!label || !label.textContent) {
      label = document.createElement('span');
      btn.appendChild(label);
    }
    var original = label.textContent;
    label.textContent = '上传中…';

    var xhr = new XMLHttpRequest();
    xhr.open('POST', '/api/isos/upload');
    xhr.upload.addEventListener('progress', function (e) {
      if (e.lengthComputable) {
        var pct = Math.round((e.loaded / e.total) * 100);
        label.textContent = '上传 ' + pct + '%';
      }
    });
    xhr.addEventListener('load', function () {
      btn.disabled = false;
      label.textContent = original;
      var body = null;
      try { body = JSON.parse(xhr.responseText); } catch (e) { body = null; }
      if (xhr.status >= 200 && xhr.status < 300 && body && body.ok) {
        var data = body.data || {};
        state.isos = data.isos || state.isos;
        renderISOs();
        var names = (data.uploaded || []).map(function (u) { return u.name; }).join('、');
        toast('ok', '上传成功：' + (names || '已完成'));
      } else {
        toast('error', '上传失败：' + ((body && body.error) || ('HTTP ' + xhr.status)));
      }
    });
    xhr.addEventListener('error', function () {
      btn.disabled = false;
      label.textContent = original;
      toast('error', '上传失败：网络错误');
    });
    xhr.send(fd);
  }

  function clearClients() {
    api('/api/clients', { method: 'DELETE' }).then(function () {
      state.clients = [];
      renderClients();
      toast('ok', '客户机列表已清空');
    }).catch(function (err) {
      toast('error', '清空失败：' + err.message);
    });
  }

  /* ---------- 事件绑定 ---------- */

  function bindEvents() {
    $('powerBtn').addEventListener('click', togglePower);
    $('rescanBtn').addEventListener('click', rescanISOs);
    $('clearClientsBtn').addEventListener('click', clearClients);

    $('uploadBtn').addEventListener('click', function () { $('fileInput').click(); });
    $('fileInput').addEventListener('change', function (e) {
      uploadFiles(e.target.files);
      e.target.value = '';
    });

    $('isoSearch').addEventListener('input', function (e) {
      state.filter = e.target.value.trim();
      renderISOs();
    });

    $('autoScroll').addEventListener('change', function (e) {
      state.autoScroll = e.target.checked;
      if (state.autoScroll) {
        var box = $('logList');
        box.scrollTop = box.scrollHeight;
      }
    });

    $('clearLogsBtn').addEventListener('click', function () {
      state.events = [];
      renderLogs(true);
    });

    $('saveConfigBtn').addEventListener('click', function () {
      var patch = {
        rangeStart: $('rangeStart').value.trim(),
        rangeEnd: $('rangeEnd').value.trim(),
        dhcpMode: $('dhcpMode').value
      };
      var iface = $('ifaceSelect').value;
      if (iface) {
        var parts = iface.split('|');
        patch.interface = parts[0];
        if (parts[1]) { patch.serverIP = parts[1]; }
      }
      saveSettings(patch, '配置已保存');
    });

    $('ifaceSelect').addEventListener('change', function (e) {
      var parts = String(e.target.value || '').split('|');
      if (!parts[0]) { return; }
      var patch = { interface: parts[0] };
      if (parts[1]) { patch.serverIP = parts[1]; }
      saveSettings(patch, '已切换到 ' + parts[0] + (parts[1] ? ' · ' + parts[1] : ''));
    });

    $('dhcpMode').addEventListener('change', function (e) {
      saveSettings({ dhcpMode: e.target.value }, 'DHCP 模式已切换为 ' + e.target.value);
    });

    ['rangeStart', 'rangeEnd'].forEach(function (id) {
      $(id).addEventListener('change', function () {
        saveSettings({ rangeStart: $('rangeStart').value.trim(), rangeEnd: $('rangeEnd').value.trim() }, 'IP 地址池已更新');
      });
    });

    $('modalClose').addEventListener('click', closeModal);
    $('modal').addEventListener('click', function (e) {
      if (e.target && e.target.getAttribute('data-close') === '1') { closeModal(); }
    });
    $('modalCopy').addEventListener('click', copyScript);

    document.addEventListener('keydown', function (e) {
      if (e.key === 'Escape') { closeModal(); }
    });

    // 页面重新可见时立刻刷新一次，避免后台标签页数据过期。
    document.addEventListener('visibilitychange', function () {
      if (!document.hidden && !state.live) {
        api('/api/status').then(function (data) {
          state.status = data.status;
          renderStatus();
        }).catch(function () { /* 忽略 */ });
      }
    });

    window.addEventListener('beforeunload', function () {
      if (es) { es.close(); }
      stopPolling();
    });
  }

  /* ---------- 启动 ---------- */

  function boot() {
    bindEvents();
    renderStatus();
    loadInitial();
    connectSSE();
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', boot);
  } else {
    boot();
  }
})();
