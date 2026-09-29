import { defineConfig } from 'vite'
import tailwindcss from '@tailwindcss/vite'

export default defineConfig({ plugins: [tailwindcss()], base: '/', build: { outDir: '../internal/portal/web', emptyOutDir: true } })
