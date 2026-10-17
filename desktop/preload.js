// The renderer is the same SPA the browser serves, so it needs almost nothing from
// the shell. Keep this surface tiny: every exposed helper is a hole in
// contextIsolation.
const { contextBridge, ipcRenderer } = require('electron');

contextBridge.exposeInMainWorld('agenthubDesktop', {
  platform: process.platform,
  // Fired when the backend process dies; the UI can show a single banner instead
  // of failing on every request.
  onBackendExited: (cb) => {
    ipcRenderer.on('backend-exited', (_e, code) => cb(code));
  },
});