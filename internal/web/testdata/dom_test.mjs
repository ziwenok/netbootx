/**
 * NetBootX 前端最小 DOM 测试骨架（零依赖，只用 node 内置模块）。
 *
 * 目的：在 go test 里真实执行 assets/app.js，抓住「渲染函数把数据清空」这类
 * 只有跑起来才会暴露的 bug（例如 renderLogs(reset) 曾经把 state.events 清掉，
 * 导致日志面板永远显示「暂无日志」）。
 *
 * 用法：node testdata/dom_test.mjs <assetsDir>
 * 退出码 0 表示全部断言通过；非 0 时打印失败原因。
 */
import fs from 'node:fs';
import path from 'node:path';
import vm from 'node:vm';

const assetsDir = process.argv[2] || path.join(import.meta.dirname, '..', 'assets');
const html = fs.readFileSync(path.join(assetsDir, 'index.html'), 'utf8');
const appJS = fs.readFileSync(path.join(assetsDir, 'app.js'), 'utf8');

let failures = 0;
function check(cond, msg) {
  if (!cond) {
    failures++;
    console.error('  ✗ ' + msg);
  } else {
    console.log('  ✓ ' + msg);
  }
}

/* ---------- 极简 DOM 实现 ---------- */

class ClassList {
  constructor(el) { this.el = el; this.set = new Set(); }
  add(...names) { names.forEach((n) => n && this.set.add(n)); }
  remove(...names) { names.forEach((n) => this.set.delete(n)); }
  contains(n) { return this.set.has(n); }
  toggle(n, force) {
    const on = force === undefined ? !this.set.has(n) : !!force;
    if (on) { this.set.add(n); } else { this.set.delete(n); }
    return on;
  }
  toString() { return [...this.set].join(' '); }
}

class Element {
  constructor(tagName) {
    this.tagName = String(tagName || 'div').toUpperCase();
    this.children = [];
    this.parentNode = null;
    this.attributes = {};
    this.classList = new ClassList(this);
    this.listeners = {};
    this.style = {};
    this._text = '';
    this._value = '';
    this.disabled = false;
    this.selected = false;
    this.checked = false;
    this.hidden = false;
    this.title = '';
    this.scrollTop = 0;
    this.scrollHeight = 100;
    this.id = '';
  }

  get className() { return this.classList.toString(); }
  set className(v) {
    this.classList.set = new Set(String(v || '').split(/\s+/).filter(Boolean));
  }

  get textContent() {
    if (this.children.length === 0) { return this._text; }
    return this._text + this.children.map((c) => c.textContent).join('');
  }
  set textContent(v) { this._text = String(v); this.children = []; }

  get innerHTML() { return this._html || ''; }
  set innerHTML(v) {
    this._html = String(v);
    if (this._html === '') { this.children = []; this._text = ''; }
  }

  get firstChild() { return this.children[0] || null; }
  get lastChild() { return this.children[this.children.length - 1] || null; }
  get childNodes() { return this.children; }

  appendChild(child) {
    if (child && child.parentNode && child.parentNode !== this) {
      child.parentNode.removeChild(child);
    }
    this.children.push(child);
    if (child) { child.parentNode = this; }
    return child;
  }

  removeChild(child) {
    const i = this.children.indexOf(child);
    if (i >= 0) { this.children.splice(i, 1); }
    if (child) { child.parentNode = null; }
    return child;
  }

  setAttribute(k, v) {
    this.attributes[k] = String(v);
    if (k === 'id') { this.id = String(v); }
  }
  getAttribute(k) { return Object.prototype.hasOwnProperty.call(this.attributes, k) ? this.attributes[k] : null; }
  hasAttribute(k) { return Object.prototype.hasOwnProperty.call(this.attributes, k); }

  addEventListener(type, fn) { (this.listeners[type] = this.listeners[type] || []).push(fn); }
  removeEventListener(type, fn) {
    const arr = this.listeners[type] || [];
    const i = arr.indexOf(fn);
    if (i >= 0) { arr.splice(i, 1); }
  }
  dispatch(type, event) {
    const ev = Object.assign({ type, target: this, preventDefault() {}, stopPropagation() {} }, event || {});
    (this.listeners[type] || []).forEach((fn) => fn(ev));
    return ev;
  }
  click() { return this.dispatch('click', {}); }
  focus() { doc.activeElement = this; }
  select() {}
  querySelector(sel) { return querySelector(this, sel); }

  get value() { return this._value; }
  set value(v) { this._value = String(v); }

  // 便捷：按 class 找后代（测试断言用）
  findByClass(cls) {
    const out = [];
    const walk = (node) => {
      for (const c of node.children) {
        if (c.classList && c.classList.contains(cls)) { out.push(c); }
        walk(c);
      }
    };
    walk(this);
    return out;
  }
}

function querySelector(root, sel) {
  const s = String(sel).trim();
  if (s.startsWith('.')) {
    const list = root.findByClass(s.slice(1));
    return list[0] || null;
  }
  if (s.startsWith('#')) {
    return findById(root, s.slice(1));
  }
  const tags = [];
  const walk = (n) => { for (const c of n.children) { tags.push(c); walk(c); } };
  walk(root);
  return tags.find((e) => e.tagName === s.toUpperCase()) || null;
}

function findById(root, id) {
  if (root.id === id) { return root; }
  for (const c of root.children) {
    const found = findById(c, id);
    if (found) { return found; }
  }
  return null;
}

/* ---------- 从真实 index.html 建 DOM ---------- */

const doc = new Element('#document');
doc.body = new Element('body');
doc.documentElement = new Element('html');
doc.activeElement = doc.body;
doc.readyState = 'complete';
doc.hidden = false;
doc.title = '';
doc.listeners = {};

const byId = new Map();
byId.set('body', doc.body);

// 用正则把 index.html 里的 id 抽出来，每个 id 建一个元素挂到 body 下。
// 足够覆盖 app.js 的 getElementById 用法。
for (const m of html.matchAll(/<([a-zA-Z][a-zA-Z0-9-]*)[^>]*\sid="([A-Za-z0-9_-]+)"[^>]*>/g)) {
  const tag = m[1];
  const id = m[2];
  const node = new Element(tag);
  node.id = id;
  byId.set(id, node);
  doc.body.appendChild(node);
}

// modalCode 里需要一个 <code> 子节点（app.js 会 querySelector('code')）
const modalCode = byId.get('modalCode');
if (modalCode) { modalCode.appendChild(new Element('code')); }
// 大按钮里需要一个 .btn-power-text 子节点
const powerBtn = byId.get('powerBtn');
if (powerBtn) {
  const span = new Element('span');
  span.className = 'btn-power-text';
  powerBtn.appendChild(span);
}
// 上传按钮的 lastChild 会被改成「上传中…」
const uploadBtn = byId.get('uploadBtn');
if (uploadBtn) { uploadBtn.appendChild(new Element('span')); }

doc.getElementById = (id) => byId.get(id) || null;
doc.createElement = (tag) => new Element(tag);
doc.createTextNode = (text) => {
  const node = new Element('#text');
  node._text = String(text);
  return node;
};
doc.createElementNS = (ns, tag) => new Element(tag);
doc.addEventListener = (type, fn) => { (doc.listeners[type] = doc.listeners[type] || []).push(fn); };
doc.removeEventListener = () => {};
doc.dispatch = (type, event) => {
  (doc.listeners[type] || []).forEach((fn) => fn(Object.assign({ type }, event || {})));
};
doc.execCommand = () => true;
doc.body.style = {};

/* ---------- fetch / EventSource / window ---------- */

const fixture = {
  status: {
    running: true, serverIP: '192.168.123.10', dhcpMode: 'proxy', startedAt: new Date(Date.now() - 600000).toISOString(),
    uptimeSec: 600, clientCount: 1, isoCount: 1, httpPort: 16000, tftpPort: 69, webPort: 16080,
    version: '1.0.0', hostname: 'test-host', os: 'darwin', arch: 'arm64',
    bootFileBIOS: 'undionly.kpxe', bootFileUEFI: 'ipxe.efi',
  },
  settings: {
    serverIP: '192.168.123.10', interface: '', rangeStart: '192.168.123.200', rangeEnd: '192.168.123.250',
    netmask: '255.255.255.0', gateway: '192.168.123.1', dhcpMode: 'proxy', httpPort: 16000,
    tftpPort: 69, webPort: 16080, bootMenuTimeout: 30, defaultISO: 'iso-a', language: 'zh-CN', dns: ['1.1.1.1'],
  },
  isos: [{
    id: 'iso-a', name: 'ubuntu-24.04.iso', osName: 'Ubuntu 24.04 LTS', family: 'linux',
    osArch: 'uefi64', size: 3200000000, sizeText: '3.0 GB', archText: 'UEFI x86_64',
    bootable: true, default: true, boot: { method: 'kernel' },
  }],
  clients: [{
    mac: 'aa:bb:cc:dd:ee:01', ip: '192.168.123.201', hostname: 'lab-node-1', arch: 'uefi64',
    state: 'booting', selected: 'iso-a', bootCount: 2,
    lastSeen: new Date(Date.now() - 5000).toISOString(),
  }],
  events: [],
  interfaces: [{ name: 'en0', ip: '192.168.123.10', mask: '255.255.255.0', up: true, default: true }],
  version: { version: '1.0.0', name: 'NetBootX', goVersion: 'go1.27', os: 'darwin', arch: 'arm64' },
};

const now = Date.now();
const eventKinds = [
  ['info', 'system', 'PXE 服务已启动'],
  ['info', 'dhcp', 'DHCP DISCOVER 来自 aa:bb:cc:dd:ee:01'],
  ['info', 'tftp', 'TFTP RRQ ipxe.efi 传输完成'],
  ['warn', 'http', 'HTTP 客户端提前断开'],
  ['error', 'boot', '客户机引导失败'],
];
for (let i = 0; i < eventKinds.length; i++) {
  fixture.events.push({
    time: new Date(now - (eventKinds.length - i) * 1000).toISOString(),
    level: eventKinds[i][0], kind: eventKinds[i][1], msg: eventKinds[i][2],
    mac: i === 1 ? 'aa:bb:cc:dd:ee:01' : '',
  });
}

const fetched = [];
function makeFetch() {
  return (url) => {
    fetched.push(url);
    const p = String(url).split('?')[0];
    let data = null;
    if (p === '/api/status') { data = { status: fixture.status, settings: fixture.settings, defaultISO: 'iso-a', version: '1.0.0' }; }
    else if (p === '/api/isos') { data = { isos: fixture.isos, count: 1, defaultISO: 'iso-a' }; }
    else if (p === '/api/clients') { data = { clients: fixture.clients, count: 1 }; }
    else if (p === '/api/events') { data = { events: fixture.events, count: fixture.events.length, limit: 200 }; }
    else if (p === '/api/settings') { data = fixture.settings; }
    else if (p === '/api/interfaces') { data = { interfaces: fixture.interfaces, count: 1 }; }
    else if (p === '/api/version') { data = fixture.version; }
    else if (p.startsWith('/api/preview/')) { data = { isoID: 'iso-a', source: 'pxe', script: '#!ipxe\necho hi\nboot\n' }; }
    if (data === null) {
      return Promise.resolve({ ok: false, status: 404, statusText: 'Not Found', text: () => Promise.resolve('{"ok":false,"error":"not found"}') });
    }
    const body = JSON.stringify({ ok: true, data });
    return Promise.resolve({ ok: true, status: 200, statusText: 'OK', text: () => Promise.resolve(body) });
  };
}

class FakeEventSource {
  constructor(url) {
    this.url = url;
    this.listeners = {};
    FakeEventSource.instances.push(this);
  }
  addEventListener(type, fn) { (this.listeners[type] = this.listeners[type] || []).push(fn); }
  close() { this.closed = true; }
  emit(type, data) {
    (this.listeners[type] || []).forEach((fn) => fn({ type, data: JSON.stringify(data) }));
  }
  emitRaw(type, raw) {
    (this.listeners[type] || []).forEach((fn) => fn({ type, data: raw }));
  }
}
FakeEventSource.instances = [];

const timers = [];
const win = {
  setTimeout: (fn) => { timers.push(fn); return timers.length; },
  clearTimeout: () => {},
  setInterval: () => 0,
  clearInterval: () => {},
  addEventListener: () => {},
  removeEventListener: () => {},
  EventSource: FakeEventSource,
  FormData: class { append() {} },
  XMLHttpRequest: class { open() {} addEventListener() {} send() {} },
};

const sandbox = {
  document: doc,
  window: win,
  navigator: { clipboard: null },
  fetch: makeFetch(),
  EventSource: FakeEventSource,
  console,
  setTimeout: win.setTimeout,
  clearTimeout: win.clearTimeout,
  setInterval: win.setInterval,
  clearInterval: win.clearInterval,
  Date,
  Math,
  JSON,
  Object,
  Array,
  String,
  Number,
  Boolean,
  RegExp,
  Error,
  isNaN,
  parseInt,
  parseFloat,
};
sandbox.globalThis = sandbox;
sandbox.self = sandbox;

/* ---------- 执行 app.js ---------- */

vm.createContext(sandbox);
vm.runInContext(appJS, sandbox, { filename: 'app.js' });

// 等待所有 fetch 的 Promise 链跑完
await new Promise((resolve) => setTimeout(resolve, 60));

console.log('\n[1] 初始加载（REST）');

const logList = byId.get('logList');
const logCount = byId.get('logCount');
const clientList = byId.get('clientList');
const isoList = byId.get('isoList');

check(fetched.includes('/api/events?limit=200') || fetched.some((u) => u.startsWith('/api/events')), '启动时请求了 /api/events');
check(fetched.includes('/api/clients'), '启动时请求了 /api/clients');
check(fetched.includes('/api/isos'), '启动时请求了 /api/isos');
check(FakeEventSource.instances.length === 1, '建立了 1 个 SSE 连接');
check(FakeEventSource.instances[0].url === '/api/events/stream', 'SSE 连的是 /api/events/stream');

check(logList.findByClass('log-row').length === fixture.events.length,
  '日志面板渲染出 ' + fixture.events.length + ' 行（实际 ' + logList.findByClass('log-row').length + ' 行）');
check(logCount.textContent === String(fixture.events.length),
  '日志计数显示 ' + fixture.events.length + '（实际 ' + logCount.textContent + '）');
check(logList.findByClass('log-msg').length > 0, '日志正文节点存在');
check(logList.findByClass('empty').length === 0, '日志面板没有错误地显示空状态');
check(clientList.findByClass('client-item').length === 1, '客户机列表渲染出 1 项');
check(isoList.findByClass('iso-item').length === 1, 'ISO 列表渲染出 1 项');

console.log('\n[2] SSE 快照（关键回归：快照不能把日志清空）');

const es = FakeEventSource.instances[0];
es.emit('open', {});
es.emit('snapshot', {
  status: fixture.status,
  clients: fixture.clients,
  isos: fixture.isos,
  events: fixture.events,
  defaultISO: 'iso-a',
  interfaces: fixture.interfaces,
  version: '1.0.0',
});
await new Promise((r) => setTimeout(r, 20));

check(logList.findByClass('log-row').length === fixture.events.length,
  '快照之后日志仍有 ' + fixture.events.length + ' 行（实际 ' + logList.findByClass('log-row').length + ' 行）');
check(logCount.textContent === String(fixture.events.length), '快照之后日志计数仍正确');

console.log('\n[3] SSE 增量事件（events 追加）');

es.emit('events', [{
  time: new Date().toISOString(), level: 'info', kind: 'dhcp', msg: '增量事件：DHCP OFFER 已下发', mac: 'aa:bb:cc:dd:ee:01',
}]);
await new Promise((r) => setTimeout(r, 20));

const rowsAfter = logList.findByClass('log-row').length;
check(rowsAfter === fixture.events.length + 1, '增量事件追加了 1 行（实际 ' + rowsAfter + ' 行）');
const msgs = logList.findByClass('log-msg').map((n) => n.textContent);
check(msgs.some((m) => m.includes('增量事件')), '增量事件内容出现在日志面板');

console.log('\n[4] 状态与客户机增量');

es.emit('status', Object.assign({}, fixture.status, { running: false, uptimeSec: 0 }));
await new Promise((r) => setTimeout(r, 10));
check(byId.get('statusText').textContent === '已停止', '状态文字变为「已停止」');
check(byId.get('powerBtn').getAttribute('data-state') === 'stopped', '启动按钮切到 stopped 状态');

es.emit('clients', []);
await new Promise((r) => setTimeout(r, 10));
check(clientList.findByClass('client-item').length === 0, '客户机清空后列表为空');
check(clientList.findByClass('empty').length === 1, '客户机为空时显示空状态');

console.log('\n[5] 网卡下拉框（回归：网卡名为空时不能出现「?」）');

const ifaceSelect = byId.get('ifaceSelect');
const optionTexts = ifaceSelect.children.map((o) => o.textContent);
check(!optionTexts.some((t) => t.includes('?')), '下拉框里没有「?」占位符：' + JSON.stringify(optionTexts));
check(optionTexts.some((t) => t.includes('192.168.123.10')), '下拉框包含当前 IP');

console.log('\n[6] 清空日志按钮');

byId.get('clearLogsBtn').dispatch('click', {});
await new Promise((r) => setTimeout(r, 10));
check(logList.findByClass('log-row').length === 0, '点击清空后日志为空');
check(logCount.textContent === '0', '点击清空后计数为 0');


console.log('\n[7] 客户机「最后活跃」时间文案的边界');

const timeCases = [
  [0, '刚刚'],
  [5, '刚刚'],
  [9, '刚刚'],
  [10, '10 秒前'],
  [59, '59 秒前'],
  [60, '1 分钟前'],
  [125, '2 分钟前'],
  [3599, '59 分钟前'],
  [3600, '1 小时前'],
  [7200, '2 小时前'],
  [86399, '23 小时前'],
  [86400, '1 天前'],
  [86400 * 301, '301 天前'],
];
const timeClients = timeCases.map((c, i) => ({
  mac: 'aa:bb:cc:dd:ee:' + String(i).padStart(2, '0'),
  ip: '192.168.123.' + (200 + i),
  hostname: 'node-' + i,
  arch: 'uefi64',
  state: 'dhcp',
  lastSeen: new Date(Date.now() - c[0] * 1000).toISOString(),
}));
es.emit('clients', timeClients);
await new Promise((r) => setTimeout(r, 10));

const renderedTimes = clientList.findByClass('client-time').map((n) => n.textContent);
let timeBad = 0;
for (let i = 0; i < timeCases.length; i++) {
  if (renderedTimes[i] !== timeCases[i][1]) {
    timeBad++;
    console.error('  ✗ lastSeen=' + timeCases[i][0] + 's 渲染成「' + renderedTimes[i] + '」，期望「' + timeCases[i][1] + '」');
  }
}
check(timeBad === 0, '「刚刚 / N 秒前 / N 分钟前 / N 小时前 / N 天前」边界全部正确（' + timeCases.length + ' 个用例）');

console.log('');
if (failures > 0) {
  console.error('❌ 前端 DOM 测试失败：' + failures + ' 项');
  process.exit(1);
}
console.log('✅ 前端 DOM 测试全部通过');
