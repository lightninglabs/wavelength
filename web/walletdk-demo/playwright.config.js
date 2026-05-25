const { defineConfig, devices } = require("@playwright/test");

const host = process.env.WALLETDK_SMOKE_HOST || "127.0.0.1";
const port = Number(process.env.WALLETDK_SMOKE_PORT || 8091);
const baseURL = `http://${host}:${port}`;

module.exports = defineConfig({
  testDir: __dirname,
  testMatch: "walletdk-smoke.spec.js",
  timeout: 120000,
  reporter: process.env.CI ? [["list"], ["html", { open: "never" }]] : "line",
  use: {
    ...devices["Desktop Chrome"],
    baseURL,
    trace: "retain-on-failure",
  },
  webServer: {
    command: `node smoke-server.js`,
    cwd: __dirname,
    env: {
      HOST: host,
      PORT: String(port),
      WALLETDK_SMOKE_VERBOSE: process.env.WALLETDK_SMOKE_VERBOSE || "",
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
