const { expect, test } = require("@playwright/test");

const arkGatewayURL = "https://arkd-signet-rest.testnet.lightningcluster.com";
const swapGatewayURL = "https://swapd-signet-rest.testnet.lightningcluster.com";
const esploraURL = "https://mempool-signet.testnet.lightningcluster.com/api";

test("walletdk demo starts with live signet defaults", async ({
  page,
}, testInfo) => {
  const consoleMessages = [];
  page.on("console", (message) => {
    const line = `[${message.type()}] ${message.text()}`;
    consoleMessages.push(line);
    if (process.env.WALLETDK_SIGNET_SMOKE_VERBOSE) {
      console.log(line);
    }
  });
  page.on("pageerror", (error) => {
    const line = `[pageerror] ${error.message}`;
    consoleMessages.push(line);
    if (process.env.WALLETDK_SIGNET_SMOKE_VERBOSE) {
      console.log(line);
    }
  });

  await page.goto("/");
  await expect(page.locator("#runtime-status")).toHaveText("wasm ready", {
    timeout: 30000,
  });
  await openAdvancedSettings(page);

  await expect(page.locator("input[name=network]")).toHaveValue("signet");
  await expect(page.locator("input[name=arkGatewayURL]")).toHaveValue(
    arkGatewayURL,
  );
  await expect(page.locator("input[name=mailboxGatewayURL]")).toHaveValue(
    arkGatewayURL,
  );
  await expect(page.locator("input[name=walletEsploraURL]")).toHaveValue(
    esploraURL,
  );
  await expect(page.locator("input[name=swapServerGatewayURL]")).toHaveValue(
    swapGatewayURL,
  );
  await expect(page.locator("input[name=swapMailboxGatewayURL]")).toHaveValue(
    swapGatewayURL,
  );

  await page.locator("input[name=dataDir]").fill(
    `/walletdk-signet-smoke-${Date.now()}`,
  );
  await page.locator("input[name=swapDatabaseFileName]").fill(
    `/walletdk-signet-swaps-${Date.now()}.db`,
  );
  await page.getByRole("button", { name: "Start runtime" }).click();

  await expect(page.locator("#runtime-status")).toHaveText(
    "wallet not created",
    { timeout: 120000 },
  );
  await expect(page.locator("#create-form")).toBeVisible({
    timeout: 30000,
  });

  await testInfo.attach("signet-start", {
    body: await page.screenshot({ fullPage: true }),
    contentType: "image/png",
  });
  await testInfo.attach("console", {
    body: consoleMessages.join("\n"),
    contentType: "text/plain",
  });
});

async function openAdvancedSettings(page) {
  const details = page.locator(".advanced-settings");
  if (await details.evaluate((node) => node.hasAttribute("open"))) {
    return;
  }

  await page.getByText("Advanced settings").click();
}
