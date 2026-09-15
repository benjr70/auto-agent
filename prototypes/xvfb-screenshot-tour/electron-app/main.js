// PROTOTYPE Electron shell: one 800x480 window loading FIXTURE_WEB_URL, CDP from argv.
const { app, BrowserWindow } = require('electron');
app.whenReady().then(() => {
  const w = new BrowserWindow({ width: 800, height: 480, useContentSize: true, frame: false });
  w.loadURL(process.env.FIXTURE_WEB_URL || 'about:blank');
});
app.on('window-all-closed', () => app.quit());
