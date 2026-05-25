const fs = require("fs");
const http = require("http");
const path = require("path");

const host = process.env.HOST || "127.0.0.1";
const port = Number(process.env.PORT || 8091);
const dist = path.join(__dirname, "dist");

const operatorPubkey = "Anm+Zn753LusVaBilc6HCwcCm/zbLc4o2VnygVsW+BeY";
const testPaymentHash = Buffer.alloc(32, 1).toString("base64");
const regtestGenesis =
  "0f9188f13cb7b2c71f2a335e3a4fc328bf5beb436012afca590b1a11466e2206";
const zeroHeader = "00".repeat(80);
const mailboxQueues = new Map();
let mailboxSeq = 1;

function setHeaders(res, contentType = "application/json") {
  res.setHeader("Cross-Origin-Opener-Policy", "same-origin");
  res.setHeader("Cross-Origin-Embedder-Policy", "require-corp");
  res.setHeader("Cross-Origin-Resource-Policy", "same-origin");
  res.setHeader("Access-Control-Allow-Origin", "*");
  res.setHeader("Access-Control-Allow-Headers", "content-type");
  res.setHeader("Content-Type", contentType);
}

function json(res, body) {
  setHeaders(res);
  res.end(JSON.stringify(body));
}

function text(res, body, statusCode = 200) {
  res.statusCode = statusCode;
  setHeaders(res, "text/plain");
  res.end(body);
}

function readJSON(req) {
  return new Promise((resolve, reject) => {
    let body = "";
    req.setEncoding("utf8");
    req.on("data", (chunk) => {
      body += chunk;
    });
    req.on("error", reject);
    req.on("end", () => {
      if (body === "") {
        resolve({});

        return;
      }

      try {
        resolve(JSON.parse(body));
      } catch (err) {
        reject(err);
      }
    });
  });
}

function enqueueMailbox(mailboxID, envelope) {
  if (!mailboxID) {
    return;
  }

  const nextEnvelope = {
    ...envelope,
    event_seq: String(mailboxSeq++),
  };
  const queue = mailboxQueues.get(mailboxID) || [];
  queue.push(nextEnvelope);
  mailboxQueues.set(mailboxID, queue);
}

function queueIndexerResponse(envelope) {
  const rpc = envelope?.rpc;
  if (
    rpc?.kind !== "KIND_REQUEST" ||
    rpc?.service !== "arkrpc.IndexerService" ||
    rpc?.method !== "RegisterReceiveScript"
  ) {
    return;
  }

  enqueueMailbox(rpc.reply_to, {
    protocol_version: envelope.protocol_version || 1,
    msg_id: `resp-${envelope.msg_id || mailboxSeq}`,
    idempotency_key: `resp-${envelope.idempotency_key || mailboxSeq}`,
    sender: envelope.recipient,
    recipient: rpc.reply_to,
    created_at_unix_ms: String(Date.now()),
    expires_at_unix_ms: String(Date.now() + 60 * 1000),
    type: "mailboxrpc.response",
    headers: {},
    body: {
      "@type":
        "type.googleapis.com/arkrpc.RegisterReceiveScriptResponse",
    },
    rpc: {
      kind: "KIND_RESPONSE",
      service: rpc.service,
      method: rpc.method,
      correlation_id: rpc.correlation_id,
    },
  });
}

function serveFile(res, filePath) {
  const ext = path.extname(filePath);
  const mimeTypes = {
    ".css": "text/css",
    ".html": "text/html",
    ".js": "application/javascript",
    ".json": "application/json",
    ".wasm": "application/wasm",
    ".gz": "application/gzip",
  };

  setHeaders(res, mimeTypes[ext] || "application/octet-stream");
  fs.createReadStream(filePath).pipe(res);
}

async function serveAPI(req, res, urlPath) {
  const apiPath = urlPath.startsWith("/api/") ? urlPath.slice(4) : urlPath;
  if (process.env.WALLETDK_SMOKE_VERBOSE) {
    console.log(`${req.method} ${apiPath}`);
  }

  if (apiPath === "/v1/ark/get-info") {
    json(res, {
      version: "playwright",
      pubkey: operatorPubkey,
      network: "regtest",
      block_height: 0,
      boarding_exit_delay: 1,
      vtxo_exit_delay: 1,
      sweep_key: operatorPubkey,
      sweep_delay: 2,
      dust_limit: "330",
      min_boarding_amount: "1000",
      max_boarding_amount: "0",
      fee_rate: "1",
      min_confirmations: 0,
      min_operator_fee: "0",
      max_oor_lineage_vbytes: 0,
    });

    return true;
  }

  if (apiPath === "/v1/ark/estimate-fee") {
    json(res, { total_fee_sat: "0" });

    return true;
  }

  if (apiPath === "/v1/mailbox/pull") {
    const body = await readJSON(req);
    const mailboxID = body.mailbox_id;
    const envelopes = mailboxQueues.get(mailboxID) || [];
    mailboxQueues.set(mailboxID, []);

    const respond = () => json(res, {
      status: { ok: true },
      envelopes,
      next_cursor: envelopes.length === 0
        ? String(body.cursor || 0)
        : String(Number(envelopes[envelopes.length - 1].event_seq) + 1),
    });

    if (envelopes.length === 0) {
      setTimeout(respond, 250);
    } else {
      respond();
    }

    return true;
  }

  if (apiPath === "/v1/mailbox/send") {
    const body = await readJSON(req);
    queueIndexerResponse(body.envelope);
    json(res, { status: { ok: true } });

    return true;
  }

  if (apiPath === "/v1/mailbox/ack-up-to") {
    json(res, { status: { ok: true } });

    return true;
  }

  if (apiPath === "/v1/swap/request-channel-id") {
    json(res, {
      route_hint: {
        node_id: operatorPubkey,
        channel_id: "42",
        fee_base_msat: "0",
        fee_proportional_ppm: "0",
        cltv_expiry_delta: 40,
      },
    });

    return true;
  }

  if (apiPath === "/v1/swap/create-in-swap") {
    json(res, {
      payment_hash: testPaymentHash,
      amount_sat: "1000",
      fee_sat: "0",
      server_pubkey: operatorPubkey,
      vhtlc_config: {
        refund_locktime: 144,
        unilateral_claim_delay: 1,
        unilateral_refund_delay: 1,
        unilateral_refund_without_receiver_delay: 1,
        swapserver_pubkey: operatorPubkey,
      },
      expiry: new Date(Date.now() + 15 * 60 * 1000).toISOString(),
      settlement_type: "SETTLEMENT_TYPE_LIGHTNING",
    });

    return true;
  }

  if (apiPath === "/blocks/tip/height") {
    text(res, "0");

    return true;
  }

  if (apiPath === "/blocks/tip/hash" || apiPath === "/block-height/0") {
    text(res, regtestGenesis);

    return true;
  }

  if (apiPath === `/block/${regtestGenesis}`) {
    json(res, {
      id: regtestGenesis,
      height: 0,
      version: 1,
      timestamp: 1296688602,
      tx_count: 0,
      size: 80,
      weight: 320,
      merkle_root:
        "4a5e1e4baab89f3a32518a88c31bc87f618f76673e2cc77ab2127b7afdeda33b",
      previousblockhash: "",
      mediantime: 1296688602,
      nonce: 2,
      bits: 545259519,
      difficulty: 1,
    });

    return true;
  }

  if (apiPath === `/block/${regtestGenesis}/header`) {
    text(res, zeroHeader);

    return true;
  }

  return false;
}

const server = http.createServer(async (req, res) => {
  if (req.method === "OPTIONS") {
    setHeaders(res);
    res.end();

    return;
  }

  const urlPath = decodeURIComponent(req.url.split("?")[0]);
  try {
    if (await serveAPI(req, res, urlPath)) {
      return;
    }
  } catch (err) {
    text(res, `bad request: ${err.message}`, 400);

    return;
  }

  const relative = urlPath === "/" ? "index.html" : urlPath.slice(1);
  const filePath = path.normalize(path.join(dist, relative));
  if (!filePath.startsWith(dist)) {
    text(res, "forbidden", 403);

    return;
  }

  if (fs.existsSync(filePath) && fs.statSync(filePath).isFile()) {
    serveFile(res, filePath);

    return;
  }

  text(res, "not found", 404);
});

server.listen(port, host, () => {
  console.log(`walletdk smoke server: http://${host}:${port}/`);
});
