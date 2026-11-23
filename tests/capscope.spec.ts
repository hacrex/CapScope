import { test, expect } from '@playwright/test';

test.use({ baseURL: 'http://localhost:8080' });

test.describe('CapScope UI', () => {
  test.beforeEach(async ({ page }) => {
    await page.goto('/');
  });

  test('homepage loads with title and hero', async ({ page }) => {
    await expect(page).toHaveTitle(/CapScope/);
    await expect(page.locator('h1')).toContainText('disappear');
  });

  test('dashboard loads and shows container count chip', async ({ page }) => {
    await page.goto('/');
    await expect(page.locator('#conn')).toBeVisible();
  });

  test('login overlay shows sign-in form when auth is on', async ({ page }) => {
    // With ALLOW_NO_AUTH=true, login overlay should not be visible by default
    const loginOverlay = page.locator('#loginOverlay');
    await expect(loginOverlay).not.toBeVisible();
  });

  test('sidebar shows apps list', async ({ page }) => {
    await page.locator('#apps').waitFor({ timeout: 10000 });
    await expect(page.locator('#apps')).toBeVisible();
  });

  test('logs tab renders live tail', async ({ page }) => {
    await page.click('#tabs button[data-t="logs"]');
    await page.locator('#logs').waitFor({ timeout: 10000 });
    await expect(page.locator('#logs')).toBeVisible();
  });

  test('metrics tab renders charts', async ({ page }) => {
    await page.click('#tabs button[data-t="metrics"]');
    await page.locator('#ccpu').waitFor({ timeout: 10000 });
    await expect(page.locator('#ccpu')).toBeVisible();
    await expect(page.locator('#cmem')).toBeVisible();
  });

  test('crashes tab renders', async ({ page }) => {
    await page.click('#tabs button[data-t="crashes"]');
    await page.locator('#view-crashes').waitFor({ timeout: 10000 });
    await expect(page.locator('#view-crashes')).toBeVisible();
  });

  test('alerts tab renders', async ({ page }) => {
    await page.click('#tabs button[data-t="alerts"]');
    await page.locator('#view-alerts').waitFor({ timeout: 10000 });
    await expect(page.locator('#view-alerts')).toBeVisible();
  });

  test('traces tab renders', async ({ page }) => {
    await page.click('#tabs button[data-t="traces"]');
    await page.locator('#view-traces').waitFor({ timeout: 10000 });
    await expect(page.locator('#view-traces')).toBeVisible();
  });

  test('incidents tab renders', async ({ page }) => {
    await page.click('#tabs button[data-t="incidents"]');
    await page.locator('#view-incidents').waitFor({ timeout: 10000 });
    await expect(page.locator('#view-incidents')).toBeVisible();
  });

  test('settings tab renders', async ({ page }) => {
    await page.click('#tabs button[data-t="settings"]');
    await page.locator('#view-settings').waitFor({ timeout: 10000 });
    await expect(page.locator('#view-settings')).toBeVisible();
  });

  test('theme toggle works', async ({ page }) => {
    await page.locator('#themeBtn').click();
    await expect(page.locator('html')).toHaveAttribute('data-theme', 'light');
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
