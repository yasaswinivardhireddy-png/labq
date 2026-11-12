import { defineConfig, devices } from '@playwright/test';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

// Runs against `labq demo`: a simulated eight-device lab with a reservation, a quarantined device
// and a mix of jobs, served by the real binary.
const bin = process.env.LABQ_BIN ?? '../labq';
const channel = process.env.PW_CHANNEL;

export default defineConfig({
  testDir: '.',
  timeout: 60_000,
  expect: { timeout: 15_000 },
  workers: 1,
  retries: 0,
  reporter: [['list'], ['html', { open: 'never', outputFolder: 'playwright-report' }]],
  use: { baseURL: 'http://127.0.0.1:8090', trace: 'retain-on-failure', screenshot: 'only-on-failure' },
  projects: [{ name: 'chromium', use: { ...devices['Desktop Chrome'], ...(channel ? { channel } : {}) } }],
  webServer: {
    command: `${bin} demo -addr 127.0.0.1:8090 -wal ${join(tmpdir(), 'labq-e2e.wal')}`,
    url: 'http://127.0.0.1:8090/healthz',
    reuseExistingServer: !process.env.CI,
    timeout: 30_000,
  },
});
