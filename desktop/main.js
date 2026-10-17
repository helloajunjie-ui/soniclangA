// Electron shell for the Go backend.
//
// Two rules shape this file: the shell never guesses the backend URL (the backend
// publishes data/runtime.json after picking a free port), and every failure has to
// be visible — an agent app that starts and silently shows nothing is impossible to
// diagnose from the outside, so each step is logged to userData/desktop.log and a
// failed start reports where that log is.
const { app, BrowserWindow, Tray, Menu, shell, dialog } = require('electron');
const { spawn } = require('child_process');
const fs = require('fs');
const path = require('path');

let mainWindow = null;
let tray = null;
let backend = null;
let quitting = false;
let logFile = null;

const isDev = !app.isPackaged;

function log(...parts) {
  const line = new Date().toISOString() + ' ' + parts.join(' ');
  try { console.log(line); } catch (_) {}
  try {
    if (!logFile) {
      logFile = path.join(app.getPath('userData'), 'desktop.log');
      fs.mkdirSync(path.dirname(logFile), { recursive: true });
    }
    fs.appendFileSync(logFile, line + '\n');
  } catch (_) { /* logging must never break startup */ }
}

function backendPath() {
  return isDev
    ? path.join(__dirname, '..', 'backend', 'agenthub.exe')
    : path.join(process.resourcesPath, 'backend', 'agenthub.exe');
}

// The backend owns the data directory rule (it defaults to %APPDATA%/AgentHub/data),
// so the shell reads its address from there instead of duplicating the layout.
function dataDir() {
  return path.join(app.getPath('appData'), 'AgentHub', 'data');
}
function runtimeFile() {
  return path.join(dataDir(), 'runtime.json');
}

function fail(title, detail) {
  log('FATAL', title, detail || '');
  try {
    dialog.showErrorBox(title, (detail || '') + '\n\n日志：' + (logFile || '(日志未创建)'));
  } catch (_) {}
  quitting = true;
  app.quit();
}

function startBackend() {
  const exe = backendPath();
  log('backend path =', exe, 'exists =', fs.existsSync(exe));
  if (!fs.existsSync(exe)) {
    fail('缺少后端', '未找到 ' + exe + '\n请先运行 build.ps1 打包后端。');
    return;
  }
  try { fs.unlinkSync(runtimeFile()); } catch (_) {}

  backend = spawn(exe, ['--addr', '127.0.0.1:8787'], {
    cwd: path.dirname(exe),
    env: { ...process.env, AGENTHUB_NO_SYSTRAY: '1' },
    stdio: ['ignore', 'pipe', 'pipe'],
    windowsHide: true,
  });
  // Without these two listeners a spawn failure is an unhandled event: the process
  // dies with no message and no window, which is exactly how a desktop app "runs but
  // shows nothing".
  backend.on('error', (err) => fail('后端启动失败', String(err)));
  backend.stdout.on('data', (b) => log('[backend]', b.toString().trim()));
  backend.stderr.on('data', (b) => log('[backend:err]', b.toString().trim()));
  backend.on('exit', (code, signal) => {
    log('backend exited code =', code, 'signal =', signal);
    backend = null;
    if (!quitting) {
      mainWindow?.webContents.send('backend-exited', code);
    }
  });
}

async function waitForRuntime(timeoutMs = 20000) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    try {
      const info = JSON.parse(fs.readFileSync(runtimeFile(), 'utf8'));
      if (info && info.url) {
        log('backend url =', info.url);
        return info.url;
      }
    } catch (_) { /* not written yet */ }
    await new Promise((r) => setTimeout(r, 250));
  }
  return null;
}

async function createWindow() {
  log('creating window');
  mainWindow = new BrowserWindow({
    width: 1280,
    height: 860,
    minWidth: 960,
    minHeight: 640,
    backgroundColor: '#111214',
    title: 'AgentHub',
    show: false,
    webPreferences: {
      preload: path.join(__dirname, 'preload.js'),
      contextIsolation: true,
      nodeIntegration: false,
      sandbox: true,
    },
  });
  mainWindow.once('ready-to-show', () => {
    log('window ready, showing');
    mainWindow.show();
    mainWindow.focus();
  });
  mainWindow.webContents.on('did-fail-load', (_e, code, desc, url) => {
    log('did-fail-load', code, desc, url);
  });

  const url = await waitForRuntime();
  if (!url) {
    fail('后端未启动', '等待 ' + runtimeFile() + ' 超时。后端可能启动失败。');
    return;
  }
  try {
    await mainWindow.loadURL(url);
    log('loaded', url);
  } catch (err) {
    fail('界面加载失败', String(err));
    return;
  }
  mainWindow.on('closed', () => { mainWindow = null; });
  mainWindow.webContents.setWindowOpenHandler(({ url: target }) => {
    shell.openExternal(target);
    return { action: 'deny' };
  });
  // A hidden window that never becomes ready would leave the app "running" with
  // nothing on screen; show it after a short grace period regardless.
  setTimeout(() => {
    if (mainWindow && !mainWindow.isDestroyed() && !mainWindow.isVisible()) {
      log('window still hidden after load, forcing show');
      mainWindow.show();
    }
  }, 4000);
}

function showWindow() {
  if (!mainWindow) { createWindow(); return; }
  if (mainWindow.isMinimized()) mainWindow.restore();
  mainWindow.show();
  mainWindow.focus();
}

function buildTray() {
  try {
    tray = new Tray(path.join(__dirname, 'build', 'icon.ico'));
    tray.setToolTip('AgentHub');
    tray.setContextMenu(Menu.buildFromTemplate([
      { label: '打开主窗口', click: showWindow },
      { label: '打开数据目录', click: () => shell.openPath(dataDir()) },
      { label: '打开日志', click: () => shell.openPath(logFile || app.getPath('userData')) },
      { type: 'separator' },
      { label: '退出', click: () => { quitting = true; app.quit(); } },
    ]));
    tray.on('double-click', showWindow);
    log('tray created');
  } catch (err) {
    // A broken tray icon must not take the window down with it.
    log('tray failed:', String(err));
    tray = null;
  }
}

log('=== AgentHub desktop starting ===', 'dev =', isDev);

if (!app.requestSingleInstanceLock()) {
  log('another instance is running; focusing it and exiting');
  app.quit();
} else {
  app.on('second-instance', showWindow);

  app.whenReady().then(() => {
    log('app ready');
    startBackend();
    createWindow();
    buildTray();
  }).catch((err) => fail('启动失败', String(err)));

  app.on('window-all-closed', () => {
    log('all windows closed, tray =', !!tray);
    if (process.platform !== 'darwin' && !tray) app.quit();
  });

  process.on('uncaughtException', (err) => fail('未捕获异常', String(err && err.stack || err)));

  app.on('before-quit', () => { quitting = true; });
  app.on('will-quit', () => {
    if (backend) {
      log('killing backend');
      try { backend.kill(); } catch (_) {}
      backend = null;
    }
  });
}