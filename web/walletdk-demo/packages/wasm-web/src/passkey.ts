const wrapStoragePrefix = "walletdk:passkey-wrap:";
const defaultAppName = "Wallet";

export type PasskeyWrapRecord = {
  credentialId: string;
  wrappedPassword: string;
};

export type PasskeyWrapOptions = {
  appName?: string;
};

type PublicKeyCredentialWithPrf = typeof PublicKeyCredential & {
  getClientExtensionResults?: () => unknown;
};

// supportsPasskeyPrf reports whether this browser can use WebAuthn PRF for
// passkey-backed password wrapping.
export async function supportsPasskeyPrf(): Promise<boolean> {
  if (!globalThis.PublicKeyCredential || !globalThis.crypto?.subtle) {
    return false;
  }

  try {
    const platformAvailable = await PublicKeyCredential
      .isUserVerifyingPlatformAuthenticatorAvailable();
    if (!platformAvailable) {
      return false;
    }

    const pkc = PublicKeyCredential as PublicKeyCredentialWithPrf;
    pkc.getClientExtensionResults?.();

    return true;
  } catch {
    return false;
  }
}

// hasPasskeyWrap returns true when a passkey wrap exists for the data dir.
export function hasPasskeyWrap(dataDir: string): boolean {
  return loadPasskeyWrap(dataDir) !== null;
}

// loadPasskeyWrap reads the stored passkey wrap for a data dir, if any.
export function loadPasskeyWrap(dataDir: string): PasskeyWrapRecord | null {
  const raw = localStorage.getItem(wrapStorageKey(dataDir));
  if (!raw) {
    return null;
  }

  try {
    const parsed = JSON.parse(raw) as PasskeyWrapRecord;
    if (!parsed.credentialId || !parsed.wrappedPassword) {
      return null;
    }

    return parsed;
  } catch {
    return null;
  }
}

// createPasskeyWrap registers a passkey and stores the wallet password
// encrypted with a PRF-derived AES key scoped to the data dir.
export async function createPasskeyWrap(
  dataDir: string,
  password: string,
  options: PasskeyWrapOptions = {},
): Promise<void> {
  const appName = options.appName || defaultAppName;
  const namespace = wrapNamespace(dataDir);
  const challenge = await deterministicChallenge(namespace);
  const userId = crypto.getRandomValues(new Uint8Array(16));
  const displayName = `${appName} (${dataDir})`;

  const credential = await navigator.credentials.create({
    publicKey: {
      challenge: challenge.buffer as ArrayBuffer,
      rp: {
        name: appName,
        id: window.location.hostname,
      },
      user: {
        id: userId,
        name: displayName,
        displayName,
      },
      pubKeyCredParams: [
        { alg: -7, type: "public-key" },
        { alg: -257, type: "public-key" },
      ],
      authenticatorSelection: {
        authenticatorAttachment: "platform",
        userVerification: "required",
        residentKey: "required",
      },
      extensions: {
        prf: {
          eval: {
            first: challenge.buffer as ArrayBuffer,
          },
        },
      },
    },
  }) as PublicKeyCredential | null;

  if (!credential) {
    throw new Error("passkey registration was cancelled");
  }

  const encryptionKey = await deriveEncryptionKey(
    credential,
    challenge,
    namespace,
  );
  const wrappedPassword = await encryptString(encryptionKey, password);

  savePasskeyWrap(dataDir, {
    credentialId: credential.id,
    wrappedPassword,
  });
}

// unwrapPasskeyPassword authenticates with the stored passkey and returns
// the wrapped wallet password.
export async function unwrapPasskeyPassword(
  dataDir: string,
): Promise<string> {
  const record = loadPasskeyWrap(dataDir);
  if (!record) {
    throw new Error("no passkey unlock is configured for this wallet");
  }

  const namespace = wrapNamespace(dataDir);
  const challenge = await deterministicChallenge(namespace);

  const credential = await navigator.credentials.get({
    publicKey: {
      challenge: challenge.buffer as ArrayBuffer,
      allowCredentials: [{
        type: "public-key",
        id: base64ToArrayBuffer(record.credentialId),
      }],
      userVerification: "required",
      extensions: {
        prf: {
          eval: {
            first: challenge.buffer as ArrayBuffer,
          },
        },
      },
    },
  }) as PublicKeyCredential | null;

  if (!credential) {
    throw new Error("passkey authentication was cancelled");
  }

  const encryptionKey = await deriveEncryptionKey(
    credential,
    challenge,
    namespace,
  );

  return decryptString(encryptionKey, record.wrappedPassword);
}

// clearPasskeyWrap removes the stored passkey wrap for a data dir.
export function clearPasskeyWrap(dataDir: string): void {
  localStorage.removeItem(wrapStorageKey(dataDir));
}

function wrapStorageKey(dataDir: string): string {
  return `${wrapStoragePrefix}${dataDir}`;
}

function wrapNamespace(dataDir: string): string {
  return `walletdk-passkey:${dataDir}`;
}

function savePasskeyWrap(dataDir: string, record: PasskeyWrapRecord): void {
  localStorage.setItem(
    wrapStorageKey(dataDir),
    JSON.stringify(record),
  );
}

async function deterministicChallenge(namespace: string): Promise<Uint8Array> {
  const hash = await crypto.subtle.digest(
    "SHA-256",
    new TextEncoder().encode(namespace),
  );

  return new Uint8Array(hash);
}

async function deriveEncryptionKey(
  credential: PublicKeyCredential,
  challenge: Uint8Array,
  namespace: string,
): Promise<CryptoKey> {
  const extensions = credential.getClientExtensionResults?.() as {
    prf?: {
      results?: {
        first?: ArrayBuffer;
      };
    };
  } | undefined;

  if (!extensions?.prf?.results?.first) {
    throw new Error("passkey PRF is not supported in this browser");
  }

  const prfOutput = new Uint8Array(extensions.prf.results.first);
  const keyMaterial = await crypto.subtle.importKey(
    "raw",
    prfOutput,
    "HKDF",
    false,
    ["deriveKey"],
  );

  return crypto.subtle.deriveKey(
    {
      name: "HKDF",
      hash: "SHA-256",
      salt: new Uint8Array(challenge),
      info: new TextEncoder().encode(namespace),
    },
    keyMaterial,
    { name: "AES-GCM", length: 256 },
    false,
    ["encrypt", "decrypt"],
  );
}

async function encryptString(
  encryptionKey: CryptoKey,
  plaintext: string,
): Promise<string> {
  const iv = crypto.getRandomValues(new Uint8Array(12));
  const ciphertext = await crypto.subtle.encrypt(
    { name: "AES-GCM", iv },
    encryptionKey,
    new TextEncoder().encode(plaintext),
  );

  const combined = new Uint8Array(iv.length + ciphertext.byteLength);
  combined.set(iv);
  combined.set(new Uint8Array(ciphertext), iv.length);

  return arrayBufferToBase64(combined.buffer);
}

async function decryptString(
  encryptionKey: CryptoKey,
  payload: string,
): Promise<string> {
  const combined = new Uint8Array(base64ToArrayBuffer(payload));
  const iv = combined.slice(0, 12);
  const ciphertext = combined.slice(12);

  const plaintext = await crypto.subtle.decrypt(
    { name: "AES-GCM", iv },
    encryptionKey,
    ciphertext,
  );

  return new TextDecoder().decode(plaintext);
}

function arrayBufferToBase64(buffer: ArrayBuffer): string {
  const bytes = new Uint8Array(buffer);
  let binary = "";

  for (const byte of bytes) {
    binary += String.fromCharCode(byte);
  }

  return btoa(binary);
}

function base64ToArrayBuffer(base64: string): ArrayBuffer {
  const base64Standard = base64.replace(/-/g, "+").replace(/_/g, "/");
  const padded = base64Standard.padEnd(
    base64Standard.length + ((4 - (base64Standard.length % 4)) % 4),
    "=",
  );
  const binary = atob(padded);
  const bytes = new Uint8Array(binary.length);

  for (let i = 0; i < binary.length; i++) {
    bytes[i] = binary.charCodeAt(i);
  }

  return bytes.buffer;
}
