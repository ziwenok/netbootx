// NetBootX 界面交互测试：用真实 Chrome 点按钮、开弹窗、验状态。
import puppeteer from '/Users/wupengcheng/Documents/deepseek-harness/default-workspace/work/harness/node_modules/puppeteer-core/lib/esm/puppeteer/puppeteer-core.js';

const url = process.argv[2] || 'http://127.0.0.1:16080';
const browser = await puppeteer.launch({
  executablePath: '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
  headless: 'new',
  args: ['--no-sandbox', '--disable-dev-shm-usage'],
});
const page = await browser.newPage();
await page.setViewport({ width: 1440, height: 900, deviceScaleFactor: 1 });

const errors = [];
page.on('pageerror', (e) => errors.push('pageerror: ' + e.message));
page.on('console', (m) => { if (m.type() === 'error') errors.push('console: ' + m.text()); });

await page.goto(url, { waitUntil: 'networkidle2', timeout: 30000 });
await new Promise((r) => setTimeout(r, 2500));

const results = [];
function check(name, pass, detail) {
  results.push({ name, pass, detail });
  console.log((pass ? '✅' : '❌') + ' ' + name + (detail ? '  — ' + detail : ''));
}

// 1. 日志面板有内容
const logRows = await page.$$eval('#logList .log-row', (n) => n.length).catch(() => 0);
check('日志面板渲染事件', logRows > 0, logRows + ' 条');

// 2. 日志按级别着色
const levels = await page.$$eval('#logList .log-row', (rows) =>
  [...new Set(rows.map((r) => r.getAttribute('data-level')))]).catch(() => []);
check('日志行带 level 属性', levels.length > 0, levels.join(','));

// 3. ISO 列表渲染
const isoRows = await page.$$eval('#isoList > *', (n) => n.length).catch(() => 0);
check('ISO 列表渲染', isoRows > 0, isoRows + ' 项');

// 4. 客户机列表渲染
const clientRows = await page.$$eval('#clientList > *', (n) => n.length).catch(() => 0);
check('客户机列表渲染', clientRows > 0, clientRows + ' 项');

// 5. 点「预览」打开脚本弹窗
let previewOK = false, previewText = '';
try {
  const btns = await page.$$('#isoList button');
  for (const b of btns) {
    const t = await page.evaluate((el) => el.textContent, b);
    if (t && t.includes('预览')) { await b.click(); break; }
  }
  await new Promise((r) => setTimeout(r, 1200));
  previewText = await page.evaluate(() => document.body.innerText);
  previewOK = previewText.includes('#!ipxe') || previewText.includes('ipxe');
} catch (e) { errors.push('preview click: ' + e.message); }
check('预览弹窗显示 iPXE 脚本', previewOK, previewOK ? '#!ipxe 出现' : '未找到');

// 关掉弹窗
await page.keyboard.press('Escape');
await new Promise((r) => setTimeout(r, 600));

// 6. SSE 状态指示
const conn = await page.evaluate(() => {
  const el = document.querySelector('[class*=conn], [id*=conn]');
  return el ? el.textContent.trim() : '';
});
check('SSE 连接状态可见', conn.includes('已连接') || conn.includes('实时'), conn);

// 7. 无 JS 错误
check('无 JavaScript 错误', errors.length === 0, errors.slice(0, 3).join(' | ') || '无');

// 8. 无外部网络请求（离线可用）
const externals = [];
page.on('request', (r) => {
  const u = r.url();
  if (!u.startsWith(url) && !u.startsWith('data:') && !u.startsWith('blob:')) externals.push(u);
});
await page.reload({ waitUntil: 'networkidle2' });
await new Promise((r) => setTimeout(r, 1500));
check('无外部依赖请求', externals.length === 0, externals.slice(0, 3).join(', ') || '全部本地');

console.log('');
const failed = results.filter((r) => !r.pass);
console.log(failed.length === 0 ? '全部通过 ✅' : (failed.length + ' 项失败 ❌'));
await browser.close();
process.exit(failed.length === 0 ? 0 : 1);
