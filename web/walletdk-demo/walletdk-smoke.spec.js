const { expect, test } = require("@playwright/test");

test("wallet create and address state persist with OPFS SQLite", async ({
  page,
}, testInfo) => {
  const password = "test-password";
  const baseURL = testInfo.project.use.baseURL;
  const dataDir = `/walletdk-smoke-${Date.now()}`;
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
  // The "Start runtime" button is the connect screen, which only renders once
  // the WASM runtime has loaded (phase runtimeReady).
  const startRuntime = page.getByRole("button", { name: "Start runtime" });
  await expect(startRuntime).toBeVisible({ timeout: 30000 });

  await configureRuntime(page, baseURL, dataDir, swapDatabaseFileName);
  await startRuntime.click();

  const createWallet = page.getByRole("button", { name: "Create wallet" });
  await expect(createWallet).toBeVisible({ timeout: 60000 });
  await page.getByLabel("Password", { exact: true }).fill(password);
  await page.getByLabel("Confirm password").fill(password);
  await createWallet.click();

  await expect(page.getByRole("heading", { name: "Recovery phrase" })).toBeVisible(
    { timeout: 60000 },
  );
  await testInfo.attach("create-wallet", {
    body: await page.screenshot({ fullPage: true }),
    contentType: "image/png",
  });

  await page.getByRole("button", { name: "I saved it" }).click();

  // The account chip carries the full identity pubkey and only renders inside
  // the authenticated app shell, so its presence confirms we reached the
  // dashboard.
  const accountChip = page.getByTestId("account-pubkey");
  await expect(accountChip).toBeVisible({ timeout: 60000 });
  const identity = await accountChip.getAttribute("data-pubkey");
  expect(identity.length).toBeGreaterThan(10);
  await expectOPFSOpen(page);

  // Fresh wallets are empty, so home shows the board-on-chain CTA.
  await page.getByRole("button", { name: "Get a boarding address" }).click();
  await page.getByRole("button", { name: "On-chain" }).click();
  await page.getByRole("button", { name: "Get boarding address" }).click();
  await expect(page.getByText(/bcrt1/)).toBeVisible({ timeout: 30000 });

  await page.getByRole("button", { name: "Lightning" }).click();
  await page.getByLabel("Amount (sats)").fill("1000");
  await page.getByRole("button", { name: "Create invoice" }).click();
  await expect(page.getByText(/lnbcrt/)).toBeVisible({ timeout: 60000 });

  await page.getByRole("button", { name: "Activity" }).click();
  await page.getByRole("button", { name: "Refresh" }).click();
  await expect(page.getByTestId("activity-row").first()).toBeVisible({
    timeout: 30000,
  });

  await testInfo.attach("dashboard", {
    body: await page.screenshot({ fullPage: true }),
    contentType: "image/png",
  });

  await page.reload();
  await expect(startRuntime).toBeVisible({ timeout: 30000 });

  await configureRuntime(page, baseURL, dataDir, swapDatabaseFileName);
  await startRuntime.click();

  const unlock = page.getByRole("button", { name: "Unlock", exact: true });
  await expect(unlock).toBeVisible({ timeout: 60000 });
  await page.getByLabel("Password", { exact: true }).fill(password);
  await unlock.click();

  await expect(accountChip).toBeVisible({ timeout: 60000 });
  const reloadedIdentity = await accountChip.getAttribute("data-pubkey");
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

async function configureRuntime(page, baseURL, dataDir, swapDatabaseFileName) {
  await page.getByRole("button", { name: "regtest", exact: true }).click();
  await page.getByLabel("Ark gateway URL").fill(baseURL);
  await page.getByLabel("Wallet Esplora URL").fill(baseURL);
  await page.getByRole("button", { name: "Advanced endpoints" }).click();
  await page.getByLabel("Mailbox gateway URL").fill(baseURL);
  await page.getByLabel("Swap server gateway URL").fill(baseURL);
  await page.getByLabel("Swap mailbox gateway URL").fill(baseURL);
  await page.getByLabel("Data directory").fill(dataDir);
  await page.getByLabel("Swap database file").fill(swapDatabaseFileName);
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
