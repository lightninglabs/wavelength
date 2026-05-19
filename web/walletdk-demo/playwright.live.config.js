const { defineConfig, devices } = require("@playwright/test");

const host = process.env.WALLETDK_LIVE_HOST || "127.0.0.1";
const port = Number(process.env.WALLETDK_LIVE_PORT || 8081);
const baseURL = `http://${host}:${port}`;

module.exports = defineConfig({
  testDir: __dirname,
  testMatch: "walletdk-live.spec.js",
  timeout: 180000,
  reporter: process.env.CI ? [["list"], ["html", { open: "never" }]] : "line",
  use: {
    ...devices["Desktop Chrome"],
    baseURL,
    trace: "retain-on-failure",
  },
  webServer: {
    command: "node server.js",
    cwd: __dirname,
    env: {
      HOST: host,
      PORT: String(port),
    },
    reuseExistingServer: !process.env.CI,
    timeout: 30000,
    url: `${baseURL}/`,
  },
  projects: [
    {
      name: "chromium",
      use: {
        ...devices["Desktop Chrome"],
      },
    },
  ],
});
