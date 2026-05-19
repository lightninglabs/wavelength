const { expect, test } = require("@playwright/test");

test("wallet demo runs against live swapdtest gateways", async ({
  page,
}, testInfo) => {
  const password = `live-test-password-${Date.now()}`;
  const dataDir = `/walletdk-live-${Date.now()}`;
  const swapDatabaseFileName = `/walletdk-live-swaps-${Date.now()}.db`;
  const endpoints = liveEndpoints();

  const consoleMessages = [];
  page.on("console", (message) => {
    const line = `[${message.type()}] ${message.text()}`;
    consoleMessages.push(line);
    if (process.env.WALLETDK_LIVE_VERBOSE) {
      console.log(line);
    }
  });
  page.on("pageerror", (error) => {
    const line = `[pageerror] ${error.message}`;
    consoleMessages.push(line);
    if (process.env.WALLETDK_LIVE_VERBOSE) {
      console.log(line);
    }
  });
  page.on("requestfailed", (request) => {
    const failure = request.failure();
    const line = `[requestfailed] ${request.method()} ${request.url()} ${
      failure?.errorText || ""
    }`;
    consoleMessages.push(line);
    if (process.env.WALLETDK_LIVE_VERBOSE) {
      console.log(line);
    }
  });

  await page.goto("/");
  await expect(page.locator("#runtime-status")).toHaveText("wasm ready", {
    timeout: 30000,
  });

  await configureRuntime(page, endpoints, dataDir, swapDatabaseFileName);
  await page.getByRole("button", { name: "Start runtime" }).click();

  await expect(page.locator("#create-form")).toBeVisible({
    timeout: 90000,
  });
  await page.locator("#create-form input[name=password]").fill(password);
  await page.getByRole("button", { name: "Create wallet" }).click();

  await expect(page.locator("#mnemonic-panel")).toBeVisible({
    timeout: 90000,
  });
  await testInfo.attach("create-wallet", {
    body: await page.screenshot({ fullPage: true }),
    contentType: "image/png",
  });

  await page.getByRole("button", {
    name: "I recorded the demo mnemonic",
  }).click();
  await expect(page.locator("#dashboard-view")).toBeVisible({
    timeout: 90000,
  });

  await waitForOverviewFact(page, "wallet ready", (value) => value === "true");
  await waitForOverviewFact(
    page,
    "server connected",
    (value) => value === "true",
  );
  const identity = await waitForOverviewFact(
    page,
    "identity pubkey",
    (value) => value.length > 10,
  );
  await expectOPFSOpen(page);

  await page.getByRole("button", { name: "New address" }).click();
  await expect(page.locator("#address-output")).toContainText("bcrt", {
    timeout: 60000,
  });

  await page.locator("input[name=amountSat]").fill("1000");
  await page.getByRole("button", { name: "Create invoice" }).click();
  await expect(page.locator("#receive-output")).toContainText("lnbcrt", {
    timeout: 120000,
  });

  await page.getByRole("button", { name: "Refresh swaps" }).click();
  await expect(page.locator("#swaps-body tr")).toHaveCount(1, {
    timeout: 60000,
  });
  await testInfo.attach("dashboard", {
    body: await page.screenshot({ fullPage: true }),
    contentType: "image/png",
  });

  await page.reload();
  await expect(page.locator("#runtime-status")).toHaveText("wasm ready", {
    timeout: 30000,
  });

  await configureRuntime(page, endpoints, dataDir, swapDatabaseFileName);
  await page.getByRole("button", { name: "Start runtime" }).click();

  await expect(page.locator("#unlock-form")).toBeVisible({
    timeout: 90000,
  });
  await page.locator("#unlock-form input[name=password]").fill(password);
  await page.getByRole("button", { name: "Unlock wallet" }).click();

  await expect(page.locator("#dashboard-view")).toBeVisible({
    timeout: 90000,
  });

  const reloadedIdentity = await waitForOverviewFact(
    page,
    "identity pubkey",
    (value) => value.length > 10,
  );
  expect(reloadedIdentity).toBe(identity);
  await expectOPFSOpen(page);

  await testInfo.attach("unlock-dashboard", {
    body: await page.screenshot({ fullPage: true }),
    contentType: "image/png",
  });
  await testInfo.attach("console", {
    body: consoleMessages.join("\n"),
    contentType: "text/plain",
  });
});

function liveEndpoints() {
  const arkGatewayURL = requiredEnv("WALLETDK_LIVE_ARK_GATEWAY_URL");
  const swapServerGatewayURL = requiredEnv(
    "WALLETDK_LIVE_SWAP_GATEWAY_URL",
  );

  return {
    walletEsploraURL: requiredEnv("WALLETDK_LIVE_ESPLORA_URL"),
    arkGatewayURL,
    mailboxGatewayURL:
      process.env.WALLETDK_LIVE_MAILBOX_GATEWAY_URL || arkGatewayURL,
    swapServerGatewayURL,
    swapMailboxGatewayURL:
      process.env.WALLETDK_LIVE_SWAP_MAILBOX_GATEWAY_URL ||
      swapServerGatewayURL,
  };
}

function requiredEnv(name) {
  const value = process.env[name];
  if (!value) {
    throw new Error(`${name} is required`);
  }

  return value;
}

async function configureRuntime(
  page,
  endpoints,
  dataDir,
  swapDatabaseFileName,
) {
  await page.locator("input[name=dataDir]").fill(dataDir);
  await page.locator("input[name=walletEsploraURL]").fill(
    endpoints.walletEsploraURL,
  );
  await page.locator("input[name=arkGatewayURL]").fill(
    endpoints.arkGatewayURL,
  );
  await page.locator("input[name=mailboxGatewayURL]").fill(
    endpoints.mailboxGatewayURL,
  );
  await page.locator("input[name=swapServerGatewayURL]").fill(
    endpoints.swapServerGatewayURL,
  );
  await page.locator("input[name=swapMailboxGatewayURL]").fill(
    endpoints.swapMailboxGatewayURL,
  );
  await page.locator("input[name=swapDatabaseFileName]").fill(
    swapDatabaseFileName,
  );
  await page.locator("input[name=disableSwaps]").uncheck();
}

async function waitForOverviewFact(page, name, predicate, timeout = 120000) {
  const deadline = Date.now() + timeout;
  let value = "";

  while (Date.now() < deadline) {
    await page.getByRole("button", { name: "Refresh", exact: true }).click();
    value = await overviewFact(page, name);
    if (predicate(value)) {
      return value;
    }

    await page.waitForTimeout(1000);
  }

  throw new Error(`overview fact ${name} did not satisfy predicate: ${value}`);
}

async function overviewFact(page, name) {
  const terms = page.locator("#info-grid dt");
  const count = await terms.count();
  for (let i = 0; i < count; i++) {
    if ((await terms.nth(i).innerText()) === name) {
      return page.locator("#info-grid dd").nth(i).innerText();
    }
  }

  throw new Error(`overview fact ${name} not found`);
}

async function expectOPFSOpen(page) {
  const opened = await page.evaluate(() => globalThis.sqliteBridgeOpenResults);
  expect(opened).toEqual(
    expect.arrayContaining([
      expect.objectContaining({
        persistent: true,
        vfsType: "opfs",
      }),
    ]),
  );
}
