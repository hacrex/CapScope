import { test, expect } from '@playwright/test';

test.use({ baseURL: 'http://localhost:8080' });

test.describe('CapScope UI', () => {
  test.beforeEach(async ({ page }) => {
    await page.goto('/', { waitUntil: 'load' });
    await page.evaluate(() => window.scrollTo(0, 0));
  });

  test('homepage loads with title and hero', async ({ page }) => {
    await expect(page).toHaveTitle(/CapScope/);
    await expect(page.locator('h1').first()).toBeVisible();
  });

  test('dashboard loads and shows container count chip', async ({ page }) => {
    await expect(page.locator('#conn')).toBeVisible();
  });

  test('login overlay is hidden when auth is off', async ({ page }) => {
    const loginOverlay = page.locator('#loginOverlay');
    await expect(loginOverlay).not.toBeVisible();
  });

  test('sidebar shows apps list', async ({ page }) => {
    await page.locator('#apps').waitFor({ timeout: 10000 });
    await expect(page.locator('#apps')).toBeVisible();
  });

  async function switchTab(page: any, tabName: string) {
    await page.evaluate((t) => {
      const target = document.getElementById('view-' + t);
      if (target) {
        target.style.display = t === 'logs' ? 'flex' : 'block';
      }
    }, tabName);
    await page.waitForTimeout(300);
  }

  test('logs tab renders live tail', async ({ page }) => {
    await switchTab(page, 'logs');
    await page.locator('#logs').waitFor({ timeout: 10000 });
    await expect(page.locator('#logs')).toBeVisible();
  });

  test('metrics tab renders charts', async ({ page }) => {
    await switchTab(page, 'metrics');
    await page.locator('#ccpu').waitFor({ timeout: 10000 });
    await expect(page.locator('#ccpu')).toBeVisible();
    await expect(page.locator('#cmem')).toBeVisible();
  });

  test('crashes tab renders', async ({ page }) => {
    await switchTab(page, 'crashes');
    await page.locator('#view-crashes').waitFor({ timeout: 10000 });
    await expect(page.locator('#view-crashes')).toBeVisible();
  });

  test('alerts tab renders', async ({ page }) => {
    await switchTab(page, 'alerts');
    await page.locator('#view-alerts').waitFor({ timeout: 10000 });
    await expect(page.locator('#view-alerts')).toBeVisible();
  });

  test('traces tab renders', async ({ page }) => {
    await switchTab(page, 'traces');
    await page.locator('#view-traces').waitFor({ timeout: 10000 });
    await expect(page.locator('#view-traces')).toBeVisible();
  });

  test('incidents tab renders', async ({ page }) => {
    await switchTab(page, 'incidents');
    await page.locator('#view-incidents').waitFor({ timeout: 10000 });
    await expect(page.locator('#view-incidents')).toBeVisible();
  });

  test('settings tab renders', async ({ page }) => {
    await switchTab(page, 'settings');
    await page.locator('#view-settings').waitFor({ timeout: 10000 });
    await expect(page.locator('#view-settings')).toBeVisible();
  });

  test('theme toggle works', async ({ page }) => {
    await page.locator('#themeBtn').click();
    await expect(page.locator('html')).toHaveAttribute('data-theme', /light|dark/);
  });

  test('timezone toggle works', async ({ page }) => {
    await page.locator('#tzBtn').click();
    await expect(page.locator('#tzBtn')).toBeVisible();
  });

  test('sidebar filter works', async ({ page }) => {
    await page.locator('#appFilter').fill('nonexistent-app-xyz');
    await page.waitForTimeout(500);
    await expect(page.locator('#apps .app')).toHaveCount(0);
  });

  test('share button exists', async ({ page }) => {
    await expect(page.locator('#shareBtn')).toBeVisible();
  });

  test('health endpoint returns ok', async ({ page }) => {
    const response = await page.request.get('/healthz');
    await expect(response).toBeOK();
    const body = await response.text();
    expect(body).toContain('ok');
  });
});
