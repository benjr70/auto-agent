// PROTOTYPE tour: what the harness screenshot tour does, minus the MCP layer.
// browser surface: headed Chrome via playwright-core at 427x952 (viewport-clipped shots).
// electron surface: connect over CDP to the running Electron shell and shoot the page.
// env: OUT_DIR, FIXTURE_WEB_URL, CHROME_BIN, CDP_URL, CHROME_SANDBOX=1 to keep Chrome's sandbox on.
const fs = require('fs'); const path = require('path');
const { chromium } = require('playwright-core'); const WebSocket = globalThis.WebSocket || require('ws');
const OUT = process.env.OUT_DIR; fs.mkdirSync(OUT, { recursive: true });
const url = process.env.FIXTURE_WEB_URL;
const note = (k, v) => { fs.appendFileSync(path.join(OUT, 'tour.txt'), `${k}=${v}\n`); console.log(`${k}=${v}`); };
(async () => {
  const browser = await chromium.launch({ headless: false, executablePath: process.env.CHROME_BIN,
    chromiumSandbox: process.env.CHROME_SANDBOX === '1' });
  note('CHROME_VERSION', browser.version());
  const ctx = await browser.newContext({ viewport: { width: 427, height: 952 }, deviceScaleFactor: 1 });
  const page = await ctx.newPage(); await page.goto(url); await page.waitForTimeout(500);
  await page.screenshot({ path: path.join(OUT, 'browser-01-home.png') });
  await page.evaluate(() => window.scrollTo(0, 900)); await page.waitForTimeout(200);
  await page.screenshot({ path: path.join(OUT, 'browser-02-scrolled.png') });
  note('BROWSER_FONT', await page.evaluate(() => getComputedStyle(document.body).fontFamily));
  note('BROWSER_DPR', await page.evaluate(() => window.devicePixelRatio));
  note('BROWSER_INNER', await page.evaluate(() => `${innerWidth}x${innerHeight}`));
  await browser.close();
  if (process.env.CDP_URL) {
    // Raw CDP: playwright's connectOverCDP rejects Electron's browser target (no context management).
    const list = await (await fetch(process.env.CDP_URL + '/json/list')).json();
    const target = list.find(t => t.type === 'page');
    const ws = new WebSocket(target.webSocketDebuggerUrl); let id = 0; const pending = new Map();
    await new Promise(r => ws.onopen = r);
    ws.onmessage = ev => { const m = JSON.parse(ev.data); if (pending.has(m.id)) { pending.get(m.id)(m.result); pending.delete(m.id); } };
    const cdp = (method, params = {}) => new Promise(res => { ws.send(JSON.stringify({ id: ++id, method, params })); pending.set(id, res); });
    const evalJs = async expr => (await cdp('Runtime.evaluate', { expression: expr, returnByValue: true })).result.value;
    await new Promise(r => setTimeout(r, 800));
    const shot = await cdp('Page.captureScreenshot', { format: 'png' });
    fs.writeFileSync(path.join(OUT, 'electron-01-home.png'), Buffer.from(shot.data, 'base64'));
    note('ELECTRON_INNER', await evalJs('`${innerWidth}x${innerHeight}`'));
    note('ELECTRON_DPR', await evalJs('window.devicePixelRatio'));
    note('ELECTRON_UA', await evalJs('navigator.userAgent.match(/Electron\\/[\\d.]+/)[0]'));
    ws.close();
  }
})().catch(e => { note('TOUR_ERROR', e.message.split('\n')[0]); process.exit(1); });
