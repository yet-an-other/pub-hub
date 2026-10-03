import { defineConfig } from 'playwright/test'

export default defineConfig({
  testDir: './browser-tests',
  use: { baseURL: 'http://127.0.0.1:4179', browserName: 'chromium', headless: true },
  webServer: { command: 'npx vite --host 127.0.0.1 --port 4179 --strictPort', url: 'http://127.0.0.1:4179', reuseExistingServer: false },
})
