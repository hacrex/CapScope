# Instructions

- Following Playwright test failed.
- Explain why, be concise, respect Playwright best practices.
- Provide a snippet of code with the fix, if possible.

# Test info

- Name: tests\capscope.spec.ts >> CapScope UI >> incidents tab renders
- Location: tests\capscope.spec.ts:62:3

# Error details

```
Error: page.goto: net::ERR_CONNECTION_REFUSED at http://localhost:8080/
Call log:
  - navigating to "http://localhost:8080/", waiting until "load"

```

# Test source

```ts
  1   | import { test, expect } from '@playwright/test';
  2   | 
  3   | test.use({ baseURL: 'http://localhost:8080' });
  4   | 
  5   | test.describe('CapScope UI', () => {
  6   |   test.beforeEach(async ({ page }) => {
> 7   |     await page.goto('/');
      |                ^ Error: page.goto: net::ERR_CONNECTION_REFUSED at http://localhost:8080/
  8   |   });
  9   | 
  10  |   test('homepage loads with title and hero', async ({ page }) => {
  11  |     await expect(page).toHaveTitle(/CapScope/);
  12  |     await expect(page.locator('h1')).toContainText('disappear');
  13  |   });
  14  | 
  15  |   test('dashboard loads and shows container count chip', async ({ page }) => {
  16  |     await page.goto('/');
  17  |     await expect(page.locator('#conn')).toBeVisible();
  18  |   });
  19  | 
  20  |   test('login overlay shows sign-in form when auth is on', async ({ page }) => {
  21  |     // With ALLOW_NO_AUTH=true, login overlay should not be visible by default
  22  |     const loginOverlay = page.locator('#loginOverlay');
  23  |     await expect(loginOverlay).not.toBeVisible();
  24  |   });
  25  | 
  26  |   test('sidebar shows apps list', async ({ page }) => {
  27  |     await page.locator('#apps').waitFor({ timeout: 10000 });
  28  |     await expect(page.locator('#apps')).toBeVisible();
  29  |   });
  30  | 
  31  |   test('logs tab renders live tail', async ({ page }) => {
  32  |     await page.click('#tabs button[data-t="logs"]');
  33  |     await page.locator('#logs').waitFor({ timeout: 10000 });
  34  |     await expect(page.locator('#logs')).toBeVisible();
  35  |   });
  36  | 
  37  |   test('metrics tab renders charts', async ({ page }) => {
  38  |     await page.click('#tabs button[data-t="metrics"]');
  39  |     await page.locator('#ccpu').waitFor({ timeout: 10000 });
  40  |     await expect(page.locator('#ccpu')).toBeVisible();
  41  |     await expect(page.locator('#cmem')).toBeVisible();
  42  |   });
  43  | 
  44  |   test('crashes tab renders', async ({ page }) => {
  45  |     await page.click('#tabs button[data-t="crashes"]');
  46  |     await page.locator('#view-crashes').waitFor({ timeout: 10000 });
  47  |     await expect(page.locator('#view-crashes')).toBeVisible();
  48  |   });
  49  | 
  50  |   test('alerts tab renders', async ({ page }) => {
  51  |     await page.click('#tabs button[data-t="alerts"]');
  52  |     await page.locator('#view-alerts').waitFor({ timeout: 10000 });
  53  |     await expect(page.locator('#view-alerts')).toBeVisible();
  54  |   });
  55  | 
  56  |   test('traces tab renders', async ({ page }) => {
  57  |     await page.click('#tabs button[data-t="traces"]');
  58  |     await page.locator('#view-traces').waitFor({ timeout: 10000 });
  59  |     await expect(page.locator('#view-traces')).toBeVisible();
  60  |   });
  61  | 
  62  |   test('incidents tab renders', async ({ page }) => {
  63  |     await page.click('#tabs button[data-t="incidents"]');
  64  |     await page.locator('#view-incidents').waitFor({ timeout: 10000 });
  65  |     await expect(page.locator('#view-incidents')).toBeVisible();
  66  |   });
  67  | 
  68  |   test('settings tab renders', async ({ page }) => {
  69  |     await page.click('#tabs button[data-t="settings"]');
  70  |     await page.locator('#view-settings').waitFor({ timeout: 10000 });
  71  |     await expect(page.locator('#view-settings')).toBeVisible();
  72  |   });
  73  | 
  74  |   test('theme toggle works', async ({ page }) => {
  75  |     await page.locator('#themeBtn').click();
  76  |     await expect(page.locator('html')).toHaveAttribute('data-theme', 'light');
  77  |   });
  78  | 
  79  |   test('timezone toggle works', async ({ page }) => {
  80  |     await page.locator('#tzBtn').click();
  81  |     await expect(page.locator('#tzBtn')).toBeVisible();
  82  |   });
  83  | 
  84  |   test('sidebar filter works', async ({ page }) => {
  85  |     await page.locator('#appFilter').fill('nonexistent-app-xyz');
  86  |     await page.waitForTimeout(500);
  87  |     await expect(page.locator('#apps .app')).toHaveCount(0);
  88  |   });
  89  | 
  90  |   test('share button exists', async ({ page }) => {
  91  |     await expect(page.locator('#shareBtn')).toBeVisible();
  92  |   });
  93  | 
  94  |   test('health endpoint returns ok', async ({ page }) => {
  95  |     const response = await page.request.get('/healthz');
  96  |     await expect(response).toBeOK();
  97  |     const body = await response.text();
  98  |     expect(body).toContain('ok');
  99  |   });
  100 | });
  101 | 
```