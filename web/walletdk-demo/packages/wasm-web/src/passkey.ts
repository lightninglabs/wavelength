const PRF_NAMESPACE = "walletdk-passkey:v1";

// supportsPasskeyPrf reports whether a user-verifying platform authenticator is
// available, which is a prerequisite for WebAuthn PRF but does not guarantee
// that the authenticator will return a PRF result — there is no synchronous
// PRF-detection API, so a browser may return true here yet fail the ceremony.
export async function supportsPasskeyPrf(): Promise<boolean> {
  if (!globalThis.PublicKeyCredential || !globalThis.crypto?.subtle) {
    return false;
  }

  try {
    return await PublicKeyCredential
      .isUserVerifyingPlatformAuthenticatorAvailable();
  } catch {
    return false;
  }
}

// prfSalt is SHA-256(PRF_NAMESPACE): the fixed PRF evaluation input shared with
// the Go SDK (PasskeyPRFNamespace). The same salt on every device/platform is
// what makes the derived wallet reproducible.
async function prfSalt(): Promise<ArrayBuffer> {
  return crypto.subtle.digest(
    "SHA-256",
    new TextEncoder().encode(PRF_NAMESPACE),
  );
}

// prfFirst extracts the first PRF evaluation output from client extension
// results, throwing when the browser did not surface a PRF value.
function prfFirst(results: unknown): ArrayBuffer {
  const ext = results as { prf?: { results?: { first?: ArrayBuffer } } };
  const first = ext?.prf?.results?.first;
  if (!first) {
    throw new Error(
      "passkey PRF extension result was not returned by this authenticator",
    );
  }

  return first;
}

// PasskeyAssertion is the PRF output (hex) plus the credential id that produced
// it, so callers can scope later assertions to the same passkey.
export type PasskeyAssertion = { prfOutput: string; credentialId: string };

// registerPasskeyWallet creates a new platform passkey and returns its PRF
// output (hex) and credential id. Some browsers do not surface PRF from
// create(), so we fall back to an assertion scoped to the just-created
// credential to read it reliably without prompting a chooser.
export async function registerPasskeyWallet(
  appName: string,
): Promise<PasskeyAssertion> {
  const salt = await prfSalt();
  const userId = crypto.getRandomValues(new Uint8Array(16));

  const created = (await navigator.credentials.create({
    publicKey: {
      challenge: salt,
      rp: { name: appName, id: window.location.hostname },
      user: { id: userId, name: appName, displayName: appName },
      pubKeyCredParams: [
        { alg: -7, type: "public-key" },
        { alg: -257, type: "public-key" },
      ],
      authenticatorSelection: {
        authenticatorAttachment: "platform",
        userVerification: "required",
        residentKey: "required",
      },
      extensions: { prf: { eval: { first: salt } } },
    },
  })) as PublicKeyCredential | null;
  if (!created) {
    throw new Error("passkey registration was cancelled");
  }

  const createResults = created.getClientExtensionResults() as {
    prf?: { results?: { first?: ArrayBuffer } };
  };
  if (createResults?.prf?.results?.first) {
    return {
      prfOutput: bufferToHex(createResults.prf.results.first),
      credentialId: created.id,
    };
  }

  return assertPasskeyPrf(created.id);
}

// assertPasskeyPrf authenticates with a passkey and returns its PRF output (hex)
// plus the credential id used. With allowCredentialId set, the assertion is
// scoped to that one credential so the OS unlocks it directly without a
// chooser; without it, the assertion is discoverable (empty allowCredentials)
// so a synced passkey can be offered on a device that has never seen this
// wallet.
export async function assertPasskeyPrf(
  allowCredentialId?: string,
): Promise<PasskeyAssertion> {
  const salt = await prfSalt();
  const allowCredentials = allowCredentialId
    ? [{ type: "public-key" as const, id: base64UrlToBuffer(allowCredentialId) }]
    : [];

  const assertion = (await navigator.credentials.get({
    publicKey: {
      challenge: salt,
      allowCredentials,
      userVerification: "required",
      extensions: { prf: { eval: { first: salt } } },
    },
  })) as PublicKeyCredential | null;
  if (!assertion) {
    throw new Error("passkey authentication was cancelled");
  }

  return {
    prfOutput: bufferToHex(prfFirst(assertion.getClientExtensionResults())),
    credentialId: assertion.id,
  };
}

// base64UrlToBuffer decodes a base64url WebAuthn credential id into bytes.
function base64UrlToBuffer(value: string): ArrayBuffer {
  const base64 = value.replace(/-/g, "+").replace(/_/g, "/");
  const padded = base64.padEnd(
    base64.length + ((4 - (base64.length % 4)) % 4),
    "=",
  );
  const binary = atob(padded);
  const bytes = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i++) {
    bytes[i] = binary.charCodeAt(i);
  }

  return bytes.buffer;
}

// bufferToHex renders a binary buffer as a lower-case hex string.
function bufferToHex(buffer: ArrayBuffer): string {
  return Array.from(new Uint8Array(buffer))
    .map((b) => b.toString(16).padStart(2, "0"))
    .join("");
}
