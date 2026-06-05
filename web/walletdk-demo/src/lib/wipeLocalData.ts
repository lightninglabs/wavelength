// PENDING_WIPE_KEY flags, across a reload, that the persisted wallet data should
// be wiped before the app and its OPFS-backed runtime start again.
const PENDING_WIPE_KEY = "walletdk:pending-wipe";

// THEME_KEY is preserved across a wipe: it is a UI preference, not wallet data.
const THEME_KEY = "walletdk-theme";

// requestWipe records the intent to wipe and reloads. The actual wipe runs at
// boot (consumePendingWipe), once the reload has torn down the SQLite worker so
// it no longer holds OPFS handles.
export function requestWipe(): void {
  sessionStorage.setItem(PENDING_WIPE_KEY, "1");
  location.reload();
}

// consumePendingWipe runs at boot. When a wipe was requested it clears the
// persisted wallet data (matching localStorage keys plus all OPFS entries),
// clears the flag, and resolves true. Otherwise it resolves false.
export async function consumePendingWipe(): Promise<boolean> {
  if (sessionStorage.getItem(PENDING_WIPE_KEY) !== "1") {
    return false;
  }

  clearLocalStorage();
  await clearOPFS();
  sessionStorage.removeItem(PENDING_WIPE_KEY);

  return true;
}

// clearLocalStorage removes every wallet-owned key (the darepod: and walletdk
// prefixes) while preserving the theme preference.
function clearLocalStorage(): void {
  for (const key of Object.keys(localStorage)) {
    if (key === THEME_KEY) {
      continue;
    }
    if (key.startsWith("darepod:") || key.startsWith("walletdk")) {
      localStorage.removeItem(key);
    }
  }
}

type DirWithEntries = FileSystemDirectoryHandle & {
  entries(): AsyncIterableIterator<[string, FileSystemHandle]>;
};

// clearOPFS deletes every entry in the origin-private file system, which holds
// the wallet's SQLite databases. Errors are logged rather than thrown so a
// stray handle never blocks boot.
async function clearOPFS(): Promise<void> {
  if (!navigator.storage?.getDirectory) {
    return;
  }

  try {
    const root = (await navigator.storage.getDirectory()) as DirWithEntries;
    for await (const [name] of root.entries()) {
      await root.removeEntry(name, { recursive: true });
    }
  } catch (err) {
    console.error("clearOPFS failed:", err);
  }
}
