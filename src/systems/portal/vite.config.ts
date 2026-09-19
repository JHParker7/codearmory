import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';

// The SPA can be served from a sub-path (e.g. behind an ingress that routes /app to the
// portal). Vite bakes `base` into every emitted asset URL at BUILD time, so this cannot
// be an ingress rewrite: index.html would load, then every /assets/* request would 404
// because the browser asks for the path the bundle was built with. PORTAL_BASE_PATH must
// therefore be set for the build, not just the runtime — the Dockerfile passes it through
// as a build arg, and portal-bff strips the same prefix when serving.
//
// Empty (the default) keeps the SPA at "/", which is what local `vite dev` and any
// root-mounted deployment want.
const basePath = (process.env.PORTAL_BASE_PATH || '').replace(/\/+$/, '');

export default defineConfig({
  base: basePath ? `${basePath}/` : '/',
  plugins: [react()],
  server: {
    port: 3001,
    proxy: {
      // In dev, proxy /api/* to the BFF. No path rewrite — BFF mounts at /api.
      '/api': {
        target: 'http://localhost:3002',
        changeOrigin: true,
      },
    },
  },
});
