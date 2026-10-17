// Electron shell for the Go backend.
//
// The backend is the product; this process only hosts it and shows its UI. Two
// rules shape the code below: the shell must never guess the backend URL (the
// backend publishes data/runtime.json after picking a free port), and the child
// process must not outlive the window (a stray backend keeps holding the vault).
const { app, BrowserWindow, Tray, Menu, shell, dialog } = require('electron');
const { spawn } = require('child_process');
const fs = require('fs');
const path = require('path');

let mainWindow = null;
let tray = null;
let backend = null;
let quitting = false;

const isDev = !app.isPackaged;

// The packaged app carries the backend as an extra resource; during development it
// sits in the sibling checkout.
function backendPath() {
  return isDev
    ? path.join(__dirname, '..', 'backend', 'agenthub.exe')
    : path.join(process.resourcesPath, 'backend', 'agenthub.exe');
}

// The backend decides where data lives (it defaults to %APPDATA%/AgentHub/data),
// so the shell asks it instead of duplicating the rule.
function dataDir() {
  return path.join(app.getPath('appData'), 'AgentHub', 'data');
}

function runtimeFile() {
  return path.join(dataDir(), 'runtime.json');
}

function startBackend() {
  const exe = backendPath();
  if (!fs.existsSync(exe)) {
    dialog.showErrorBox('缺少后端', '未找到 backend/agenthub.exe，请先运行 build.ps1 打包后端。');
    app.quit();
    return;
  }
  // A stale runtime file would make the window load a dead port.
  try { fs.unlinkSync(runtimeFile()); } catch (_) {}

  backend = spawn(exe, ['--addr', '127.0.0.1:8787'], {
    cwd: path.dirname(exe),
    env: { ...process.env, AGENTHUB_NO_SYSTRAY: '1' },
    stdio: 'ignore',
    windowsHide: true,
  });
  backend.on('exit', (code) => {
    backend = null;
    if (!quitting) {
      mainWindow?.webContents.send('backend-exited', code);
    }
  });
}

// Poll for the file the backend writes once it is listening. Ten seconds is
// generous for a cold start on a spinning disk, and short enough that a broken
// backend shows an error instead of an empty window.
async function waitForRuntime(timeoutMs = 15000) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    try {
      const raw = fs.readFileSync(runtimeFile(), 'utf8');
      const info = JSON.parse(raw);
      if (info && info.url) return info.url;
    } catch (_) { /* not written yet */ }
    await new Promise((r) => setTimeout(r, 250));
  }
  return null;
}

async function createWindow() {
  mainWindow = new BrowserWindow({
    width: 1280,
    height: 860,
    minWidth: 960,
    minHeight: 640,
    backgroundColor: '#111214',
    title: 'AgentHub',
    webPreferences: {
      preload: path.join(__dirname, 'preload.js'),
      contextIsolation: true,
      nodeIntegration: false,
      sandbox: true,
    },
  });

  const url = await waitForRuntime();
  if (!url) {
    dialog.showErrorBox('后端未启动', '等待后端超时。请查看日志目录下的 agenthub.log。');
    app.quit();
    return;
  }
  await mainWindow.loadURL(url);
  mainWindow.on('closed', () => { mainWindow = null; });
  // External links open in the real browser, not inside the app shell.
  mainWindow.webContents.setWindowOpenHandler(({ url: target }) => {
    shell.openExternal(target);
    return { action: 'deny' };
  });
}

function showWindow() {
  if (!mainWindow) { createWindow(); return; }
  if (mainWindow.isMinimized()) mainWindow.restore();
  mainWindow.show();
  mainWindow.focus();
}

function buildTray() {
  tray = new Tray(path.join(__dirname, 'build', 'icon.ico'));
  tray.setToolTip('AgentHub');
  tray.setContextMenu(Menu.buildFromTemplate([
    { label: '打开主窗口', click: showWindow },
    { label: '打开数据目录', click: () => shell.openPath(dataDir()) },
    { type: 'separator' },
    { label: '退出', click: () => { quitting = true; app.quit(); } },
  ]));
  tray.on('double-click', showWindow);
}

// One instance only: a second launch focuses the existing window instead of
// starting a second backend against the same database.
if (!app.requestSingleInstanceLock()) {
  app.quit();
} else {
  app.on('second-instance', showWindow);

  app.whenReady().then(() => {
    startBackend();
    createWindow();
    buildTray();
  });

  app.on('window-all-closed', () => {
    // Closing the window keeps the app in the tray, like other desktop agents.
    if (process.platform !== 'darwin' && !tray) app.quit();
  });

  app.on('before-quit', () => { quitting = true; });

  app.on('will-quit', () => {
    if (backend) {
      try { backend.kill(); } catch (_) {}
      backend = null;
    }
  });
}