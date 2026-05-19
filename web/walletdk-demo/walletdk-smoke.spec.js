const { expect, test } = require("@playwright/test");

test("wallet create and address state persist with OPFS SQLite", async ({
  page,
}, testInfo) => {
  const password = "test-password";
  const baseURL = testInfo.project.use.baseURL;
  const swapDatabaseFileName = `/walletdk-swaps-${Date.now()}.db`;

  const consoleMessages = [];
  page.on("console", (message) => {
    const line = `[${message.type()}] ${message.text()}`;
    consoleMessages.push(line);
    if (process.env.WALLETDK_SMOKE_VERBOSE) {
      console.log(line);
    }
  });
  page.on("pageerror", (error) => {
    const line = `[pageerror] ${error.message}`;
    consoleMessages.push(line);
    if (process.env.WALLETDK_SMOKE_VERBOSE) {
      console.log(line);
    }
  });

  await page.goto("/");
  await expect(page.locator("#runtime-status")).toHaveText("wasm ready", {
    timeout: 30000,
  });

  await configureRuntime(page, baseURL, swapDatabaseFileName);
  await page.getByRole("button", { name: "Start runtime" }).click();

  await expect(page.locator("#create-form")).toBeVisible({
    timeout: 60000,
  });
  await page.locator("#create-form input[name=password]").fill(password);
  await page.getByRole("button", { name: "Create wallet" }).click();

  await expect(page.locator("#mnemonic-panel")).toBeVisible({
    timeout: 60000,
  });
  await testInfo.attach("create-wallet", {
    body: await page.screenshot({ fullPage: true }),
    contentType: "image/png",
  });

  await page.getByRole("button", {
    name: "I recorded the demo mnemonic",
  }).click();
  await expect(page.locator("#dashboard-view")).toBeVisible({
    timeout: 60000,
  });

  await page.getByRole("button", { name: "Refresh", exact: true }).click();
  const identity = await identityPubkey(page);
  expect(identity.length).toBeGreaterThan(10);
  await expectOPFSOpen(page);

  await page.getByRole("button", { name: "New address" }).click();
  await expect(page.locator("#address-output")).toContainText("bcrt", {
    timeout: 30000,
  });

  await page.locator("input[name=amountSat]").fill("1000");
  await page.getByRole("button", { name: "Create invoice" }).click();
  await expect(page.locator("#receive-output")).toContainText("lnbcrt", {
    timeout: 60000,
  });

  await page.getByRole("button", { name: "Refresh swaps" }).click();
  await expect(page.locator("#swaps-body tr")).toHaveCount(1, {
    timeout: 30000,
  });

  await testInfo.attach("dashboard", {
    body: await page.screenshot({ fullPage: true }),
    contentType: "image/png",
  });

  await page.reload();
  await expect(page.locator("#runtime-status")).toHaveText("wasm ready", {
    timeout: 30000,
  });

  await configureRuntime(page, baseURL, swapDatabaseFileName);
  await page.getByRole("button", { name: "Start runtime" }).click();

  await expect(page.locator("#unlock-form")).toBeVisible({
    timeout: 60000,
  });
  await page.locator("#unlock-form input[name=password]").fill(password);
  await page.getByRole("button", { name: "Unlock wallet" }).click();

  await expect(page.locator("#dashboard-view")).toBeVisible({
    timeout: 60000,
  });
  await page.getByRole("button", { name: "Refresh", exact: true }).click();

  const reloadedIdentity = await identityPubkey(page);
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

async function configureRuntime(page, baseURL, swapDatabaseFileName) {
  await page.locator("input[name=walletEsploraURL]").fill(baseURL);
  await page.locator("input[name=arkGatewayURL]").fill(baseURL);
  await page.locator("input[name=mailboxGatewayURL]").fill(baseURL);
  await page.locator("input[name=swapServerGatewayURL]").fill(baseURL);
  await page.locator("input[name=swapMailboxGatewayURL]").fill(baseURL);
  await page.locator("input[name=swapDatabaseFileName]").fill(
    swapDatabaseFileName,
  );
  await page.locator("input[name=disableSwaps]").uncheck();
}

async function identityPubkey(page) {
  const terms = page.locator("#info-grid dt");
  const count = await terms.count();
  for (let i = 0; i < count; i++) {
    if ((await terms.nth(i).innerText()) === "identity pubkey") {
      return page.locator("#info-grid dd").nth(i).innerText();
    }
  }

  throw new Error("identity pubkey fact not found");
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
