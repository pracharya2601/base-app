import { defineConfig } from 'vite'
import { svelte } from '@sveltejs/vite-plugin-svelte'

// The console is served under /admin by the Go binary (apis.Static over the
// embedded build output), so assets must be referenced from /admin/assets/...
export default defineConfig({
  plugins: [svelte()],
  base: '/admin/',
  build: {
    outDir: '../internal/adminui/spa',
    emptyOutDir: true,
  },
})
