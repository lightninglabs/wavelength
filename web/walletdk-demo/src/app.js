const statusEl = document.querySelector("#runtime-status");
const setupView = document.querySelector("#setup-view");
const dashboardView = document.querySelector("#dashboard-view");
const activity = document.querySelector("#activity");

let wasmReady = false;
let runtimeStarted = false;

window.addEventListener("walletdk-ready", () => {
  wasmReady = true;
  statusEl.textContent = "wasm ready";
});

function log(message) {
  const item = document.createElement("li");
  item.textContent = `${new Date().toLocaleTimeString()} ${message}`;
  activity.prepend(item);
}

function errorMessage(err) {
  if (err instanceof Error && err.message) {
    return err.message;
  }
  if (typeof err === "string") {
    return err;
  }
  return JSON.stringify(err);
}

async function call(method, req = {}) {
  if (!wasmReady || typeof window.walletdkCall !== "function") {
    throw new Error("walletdk wasm runtime is not ready");
  }
  return window.walletdkCall(method, req);
}

function formData(form) {
  const data = Object.fromEntries(new FormData(form).entries());
  for (const input of form.querySelectorAll("input[type=checkbox]")) {
    data[input.name] = input.checked;
  }
  return data;
}

function showDashboard() {
  setupView.hidden = true;
  dashboardView.hidden = false;
}

function showWalletActions(info) {
  document.querySelector("#create-form").hidden = info.WalletReady;
  document.querySelector("#unlock-form").hidden = info.WalletReady;
  if (info.WalletReady) {
    showDashboard();
  } else {
    setupView.hidden = false;
    dashboardView.hidden = true;
  }
}

function renderFacts(target, facts) {
  target.replaceChildren();
  for (const [key, value] of Object.entries(facts)) {
    const term = document.createElement("dt");
    term.textContent = key;
    const desc = document.createElement("dd");
    desc.textContent = value ?? "";
    target.append(term, desc);
  }
}

async function refreshInfo() {
  const info = await call("getInfo");
  renderFacts(document.querySelector("#info-grid"), {
    network: info.Network,
    height: info.BlockHeight,
    "server connected": info.ServerConnected,
    "wallet ready": info.WalletReady,
    "wallet type": info.WalletType,
    "identity pubkey": info.IdentityPubKey,
  });
  showWalletActions(info);
  return info;
}

function delay(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

async function waitForWalletReady() {
  for (let attempt = 0; attempt < 120; attempt++) {
    const info = await refreshInfo();
    if (info.WalletReady && info.IdentityPubKey) {
      return info;
    }

    await delay(500);
  }

  throw new Error("wallet did not become ready");
}

async function refreshBalance() {
  const balance = await call("listBalance");
  renderFacts(document.querySelector("#balance-grid"), {
    "boarding confirmed": balance.BoardingConfirmedSat,
    "boarding unconfirmed": balance.BoardingUnconfirmedSat,
    "vtxo balance": balance.VTXOBalanceSat,
    "total confirmed": balance.TotalConfirmedSat,
    "on-chain wallet": balance.OnchainWalletConfirmedSat,
  });
}

async function refreshSwaps() {
  const swaps = await call("listSwaps", { pendingOnly: false });
  const body = document.querySelector("#swaps-body");
  body.replaceChildren();
  for (const swap of swaps || []) {
    const row = document.createElement("tr");
    row.innerHTML = `<td>${swap.Direction}</td><td>${swap.State}</td><td>${swap.AmountSat}</td><td>${swap.PaymentHash}</td>`;
    body.append(row);
  }
}

async function refreshAll() {
  await refreshInfo();
  await refreshBalance().catch((err) => log(errorMessage(err)));
  await refreshSwaps().catch((err) => log(errorMessage(err)));
}

document.querySelector("#runtime-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  try {
    statusEl.textContent = "starting";
    const req = formData(event.currentTarget);
    const info = await call("start", req);
    runtimeStarted = true;
    statusEl.textContent = "runtime started";
    log("runtime started");
    showWalletActions(info);
    await refreshAll().catch((err) => log(errorMessage(err)));
  } catch (err) {
    statusEl.textContent = "runtime error";
    log(errorMessage(err));
  }
});

document.querySelector("#create-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  try {
    const result = await call("createWallet", formData(event.currentTarget));
    const mnemonicPanel = document.querySelector("#mnemonic-panel");
    document.querySelector("#mnemonic").textContent = result.Mnemonic.join(" ");
    mnemonicPanel.hidden = false;
    log(`wallet created ${result.IdentityPubKey}`);
  } catch (err) {
    log(errorMessage(err));
  }
});

document.querySelector("#mnemonic-ack").addEventListener("click", async () => {
  document.querySelector("#mnemonic-panel").hidden = true;
  showDashboard();
  await refreshAll().catch((err) => log(errorMessage(err)));
});

document.querySelector("#unlock-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  try {
    const result = await call("unlockWallet", formData(event.currentTarget));
    log(`wallet unlocked ${result.IdentityPubKey}`);
    await waitForWalletReady();
    showDashboard();
    await refreshAll().catch((err) => log(errorMessage(err)));
  } catch (err) {
    log(errorMessage(err));
  }
});

document.querySelector("#refresh").addEventListener("click", () => {
  refreshAll().catch((err) => log(errorMessage(err)));
});

document.querySelector("#stop").addEventListener("click", async () => {
  await call("stop").catch((err) => log(errorMessage(err)));
  runtimeStarted = false;
  dashboardView.hidden = true;
  setupView.hidden = false;
  statusEl.textContent = "runtime stopped";
});

window.addEventListener("pagehide", () => {
  if (!runtimeStarted || typeof window.walletdkCall !== "function") {
    return;
  }

  window.walletdkCall("stop").catch(() => {});
});

document.querySelector("#address-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  try {
    const result = await call("getOnchainAddress");
    document.querySelector("#address-output").textContent = result.Address;
    log("created onboarding address");
  } catch (err) {
    log(errorMessage(err));
  }
});

document.querySelector("#receive-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  try {
    const data = formData(event.currentTarget);
    data.amountSat = Number(data.amountSat);
    const result = await call("receive", data);
    document.querySelector("#receive-output").textContent = result.Invoice;
    log(`receive swap ${result.PaymentHash}`);
    await refreshSwaps();
  } catch (err) {
    log(errorMessage(err));
  }
});

document.querySelector("#send-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  try {
    const data = formData(event.currentTarget);
    data.maxFeeSat = Number(data.maxFeeSat);
    const result = await call("send", data);
    document.querySelector("#send-output").textContent = result.PaymentHash;
    log(`send swap ${result.PaymentHash}`);
    await refreshSwaps();
  } catch (err) {
    log(errorMessage(err));
  }
});

document.querySelector("#refresh-swaps").addEventListener("click", () => {
  refreshSwaps().catch((err) => log(errorMessage(err)));
});

async function boot() {
  const go = new Go();
  const result = await instantiateWasm(go.importObject);
  go.run(result.instance);
}

async function instantiateWasm(importObject) {
  if ("DecompressionStream" in window) {
    try {
      return await instantiateCompressedWasm(importObject);
    } catch (err) {
      log(`compressed wasm load failed: ${errorMessage(err)}`);
    }
  }

  return instantiateRawWasm(importObject);
}

async function instantiateCompressedWasm(importObject) {
  const response = await fetch("walletdk.wasm.gz");
  if (!response.ok) {
    throw new Error("walletdk wasm artifact not found");
  }

  const stream = response.body.pipeThrough(new DecompressionStream("gzip"));
  const bytes = await new Response(stream).arrayBuffer();
  return WebAssembly.instantiate(bytes, importObject);
}

async function instantiateRawWasm(importObject) {
  const response = await fetch("walletdk.wasm");
  if (!response.ok) {
    throw new Error("walletdk wasm artifact not found");
  }

  return WebAssembly.instantiateStreaming(response, importObject);
}

boot().catch((err) => {
  statusEl.textContent = "wasm load failed";
  log(errorMessage(err));
});
