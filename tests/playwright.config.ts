import { defineConfig, devices } from '@playwright/test';

export default defineConfig({
  testDir: './tests',
  fullyParallel: true,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 2 : 0,
  workers: process.env.CI ? 1 : undefined,
  reporter: 'html',
  use: {
    baseURL: 'http://localhost:8080',
    trace: 'on-first-retry',
    screenshot: 'only-on-failure',
    video: 'retain-on-failure',
  },
  projects: [
    {
      name: 'chromium',
      use: { ...devices['Desktop Chrome'] },
    },
  ],
  webServer: {
    command: 'ALLOW_NO_AUTH=true DB_PATH=./data/test.db LISTEN=127.0.0.1:8080 go run -tags "sqlite_omit_load_extension fts5" ./cmd/capscope',
    url: 'http://localhost:8080/healthz',
    timeout: 120 * 1000,
    reuseExistingServer: !process.env.CI,
  },
});
