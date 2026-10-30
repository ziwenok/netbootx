// 用真实 Chrome 截图 NetBootX 界面，用于验证前端渲染效果。
// 用法: node scripts/shot.mjs <url> <outfile> [width] [height]
import puppeteer from '/Users/wupengcheng/Documents/deepseek-harness/default-workspace/work/harness/node_modules/puppeteer-core/lib/esm/puppeteer/puppeteer-core.js';

const url = process.argv[2] || 'http://127.0.0.1:16080';
const out = process.argv[3] || '/tmp/nbx-shot.png';
const width = parseInt(process.argv[4] || '1440', 10);
const height = parseInt(process.argv[5] || '900', 10);

const browser = await puppeteer.launch({
  executablePath: '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
  headless: 'new',
  args: ['--no-sandbox', '--disable-dev-shm-usage', '--force-color-profile=srgb'],
});
const page = await browser.newPage();
await page.setViewport({ width, height, deviceScaleFactor: 2 });

const errors = [];
page.on('console', (m) => { if (m.type() === 'error') errors.push('console: ' + m.text()); });
page.on('pageerror', (e) => errors.push('pageerror: ' + e.message));
page.on('requestfailed', (r) => errors.push('reqfail: ' + r.url() + ' ' + r.failure()?.errorText));

await page.goto(url, { waitUntil: 'networkidle2', timeout: 30000 });
await new Promise((r) => setTimeout(r, 2500)); // 等 SSE 推一次数据

await page.screenshot({ path: out, fullPage: false });
console.log('screenshot ->', out);
if (errors.length) {
  console.log('--- 页面错误 ---');
  for (const e of errors) console.log('  ' + e);
} else {
  console.log('无页面错误 ✅');
}

// 顺便把关键文本抓出来，确认数据渲染了
const txt = await page.evaluate(() => document.body.innerText.slice(0, 800));
console.log('--- 页面文本片段 ---');
console.log(txt);

await browser.close();
