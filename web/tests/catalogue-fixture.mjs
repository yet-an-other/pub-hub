import { createServer } from 'vite'
import { chromium } from 'playwright'

export async function startBrowser() {
  const server = await createServer({ server: { host: '127.0.0.1', port: 0 } })
  try {
    await server.listen()
    const browser = await chromium.launch({ headless: true, args: ['--no-sandbox'], ...(process.env.CHROMIUM_PATH ? { executablePath: process.env.CHROMIUM_PATH } : {}) })
    return { server, browser, origin: server.resolvedUrls.local[0] }
  } catch (error) {
    await server.close()
    throw error
  }
}

export async function stopBrowser({ browser, server } = {}) {
  await browser?.close()
  await server?.close()
}

export const json = body => ({ status: 200, contentType: 'application/json', body: JSON.stringify(body) })

export function catalogueBody(path, artifacts, projects, label = 'owner@example.test') {
  return path.endsWith('/artifacts') ? artifacts
    : path.endsWith('/projects') ? projects
      : path.endsWith('/config') ? { public_base_url: 'https://pub.example.test' }
        : { label }
}
