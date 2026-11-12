import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// The panel serves the build under a configurable admin path and injects
// window.__POLARIS_BASE__, so assets use relative URLs.
export default defineConfig({
  base: './',
  plugins: [react()],
  build: {
    outDir: 'dist',
    // Batch1's npm prebuild already clears dist/* (rm -rf dist/* keeps
    // the tracked dist/.keep dotfile that go:embed needs); emptyOutDir
    // stays false for the same reason.
    emptyOutDir: false,
    chunkSizeWarningLimit: 800,
    rollupOptions: {
      output: {
        // Split the framework out of the app chunks: react/react-dom/
        // react-router-dom change far less often than panel code, so the
        // vendor chunk stays cacheable across releases. The English
        // dictionary files are already split by dynamic imports in
        // i18n.ts; do not merge them back into a vendor chunk here.
        manualChunks(id) {
          if (id.includes('node_modules/react-dom') || id.includes('node_modules/react-router') || id.includes('node_modules/react/')) return 'vendor-react'
          if (id.includes('node_modules/qrcode')) return 'vendor-qrcode'
          return undefined
        },
      },
    },
  },
  server: {
    proxy: { '/api': 'http://127.0.0.1:8080', '^/sub/': 'http://127.0.0.1:8080' },
  },
})
