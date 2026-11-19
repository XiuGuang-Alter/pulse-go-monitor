const { test, expect } = require('@playwright/test');
const fs = require('node:fs/promises');
const os = require('node:os');
const path = require('node:path');
const { launchServer, stopChild } = require('../scripts/launch-server.cjs');

test('演示模式可以切换故障和恢复', async ({ page, context, request }, info) => {
  const directory = await fs.mkdtemp(path.join(os.tmpdir(), 'pulse-demo-e2e-'));
  const exe = path.resolve('.cache', process.platform === 'win32' ? 'pulse-e2e.exe' : 'pulse-e2e');
  let app;
  try {
    app = await launchServer(exe, [
      'serve', '-addr', '127.0.0.1:0', '-demo', '-demo-addr', '127.0.0.1:0',
      '-db', path.join(directory, 'demo.db'),
    ]);
    const targets = async () => (await request.get(app.url + '/api/targets')).json();
    const name = '演示 · 故障与恢复';
    const target = (await targets()).find(t => t.name === name);
    expect(target).toBeDefined();
    const incidents = async () => {
      const response = await request.get(app.url + '/api/incidents?target_id=' + target.id);
      return (await response.json()).items;
    };
    await expect.poll(async () => (await targets()).find(t => t.id === target.id).latest?.status_code).toBe(200);

    await page.goto(app.url);
    const controls = await context.newPage();
    await controls.goto(new URL('/demo', target.url).href);
    await expect(controls.getByText('HTTP 200', { exact: true })).toBeVisible();
    await controls.getByRole('button', { name: '模拟故障' }).click();
    await expect(controls.getByText('HTTP 503', { exact: true })).toBeVisible();

    await expect.poll(async () => {
      const current = (await targets()).find(t => t.id === target.id);
      return !current.running && current.latest?.status_code;
    }).toBe(503);
    await expect.poll(async () => (await incidents())[0]?.status).toBe('active');

    await controls.getByRole('button', { name: '恢复服务' }).click();
    await expect(controls.getByText('HTTP 200', { exact: true })).toBeVisible();
    await expect.poll(async () => (await incidents())[0]?.status).toBe('recovered');

    await page.bringToFront();
    await page.locator('#incidents-open').click();
    await page.locator('#incident-target').selectOption(String(target.id));
    await page.locator('#incident-rows').getByRole('button', { name: '查看检查' }).click();
    const detail = page.locator('#incident-detail-dialog');
    await expect(detail).toContainText('已恢复');
    const rows = detail.locator('tbody tr');
    await expect(rows).toHaveCount(3);
    await expect(rows.first()).toContainText('503');
    await expect(rows.last()).toContainText('200');
    if (info.project.name === 'chromium') {
      await detail.screenshot({ path: info.outputPath('故障与恢复.png'), animations: 'disabled' });
    }
  } finally {
    await stopChild(app?.child);
    await fs.rm(directory, { recursive: true, force: true });
  }
});
