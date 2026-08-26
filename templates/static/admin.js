"use strict";

//
// HAVEN relay management client.
//
// Everything here talks NIP-86 over HTTP: a JSON body POSTed to the relay's own
// URL with an Authorization header carrying a NIP-98 event signed by a NIP-07
// browser extension. No key ever reaches this page.
//

const enc = new TextEncoder();
const cfg = document.body.dataset;

const OWNER_PUBKEY = cfg.ownerPubkey || "";
const OWNER_NPUB = cfg.ownerNpub || "";
const CONTENT_TYPE = "application/nostr+json+rpc";

// Endpoints come from the tab strip the server rendered. path is where the
// browser POSTs (same origin, so no CORS preflight); serviceURL is what the
// relay compares the auth event's u tag against, and the two differ whenever
// the owner is browsing through a tunnel.
const RELAYS = [...document.querySelectorAll(".relay-tab")].map((tab) => ({
  key: tab.dataset.key,
  // from the attribute rather than the node's text: the button also carries the
  // path in a nested span, so textContent would make the label "Outbox /" and
  // that reads as a typo everywhere it is printed in a sentence
  label: tab.dataset.label || tab.textContent.trim(),
  path: tab.dataset.path,
  serviceURL: tab.dataset.serviceUrl,
  tab,
}));

// The ban, allow and IP lists are relay-wide. Pinning them to one endpoint
// keeps their signed u tag and their printed endpoint from moving when the
// owner switches tabs, which is the whole point of the two-column split.
const GLOBAL = RELAYS.find((r) => r.key === "outbox") || RELAYS[0];

let selected = GLOBAL;
let connectedPubkey = null;

//
// primitives
//

function toHex(buffer) {
  const bytes = new Uint8Array(buffer);
  let out = "";
  for (let i = 0; i < bytes.length; i++) out += bytes[i].toString(16).padStart(2, "0");
  return out;
}

async function sha256Hex(bytes) {
  return toHex(await crypto.subtle.digest("SHA-256", bytes));
}

// btoa() works on Latin-1 code units and throws on anything above U+00FF. Relay
// names and ban reasons routinely carry emoji, so the event has to be turned
// into UTF-8 bytes first. Chunked because String.fromCharCode.apply blows the
// call stack somewhere past a hundred thousand arguments.
function base64FromBytes(bytes) {
  let binary = "";
  const CHUNK = 0x8000;
  for (let i = 0; i < bytes.length; i += CHUNK) {
    binary += String.fromCharCode.apply(null, bytes.subarray(i, i + CHUNK));
  }
  return btoa(binary);
}

//
// bech32, so the page can speak npub while the API speaks hex
//

const B32 = "qpzry9x8gf2tvdw0s3jn54khce6mua7l";
const B32_REV = Object.fromEntries([...B32].map((c, i) => [c, i]));
const GEN = [0x3b6a57b2, 0x26508e6d, 0x1ea119fa, 0x3d4233dd, 0x2a1462b3];

function polymod(values) {
  let chk = 1;
  for (const v of values) {
    const top = chk >>> 25;
    chk = ((chk & 0x1ffffff) << 5) ^ v;
    for (let i = 0; i < 5; i++) if ((top >>> i) & 1) chk ^= GEN[i];
  }
  return chk >>> 0;
}

function hrpExpand(hrp) {
  const out = [];
  for (let i = 0; i < hrp.length; i++) out.push(hrp.charCodeAt(i) >>> 5);
  out.push(0);
  for (let i = 0; i < hrp.length; i++) out.push(hrp.charCodeAt(i) & 31);
  return out;
}

function convertBits(data, from, to, pad) {
  let acc = 0;
  let bits = 0;
  const out = [];
  const maxv = (1 << to) - 1;
  for (const value of data) {
    if (value < 0 || value >> from !== 0) throw new Error("invalid value");
    acc = (acc << from) | value;
    bits += from;
    while (bits >= to) {
      bits -= to;
      out.push((acc >> bits) & maxv);
    }
  }
  if (pad) {
    if (bits > 0) out.push((acc << (to - bits)) & maxv);
  } else if (bits >= from || ((acc << (to - bits)) & maxv)) {
    throw new Error("invalid padding");
  }
  return out;
}

// hexToBech32 encodes 32 bytes of hex under any human readable prefix. It was
// hexToNpub with "npub" baked in until the notes browser needed note1 ids too.
function hexToBech32(hex, hrp) {
  if (!/^[0-9a-f]{64}$/.test(hex)) return hex;
  const bytes = [];
  for (let i = 0; i < 64; i += 2) bytes.push(parseInt(hex.slice(i, i + 2), 16));
  const data = convertBits(bytes, 8, 5, true);
  // six zero values, not five: bech32 reserves one per checksum character
  const chk = polymod(hrpExpand(hrp).concat(data, [0, 0, 0, 0, 0, 0])) ^ 1;
  const checksum = [];
  for (let i = 0; i < 6; i++) checksum.push((chk >>> (5 * (5 - i))) & 31);
  return hrp + "1" + data.concat(checksum).map((d) => B32[d]).join("");
}

function hexToNpub(hex) {
  return hexToBech32(hex, "npub");
}

function hexToNote(hex) {
  return hexToBech32(hex, "note");
}

function npubToHex(input) {
  const raw = String(input).trim();
  if (raw !== raw.toLowerCase() && raw !== raw.toUpperCase()) {
    throw new Error("a mixed-case npub is not valid bech32");
  }
  const s = raw.toLowerCase();
  if (!s.startsWith("npub1")) throw new Error("not an npub");
  const dataPart = s.slice(5);
  if (dataPart.length < 7) throw new Error("that npub is too short");

  const values = [];
  for (const c of dataPart) {
    const v = B32_REV[c];
    if (v === undefined) throw new Error(`invalid character "${c}" in that npub`);
    values.push(v);
  }
  if (polymod(hrpExpand("npub").concat(values)) !== 1) {
    throw new Error("that npub's checksum is wrong — it was probably mistyped or truncated");
  }
  const bytes = convertBits(values.slice(0, -6), 5, 8, false);
  if (bytes.length !== 32) throw new Error("that npub does not encode a 32 byte key");
  return bytes.map((b) => b.toString(16).padStart(2, "0")).join("");
}

// toPubkeyHex normalises whatever the owner pasted. The nsec branch is checked
// before anything is decoded, and the caller clears the field straight away:
// nothing about a private key is decoded, logged or sent.
function toPubkeyHex(input) {
  let s = String(input).trim();
  if (s.startsWith("nostr:")) s = s.slice(6);

  if (/^nsec1/i.test(s)) {
    throw new Error(
      "That is a PRIVATE key (nsec), not a public key. Nothing was sent. " +
        "Clear it from your clipboard and treat it as compromised if it has been pasted anywhere else."
    );
  }
  const other = /^(nprofile|note|nevent|naddr)1/i.exec(s);
  if (other) throw new Error(`That is an ${other[1]}, not an npub. Paste an npub or 64 hex characters.`);
  if (/^[0-9a-fA-F]{64}$/.test(s)) return s.toLowerCase();
  if (/^npub1/i.test(s)) return npubToHex(s);
  throw new Error("Enter a pubkey as npub1… or 64 hex characters.");
}

function shortNpub(hex) {
  const npub = hexToNpub(hex);
  return npub.length > 20 ? `${npub.slice(0, 12)}…${npub.slice(-6)}` : npub;
}

//
// errors
//

class ApiError extends Error {
  constructor(kind, message) {
    super(message);
    this.kind = kind;
  }
}

const CLOCK_RE = /created_at|too old|expired|clock|skew/i;
const AUTH_RE = /auth|signature|u tag|payload|unauthorized|restricted|owner|method tag/i;
const UNSUPPORTED_RE = /not supported|not known|unknown method|is disabled/i;

function classify(message) {
  if (CLOCK_RE.test(message)) return "clock";
  if (UNSUPPORTED_RE.test(message)) return "unsupported";
  if (AUTH_RE.test(message)) return "auth";
  return "api";
}

//
// clock skew
//
// The relay allows sixty seconds either way. A browser clock further out than
// that fails every single call with something that reads like an auth bug, so
// the Date header — always sent by Go, and readable because this is same origin
// — is used to correct the timestamps and to tell the owner what is wrong.
//

let clockSkewSeconds = 0;

function serverEpoch(response) {
  const header = response.headers.get("Date");
  if (!header) return null;
  const parsed = Date.parse(header);
  return Number.isFinite(parsed) ? Math.floor(parsed / 1000) : null;
}

function adoptSkew(response) {
  const server = serverEpoch(response);
  if (server === null) return false;
  const drift = server - Math.floor(Date.now() / 1000);
  // bounded, so a broken proxy Date header cannot push timestamps anywhere absurd
  if (Math.abs(drift) < 5 || Math.abs(drift) > 86400) return false;
  clockSkewSeconds = drift;
  showBanner(
    "clock",
    "warn",
    `Your computer's clock is ${Math.abs(drift)}s ${drift > 0 ? "behind" : "ahead of"} the relay. ` +
      "Timestamps are being corrected, but the clock should be fixed."
  );
  return true;
}

//
// the NIP-86 call
//

async function nip86(endpoint, method, params = [], retry = true) {
  if (!window.nostr) throw new ApiError("auth", "no NIP-07 signer is available");

  // serialised once: hashing a different string than the one that is sent is
  // the classic NIP-98 mistake, and keeping a single byte array makes it
  // impossible
  const body = enc.encode(JSON.stringify({ method, params }));
  const payload = await sha256Hex(body);

  let authEvent;
  try {
    authEvent = await window.nostr.signEvent({
      kind: 27235,
      created_at: Math.floor(Date.now() / 1000) + clockSkewSeconds,
      content: "",
      tags: [
        ["u", endpoint.serviceURL],
        ["method", "POST"],
        ["payload", payload],
      ],
    });
  } catch (e) {
    throw new ApiError("auth", "your signer refused or cancelled the request");
  }

  let response;
  try {
    response = await fetch(endpoint.path, {
      method: "POST",
      headers: {
        "Content-Type": CONTENT_TYPE,
        Authorization: "Nostr " + base64FromBytes(enc.encode(JSON.stringify(authEvent))),
      },
      body,
      cache: "no-store",
      credentials: "omit",
      redirect: "error",
    });
  } catch (e) {
    throw new ApiError("network", "could not reach the relay");
  }

  const text = await response.text();
  let parsed = null;
  try {
    parsed = JSON.parse(text);
  } catch (e) {
    /* not JSON; handled below */
  }

  const inBand = parsed && typeof parsed.error === "string" && parsed.error !== "" ? parsed.error : "";

  if (!response.ok) {
    const message = inBand || `the relay returned HTTP ${response.status}`;
    const kind = classify(message);
    if (kind === "clock" && retry && adoptSkew(response)) {
      return nip86(endpoint, method, params, false);
    }
    throw new ApiError(response.status === 401 ? "auth" : kind, message);
  }
  if (!parsed) throw new ApiError("protocol", "the relay did not return JSON");
  if (inBand) throw new ApiError(classify(inBand), inBand);

  return parsed.result;
}

// The relay's own NIP-11 document. Unauthenticated, so it costs no signature
// prompt, and it always reflects whatever changerelayname last did.
async function fetchRelayInfo(endpoint) {
  const response = await fetch(endpoint.path, {
    headers: { Accept: "application/nostr+json" },
    cache: "no-store",
  });
  if (!response.ok) throw new ApiError("http", `could not read the relay's NIP-11 document (HTTP ${response.status})`);
  return response.json();
}

//
// DOM helpers
//
// Nothing that came off the wire is ever written as markup. Reasons, relay
// names, error strings and every key of the stats object go in through
// textContent, and there is no innerHTML in this file.
//

function el(tag, className, text) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text !== undefined && text !== null) node.textContent = String(text);
  return node;
}

function clear(node) {
  while (node.firstChild) node.removeChild(node.firstChild);
}

function button(label, className, onClick) {
  const node = el("button", className, label);
  node.type = "button";
  node.addEventListener("click", onClick);
  return node;
}

function lockedCell(label, explanation) {
  const cell = el("td", "text-xs text-gray-500");
  cell.appendChild(el("span", "", `🔒 ${label}`));
  cell.appendChild(el("div", "", explanation));
  return cell;
}

// markViewUnavailable disables a tab whose script did not load. The page is four
// classic scripts now, and a stale cache or a half-finished deploy can serve
// three of them; the tab saying which file is missing is a better failure than a
// panel that never responds.
function markViewUnavailable(view, src) {
  const tab = document.getElementById(`view-tab-${view}`);
  const panel = document.getElementById(`view-${view}`);
  if (tab) {
    tab.disabled = true;
    tab.title = `${src} did not load`;
    tab.classList.add("opacity-50");
  }
  if (!panel) return;
  const section = panel.querySelector("[data-panel]");
  if (section) setStatus(section, "error", `${src} did not load, so this view is unavailable.`);
}

function showBanner(id, level, message) {
  const banners = document.getElementById("banners");
  let banner = document.getElementById(`banner-${id}`);
  if (!banner) {
    banner = el("div", "");
    banner.id = `banner-${id}`;
    banners.appendChild(banner);
  }
  const colours = {
    error: "border-red-700 bg-red-950/50 text-red-200",
    warn: "border-amber-600 bg-amber-950/40 text-amber-200",
    info: "border-gray-700 bg-gray-800/50 text-gray-300",
  };
  banner.className = `rounded border px-4 py-3 text-sm ${colours[level] || colours.info}`;
  clear(banner);
  banner.appendChild(el("p", "", message));
  return banner;
}

function setStatus(panel, level, message) {
  const status = panel.querySelector(".status");
  if (!status) return;
  const colours = { error: "text-red-300", ok: "text-green-300", info: "text-gray-400" };
  status.className = `status text-sm mt-3 ${colours[level] || colours.info}`;
  status.textContent = message || "";
}

function describeError(error) {
  if (!(error instanceof ApiError)) return String(error && error.message ? error.message : error);
  switch (error.kind) {
    case "network":
      return `Could not reach the relay: ${error.message}`;
    case "clock":
      return `Rejected because of the clock: ${error.message}`;
    case "auth":
      return `Rejected: ${error.message}`;
    default:
      return error.message;
  }
}

//
// confirmation
//

// confirmActionWithOptions is the confirmation dialog plus the two things
// deleting media needs: checkboxes that ride along with the decision, and a
// typed guard for deletions too large to take back. It resolves to null when the
// owner backed out and to a map of option values when they went ahead, so
// "cancelled" and "confirmed with nothing ticked" stay distinguishable.
function confirmActionWithOptions({ title, body, detail, danger, options = [], requireTyped = "" }) {
  const dialog = document.getElementById("confirm-dialog");

  if (!dialog || typeof dialog.showModal !== "function") {
    // the fallback path cannot render checkboxes, so it must not report that
    // anything was ticked — every option comes back false, and a typed guard is
    // asked for separately rather than quietly dropped
    if (!window.confirm(`${title}\n\n${body}\n\n${detail || ""}`)) return Promise.resolve(null);
    if (requireTyped && window.prompt(`Type ${requireTyped} to confirm`) !== requireTyped) {
      return Promise.resolve(null);
    }
    return Promise.resolve(Object.fromEntries(options.map((option) => [option.key, false])));
  }

  const optionBox = document.getElementById("confirm-options");
  const typedWrap = document.getElementById("confirm-typed-wrap");
  const typedInput = document.getElementById("confirm-typed");
  const okButton = document.getElementById("confirm-ok");

  document.getElementById("confirm-title").textContent = title;
  document.getElementById("confirm-body").textContent = body;
  document.getElementById("confirm-detail").textContent = detail || "";
  okButton.textContent = danger || "Confirm";

  clear(optionBox);
  const boxes = new Map();
  for (const option of options) {
    const label = el("label", "flex items-start gap-2 text-sm text-gray-300");
    const input = document.createElement("input");
    input.type = "checkbox";
    input.className = "mt-1";
    input.checked = Boolean(option.checked);
    label.appendChild(input);
    label.appendChild(el("span", "", option.label));
    optionBox.appendChild(label);
    boxes.set(option.key, input);
  }
  optionBox.hidden = options.length === 0;

  typedInput.value = "";
  typedWrap.hidden = !requireTyped;
  document.getElementById("confirm-typed-word").textContent = requireTyped;

  const check = () => {
    okButton.disabled = Boolean(requireTyped) && typedInput.value.trim() !== requireTyped;
  };
  check();
  typedInput.addEventListener("input", check);

  return new Promise((resolve) => {
    dialog.addEventListener(
      "close",
      () => {
        typedInput.removeEventListener("input", check);
        okButton.disabled = false;
        // the boxes are read before they are torn down: the form has already
        // closed by the time this runs, but the nodes are still here
        const chosen =
          dialog.returnValue === "ok"
            ? Object.fromEntries([...boxes].map(([key, input]) => [key, input.checked]))
            : null;
        clear(optionBox);
        optionBox.hidden = true;
        typedWrap.hidden = true;
        resolve(chosen);
      },
      { once: true }
    );
    dialog.showModal();
  });
}

function confirmAction(spec) {
  return confirmActionWithOptions(spec).then((chosen) => chosen !== null);
}

//
// panels
//

const panels = {};

function panelNode(name) {
  return document.querySelector(`[data-panel="${name}"]`);
}

// Panels start collapsed behind a Load button because NIP-86 has no batching:
// one method call is one signature prompt, and populating seven panels on
// connect would fire seven prompts in a row.
function definePanel(name, { endpoint, load }) {
  const node = panelNode(name);
  if (!node) return;

  const panel = {
    name,
    node,
    body: node.querySelector(".panel-body"),
    endpoint,
    load,
    loaded: false,
  };
  panels[name] = panel;

  const loadButton = node.querySelector('[data-action="load"]');
  if (loadButton) loadButton.addEventListener("click", () => refreshPanel(name, true));
  return panel;
}

async function refreshPanel(name, remember) {
  const panel = panels[name];
  if (!panel) return;

  setStatus(panel.node, "info", "loading…");
  try {
    await panel.load(panel.endpoint());
    panel.loaded = true;
    if (panel.body) panel.body.hidden = false;
    setStatus(panel.node, "info", "");
    if (remember) rememberPanel(name);
  } catch (error) {
    setStatus(panel.node, "error", describeError(error));
  }
}

// Which panels the owner actually uses is remembered for the tab, so the
// habitual workflow costs its signature prompts once rather than every reload.
function rememberPanel(name) {
  try {
    const open = new Set(JSON.parse(sessionStorage.getItem("haven-admin-panels") || "[]"));
    open.add(name);
    sessionStorage.setItem("haven-admin-panels", JSON.stringify([...open]));
  } catch (e) {
    /* private mode, or storage disabled: not worth reporting */
  }
}

function rememberedPanels() {
  try {
    return JSON.parse(sessionStorage.getItem("haven-admin-panels") || "[]");
  } catch (e) {
    return [];
  }
}

function setEndpointLabels() {
  for (const panel of Object.values(panels)) {
    const label = panel.node.querySelector(".endpoint");
    if (label) label.textContent = `POST ${panel.endpoint().serviceURL}`;
  }
}

//
// relay information
//

function definePanels() {
  const perRelay = () => selected;
  const global = () => GLOBAL;

  definePanel("info", {
    endpoint: perRelay,
    load: async (endpoint) => {
      const info = await fetchRelayInfo(endpoint);
      document.getElementById("info-name").value = info.name || "";
      document.getElementById("info-description").value = info.description || "";
      document.getElementById("info-icon").value = info.icon || "";
      showIconPreview(info.icon || "");
    },
  });

  definePanel("kinds", {
    endpoint: perRelay,
    load: async (endpoint) => {
      const [allowed, disallowed] = await Promise.all([
        nip86(endpoint, "listallowedkinds"),
        nip86(endpoint, "listdisallowedkinds"),
      ]);
      renderKinds("kinds-allowed", allowed || [], "allowed");
      renderKinds("kinds-disallowed", disallowed || [], "disallowed");
    },
  });

  definePanel("events", {
    endpoint: perRelay,
    load: async (endpoint) => renderBannedEvents(await nip86(endpoint, "listbannedevents")),
  });

  definePanel("stats", {
    endpoint: perRelay,
    load: async (endpoint) => renderStats(await nip86(endpoint, "stats")),
  });

  definePanel("banned", {
    endpoint: global,
    load: async (endpoint) => renderPubkeys("banned-rows", await nip86(endpoint, "listbannedpubkeys"), "ban"),
  });

  definePanel("allowed", {
    endpoint: global,
    load: async (endpoint) => renderPubkeys("allowed-rows", await nip86(endpoint, "listallowedpubkeys"), "allow"),
  });

  definePanel("ips", {
    endpoint: global,
    load: async (endpoint) => renderBlockedIPs(await nip86(endpoint, "listblockedips")),
  });

  // Blossom is served by the outbox relay alone, so all three of these are
  // pinned to the global endpoint like the ban and allow lists are.
  definePanel("media", { endpoint: global, load: loadMediaInventory });

  definePanel("media-blocked", {
    endpoint: global,
    load: async (endpoint) => {
      // the inventory call already brought these back, so opening this panel
      // after the library costs no second signature
      if (!media.loaded) media.blocked = (await nip86(endpoint, "listblockedblobs")) || [];
      renderBlockedBlobs();
    },
  });

  definePanel("media-orphans", {
    endpoint: global,
    load: async (endpoint) => renderOrphans(await nip86(endpoint, "listorphanblobs")),
  });
}

function showIconPreview(url) {
  const preview = document.getElementById("info-icon-preview");
  let safe = null;
  // an empty value would resolve against location.href and preview this very
  // page as an image, so it is rejected before parsing
  if (String(url || "").trim() !== "") {
    try {
      const parsed = new URL(url, location.href);
      if (parsed.protocol === "http:" || parsed.protocol === "https:") safe = parsed.href;
    } catch (e) {
      safe = null;
    }
  }
  if (safe) {
    preview.src = safe;
    preview.hidden = false;
  } else {
    preview.removeAttribute("src");
    preview.hidden = true;
  }
}

function renderKinds(containerId, kinds, kind) {
  const container = document.getElementById(containerId);
  clear(container);
  if (!kinds.length) {
    container.appendChild(el("span", "text-xs text-gray-500", kind === "allowed" ? "no allow list — every kind this relay otherwise accepts gets through" : "none"));
    return;
  }
  for (const value of kinds) {
    const chip = el("span", "inline-flex items-center gap-2 rounded bg-gray-800 px-2 py-1 text-sm");
    chip.appendChild(el("span", "mono", value));
    chip.appendChild(
      button("×", "text-gray-400 hover:text-white", async () => {
        // taking a kind off one list is the same call as putting it on the other
        const method = kind === "allowed" ? "disallowkind" : "allowkind";
        await runAction("kinds", () => nip86(selected, method, [value]));
      })
    );
    container.appendChild(chip);
  }
}

function renderPubkeys(rowsId, entries, mode) {
  const rows = document.getElementById(rowsId);
  clear(rows);
  entries = entries || [];

  if (!entries.length) {
    const row = el("tr");
    row.appendChild(el("td", "text-sm text-gray-500", "none"));
    rows.appendChild(row);
    return;
  }

  for (const entry of entries) {
    const row = el("tr");

    const key = el("td", "text-sm");
    const label = el("span", "mono text-purple-300", shortNpub(entry.pubkey));
    label.title = hexToNpub(entry.pubkey);
    key.appendChild(label);
    row.appendChild(key);

    row.appendChild(el("td", "text-sm text-gray-300", entry.reason || ""));

    // an entry the relay cannot undo says so instead of offering a button that
    // will fail
    if (entry.source === "list") {
      row.appendChild(lockedCell("kind 10084 list", "publish an updated list from a nostr client to remove this"));
    } else if (entry.source === "file") {
      row.appendChild(lockedCell("npubs file", "edit the file and restart HAVEN"));
    } else if (entry.source === "owner") {
      row.appendChild(lockedCell("relay owner", "the owner is always allowed"));
    } else {
      const action = el("td", "text-right");
      const verb = mode === "ban" ? "Unban" : "Remove";
      action.appendChild(
        button(verb, "rounded border border-gray-600 px-2 py-1 text-xs", async () => {
          const ok = await confirmAction({
            title: `${verb} this pubkey?`,
            body:
              mode === "ban"
                ? "This pubkey will be able to write to your relay again."
                : "This pubkey will lose owner-level access to your relay.",
            detail: hexToNpub(entry.pubkey),
            danger: verb,
          });
          if (!ok) return;
          const method = mode === "ban" ? "unbanpubkey" : "unallowpubkey";
          await runAction(mode === "ban" ? "banned" : "allowed", () => nip86(GLOBAL, method, [entry.pubkey]));
        })
      );
      row.appendChild(action);
    }

    rows.appendChild(row);
  }
}

function renderBannedEvents(entries) {
  const rows = document.getElementById("events-rows");
  clear(rows);
  entries = entries || [];

  if (!entries.length) {
    const row = el("tr");
    row.appendChild(el("td", "text-sm text-gray-500", "none"));
    rows.appendChild(row);
    return;
  }

  for (const entry of entries) {
    const row = el("tr");
    const id = el("td", "mono break-all text-xs text-purple-300", entry.id);
    row.appendChild(id);
    row.appendChild(el("td", "text-sm text-gray-300", entry.reason || ""));

    const action = el("td", "text-right");
    action.appendChild(
      button("Unban", "rounded border border-gray-600 px-2 py-1 text-xs", async () => {
        const ok = await confirmAction({
          title: "Unban this event?",
          body: "It will be accepted again if somebody publishes it. The copy that was deleted does not come back.",
          detail: entry.id,
          danger: "Unban",
        });
        if (!ok) return;
        await runAction("events", () => nip86(selected, "allowevent", [entry.id]));
      })
    );
    row.appendChild(action);
    rows.appendChild(row);
  }
}

function renderBlockedIPs(entries) {
  const rows = document.getElementById("ips-rows");
  clear(rows);
  entries = entries || [];

  if (!entries.length) {
    const row = el("tr");
    row.appendChild(el("td", "text-sm text-gray-500", "none"));
    rows.appendChild(row);
    return;
  }

  for (const entry of entries) {
    const row = el("tr");
    row.appendChild(el("td", "mono text-sm text-purple-300", entry.ip));
    row.appendChild(el("td", "text-sm text-gray-300", entry.reason || ""));

    const action = el("td", "text-right");
    action.appendChild(
      button("Unblock", "rounded border border-gray-600 px-2 py-1 text-xs", async () => {
        const ok = await confirmAction({
          title: "Unblock this address?",
          body: "Connections from it will be accepted again.",
          detail: entry.ip,
          danger: "Unblock",
        });
        if (!ok) return;
        await runAction("ips", () => nip86(GLOBAL, "unblockip", [entry.ip]));
      })
    );
    row.appendChild(action);
    rows.appendChild(row);
  }
}

function renderStats(stats) {
  const rows = document.getElementById("stats-rows");
  clear(rows);
  if (!stats || typeof stats !== "object") return;

  // walked generically: the shape is not fixed, and both keys and values come
  // off the wire, so both go in as text
  const add = (label, value) => {
    const row = el("tr");
    row.appendChild(el("td", "text-sm text-gray-400", label));
    row.appendChild(el("td", "mono text-sm text-right", value));
    rows.appendChild(row);
  };

  for (const [key, value] of Object.entries(stats)) {
    const label = key.replace(/_/g, " ");
    if (value && typeof value === "object") {
      for (const [subKey, subValue] of Object.entries(value)) {
        add(`${label} · ${subKey}`, typeof subValue === "number" ? subValue.toLocaleString() : String(subValue));
      }
    } else {
      add(label, typeof value === "number" ? value.toLocaleString() : String(value));
    }
  }
}

//
// actions
//
// A mutating method answers true, not the new list, so there is nothing
// authoritative to adopt from the response — and a refetch would cost a second
// signature prompt for every action. On success the panel is reloaded only when
// the call is cheap enough to justify it; on failure it always is, because the
// view on screen may now be wrong.
//

async function runAction(panelName, call) {
  const panel = panels[panelName];
  if (panel) setStatus(panel.node, "info", "working…");
  try {
    await call();
    if (panel) {
      await panel.load(panel.endpoint());
      setStatus(panel.node, "ok", "done");
    }
  } catch (error) {
    if (panel) {
      setStatus(panel.node, "error", describeError(error));
      // the local view may be stale now, so pull the truth back
      try {
        await panel.load(panel.endpoint());
      } catch (e) {
        /* the original error is the one worth showing */
      }
    }
  }
}

function wireActions() {
  const value = (id) => document.getElementById(id).value;
  const reset = (...ids) => ids.forEach((id) => (document.getElementById(id).value = ""));

  document.querySelector('[data-action="save-name"]').addEventListener("click", () =>
    runAction("info", () => nip86(selected, "changerelayname", [value("info-name")]))
  );
  document.querySelector('[data-action="save-description"]').addEventListener("click", () =>
    runAction("info", () => nip86(selected, "changerelaydescription", [value("info-description")]))
  );
  document.querySelector('[data-action="save-icon"]').addEventListener("click", () =>
    runAction("info", () => nip86(selected, "changerelayicon", [value("info-icon")]))
  );
  document.getElementById("info-icon").addEventListener("change", (e) => showIconPreview(e.target.value));

  document.querySelector('[data-action="allow-kind"]').addEventListener("click", () => {
    const kind = Number(value("kind-input"));
    if (!Number.isInteger(kind)) return setStatus(panelNode("kinds"), "error", "enter a whole kind number");
    runAction("kinds", () => nip86(selected, "allowkind", [kind])).then(() => reset("kind-input"));
  });

  document.querySelector('[data-action="disallow-kind"]').addEventListener("click", async () => {
    const kind = Number(value("kind-input"));
    if (!Number.isInteger(kind)) return setStatus(panelNode("kinds"), "error", "enter a whole kind number");
    const ok = await confirmAction({
      title: "Disallow this kind?",
      body: `This relay will stop accepting kind ${kind}.`,
      detail: `on ${selected.serviceURL}`,
      danger: "Disallow",
    });
    if (!ok) return;
    await runAction("kinds", () => nip86(selected, "disallowkind", [kind]));
    reset("kind-input");
  });

  document.querySelector('[data-action="ban-event"]').addEventListener("click", async () => {
    const id = value("event-input").trim().toLowerCase();
    if (!/^[0-9a-f]{64}$/.test(id)) return setStatus(panelNode("events"), "error", "enter an event id as 64 hex characters");
    const ok = await confirmAction({
      title: "Ban this event?",
      body: "The stored copy is deleted and the event is refused if it is published again. This can be undone, but the deleted copy does not come back.",
      detail: id,
      danger: "Ban event",
    });
    if (!ok) return;
    await runAction("events", () => nip86(selected, "banevent", [id, value("event-reason")]));
    reset("event-input", "event-reason");
  });

  document.querySelector('[data-action="ban"]').addEventListener("click", async () => {
    let pubkey;
    try {
      pubkey = toPubkeyHex(value("ban-input"));
    } catch (error) {
      document.getElementById("ban-input").value = "";
      return setStatus(panelNode("banned"), "error", error.message);
    }
    if (pubkey === OWNER_PUBKEY) {
      return setStatus(panelNode("banned"), "error", "the relay owner cannot be banned");
    }
    const ok = await confirmAction({
      title: "Ban this pubkey?",
      body: "It will not be able to write anything to any of your four relays.",
      detail: hexToNpub(pubkey),
      danger: "Ban",
    });
    if (!ok) return;
    await runAction("banned", () => nip86(GLOBAL, "banpubkey", [pubkey, value("ban-reason")]));
    reset("ban-input", "ban-reason");
  });

  document.querySelector('[data-action="allow"]').addEventListener("click", async () => {
    let pubkey;
    try {
      pubkey = toPubkeyHex(value("allow-input"));
    } catch (error) {
      document.getElementById("allow-input").value = "";
      return setStatus(panelNode("allowed"), "error", error.message);
    }
    await runAction("allowed", () => nip86(GLOBAL, "allowpubkey", [pubkey, value("allow-reason")]));
    reset("allow-input", "allow-reason");
  });

  document.querySelector('[data-action="block-ip"]').addEventListener("click", async () => {
    const ip = value("ip-input").trim();
    if (!ip) return setStatus(panelNode("ips"), "error", "enter an IP address");
    const ok = await confirmAction({
      title: "Block this address?",
      body: "If this is your own address you may lock yourself out of the relay.",
      detail: ip,
      danger: "Block",
    });
    if (!ok) return;
    await runAction("ips", () => nip86(GLOBAL, "blockip", [ip, value("ip-reason")]));
    reset("ip-input", "ip-reason");
  });
}

//
// relay selection
//

function selectRelay(key) {
  // notes is deliberately NOT in the refresh list below: every refresh there is a
  // signer prompt, so switching relay would spend one on a feed the owner may not
  // even be looking at. It is told to invalidate instead, and offers a reload.
  if (typeof notesInvalidate === "function") notesInvalidate(key);

  const relay = RELAYS.find((r) => r.key === key) || GLOBAL;
  selected = relay;

  for (const candidate of RELAYS) {
    const active = candidate === relay;
    candidate.tab.classList.toggle("border-purple-500", active);
    candidate.tab.classList.toggle("text-purple-300", active);
    candidate.tab.setAttribute("aria-selected", active ? "true" : "false");
  }

  document.getElementById("selected-service-url").textContent = `u = ${relay.serviceURL}`;
  document.getElementById("scope-relay-label").textContent = relay.label.split("\n")[0].trim();
  setEndpointLabels();

  // only the per-relay panels are scoped to the selection; the global ones stay
  // pinned to one endpoint and keep whatever they were showing
  for (const name of ["info", "kinds", "events", "stats"]) {
    const panel = panels[name];
    if (!panel) continue;
    if (panel.loaded || name === "info") refreshPanel(name, false);
  }
}

//
// connecting
//

async function detectSigner() {
  // crypto.subtle is undefined outside a secure context and most NIP-07
  // extensions will not inject there either, so an owner on a plain http LAN
  // address gets a page that silently does nothing unless this is caught
  if (!window.isSecureContext || !window.crypto || !crypto.subtle) {
    return { ok: false, reason: "insecure-context" };
  }
  // some extensions inject window.nostr after DOMContentLoaded
  for (let i = 0; i < 20; i++) {
    if (window.nostr) return { ok: true };
    await new Promise((resolve) => setTimeout(resolve, 100));
  }
  return { ok: false, reason: "no-extension" };
}

async function connect() {
  const signer = await detectSigner();
  if (!signer.ok) {
    if (signer.reason === "insecure-context") {
      showBanner(
        "signer",
        "error",
        "This page needs a secure context before it can sign anything. Open it over https://, or from " +
          "http://localhost — for example ssh -L 3355:localhost:3355 you@your-server, then visit " +
          "http://localhost:3355/admin. Tor .onion addresses count as secure and work as they are."
      );
    } else {
      showBanner(
        "signer",
        "error",
        "No nostr signer found. Install a NIP-07 browser extension such as nos2x or Alby, or connect a " +
          "NIP-46 bunker that injects window.nostr, then reload this page."
      );
    }
    return;
  }

  let pubkey;
  try {
    pubkey = await window.nostr.getPublicKey();
  } catch (e) {
    showBanner("signer", "error", "Your signer refused to share its public key.");
    return;
  }

  connectedPubkey = pubkey;
  const identity = document.getElementById("identity");
  const npub = document.getElementById("identity-npub");
  npub.textContent = shortNpub(pubkey);
  npub.title = hexToNpub(pubkey);
  identity.hidden = false;

  if (pubkey !== OWNER_PUBKEY) {
    // the page already knows who the owner is, so it can say this without
    // spending a doomed signature prompt first
    document.getElementById("identity-badge").textContent = "not the owner";
    showBanner(
      "owner",
      "error",
      `You are signed in as ${hexToNpub(pubkey)}, but this relay is managed by ${OWNER_NPUB}. ` +
        "The relay will reject every request from this key. If your signer holds more than one account, " +
        "switch to the owner's and reload."
    );
    return;
  }
  document.getElementById("identity-badge").textContent = "owner";

  // one signed call on connect. It doubles as the login check: if it works,
  // the signer, the clock, the u tag, the payload hash and the ownership check
  // all work, and one clear banner beats seven identical errors later.
  try {
    await nip86(GLOBAL, "supportedmethods");
  } catch (error) {
    showBanner("owner", "error", describeError(error));
    return;
  }

  const banner = document.getElementById("banner-owner");
  if (banner) banner.remove();

  document.getElementById("connect-card").hidden = true;
  document.getElementById("console").hidden = false;

  applyRoute();
  for (const name of rememberedPanels()) refreshPanel(name, false);
}

//
// boot
//

function boot() {
  definePanels();
  wireActions();
  wireViewTabs();
  wireMediaToolbar();
  wireMediaDialog();
  wireGridKeyboard();
  setEndpointLabels();

  for (const relay of RELAYS) {
    relay.tab.addEventListener("click", () => {
      // switching relay must not also switch view: the bare relay key still means
      // moderation, so that bookmarks made before the other views existed keep
      // meaning exactly what they meant, but from Notes it stays on Notes
      const current = parseRoute(location.hash).view;
      location.hash = current === "moderation" ? relay.key : `${current}/${relay.key}`;
      selectRelay(relay.key);
    });
  }
  window.addEventListener("hashchange", () => {
    if (connectedPubkey === OWNER_PUBKEY) applyRoute();
  });

  // Each split file wires itself if it loaded. A missing one degrades to a
  // console with the other tabs working, and the tab itself says which asset
  // failed, rather than a dead page with a console error nobody will read.
  if (typeof notesWire === "function") notesWire();
  else markViewUnavailable("notes", "/static/admin-notes.js");
  if (typeof dashWire === "function" && typeof chartFrame === "function") dashWire();
  else markViewUnavailable("dashboard", "/static/admin-dashboard.js");

  document.getElementById("connect").addEventListener("click", connect);
  document.getElementById("copy-identity").addEventListener("click", () => {
    if (connectedPubkey) navigator.clipboard.writeText(hexToNpub(connectedPubkey));
  });
}

//
// media
//
// The library is fetched once and everything after that — filtering, sorting,
// searching, paging — happens here. That is not an optimisation: NIP-86 has no
// batching, so a server-side page would cost a signature prompt per scroll.
// Thumbnails are free too, because blob reads need no authorization at all.
//

const VIEWS = ["dashboard", "notes", "moderation", "media"];

// The views that are scoped to one relay, and so show the relay switcher and
// carry the relay in their hash. Dashboard reports on all four at once and
// blossom is served by the outbox relay alone, so neither of those is here.
const PER_RELAY_VIEWS = new Set(["notes", "moderation"]);

const PAGE_SIZE = 100; // tiles appended per page
const THUMB_ROOT_MARGIN = "800px 0px"; // mount images roughly two screens ahead
const MAX_MOUNTED_IMAGES = 200;
// how many tiles are mounted without waiting to be asked. The observer is the
// right mechanism for a long scroll, but it reports nothing at all while a tab
// is in the background, so a library opened in a restored session would render
// as a wall of placeholders until the owner scrolled. A screenful up front costs
// little and makes the first paint independent of it.
const PRIMED_TILES = 24;
// bytes, not just a count: a 3 MB JPEG can hold 48 MB of decoded bitmap, and the
// file size is the only proxy for that we are given
const MAX_MOUNTED_BYTES = 60_000_000;
const PREVIEW_MAX_BYTES = 2_000_000; // above this a tile waits to be asked
const PREVIEW_HARD_MAX = 25_000_000; // above this only the drawer ever loads it
const BIG_LIBRARY_HINT = 2_000;
const MAX_DELETE_PER_CALL = 500; // matches maxBlobsPerCall on the relay

const media = {
  loaded: false,
  blobs: [],
  byHash: new Map(),
  blocked: [],
  stats: null,
  countedAt: 0,
  nextOffset: 0,
  view: [],
  filter: { type: "all", query: "" },
  sort: "newest",
  density: "comfortable",
  selecting: false,
  selection: new Set(),
  anchor: -1,
  page: 1,
  focusIndex: 0,
  columns: 1,
  previewOverride: new Set(),
  revealed: new Set(),
  mounted: new Map(),
  mountedBytes: 0,
  thumbObserver: null,
  pageObserver: null,
  openSha: null,
  openIndex: -1,
  active: false,
};

//
// formatting
//

function formatBytes(n) {
  const bytes = Number(n) || 0;
  if (bytes < 1000) return `${bytes} B`;
  const units = ["kB", "MB", "GB", "TB"];
  let value = bytes / 1000;
  let unit = 0;
  while (value >= 1000 && unit < units.length - 1) {
    value /= 1000;
    unit++;
  }
  return `${value >= 100 ? Math.round(value) : value.toFixed(1)} ${units[unit]}`;
}

function formatDate(seconds) {
  if (!seconds) return "—";
  return new Date(seconds * 1000).toLocaleString();
}

function formatRelative(seconds) {
  if (!seconds) return "";
  const delta = Math.round(seconds - Date.now() / 1000);
  const steps = [
    [60, "second", 1],
    [3600, "minute", 60],
    [86400, "hour", 3600],
    [2592000, "day", 86400],
    [31536000, "month", 2592000],
    [Infinity, "year", 31536000],
  ];
  const magnitude = Math.abs(delta);
  for (const [limit, unit, divisor] of steps) {
    if (magnitude < limit) {
      try {
        return new Intl.RelativeTimeFormat(undefined, { numeric: "auto" }).format(Math.round(delta / divisor), unit);
      } catch (e) {
        return "";
      }
    }
  }
  return "";
}

function shortHash(sha) {
  return sha.length > 20 ? `${sha.slice(0, 10)}…${sha.slice(-6)}` : sha;
}

const IMAGE_TYPES = /^image\/(png|jpeg|gif|webp|avif|bmp|x-icon|vnd\.microsoft\.icon|svg\+xml)$/;
const VIDEO_TYPES = /^video\/(mp4|webm|ogg|quicktime)$/;
const AUDIO_TYPES = /^audio\/(mpeg|mp4|ogg|wav|x-wav|webm|flac)$/;

// mediaKind maps a mime type onto the handful of shapes the grid draws. It is a
// whitelist on purpose: the type came off the wire, and it decides which element
// the drawer is about to build.
function mediaKind(type) {
  const clean = String(type || "").split(";")[0].trim().toLowerCase();
  if (IMAGE_TYPES.test(clean)) return "image";
  if (VIDEO_TYPES.test(clean)) return "video";
  if (AUDIO_TYPES.test(clean)) return "audio";
  if (clean === "application/pdf") return "pdf";
  if (clean.startsWith("text/") || clean === "application/json") return "text";
  if (/^application\/(zip|gzip|x-tar|x-7z-compressed|vnd\.android\.package-archive)$/.test(clean)) return "archive";
  return "other";
}

const KIND_GLYPHS = {
  image: "▣",
  video: "▶",
  audio: "♪",
  pdf: "▤",
  text: "¶",
  archive: "▩",
  other: "⬡",
};

const KIND_COLOURS = {
  image: "#a78bfa",
  video: "#fbbf24",
  audio: "#34d399",
  pdf: "#f87171",
  text: "#60a5fa",
  archive: "#c084fc",
  other: "#6b7280",
};

const KIND_LABELS = {
  image: "Images",
  video: "Video",
  audio: "Audio",
  pdf: "PDFs",
  text: "Text",
  archive: "Archives",
  other: "Other",
};

const EXT_LABELS = {
  jpeg: "JPG",
  "svg+xml": "SVG",
  "octet-stream": "BIN",
  quicktime: "MOV",
  "x-icon": "ICO",
  "vnd.microsoft.icon": "ICO",
  plain: "TXT",
  "x-wav": "WAV",
  "vnd.android.package-archive": "APK",
  "x-7z-compressed": "7Z",
};

function extLabel(type) {
  const clean = String(type || "").split(";")[0].trim().toLowerCase();
  if (!clean) return "?";
  const sub = clean.split("/")[1] || "";
  if (sub === "mpeg") return clean.startsWith("video/") ? "MPG" : "MP3";
  if (EXT_LABELS[sub]) return EXT_LABELS[sub];
  return sub.replace(/[^a-z0-9]/g, "").slice(0, 4).toUpperCase() || "?";
}

// bech32 is not cheap and the same handful of uploaders repeat across thousands
// of blobs, so the conversion is done once per key rather than once per row
const npubCache = new Map();

function npubFor(hex) {
  let npub = npubCache.get(hex);
  if (npub === undefined) {
    npub = hexToNpub(hex);
    npubCache.set(hex, npub);
  }
  return npub;
}

// blobPath is what an <img>, a <video> or a download link points at: same
// origin, which is the only form img-src 'self' permits and the only one that
// survives an owner browsing through an ssh tunnel.
function blobPath(blob) {
  return `/${blob.sha256}`;
}

// blobURL is what the owner copies and shares. It has to be the relay's own
// absolute URL — http://localhost:3355/… is useless to anybody else — so the
// server's value is preferred and only sanity checked here.
function blobURL(blob) {
  const fallback = `${GLOBAL.serviceURL}/${blob.sha256}${blob.ext || ""}`;
  if (!blob.url) return fallback;
  try {
    if (new URL(blob.url).origin === new URL(GLOBAL.serviceURL).origin) return blob.url;
  } catch (e) {
    /* fall through */
  }
  return fallback;
}

//
// loading the inventory
//

function normaliseBlob(raw) {
  if (!raw || typeof raw !== "object") return null;
  const sha256 = String(raw.sha256 || "").toLowerCase();
  if (!/^[0-9a-f]{64}$/.test(sha256)) return null;

  const type = typeof raw.type === "string" ? raw.type : "";
  const owners = Array.isArray(raw.owners) ? raw.owners.filter((o) => /^[0-9a-f]{64}$/.test(String(o))) : [];
  const url = typeof raw.url === "string" ? raw.url : "";
  // the extension only ever comes from the server's URL: rebuilding it here
  // would mean duplicating khatru's mime table and drifting from it
  const ext = url.slice(url.lastIndexOf("/") + 1).slice(64);

  return {
    sha256,
    size: Number(raw.size) || 0,
    diskSize: Number(raw.disk_size) || 0,
    type,
    kind: mediaKind(type),
    ext,
    url,
    uploaded: Number(raw.uploaded) || 0,
    owners,
    onDisk: Boolean(raw.on_disk),
    blocked: Boolean(raw.blocked),
  };
}

async function loadMediaInventory(endpoint, offset = 0) {
  const result = await nip86(endpoint, "listblobs", offset ? [offset] : []);
  if (!result || !Array.isArray(result.blobs)) throw new ApiError("protocol", "the relay did not return a media library");

  const rows = result.blobs.map(normaliseBlob).filter(Boolean);
  if (offset === 0) {
    media.blobs = rows;
    media.byHash = new Map();
    media.selection.clear();
    media.previewOverride.clear();
    media.revealed.clear();
  } else {
    media.blobs = media.blobs.concat(rows);
  }
  for (const blob of rows) media.byHash.set(blob.sha256, blob);

  media.blocked = Array.isArray(result.blocked) ? result.blocked : [];
  media.stats = result.stats || null;
  media.countedAt = Number(result.counted_at) || 0;
  media.nextOffset = result.truncated ? Number(result.next_offset) || 0 : 0;
  media.loaded = true;
  media.stale = "";
  setMediaStale("");

  applyMediaFilters();
  renderBlockedBlobs();
}

// setMediaStale shows the one thing the media panel cannot do automatically:
// re-read the library. Every other panel just reloads itself after an action,
// but here a reload is a signature prompt, so it is offered rather than spent.
function setMediaStale(reason) {
  const node = document.getElementById("media-stale");
  if (!node) return;
  clear(node);
  if (!reason) {
    node.hidden = true;
    return;
  }
  node.hidden = false;
  node.appendChild(el("span", "", `${reason} `));
  node.appendChild(
    button("Reload library", "underline", () => refreshPanel("media", false))
  );
}

//
// the summary
//

function deriveSummary() {
  const counts = new Map();
  let bytes = 0;
  let largest = null;
  let newest = null;
  const uploaders = new Set();

  for (const blob of media.blobs) {
    const size = blob.onDisk ? blob.diskSize || blob.size : blob.size;
    bytes += size;
    counts.set(blob.kind, (counts.get(blob.kind) || 0) + size);
    for (const owner of blob.owners) uploaders.add(owner);
    if (!largest || size > (largest.diskSize || largest.size)) largest = blob;
    if (!newest || blob.uploaded > newest.uploaded) newest = blob;
  }

  return { counts, bytes, largest, newest, uploaders: uploaders.size };
}

function renderMediaSummary() {
  const summary = deriveSummary();
  const stats = media.stats || {};

  document.getElementById("media-total-bytes").textContent = formatBytes(summary.bytes);
  document.getElementById("media-total-count").textContent =
    `${media.blobs.length.toLocaleString()} ${media.blobs.length === 1 ? "file" : "files"}`;
  document.getElementById("media-disk").textContent =
    `${formatBytes(stats.disk_bytes || 0)} · ${(stats.on_disk || 0).toLocaleString()}`;
  document.getElementById("media-largest").textContent = summary.largest
    ? formatBytes(summary.largest.diskSize || summary.largest.size)
    : "—";
  document.getElementById("media-newest").textContent = summary.newest ? formatRelative(summary.newest.uploaded) : "—";
  document.getElementById("media-uploaders").textContent = String(summary.uploaders);

  const bar = document.getElementById("media-composition");
  clear(bar);
  if (summary.bytes > 0) {
    for (const [kind, size] of [...summary.counts].sort((a, b) => b[1] - a[1])) {
      const segment = el("span");
      // CSSOM rather than a style attribute: never parsed as markup
      segment.style.width = `${(size / summary.bytes) * 100}%`;
      segment.style.background = KIND_COLOURS[kind] || KIND_COLOURS.other;
      segment.title = `${KIND_LABELS[kind] || kind} · ${formatBytes(size)}`;
      bar.appendChild(segment);
    }
  }

  const notes = document.getElementById("media-notes");
  clear(notes);
  const lines = [];
  if (stats.complete === false) {
    lines.push(
      `${(stats.index_entries || 0).toLocaleString()} of ${(stats.index_entries_expected || 0).toLocaleString()} index entries could be read, so some media is missing from this list.`
    );
  }
  if (stats.skipped_index_entries) {
    lines.push(
      `${stats.skipped_index_entries} index ${stats.skipped_index_entries === 1 ? "entry" : "entries"} could not be read and ${stats.skipped_index_entries === 1 ? "is" : "are"} not listed.`
    );
  }
  if (stats.missing_files) {
    lines.push(
      `${stats.missing_files} ${stats.missing_files === 1 ? "entry points" : "entries point"} at files that are no longer on disk.`
    );
  }
  if (stats.unindexed) {
    lines.push(
      `${stats.unindexed} ${stats.unindexed === 1 ? "file is" : "files are"} on disk but not in the index (${formatBytes(stats.unindexed_bytes || 0)}).`
    );
  }
  if (media.countedAt) lines.push(`Counted ${formatRelative(media.countedAt)}.`);

  for (const line of lines) notes.appendChild(el("span", "mr-2", line));
  if (media.nextOffset) {
    notes.appendChild(
      button(`Load the next ${PAGE_SIZE * 100} — costs one signature`, "underline", () =>
        runMediaAction(() => loadMediaInventory(GLOBAL, media.nextOffset), () => {}, "loading more…")
      )
    );
  }
  notes.hidden = lines.length === 0 && !media.nextOffset;
}

//
// filtering, sorting, rendering
//

function typeCounts() {
  const counts = new Map();
  for (const blob of media.blobs) counts.set(blob.kind, (counts.get(blob.kind) || 0) + 1);
  return counts;
}

function matchesFilter(blob) {
  if (media.filter.type === "missing") {
    if (blob.onDisk) return false;
  } else if (media.filter.type === "blocked") {
    if (!blob.blocked) return false;
  } else if (media.filter.type === "unindexed") {
    if (blob.owners.length > 0) return false;
  } else if (media.filter.type !== "all" && blob.kind !== media.filter.type) {
    return false;
  }

  const query = media.filter.query;
  if (!query) return true;
  if (blob.sha256.startsWith(query) || blob.type.toLowerCase().includes(query)) return true;
  return blob.owners.some((owner) => owner.startsWith(query) || npubFor(owner).toLowerCase().includes(query));
}

function compareBlobs(a, b) {
  switch (media.sort) {
    case "oldest":
      return a.uploaded - b.uploaded || a.sha256.localeCompare(b.sha256);
    case "largest":
      return b.size - a.size || a.sha256.localeCompare(b.sha256);
    case "smallest":
      return a.size - b.size || a.sha256.localeCompare(b.sha256);
    case "type":
      return a.type.localeCompare(b.type) || b.uploaded - a.uploaded;
    default:
      return b.uploaded - a.uploaded || a.sha256.localeCompare(b.sha256);
  }
}

function applyMediaFilters({ resetPage = true } = {}) {
  media.view = media.blobs.filter(matchesFilter).sort(compareBlobs);
  if (resetPage) media.page = 1;
  if (media.focusIndex >= media.view.length) media.focusIndex = Math.max(0, media.view.length - 1);
  renderMediaSummary();
  renderTypeFilters();
  renderMedia();
  renderSelectionBar();
}

function renderTypeFilters() {
  const container = document.getElementById("media-type-filters");
  clear(container);

  const counts = typeCounts();
  const chips = [["all", "All", media.blobs.length, "#fbbf24"]];
  for (const kind of ["image", "video", "audio", "pdf", "text", "archive", "other"]) {
    if (counts.get(kind)) chips.push([kind, KIND_LABELS[kind], counts.get(kind), KIND_COLOURS[kind]]);
  }
  // problem filters only appear once there is a problem to filter to
  const missing = media.blobs.filter((b) => !b.onDisk).length;
  const unindexed = media.blobs.filter((b) => b.owners.length === 0).length;
  const blocked = media.blobs.filter((b) => b.blocked).length;
  if (missing) chips.push(["missing", "Missing", missing, "#f87171"]);
  if (unindexed) chips.push(["unindexed", "Unindexed", unindexed, "#f87171"]);
  if (blocked) chips.push(["blocked", "Blocked", blocked, "#d97706"]);

  for (const [key, label, count, colour] of chips) {
    const chip = el("button", "media-chip inline-flex items-center gap-1.5 rounded border border-gray-700 px-2 py-1 text-xs text-gray-300");
    chip.type = "button";
    chip.dataset.type = key;
    chip.setAttribute("aria-pressed", media.filter.type === key ? "true" : "false");
    if (media.filter.type === key) chip.classList.add("bg-amber-900", "text-amber-200");

    const dot = el("span", "inline-block h-2 w-2 rounded-full");
    dot.style.background = colour;
    chip.appendChild(dot);
    chip.appendChild(el("span", "", label));
    chip.appendChild(el("span", "mono text-gray-500", String(count)));
    chip.addEventListener("click", () => {
      media.filter.type = key;
      applyMediaFilters();
    });
    container.appendChild(chip);
  }
}

function renderMedia() {
  const grid = document.getElementById("media-grid");
  const tableWrap = document.getElementById("media-table-wrap");
  const empty = document.getElementById("media-empty");
  const isList = media.density === "list";

  grid.hidden = isList;
  tableWrap.hidden = !isList;

  if (!media.view.length) {
    clear(grid);
    clear(document.getElementById("media-rows"));
    empty.hidden = false;
    empty.textContent = media.blobs.length
      ? "Nothing matches that filter."
      : "No media has been uploaded to this relay yet.";
    document.getElementById("media-more").hidden = true;
    return;
  }
  empty.hidden = true;

  if (isList) renderTable();
  else renderGrid();

  const shown = Math.min(media.page * PAGE_SIZE, media.view.length);
  const more = document.getElementById("media-more");
  if (shown < media.view.length) {
    more.hidden = false;
    more.textContent = `Show more — ${shown.toLocaleString()} of ${media.view.length.toLocaleString()}`;
  } else {
    more.hidden = true;
  }
}

function renderGrid() {
  const grid = document.getElementById("media-grid");

  // every tile node is about to be replaced, so the mount ledger has to go with
  // them. Without this the budget stays charged for images whose elements no
  // longer exist, and after enough filter changes it holds nothing but ghosts
  // and the grid quietly stops loading thumbnails at all.
  if (media.thumbObserver) media.thumbObserver.disconnect();
  media.mounted.clear();
  media.mountedBytes = 0;

  clear(grid);
  grid.classList.toggle("is-compact", media.density === "compact");
  grid.classList.toggle("is-selecting", media.selecting);

  const shown = Math.min(media.page * PAGE_SIZE, media.view.length);
  const observer = ensureThumbObserver();
  const tiles = [];
  for (let i = 0; i < shown; i++) {
    const cell = buildTile(media.view[i], i);
    grid.appendChild(cell);
    tiles.push(cell.firstChild);
    if (observer) observer.observe(cell.firstChild);
  }

  // without an observer there is nothing else coming, so everything on the page
  // is mounted and the budget is left to do the limiting
  const prime = observer ? Math.min(PRIMED_TILES, tiles.length) : tiles.length;
  for (let i = 0; i < prime; i++) mountThumb(tiles[i], media.view[i]);

  recomputeGridColumns();
  ensurePageObserver();
}

// buildTile draws one blob. Nothing that came off the wire is written as markup
// here any more than anywhere else in this file: the type, the size and the hash
// all go in through textContent.
function buildTile(blob, index) {
  const cell = el("li", "media-cell");
  const tile = el("button", "media-tile group");
  tile.type = "button";
  tile.dataset.sha = blob.sha256;
  tile.dataset.index = String(index);
  tile.tabIndex = index === media.focusIndex ? 0 : -1;
  tile.title = `${blob.type || "unknown type"} · ${formatBytes(blob.size)}`;

  const img = el("img", "media-thumb absolute inset-0 h-full w-full object-cover");
  img.alt = "";
  img.loading = "lazy";
  img.decoding = "async";
  img.addEventListener("load", () => {
    img.classList.add("is-loaded");
    glyph.hidden = true;
  });
  // a thumbnail that will not load is a dead index entry telling us so for free
  img.addEventListener("error", () => {
    if (!img.getAttribute("src")) return; // an unmount cleared it; not an error
    img.classList.remove("is-loaded");
    glyph.hidden = false;
    tile.dataset.state = "missing";
  });
  tile.appendChild(img);

  const glyph = el("span", "media-glyph absolute inset-0 flex flex-col items-center justify-center gap-1");
  glyph.appendChild(el("span", "text-2xl leading-none text-gray-500", KIND_GLYPHS[blob.kind] || KIND_GLYPHS.other));
  glyph.appendChild(el("span", "mono text-[11px] uppercase tracking-wide text-gray-400", extLabel(blob.type)));
  if (!blob.onDisk) {
    glyph.appendChild(el("span", "text-[10px] text-red-300", "file missing"));
  } else if (blob.blocked) {
    glyph.appendChild(el("span", "text-[10px] text-amber-300", "BLOCKED"));
  } else if (blob.kind === "image" && !shouldAutoPreview(blob)) {
    glyph.appendChild(el("span", "text-[10px] text-gray-500", "tap to preview"));
  }
  tile.appendChild(glyph);

  tile.appendChild(el("span", "media-badge absolute left-1.5 top-1.5 rounded bg-gray-950/80 px-1.5 py-0.5 mono text-[10px] uppercase text-gray-300", extLabel(blob.type)));

  const check = el("span", "media-check absolute right-1.5 top-1.5 grid h-5 w-5 place-items-center rounded border border-gray-500 bg-gray-950/80 text-xs");
  check.setAttribute("aria-hidden", "true");
  tile.appendChild(check);

  const meta = el("span", "media-meta absolute inset-x-0 bottom-0 bg-gradient-to-t from-gray-950/90 to-transparent px-2 pb-1.5 pt-6");
  meta.appendChild(el("span", "mono block text-[11px] text-gray-200", formatBytes(blob.size)));
  meta.appendChild(
    el("span", "block text-[10px] text-gray-400 opacity-0 transition-opacity group-hover:opacity-100 group-focus-visible:opacity-100", formatDate(blob.uploaded).split(",")[0])
  );
  tile.appendChild(meta);

  if (!blob.onDisk) tile.dataset.state = "missing";
  else if (blob.blocked) tile.dataset.state = "blocked";
  else tile.dataset.state = "ready";

  tile.addEventListener("click", (event) => onTileActivate(blob, index, event));
  cell.appendChild(tile);
  applyTileSelection(tile, blob);
  return cell;
}

function onTileActivate(blob, index, event) {
  media.focusIndex = index;
  if (media.selecting || event.ctrlKey || event.metaKey || event.shiftKey) {
    toggleSelection(blob.sha256, index, { shift: event.shiftKey, additive: true });
    return;
  }
  // a heavy image loads in place first, so the owner can see what it is before
  // deciding to open anything
  const tile = event.currentTarget;
  if (blob.kind === "image" && blob.onDisk && !shouldAutoPreview(blob) && !media.previewOverride.has(blob.sha256)) {
    media.previewOverride.add(blob.sha256);
    mountThumb(tile, blob);
    return;
  }
  openMediaDetail(blob.sha256, index);
}

function applyTileSelection(tile, blob) {
  const selected = media.selection.has(blob.sha256);
  const check = tile.querySelector(".media-check");
  if (check) check.textContent = selected ? "✓" : "";
  tile.classList.toggle("ring-2", selected);
  tile.classList.toggle("ring-amber-400", selected);
  if (media.selecting) tile.setAttribute("aria-pressed", selected ? "true" : "false");
  else tile.removeAttribute("aria-pressed");
}

function updateTileState(sha) {
  const blob = media.byHash.get(sha);
  const tile = document.querySelector(`.media-tile[data-sha="${sha}"]`);
  if (blob && tile) applyTileSelection(tile, blob);
}

function renderTable() {
  const rows = document.getElementById("media-rows");
  clear(rows);
  const shown = Math.min(media.page * PAGE_SIZE, media.view.length);

  for (let i = 0; i < shown; i++) {
    const blob = media.view[i];
    const row = el("tr");
    if (media.selection.has(blob.sha256)) row.className = "bg-amber-950/30";

    const hash = el("td", "mono text-xs text-amber-300");
    hash.appendChild(el("span", "mr-2 text-gray-500", KIND_GLYPHS[blob.kind] || KIND_GLYPHS.other));
    const link = el("span", "", shortHash(blob.sha256));
    link.title = blob.sha256;
    hash.appendChild(link);
    if (blob.blocked) hash.appendChild(el("span", "ml-2 rounded bg-amber-900 px-1 text-[10px] text-amber-200", "BLOCKED"));
    if (!blob.onDisk) hash.appendChild(el("span", "ml-2 text-[10px] text-red-300", "file missing"));
    if (blob.owners.length === 0) hash.appendChild(el("span", "ml-2 text-[10px] text-red-300", "unindexed"));
    row.appendChild(hash);

    row.appendChild(el("td", "text-xs text-gray-400", blob.type || "—"));
    row.appendChild(el("td", "mono text-xs text-right text-gray-300", formatBytes(blob.size)));
    row.appendChild(el("td", "text-xs text-gray-400", formatDate(blob.uploaded)));

    const who = el("td", "mono text-xs text-purple-300");
    if (blob.owners.length === 0) {
      who.textContent = "—";
    } else {
      who.textContent = shortNpub(blob.owners[0]);
      who.title = blob.owners.map(npubFor).join("\n");
      if (blob.owners.length > 1) who.textContent += ` +${blob.owners.length - 1}`;
    }
    row.appendChild(who);

    const actions = el("td", "text-right");
    const index = i;
    actions.appendChild(
      button("Details", "rounded border border-gray-600 px-2 py-1 text-xs", () => openMediaDetail(blob.sha256, index))
    );
    row.appendChild(actions);
    rows.appendChild(row);
  }
}

//
// the mount window
//
// Tiles enter the DOM with no src at all. An observer sets it when a tile comes
// close to the viewport and clears it again once the budget is exceeded, which
// is cheap to undo: blobs are served immutable for a week, so a remount is a
// cache hit rather than a request.
//

function shouldAutoPreview(blob) {
  if (blob.kind !== "image" || !blob.onDisk) return false;
  // a blob blocked for what it contains should not be painted on screen every
  // time the owner opens the library
  if (blob.blocked && !media.revealed.has(blob.sha256)) return false;
  if (media.previewOverride.has(blob.sha256)) return blob.size <= PREVIEW_HARD_MAX;
  return blob.size <= PREVIEW_MAX_BYTES;
}

function ensureThumbObserver() {
  if (media.thumbObserver || typeof IntersectionObserver !== "function") return media.thumbObserver;
  media.thumbObserver = new IntersectionObserver(
    (entries) => {
      for (const entry of entries) {
        const tile = entry.target;
        const blob = media.byHash.get(tile.dataset.sha);
        if (!blob) continue;
        if (entry.isIntersecting) {
          tile.dataset.visible = "1";
          mountThumb(tile, blob);
        } else {
          delete tile.dataset.visible;
        }
      }
      evictThumbs();
    },
    { rootMargin: THUMB_ROOT_MARGIN }
  );
  return media.thumbObserver;
}

function mountThumb(tile, blob) {
  if (!shouldAutoPreview(blob)) return;
  const img = tile.querySelector(".media-thumb");
  if (!img || img.getAttribute("src")) return;
  img.src = blobPath(blob);
  media.mounted.set(blob.sha256, blob.size || 0);
  media.mountedBytes += blob.size || 0;
  evictThumbs();
}

// unmountThumb tolerates a hash whose tile is already gone: eviction and a grid
// rebuild can both reach it, and only the ledger is guaranteed to still be here.
function unmountThumb(sha) {
  const tile = document.querySelector(`.media-tile[data-sha="${sha}"]`);
  if (tile) {
    const img = tile.querySelector(".media-thumb");
    if (img) {
      img.removeAttribute("src");
      img.classList.remove("is-loaded");
      const glyph = tile.querySelector(".media-glyph");
      if (glyph) glyph.hidden = false;
    }
  }
  media.mountedBytes -= media.mounted.get(sha) || 0;
  media.mounted.delete(sha);
  if (media.mountedBytes < 0) media.mountedBytes = 0;
}

// evictThumbs drops the least recently mounted images that are no longer on
// screen. A Map keeps insertion order, so this needs no separate bookkeeping.
function evictThumbs() {
  if (media.mounted.size <= MAX_MOUNTED_IMAGES && media.mountedBytes <= MAX_MOUNTED_BYTES) return;
  for (const sha of [...media.mounted.keys()]) {
    if (media.mounted.size <= MAX_MOUNTED_IMAGES && media.mountedBytes <= MAX_MOUNTED_BYTES) return;
    const tile = document.querySelector(`.media-tile[data-sha="${sha}"]`);
    if (tile && tile.dataset.visible) continue;
    unmountThumb(sha);
  }
}

function ensurePageObserver() {
  if (typeof IntersectionObserver !== "function") return;
  if (media.pageObserver) return;
  const sentinel = document.getElementById("media-sentinel");
  if (!sentinel) return;
  media.pageObserver = new IntersectionObserver((entries) => {
    if (entries.some((entry) => entry.isIntersecting)) appendNextPage();
  });
  media.pageObserver.observe(sentinel);
}

function appendNextPage() {
  if (!media.active || media.density === "list") return;
  if (media.page * PAGE_SIZE >= media.view.length) return;
  media.page++;
  renderMedia();
}

function recomputeGridColumns() {
  const grid = document.getElementById("media-grid");
  if (!grid || grid.hidden) return;
  try {
    const columns = getComputedStyle(grid).gridTemplateColumns;
    media.columns = Math.max(1, columns.split(" ").filter(Boolean).length);
  } catch (e) {
    media.columns = 1;
  }
}

//
// selection
//

function setSelectMode(on) {
  media.selecting = on;
  const toggle = document.getElementById("media-select-mode");
  toggle.setAttribute("aria-pressed", on ? "true" : "false");
  toggle.classList.toggle("bg-amber-900", on);
  toggle.classList.toggle("text-amber-200", on);

  // this class is what reveals the per-tile checkboxes. Toggling it here rather
  // than leaving it to renderGrid means switching modes costs no rebuild, which
  // matters because a rebuild would drop every mounted thumbnail and fetch the
  // visible ones all over again.
  const grid = document.getElementById("media-grid");
  if (grid) grid.classList.toggle("is-selecting", on);

  if (!on) clearSelection();

  // a tile is a toggle button in selection mode and a plain button outside it,
  // so its pressed state changes meaning whenever the mode does
  for (const tile of document.querySelectorAll(".media-tile")) {
    const blob = media.byHash.get(tile.dataset.sha);
    if (blob) applyTileSelection(tile, blob);
  }
  renderSelectionBar();
}

function toggleSelection(sha, index, { shift = false, additive = false } = {}) {
  if (shift && media.anchor >= 0) {
    selectRange(media.anchor, index);
  } else {
    if (media.selection.has(sha)) media.selection.delete(sha);
    else media.selection.add(sha);
    media.anchor = index;
    updateTileState(sha);
  }
  if (!media.selecting && additive) setSelectMode(true);
  else renderSelectionBar();
  if (media.density === "list") renderTable();
}

// ranges are taken over the filtered view, never over the DOM: paged rendering
// means a range routinely spans tiles that were never mounted
function selectRange(from, to) {
  const start = Math.min(from, to);
  const end = Math.max(from, to);
  for (let i = start; i <= end && i < media.view.length; i++) {
    const sha = media.view[i].sha256;
    media.selection.add(sha);
    updateTileState(sha);
  }
}

function selectAllFiltered() {
  for (const blob of media.view) media.selection.add(blob.sha256);
  renderMedia();
  renderSelectionBar();
}

function clearSelection() {
  const previous = [...media.selection];
  media.selection.clear();
  media.anchor = -1;
  for (const sha of previous) updateTileState(sha);
  if (media.density === "list") renderTable();
  renderSelectionBar();
}

function selectionSummary() {
  let bytes = 0;
  let hidden = 0;
  const visible = new Set(media.view.map((blob) => blob.sha256));
  for (const sha of media.selection) {
    const blob = media.byHash.get(sha);
    if (blob) bytes += blob.size;
    if (!visible.has(sha)) hidden++;
  }
  return { count: media.selection.size, bytes, hidden };
}

function renderSelectionBar() {
  const bar = document.getElementById("media-selection");
  const summary = selectionSummary();
  bar.hidden = summary.count === 0 && !media.selecting;
  if (bar.hidden) return;

  document.getElementById("selection-count").textContent =
    summary.count === 0 ? "Nothing selected" : `${summary.count.toLocaleString()} selected`;
  document.getElementById("selection-bytes").textContent = summary.count ? ` · ${formatBytes(summary.bytes)}` : "";

  const hiddenNote = document.getElementById("selection-hidden");
  // filtering deliberately does not clear the selection, which makes saying so
  // mandatory rather than optional
  hiddenNote.hidden = summary.hidden === 0;
  hiddenNote.textContent = `${summary.hidden} hidden by the current filter`;

  document.getElementById("selection-all").textContent = `Select all ${media.view.length.toLocaleString()}`;
  const del = document.getElementById("selection-delete");
  del.textContent = summary.count ? `Delete ${summary.count.toLocaleString()}` : "Delete";
  del.disabled = summary.count === 0;
  document.getElementById("selection-block").disabled = summary.count === 0;
}

//
// the detail drawer
//

function openMediaDetail(sha, index) {
  const blob = media.byHash.get(sha);
  if (!blob) return;
  media.openSha = sha;
  media.openIndex = index;

  document.getElementById("media-dialog-title").textContent = blob.sha256;

  const preview = document.getElementById("media-dialog-preview");
  clear(preview);
  preview.appendChild(buildDetailPreview(blob));

  const meta = document.getElementById("media-dialog-meta");
  clear(meta);
  meta.appendChild(buildDetailMeta(blob));

  const blockButton = document.getElementById("media-block");
  blockButton.textContent = blob.blocked ? "Unblock" : "Block hash";

  const dialog = document.getElementById("media-dialog");
  if (typeof dialog.showModal === "function") {
    dialog.showModal();
    document.getElementById("media-dialog-close").focus();
  }
}

function closeMediaDetail() {
  const dialog = document.getElementById("media-dialog");
  if (dialog.open) dialog.close();
}

// buildDetailPreview picks the element from a whitelist rather than from the
// type string. There is deliberately no iframe, object or embed here — not even
// for PDFs — so object-src and frame-src stay at 'none' by inheritance on the
// one page that holds a live signer handle.
function buildDetailPreview(blob) {
  if (!blob.onDisk) {
    const gone = el("div", "text-center");
    gone.appendChild(el("p", "text-3xl text-gray-600", "⚠"));
    gone.appendChild(el("p", "mt-2 text-sm text-red-300", "This file is no longer on disk."));
    gone.appendChild(el("p", "mt-1 text-xs text-gray-500", "Its public URL already returns 404."));
    return gone;
  }

  if (blob.blocked && !media.revealed.has(blob.sha256)) {
    const hidden = el("div", "text-center");
    hidden.appendChild(el("p", "text-3xl text-amber-500", "⦸"));
    hidden.appendChild(el("p", "mt-2 text-sm text-amber-200", "This hash is blocked."));
    hidden.appendChild(el("p", "mt-1 text-xs text-gray-500", "The relay no longer serves it, and it is not shown here unless you ask."));
    hidden.appendChild(
      button("Reveal", "mt-3 rounded border border-gray-600 px-3 py-1.5 text-sm", () => {
        media.revealed.add(blob.sha256);
        openMediaDetail(blob.sha256, media.openIndex);
      })
    );
    return hidden;
  }

  if (blob.kind === "image") {
    const img = el("img", "max-h-[60vh] max-w-full object-contain");
    img.alt = "";
    img.decoding = "async";
    img.src = blobPath(blob);
    return img;
  }

  if (blob.kind === "video" || blob.kind === "audio") {
    const player = el(blob.kind, blob.kind === "video" ? "max-h-[60vh] max-w-full" : "w-full");
    player.controls = true;
    player.preload = "metadata";
    if (blob.kind === "video") player.playsInline = true;
    player.src = blobPath(blob);
    return player;
  }

  const card = el("div", "text-center");
  card.appendChild(el("p", "text-4xl text-gray-600", KIND_GLYPHS[blob.kind] || KIND_GLYPHS.other));
  card.appendChild(el("p", "mono mt-2 text-sm text-gray-400", extLabel(blob.type)));
  card.appendChild(el("p", "mt-1 text-xs text-gray-500", "No preview — download it to look at it."));
  const download = el("a", "mt-3 inline-block rounded border border-gray-600 px-3 py-1.5 text-sm");
  // download rather than a plain link: blobs are served as their own mime type
  // from this very origin, so anything not on the preview whitelist must never
  // be handed to the browser to render
  download.href = blobPath(blob);
  download.download = blob.sha256 + (blob.ext || "");
  download.textContent = "Download";
  card.appendChild(download);
  return card;
}

function buildDetailMeta(blob) {
  const fragment = document.createDocumentFragment();
  const row = (label, node) => {
    fragment.appendChild(el("dt", "text-gray-500", label));
    fragment.appendChild(node);
  };

  const size = el("dd", "mono text-gray-200", formatBytes(blob.size));
  size.title = `${blob.size.toLocaleString()} bytes`;
  row("Size", size);
  row("Type", el("dd", "text-gray-200", blob.type || "unknown"));

  const when = el("dd", "text-gray-200", formatDate(blob.uploaded));
  when.title = formatRelative(blob.uploaded);
  row("Uploaded", when);

  const who = el("dd", "mono text-purple-300 break-all");
  if (blob.owners.length === 0) {
    who.textContent = "nothing in the index points at this file";
    who.className = "text-xs text-red-300";
  } else {
    for (const owner of blob.owners) {
      const line = el("div", "", shortNpub(owner));
      line.title = npubFor(owner);
      who.appendChild(line);
    }
  }
  row(blob.owners.length > 1 ? "Uploaders" : "Uploader", who);

  row("URL", el("dd", "mono break-all text-xs text-gray-300", blobURL(blob)));
  return fragment;
}

function copyToClipboard(text, node) {
  const restore = node.textContent;
  const done = () => {
    node.textContent = "Copied";
    setTimeout(() => (node.textContent = restore), 1200);
  };
  if (navigator.clipboard) navigator.clipboard.writeText(text).then(done, () => (node.textContent = restore));
}

//
// actions
//
// runAction reloads its panel after every call. Here that reload is listblobs,
// which is a signature prompt — so a delete would cost two. The local state is
// updated instead, and a reload is offered rather than spent when something goes
// wrong and the view on screen may no longer be true.
//

async function runMediaAction(call, after, workingLabel) {
  const panel = panels.media;
  if (panel) setStatus(panel.node, "info", workingLabel || "waiting for your signer…");
  try {
    const result = await call();
    if (after) after(result);
    if (panel) setStatus(panel.node, "ok", "done");
  } catch (error) {
    if (panel) setStatus(panel.node, "error", describeError(error));
    setMediaStale("The library on screen may no longer match the relay.");
  }
}

function chunk(array, size) {
  const out = [];
  for (let i = 0; i < array.length; i += size) out.push(array.slice(i, i + size));
  return out;
}

function forgetBlobs(hashes, { blocked = false, reason = "" } = {}) {
  const gone = new Set(hashes);
  for (const sha of gone) {
    unmountThumb(sha);
    media.selection.delete(sha);
    media.byHash.delete(sha);
    if (blocked && !media.blocked.some((entry) => entry.sha256 === sha)) {
      media.blocked.push({ sha256: sha, reason });
    }
  }
  media.blobs = media.blobs.filter((blob) => !gone.has(blob.sha256));
  applyMediaFilters({ resetPage: false });
  renderBlockedBlobs();
}

function describeDeletion(blobs, hidden) {
  const bytes = blobs.reduce((total, blob) => total + blob.size, 0);
  const calls = Math.ceil(blobs.length / MAX_DELETE_PER_CALL);
  const parts = [formatBytes(bytes)];
  parts.push(calls === 1 ? "one signature" : `${calls} signatures`);
  if (hidden) parts.push(`${hidden} hidden by the current filter`);
  return {
    detail: parts.join(" · "),
    // deleting the whole library is otherwise one Select all and one click away
    requireTyped: blobs.length > 50 || bytes > 1_000_000_000 ? "DELETE" : "",
  };
}

async function deleteSelected(hashes) {
  const blobs = hashes.map((sha) => media.byHash.get(sha)).filter(Boolean);
  if (!blobs.length) return;
  const summary = selectionSummary();
  const described = describeDeletion(blobs, hashes.length === summary.count ? summary.hidden : 0);

  const chosen = await confirmActionWithOptions({
    title: `Delete ${blobs.length.toLocaleString()} ${blobs.length === 1 ? "file" : "files"}?`,
    body:
      "These files are removed from disk and from the index. Their URLs stop working immediately, and anything on nostr that embeds them breaks. This cannot be undone — there is no trash.",
    detail: described.detail,
    danger: `Delete ${blobs.length.toLocaleString()}`,
    requireTyped: described.requireTyped,
    options: [
      {
        key: "block",
        // unchecked by default: blocking is a longer lived, stronger statement
        // than deleting, and a default should not escalate one into the other
        label: "Also block these hashes, so the same bytes cannot be uploaded again",
        checked: false,
      },
    ],
  });
  if (!chosen) return;

  await runMediaAction(
    async () => {
      for (const batch of chunk(hashes, MAX_DELETE_PER_CALL)) {
        await nip86(GLOBAL, "deleteblobs", [batch, chosen.block, ""]);
        forgetBlobs(batch, { blocked: chosen.block });
      }
    },
    null,
    `deleting ${blobs.length.toLocaleString()}…`
  );
}

async function blockSelected(hashes) {
  const ok = await confirmAction({
    title: `Block ${hashes.length.toLocaleString()} ${hashes.length === 1 ? "hash" : "hashes"}?`,
    body:
      "The relay stops serving them and refuses them if they are uploaded again. The files stay on disk until you delete them.",
    detail: `${hashes.length.toLocaleString()} · one signature per hash`,
    danger: "Block",
  });
  if (!ok) return;

  await runMediaAction(
    async () => {
      for (const sha of hashes) {
        await nip86(GLOBAL, "blockblob", [sha, ""]);
        const blob = media.byHash.get(sha);
        if (blob) blob.blocked = true;
        if (!media.blocked.some((entry) => entry.sha256 === sha)) media.blocked.push({ sha256: sha, reason: "" });
      }
      applyMediaFilters({ resetPage: false });
      renderBlockedBlobs();
    },
    null,
    "blocking…"
  );
}

async function unblockBlob(sha) {
  await runMediaAction(
    async () => {
      await nip86(GLOBAL, "unblockblob", [sha]);
      media.blocked = media.blocked.filter((entry) => entry.sha256 !== sha);
      const blob = media.byHash.get(sha);
      if (blob) blob.blocked = false;
      applyMediaFilters({ resetPage: false });
      renderBlockedBlobs();
    },
    null,
    "unblocking…"
  );
}

//
// blocked hashes
//

function renderBlockedBlobs() {
  const rows = document.getElementById("blocked-rows");
  if (!rows) return;
  clear(rows);

  if (!media.blocked.length) {
    const row = el("tr");
    row.appendChild(el("td", "text-sm text-gray-500", "none"));
    rows.appendChild(row);
    return;
  }

  for (const entry of media.blocked) {
    const row = el("tr");
    const stored = media.byHash.get(entry.sha256);

    const hash = el("td", "mono break-all text-xs text-amber-300");
    hash.appendChild(el("span", "mr-2 text-gray-500", "⦸"));
    hash.appendChild(el("span", "", entry.sha256));
    row.appendChild(hash);

    row.appendChild(el("td", "text-sm text-gray-300", entry.reason || ""));

    // "blocked but still stored" is a state the owner almost certainly wants to
    // finish resolving, so it says so rather than looking identical to the rest
    const state = el("td", "text-xs");
    if (stored && stored.onDisk) {
      state.className = "text-xs text-amber-300/80";
      state.textContent = `still stored — ${formatBytes(stored.size)}`;
    } else {
      state.className = "text-xs text-gray-500";
      state.textContent = "no stored file";
    }
    row.appendChild(state);

    const actions = el("td", "text-right whitespace-nowrap");
    if (stored && stored.onDisk) {
      actions.appendChild(
        button("Delete file", "mr-2 rounded border border-gray-600 px-2 py-1 text-xs", () => deleteSelected([entry.sha256]))
      );
    }
    actions.appendChild(
      button("Unblock", "rounded border border-gray-600 px-2 py-1 text-xs", async () => {
        const ok = await confirmAction({
          title: "Unblock this hash?",
          body: "It can be uploaded again, and any copy still on disk is served again.",
          detail: entry.sha256,
          danger: "Unblock",
        });
        if (ok) unblockBlob(entry.sha256);
      })
    );
    row.appendChild(actions);
    rows.appendChild(row);
  }
}

//
// reconciliation
//

function renderOrphans(report) {
  const cards = document.getElementById("orphan-cards");
  const grace = document.getElementById("orphan-grace");
  clear(cards);
  clear(grace);

  if (!report || typeof report !== "object") return;

  const missing = Number(report.missing_files_total) || 0;
  const missingRecent = Number(report.missing_files_recent) || 0;
  const unindexed = Number(report.unindexed_total) || 0;
  const unindexedRecent = Number(report.unindexed_recent) || 0;
  const totalFiles = Number(report.total_files) || 0;

  if (missing + missingRecent + unindexed + unindexedRecent === 0) {
    cards.appendChild(
      el(
        "p",
        "text-sm text-gray-400",
        "Nothing to reconcile — every file has an index entry and every entry has a file."
      )
    );
    return;
  }

  // an unindexed file is not "wasted space": khatru serves a blob whether or not
  // the index knows about it, so it is a live public URL nothing here lists
  cards.appendChild(
    buildOrphanCard({
      title: unindexed === 1 ? "A file with no index entry" : "Files with no index entry",
      count: unindexed,
      recent: unindexedRecent,
      body: `${unindexed === 1 ? "It still downloads" : "They still download"} over ${unindexed === 1 ? "its" : "their"} public URL, and nothing in the library shows ${unindexed === 1 ? "it" : "them"}. ${formatBytes(report.unindexed_bytes || 0)}.`,
      entries: report.unindexed || [],
      // a handful is information; a meaningful slice of the disk is a warning
      alarming: (report.unindexed_bytes || 0) > 100_000_000,
      action: "files",
      actionLabel: `Delete ${unindexed.toLocaleString()} unindexed files`,
      // this shape is a lost or rolled-back blossom database, not a leak — and
      // haven's backups carry the index, never the files, so cleaning here after
      // a partial restore would wipe the library
      lost: totalFiles > 0 && unindexed + unindexedRecent > totalFiles / 2,
      lostBody: `${(unindexed + unindexedRecent).toLocaleString()} of ${totalFiles.toLocaleString()} files are unindexed. That is what a lost or rolled back blossom database looks like, not a leak. Restore db/blossom from a backup before cleaning — deleting these would delete almost your whole library.`,
    })
  );

  cards.appendChild(
    buildOrphanCard({
      title: missing === 1 ? "An index entry with no file" : "Index entries with no file",
      count: missing,
      recent: missingRecent,
      body:
        missing === 1
          ? "Its URL already returns 404. Removing the entry tidies the library; there is no file left to delete."
          : "Their URLs already return 404. Removing the entries tidies the library; there is no file left to delete.",
      entries: report.missing_files || [],
      alarming: false,
      action: "index",
      actionLabel: `Remove ${missing.toLocaleString()} dead entries`,
      lost: false,
    })
  );

  const minutes = Math.round((Number(report.grace_seconds) || 0) / 60);
  grace.textContent = `Anything newer than ${minutes} minutes is held back: an upload writes its index entry before its file, so one in flight is indistinguishable from a lost one.`;
  if (report.warning) {
    grace.appendChild(el("span", "block text-amber-300/80", report.warning));
  }
}

function buildOrphanCard(spec) {
  const card = el("div", `rounded-lg border p-4 ${spec.alarming ? "border-amber-600/60 bg-amber-950/20" : "border-gray-700 bg-gray-800/40"}`);

  card.appendChild(el("h4", "font-semibold text-sm", spec.title));

  // Nothing to clean is not the same as nothing wrong: an upload still in flight
  // is held back on purpose, and a big amber zero next to a byte figure that
  // excludes it reads like a contradiction. Say which it is instead.
  if (spec.count === 0) {
    card.appendChild(
      el(
        "p",
        "mt-2 text-xs text-gray-400",
        spec.recent
          ? `Nothing to clean up — ${spec.recent} ${spec.recent === 1 ? "is" : "are"} too recent to touch, and may still be arriving.`
          : "None."
      )
    );
    return card;
  }

  const figure = el("p", "mt-1 flex items-baseline gap-2");
  figure.appendChild(el("span", "mono text-2xl font-bold leading-none text-amber-300", spec.count.toLocaleString()));
  if (spec.recent) figure.appendChild(el("span", "text-xs text-gray-500", `${spec.recent} too recent to touch`));
  card.appendChild(figure);
  card.appendChild(el("p", "mt-2 text-xs text-gray-400", spec.body));

  if (spec.entries.length) {
    const list = el("div", "mt-3 max-h-40 overflow-auto rounded border border-gray-700 bg-gray-900 p-2");
    for (const raw of spec.entries.slice(0, 50)) {
      const entry = normaliseBlob(raw);
      if (!entry) continue;
      const line = el("div", "mono flex justify-between gap-2 text-[11px] text-gray-400");
      line.appendChild(el("span", "truncate", shortHash(entry.sha256)));
      line.appendChild(el("span", "shrink-0 text-gray-500", formatBytes(entry.diskSize || entry.size)));
      list.appendChild(line);
    }
    if (spec.entries.length > 50) {
      list.appendChild(el("div", "mt-1 text-[11px] text-gray-500", `…and ${(spec.entries.length - 50).toLocaleString()} more`));
    }
    card.appendChild(list);
  }

  if (spec.lost) {
    // deliberately replaced, not merely warned about: a warning gets clicked
    // through, and this particular click is not recoverable
    card.appendChild(el("p", "mt-3 rounded border border-amber-600 bg-amber-950/40 px-3 py-2 text-xs text-amber-200", spec.lostBody));
    return card;
  }

  {
    card.appendChild(
      button(spec.actionLabel, "mt-3 rounded bg-amber-700 px-3 py-1.5 text-sm font-semibold hover:bg-amber-600", async () => {
        const ok = await confirmAction({
          title: spec.actionLabel + "?",
          body:
            spec.action === "files"
              ? "These files are deleted from disk. Any URL still pointing at one stops working. This cannot be undone."
              : "These index entries are removed. No files are deleted — there are none left to delete.",
          detail: `${spec.count.toLocaleString()} · one signature`,
          danger: "Clean up",
        });
        if (!ok) return;
        await runMediaAction(
          async () => {
            await nip86(GLOBAL, "deleteorphanblobs", [spec.action]);
            await refreshPanel("media-orphans", false);
            setMediaStale("Reconciliation changed the library.");
          },
          null,
          "cleaning up…"
        );
      })
    );
  }

  return card;
}

//
// routing and the view switcher
//
// The bare relay keys keep meaning exactly what they always meant, so every
// bookmark made before the media view existed still lands where it did.
//

function parseRoute(hash) {
  const raw = String(hash || "").replace(/^#/, "");
  const [first, second] = raw.split("/");

  // relay keys are tested before view names: if a relay is ever named like a
  // view, the URL still resolves to the relay rather than silently changing
  // meaning underneath somebody's bookmark
  if (RELAYS.some((relay) => relay.key === first)) return { view: "moderation", relay: first };
  if (VIEWS.includes(first)) {
    return { view: first, relay: RELAYS.some((relay) => relay.key === second) ? second : selected.key };
  }
  return { view: "dashboard", relay: GLOBAL.key };
}

let routed = false;

function applyRoute() {
  const route = parseRoute(location.hash);
  // selectRelay reloads the per-relay panels, and each reload is a signature
  // prompt, so it only runs when the relay actually changed
  if (!routed || route.relay !== selected.key) {
    routed = true;
    selectRelay(route.relay);
  }
  showView(route.view);
}

function showView(name) {
  const view = VIEWS.includes(name) ? name : "dashboard";
  media.active = view === "media";
  if (typeof notesSetActive === "function") notesSetActive(view === "notes");
  if (typeof dashSetActive === "function") dashSetActive(view === "dashboard");

  // the relay switcher is shared, so it follows the view rather than living in one
  const switcher = document.getElementById("relay-switcher");
  if (switcher) switcher.hidden = !PER_RELAY_VIEWS.has(view);

  for (const candidate of VIEWS) {
    const panel = document.getElementById(`view-${candidate}`);
    const tab = document.getElementById(`view-tab-${candidate}`);
    const active = candidate === view;
    if (panel) panel.hidden = !active;
    if (tab) {
      tab.setAttribute("aria-selected", active ? "true" : "false");
      tab.tabIndex = active ? 0 : -1;
      tab.classList.toggle("bg-gray-900", active);
      tab.classList.toggle("text-white", active);
      tab.classList.toggle("text-gray-400", !active);
    }
  }

  if (media.active) {
    recomputeGridColumns();
    ensurePageObserver();
  }
}

function goToView(name) {
  // moderation keeps the bare relay key it has always used, because that form is
  // in people's bookmarks and must not change meaning; the other per-relay views
  // carry the relay after their own name so a bookmark reopens the same one.
  const next =
    name === "moderation"
      ? selected.key
      : PER_RELAY_VIEWS.has(name)
        ? `${name}/${selected.key}`
        : name;
  // setting an unchanged hash fires no event, so the route is applied directly
  if (location.hash.replace(/^#/, "") === next) applyRoute();
  else location.hash = next;
}

function wireViewTabs() {
  const tabs = [...document.querySelectorAll(".view-tab")];
  for (const tab of tabs) {
    tab.addEventListener("click", () => goToView(tab.dataset.view));
    tab.addEventListener("keydown", (event) => {
      const index = tabs.indexOf(tab);
      let next = -1;
      if (event.key === "ArrowRight") next = (index + 1) % tabs.length;
      else if (event.key === "ArrowLeft") next = (index - 1 + tabs.length) % tabs.length;
      else if (event.key === "Home") next = 0;
      else if (event.key === "End") next = tabs.length - 1;
      if (next < 0) return;
      event.preventDefault();
      tabs[next].focus();
      goToView(tabs[next].dataset.view);
    });
  }
}

//
// toolbar and dialog wiring
//

function wireMediaToolbar() {
  const search = document.getElementById("media-search");
  let debounce = null;
  search.addEventListener("input", () => {
    clearTimeout(debounce);
    debounce = setTimeout(() => {
      media.filter.query = search.value.trim().toLowerCase();
      applyMediaFilters();
    }, 120);
  });

  document.getElementById("media-sort").addEventListener("change", (event) => {
    media.sort = event.target.value;
    applyMediaFilters();
  });

  for (const densityButton of document.querySelectorAll(".density-btn")) {
    densityButton.addEventListener("click", () => {
      media.density = densityButton.dataset.density;
      for (const other of document.querySelectorAll(".density-btn")) {
        const active = other === densityButton;
        other.setAttribute("aria-pressed", active ? "true" : "false");
        other.classList.toggle("bg-amber-900", active);
        other.classList.toggle("text-amber-200", active);
      }
      media.page = 1;
      renderMedia();
    });
  }

  document.getElementById("media-select-mode").addEventListener("click", () => setSelectMode(!media.selecting));
  document.getElementById("media-more").addEventListener("click", appendNextPage);
  document.getElementById("selection-all").addEventListener("click", selectAllFiltered);
  document.getElementById("selection-clear").addEventListener("click", clearSelection);
  document.getElementById("selection-delete").addEventListener("click", () => deleteSelected([...media.selection]));
  document.getElementById("selection-block").addEventListener("click", () => blockSelected([...media.selection]));

  window.addEventListener("resize", recomputeGridColumns);
}

function wireMediaDialog() {
  const dialog = document.getElementById("media-dialog");
  document.getElementById("media-dialog-close").addEventListener("click", closeMediaDetail);

  // focus goes back to the tile it came from, not to the top of the page: on a
  // library of five hundred that is the difference between usable and unusable
  dialog.addEventListener("close", () => {
    const index = media.openIndex;
    media.openSha = null;
    media.openIndex = -1;
    if (index >= 0) focusTile(index);
  });

  document.getElementById("media-copy-url").addEventListener("click", (event) => {
    const blob = media.byHash.get(media.openSha);
    if (blob) copyToClipboard(blobURL(blob), event.currentTarget);
  });
  document.getElementById("media-copy-hash").addEventListener("click", (event) => {
    if (media.openSha) copyToClipboard(media.openSha, event.currentTarget);
  });

  document.getElementById("media-block").addEventListener("click", async () => {
    const blob = media.byHash.get(media.openSha);
    if (!blob) return;
    closeMediaDetail();
    if (blob.blocked) await unblockBlob(blob.sha256);
    else await blockSelected([blob.sha256]);
  });

  document.getElementById("media-delete").addEventListener("click", async () => {
    const sha = media.openSha;
    if (!sha) return;
    closeMediaDetail();
    await deleteSelected([sha]);
  });
}

//
// keyboard navigation across the grid
//

function focusTile(index) {
  const bounded = Math.max(0, Math.min(index, media.view.length - 1));
  media.focusIndex = bounded;
  // arrowing past the last rendered tile must not trap a keyboard user behind
  // the lazy pager
  while (bounded >= media.page * PAGE_SIZE && media.page * PAGE_SIZE < media.view.length) {
    media.page++;
    renderMedia();
  }
  for (const tile of document.querySelectorAll(".media-tile")) {
    tile.tabIndex = Number(tile.dataset.index) === bounded ? 0 : -1;
  }
  const target = document.querySelector(`.media-tile[data-index="${bounded}"]`);
  if (target) target.focus();
}

function wireGridKeyboard() {
  const grid = document.getElementById("media-grid");
  if (!grid) return;

  grid.addEventListener("keydown", (event) => {
    const tile = event.target.closest(".media-tile");
    if (!tile) return;
    const index = Number(tile.dataset.index);
    const columns = media.columns || 1;
    let next = null;

    switch (event.key) {
      case "ArrowRight":
        next = index + 1;
        break;
      case "ArrowLeft":
        next = index - 1;
        break;
      case "ArrowDown":
        next = index + columns;
        break;
      case "ArrowUp":
        next = index - columns;
        break;
      case "PageDown":
        next = index + columns * 4;
        break;
      case "PageUp":
        next = index - columns * 4;
        break;
      case "Home":
        next = 0;
        break;
      case "End":
        next = media.view.length - 1;
        break;
      case " ":
      case "Spacebar":
        event.preventDefault();
        toggleSelection(media.view[index].sha256, index, { shift: event.shiftKey, additive: true });
        return;
      default:
        return;
    }

    event.preventDefault();
    if (event.shiftKey && next !== null) selectRange(index, Math.max(0, Math.min(next, media.view.length - 1)));
    focusTile(next);
  });
}

// Deferred scripts all execute, in order, before DOMContentLoaded fires, so
// booting here means every file is present regardless of which one boot lives in.
// That is what makes splitting this into several classic scripts safe: an
// ordering mistake becomes impossible rather than silent.
window.addEventListener("DOMContentLoaded", boot);
