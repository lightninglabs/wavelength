// This script enables SharedArrayBuffer via a service worker for static hosts.
if (typeof window === "undefined") {
  self.addEventListener("install", () => self.skipWaiting());
  self.addEventListener("activate", (event) => {
    event.waitUntil(self.clients.claim());
  });

  async function handleFetch(request) {
    if (request.cache === "only-if-cached" && request.mode !== "same-origin") {
      return undefined;
    }

    if (request.mode === "no-cors") {
      request = new Request(request.url, {
        cache: request.cache,
        credentials: "omit",
        headers: request.headers,
        integrity: request.integrity,
        destination: request.destination,
        keepalive: request.keepalive,
        method: request.method,
        mode: request.mode,
        redirect: request.redirect,
        referrer: request.referrer,
        referrerPolicy: request.referrerPolicy,
        signal: request.signal,
      });
    }

    const response = await fetch(request).catch((err) => {
      console.error(err);
    });
    if (!response || response.status === 0) {
      return response;
    }

    const headers = new Headers(response.headers);
    headers.set("Cross-Origin-Embedder-Policy", "credentialless");
    headers.set("Cross-Origin-Opener-Policy", "same-origin");

    return new Response(response.body, {
      status: response.status,
      statusText: response.statusText,
      headers,
    });
  }

  self.addEventListener("fetch", (event) => {
    event.respondWith(handleFetch(event.request));
  });
} else {
  (async function enableThreads() {
    if (window.crossOriginIsolated !== false) {
      return;
    }

    const script = window.document.currentScript;
    const registration = await navigator.serviceWorker
      .register(script.src)
      .catch((err) => {
        console.error("COOP/COEP service worker failed to register:", err);
      });
    if (!registration) {
      return;
    }

    registration.addEventListener("updatefound", () => {
      window.location.reload();
    });

    if (registration.active && !navigator.serviceWorker.controller) {
      window.location.reload();
    }
  })();
}
