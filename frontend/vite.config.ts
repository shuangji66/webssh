import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'
import tailwindcss from '@tailwindcss/vite'
import { fileURLToPath, URL } from 'node:url'

// 前端产物会被 //go:embed 打进 Go 二进制，在 unix socket 上以 baseurl 前缀提供服务。
// baseurl 在构建期**未知**（运行时由 WEBSSH_ADMIN_BASEURL 决定），因此必须用相对 base
// （./assets/...）构建，真实前缀由后端在响应 index.html 时注入 <base href>。
// 详见 docs/CONTRACT.md 的「资源与路径」一节——不要把 baseurl 烧进产物。
export default defineConfig({
  plugins: [vue(), tailwindcss()],
  resolve: {
    alias: {
      '@': fileURLToPath(new URL('./src', import.meta.url))
    }
  },
  base: './',
  build: {
    outDir: 'dist',
    emptyOutDir: true
  }
})
