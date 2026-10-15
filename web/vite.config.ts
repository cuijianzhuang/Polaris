import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// The panel serves the build under a configurable admin path and injects
// window.__POLARIS_BASE__, so assets use relative URLs.
export default defineConfig({
  base: './',
  plugins: [react()],
  build: { outDir: 'dist', emptyOutDir: false, chunkSizeWarningLimit: 800 },
  server: {
    proxy: { '/api': 'http://127.0.0.1:8080', '/sub': 'http://127.0.0.1:8080' },
  },
})
