import assert from "node:assert/strict";
import { afterEach, beforeEach, describe, it, mock } from "node:test";
import { assertPasskeyPrf } from "./passkey.ts";

// The browser globals these tests stub out are not present under Node, so we
// snapshot and restore whatever was there to keep the cases isolated.
const savedCrypto = globalThis.crypto;
const savedNavigator = (globalThis as { navigator?: unknown }).navigator;

function stubGlobal(name: string, value: unknown): void {
  Object.defineProperty(globalThis, name, {
    value,
    configurable: true,
    writable: true,
  });
}

describe("assertPasskeyPrf", () => {
  beforeEach(() => {
    stubGlobal("crypto", {
      subtle: { digest: async () => new Uint8Array(32).buffer },
    });
  });

  afterEach(() => {
    stubGlobal("crypto", savedCrypto);
    stubGlobal("navigator", savedNavigator);
  });

  it("requests a discoverable credential and returns hex PRF and credential id", async () => {
    const get = mock.fn(async () => ({
      id: "cred-abc",
      getClientExtensionResults: () => ({
        prf: { results: { first: new Uint8Array([0xab, 0xcd]).buffer } },
      }),
    }));
    stubGlobal("navigator", { credentials: { get } });

    const result = await assertPasskeyPrf();

    assert.equal(result.prfOutput, "abcd");
    assert.equal(result.credentialId, "cred-abc");

    const opts = (get.mock.calls[0].arguments[0] as {
      publicKey: { allowCredentials: unknown[]; userVerification: string };
    }).publicKey;
    assert.deepEqual(opts.allowCredentials, []);
    assert.equal(opts.userVerification, "required");
  });

  it("scopes assertion to the given credential id without a chooser", async () => {
    const get = mock.fn(async () => ({
      id: "cred-xyz",
      getClientExtensionResults: () => ({
        prf: { results: { first: new Uint8Array([0xab, 0xcd]).buffer } },
      }),
    }));
    stubGlobal("navigator", { credentials: { get } });

    await assertPasskeyPrf("cred-xyz");

    const opts = (get.mock.calls[0].arguments[0] as {
      publicKey: {
        allowCredentials: { type: string; id: ArrayBuffer }[];
        userVerification: string;
      };
    }).publicKey;
    assert.equal(opts.allowCredentials.length, 1);
    assert.equal(opts.allowCredentials[0].type, "public-key");
  });

  it("throws when getClientExtensionResults returns no prf key", async () => {
    const get = mock.fn(async () => ({
      id: "cred-noprf",
      // Authenticator succeeded but returned no PRF extension results.
      getClientExtensionResults: () => ({}),
    }));
    stubGlobal("navigator", { credentials: { get } });

    await assert.rejects(
      () => assertPasskeyPrf(),
      /passkey PRF extension result was not returned by this authenticator/,
    );
  });

  it("throws when navigator.credentials.get resolves to null (cancellation)", async () => {
    const get = mock.fn(async () => null);
    stubGlobal("navigator", { credentials: { get } });

    await assert.rejects(
      () => assertPasskeyPrf(),
      /passkey authentication was cancelled/,
    );
  });
});
