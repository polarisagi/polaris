import { defineConfig } from 'vite'
import tailwindcss from '@tailwindcss/vite'
import htmlFragments from './vite-plugin-fragments.js'

export default defineConfig({
  plugins: [htmlFragments(), tailwindcss()],
  root: 'src',
  publicDir: '../public',
  build: {
    outDir: '../dist',
    emptyOutDir: true,
    rollupOptions: {
      input: {
        main: 'src/index.html',
      },
    },
  },
  server: {
    port: 5173,
    proxy: {
      '/v1': 'http://localhost:28888',
      '/healthz': 'http://localhost:28888',
      '/metrics': 'http://localhost:28888',
    },
  },
})
