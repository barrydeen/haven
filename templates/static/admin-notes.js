"use strict";

//
// the notes browser
//
// A read-only nostr client over one relay's own store, with delete.
//
// Two rules shape everything here, and both come from the transport:
//
//   1. NIP-86 has no batching, so every call is a signer prompt. Nothing in this
//      file fetches anything the owner did not press a button for — no infinite
//      scroll, no prefetch, no reload after a delete, no refetch when the relay
//      switcher moves.
//   2. The admin page's CSP is connect-src 'self', so there is nowhere to fetch a
//      profile or a remote image from except this relay. Author names ride along
//      with the page that needed them; remote images are opt-in and say why.
//
// Every binding is prefixed note/notes because these are classic scripts sharing
// one global scope: redeclaring a const another file already owns throws, and the
// file that loses does not execute at all. admin.js already owns KIND_COLOURS,
// KIND_LABELS and KIND_GLYPHS, which mean *media* kinds — hence NOTE_KIND_NAMES.
//

const NOTES_PAGE_LIMIT = 200; // rows per signed call
const NOTES_RENDER_STEP = 50; // cards rendered per free "show more"
const NOTES_MAX_DELETE = 500; // matches the server's maxEventsPerDelete
const NOTES_CONTENT_CAP = 2000; // characters rendered in a feed card
const NOTES_TYPED_GUARD = 20; // selection size that starts demanding a typed word

const NOTE_KIND_NAMES = {
  0: "profile",
  1: "note",
  3: "follow list",
  4: "encrypted DM",
  5: "delete request",
  6: "repost",
  7: "reaction",
  13: "seal",
  14: "direct message",
  16: "generic repost",
  1059: "gift wrap",
  1063: "file metadata",
  1111: "comment",
  1984: "report",
  9735: "zap receipt",
  10002: "relay list",
  10084: "ban list",
  24242: "blob index",
  30023: "long-form",
};

// kinds whose content is ciphertext. Never rendered, never guessed at: the relay
// cannot read them and neither can this page.
const NOTE_ENCRYPTED_KINDS = new Set([4, 13, 14, 1059]);

const notes = {
  active: false,
  relay: null, // the relay key the loaded events actually came from
  events: [],
  byId: new Map(),
  profiles: new Map(),
  view: [],
  page: 1,
  density: "cards",
  search: "",
  kinds: new Set(), // client-side kind chips
  sort: "newest",
  selection: new Set(),
  selectMode: false,
  remoteMedia: false,
  revealed: new Set(),
  cursor: "",
  total: 0,
  countedAt: 0,
  scanned: 0,
  complete: true,
  warning: "",
  query: null, // the server-side filter currently in force
  stale: "",
  openIndex: -1,
  loading: false,
};

//
// small helpers
//

function noteEl(tag, className, text) {
  return el(tag, className, text);
}

function noteKindName(kind) {
  return NOTE_KIND_NAMES[kind] || null;
}

function noteKindLabel(kind) {
  const name = noteKindName(kind);
  return name ? `${name} · ${kind}` : `kind ${kind}`;
}

function noteIsEncrypted(event) {
  return NOTE_ENCRYPTED_KINDS.has(event.kind) || (event.kind >= 4400 && event.kind <= 4499);
}

// noteAvatar draws a deterministic identicon from the pubkey itself.
//
// It is inline SVG rather than an <img>: a real profile picture is a request to
// somebody else's server, which is the one thing this page tries not to make by
// accident. A wall of identical grey circles would also make a feed unreadable,
// and these are at least distinguishable at a glance.
function noteAvatar(pubkey, size) {
  const svg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
  svg.setAttribute("width", String(size));
  svg.setAttribute("height", String(size));
  svg.setAttribute("viewBox", "0 0 5 5");
  svg.setAttribute("aria-hidden", "true");
  svg.setAttribute("class", "shrink-0 rounded");

  const hex = /^[0-9a-f]{64}$/.test(pubkey || "") ? pubkey : "0".repeat(64);
  const hues = ["#8b5cf6", "#0891b2", "#16a34a", "#e04c9a", "#d97706"];
  const colour = hues[parseInt(hex.slice(0, 2), 16) % hues.length];

  const bg = document.createElementNS(svg.namespaceURI, "rect");
  bg.setAttribute("width", "5");
  bg.setAttribute("height", "5");
  bg.setAttribute("fill", "#1f2937");
  svg.appendChild(bg);

  // mirrored down the middle, which is what makes a 25 cell grid read as a face
  for (let col = 0; col < 3; col++) {
    for (let row = 0; row < 5; row++) {
      const bit = parseInt(hex[(col * 5 + row) % 64], 16) % 2;
      if (!bit) continue;
      for (const x of col === 2 ? [2] : [col, 4 - col]) {
        const cell = document.createElementNS(svg.namespaceURI, "rect");
        cell.setAttribute("x", String(x));
        cell.setAttribute("y", String(row));
        cell.setAttribute("width", "1");
        cell.setAttribute("height", "1");
        cell.setAttribute("fill", colour);
        svg.appendChild(cell);
      }
    }
  }
  return svg;
}

function noteProfile(pubkey) {
  return notes.profiles.get(pubkey) || null;
}

function noteAuthorName(pubkey) {
  const profile = noteProfile(pubkey);
  const name = profile && (profile.display_name || profile.name);
  if (typeof name !== "string") return null;
  const trimmed = name.trim();
  if (!trimmed) return null;
  // untrusted, attacker controlled, and inserted with textContent everywhere it
  // is used — capped so one author cannot push the rest of a card off screen
  return trimmed.length > 48 ? trimmed.slice(0, 48) + "…" : trimmed;
}

//
// the content renderer
//

const NOTE_TOKEN_RE =
  /(https?:\/\/[^\s<>"'`]+)|(nostr:(?:npub|nprofile|note|nevent|naddr)1[02-9ac-hj-np-z]+)|(#[\p{L}\p{N}_-]+)/gu;

function noteTokenise(text) {
  const tokens = [];
  let last = 0;
  for (const match of String(text).matchAll(NOTE_TOKEN_RE)) {
    if (match.index > last) tokens.push({ type: "text", value: text.slice(last, match.index) });
    if (match[1]) tokens.push({ type: "url", value: match[1] });
    else if (match[2]) tokens.push({ type: "mention", value: match[2].slice("nostr:".length) });
    else if (match[3]) tokens.push({ type: "hashtag", value: match[3] });
    last = match.index + match[0].length;
  }
  if (last < text.length) tokens.push({ type: "text", value: text.slice(last) });
  return tokens;
}

// noteClassifyURL decides whether a URL is one of this relay's own blobs.
//
// Only a blob served from this origin is ever inlined without asking. Anything
// else is a request to a third party, which on a Tor-only relay is the one thing
// this page can leak, so it goes behind the placeholder below.
function noteClassifyURL(raw) {
  let url;
  try {
    url = new URL(raw, location.href);
  } catch (e) {
    return { kind: "link", href: raw, host: "" };
  }
  if (url.protocol !== "http:" && url.protocol !== "https:") {
    return { kind: "link", href: raw, host: url.host };
  }

  let ours = url.origin === location.origin;
  if (!ours) {
    try {
      ours = url.origin === new URL(GLOBAL.serviceURL).origin;
    } catch (e) {
      /* a service URL we cannot parse simply is not a match */
    }
  }

  const blob = url.pathname.match(/^\/([0-9a-f]{64})(?:\.[a-z0-9]{1,5})?$/i);
  if (ours && blob) {
    const ext = url.pathname.includes(".") ? url.pathname.split(".").pop().toLowerCase() : "";
    const media =
      /^(png|jpe?g|gif|webp|avif|bmp|svg)$/.test(ext) ? "image"
      : /^(mp4|webm|mov|m4v)$/.test(ext) ? "video"
      : /^(mp3|ogg|wav|m4a|flac)$/.test(ext) ? "audio"
      : "other";
    return { kind: "blossom", media, href: url.href, host: url.host, sha: blob[1].toLowerCase() };
  }

  const ext = url.pathname.includes(".") ? url.pathname.split(".").pop().toLowerCase() : "";
  if (/^(png|jpe?g|gif|webp|avif|bmp)$/.test(ext)) {
    return { kind: "remote-image", href: url.href, host: url.host };
  }
  if (/^(mp4|webm|mov|mp3|ogg|wav|m4a)$/.test(ext)) {
    return { kind: "remote-media", href: url.href, host: url.host };
  }
  return { kind: "link", href: url.href, host: url.host };
}

function noteBlossomNode(info) {
  if (info.media === "image") {
    const img = noteEl("img", "note-media");
    img.alt = "";
    img.loading = "lazy";
    img.decoding = "async";
    img.src = info.href;
    return img;
  }
  if (info.media === "video" || info.media === "audio") {
    const player = noteEl(info.media, info.media === "video" ? "note-media" : "w-full");
    player.controls = true;
    player.preload = "metadata";
    if (info.media === "video") player.playsInline = true;
    player.src = info.href;
    return player;
  }
  return noteLinkNode(info);
}

function noteLinkNode(info) {
  const wrap = noteEl("span", "inline-flex items-center gap-1");
  wrap.appendChild(noteEl("span", "break-all text-gray-300", info.href));
  const open = noteEl("a", "shrink-0 text-purple-300 underline", "⧉");
  open.href = info.href;
  open.target = "_blank";
  // no referrer and no opener: this page holds a live window.nostr handle
  open.rel = "noopener noreferrer nofollow";
  open.referrerPolicy = "no-referrer";
  open.title = info.href;
  wrap.appendChild(open);
  return wrap;
}

// noteRemoteNode is the click-to-load gate.
//
// Images can be loaded in place. Video and audio cannot be, ever: the CSP says
// media-src 'self', so a remote player would fail silently — so that case offers
// a new tab instead of a button that does nothing.
function noteRemoteNode(info, rerender) {
  if (notes.remoteMedia || notes.revealed.has(info.href)) {
    if (info.kind === "remote-image") {
      const img = noteEl("img", "note-media");
      img.alt = "";
      img.loading = "lazy";
      img.decoding = "async";
      img.referrerPolicy = "no-referrer";
      img.src = info.href;
      return img;
    }
  }

  if (info.kind === "remote-media") {
    const wrap = noteEl("span", "note-remote");
    wrap.appendChild(noteEl("span", "", "⇱"));
    wrap.appendChild(noteEl("span", "", `${info.host} — this relay cannot play remote media`));
    const open = noteEl("a", "underline", "open in a new tab");
    open.href = info.href;
    open.target = "_blank";
    open.rel = "noopener noreferrer nofollow";
    open.referrerPolicy = "no-referrer";
    wrap.appendChild(open);
    return wrap;
  }

  const button = noteEl("button", "note-remote");
  button.type = "button";
  button.title = info.href;
  button.appendChild(noteEl("span", "", "⇱"));
  button.appendChild(noteEl("span", "", `${info.host} — load image`));
  button.addEventListener("click", (event) => {
    event.stopPropagation();
    notes.revealed.add(info.href);
    if (rerender) rerender();
  });
  return button;
}

function noteRenderContent(host, event, { full = false, rerender = null } = {}) {
  let text = String(event.content || "");
  let clipped = false;
  const cap = full ? 50000 : NOTES_CONTENT_CAP;
  if (text.length > cap) {
    text = text.slice(0, cap);
    clipped = true;
  }

  const body = noteEl("div", "note-content text-sm text-gray-200");
  for (const token of noteTokenise(text)) {
    if (token.type === "text") {
      body.appendChild(document.createTextNode(token.value));
      continue;
    }
    if (token.type === "hashtag") {
      body.appendChild(noteEl("span", "text-purple-300", token.value));
      continue;
    }
    if (token.type === "mention") {
      body.appendChild(noteMentionNode(token.value));
      continue;
    }
    const info = noteClassifyURL(token.value);
    if (info.kind === "blossom") {
      body.appendChild(document.createTextNode(" "));
      body.appendChild(noteBlossomNode(info));
      continue;
    }
    if (info.kind === "remote-image" || info.kind === "remote-media") {
      body.appendChild(document.createTextNode(" "));
      body.appendChild(noteRemoteNode(info, rerender));
      continue;
    }
    body.appendChild(noteLinkNode(info));
  }
  host.appendChild(body);

  if (clipped) {
    host.appendChild(
      noteEl("p", "mt-1 text-xs text-gray-500", `… ${event.content.length.toLocaleString()} characters in total`)
    );
  }
}

function noteMentionNode(bech32) {
  const chip = noteEl("span", "mono text-purple-300");
  // a mention is only resolved to a name if this relay happens to store that
  // profile; nothing is fetched to find out
  if (bech32.startsWith("npub1")) {
    try {
      const hex = npubToHex(bech32);
      const name = noteAuthorName(hex);
      chip.textContent = name ? `@${name}` : `@${shortNpub(hex)}`;
      chip.title = bech32;
      return chip;
    } catch (e) {
      /* not decodable: fall through and show it as written */
    }
  }
  chip.textContent = bech32.length > 24 ? `${bech32.slice(0, 20)}…` : bech32;
  chip.title = bech32;
  return chip;
}

//
// per-kind bodies
//

function noteRenderBody(host, event, opts) {
  if (noteIsEncrypted(event)) {
    const locked = noteEl("div", "note-locked text-sm");
    locked.appendChild(
      noteEl("p", "", `🔒 Encrypted (${noteKindLabel(event.kind)}). This is sealed to its recipient — the relay cannot read it, and neither can this page.`)
    );
    locked.appendChild(
      noteEl("p", "mono mt-1 text-xs opacity-80", `${(event.content || "").length.toLocaleString()} bytes of ciphertext`)
    );
    host.appendChild(locked);
    return;
  }

  switch (event.kind) {
    case 0:
      return noteRenderProfile(host, event, opts);
    case 3:
      return noteRenderTagList(host, event, "p", "follows");
    case 5:
      return noteRenderDeletion(host, event);
    case 7:
      return noteRenderReaction(host, event);
    case 30023:
      return noteRenderLongForm(host, event, opts);
    default:
      return noteRenderContent(host, event, opts);
  }
}

function noteRenderProfile(host, event, opts) {
  let parsed = null;
  try {
    parsed = JSON.parse(event.content || "{}");
  } catch (e) {
    host.appendChild(noteEl("p", "text-sm text-amber-300", "This profile's content is not valid JSON."));
    return noteRenderContent(host, event, opts);
  }
  const list = noteEl("dl", "grid grid-cols-[auto_1fr] gap-x-3 gap-y-1 text-sm");
  for (const key of ["name", "display_name", "nip05", "about", "website"]) {
    const value = parsed && parsed[key];
    if (typeof value !== "string" || !value.trim()) continue;
    list.appendChild(noteEl("dt", "text-gray-500", key));
    // textContent via el(), never markup: this is a stranger's string
    list.appendChild(noteEl("dd", "break-words text-gray-200", value.slice(0, 400)));
  }
  if (!list.childElementCount) {
    host.appendChild(noteEl("p", "text-sm text-gray-400", "This profile carries no recognised fields."));
    return;
  }
  host.appendChild(list);
}

function noteRenderTagList(host, event, tagName, verb) {
  const values = (event.tags || []).filter((tag) => tag[0] === tagName).map((tag) => tag[1]);
  host.appendChild(
    noteEl("p", "text-sm text-gray-200", `${verb} ${values.length.toLocaleString()} ${values.length === 1 ? "pubkey" : "pubkeys"}`)
  );
  const chips = noteEl("div", "mt-2 flex flex-wrap gap-1");
  for (const value of values.slice(0, 12)) {
    const chip = noteEl("span", "mono rounded bg-gray-900 px-1.5 py-0.5 text-xs text-gray-400", shortNpub(value));
    chip.title = value;
    chips.appendChild(chip);
  }
  if (values.length > 12) {
    chips.appendChild(noteEl("span", "text-xs text-gray-500", `+${(values.length - 12).toLocaleString()} more`));
  }
  host.appendChild(chips);
}

function noteRenderDeletion(host, event) {
  const targets = (event.tags || []).filter((tag) => tag[0] === "e" || tag[0] === "a").map((tag) => tag[1]);
  host.appendChild(
    noteEl("p", "text-sm text-gray-200", `Requests deletion of ${targets.length.toLocaleString()} ${targets.length === 1 ? "event" : "events"}`)
  );
  const chips = noteEl("div", "mt-2 flex flex-wrap gap-1");
  for (const value of targets.slice(0, 12)) {
    const chip = noteEl("span", "mono rounded bg-gray-900 px-1.5 py-0.5 text-xs text-gray-400", shortHash(value));
    chip.title = value;
    chips.appendChild(chip);
  }
  host.appendChild(chips);
  if (event.content) host.appendChild(noteEl("p", "mt-2 text-sm text-gray-300", event.content.slice(0, 400)));
}

function noteRenderReaction(host, event) {
  const glyph = (event.content || "+").trim() || "+";
  host.appendChild(noteEl("p", "text-3xl leading-none text-gray-100", glyph.slice(0, 8)));
  const target = (event.tags || []).filter((tag) => tag[0] === "e").pop();
  if (target) {
    const line = noteEl("p", "mt-2 text-xs text-gray-500", "reacting to ");
    const chip = noteEl("span", "mono text-gray-400", shortHash(target[1]));
    chip.title = target[1];
    line.appendChild(chip);
    host.appendChild(line);
  }
}

function noteRenderLongForm(host, event, opts) {
  const tag = (name) => {
    const found = (event.tags || []).find((t) => t[0] === name);
    return found ? found[1] : "";
  };
  const title = tag("title");
  if (title) host.appendChild(noteEl("h4", "mb-1 text-base font-semibold text-gray-100", title.slice(0, 200)));
  const summary = tag("summary");
  if (summary) host.appendChild(noteEl("p", "mb-2 text-xs text-gray-400", summary.slice(0, 300)));
  noteRenderContent(host, event, opts);
  host.appendChild(
    noteEl("p", "mt-2 text-xs text-gray-500", "Long-form content is Markdown; it is shown here exactly as written.")
  );
}

//
// fetching
//
// Every function here is reached from a button. Nothing below runs on scroll, on
// a relay switch, or after a delete.
//

function notesEndpoint() {
  return selected;
}

function notesBuildQuery() {
  const query = {};
  const kind = document.getElementById("notes-q-kind").value.trim();
  if (kind !== "") {
    const n = Number(kind);
    if (!Number.isInteger(n) || n < 0 || n > 65535) throw new Error("kind must be a whole number between 0 and 65535");
    query.kinds = [n];
  }
  const author = document.getElementById("notes-q-author").value.trim();
  if (author) query.authors = [toPubkeyHex(author)];

  const since = document.getElementById("notes-q-since").value;
  if (since) query.since = Math.floor(new Date(since + "T00:00:00Z").getTime() / 1000);
  const until = document.getElementById("notes-q-until").value;
  // the picker gives a date, and the owner means the whole of it
  if (until) query.until = Math.floor(new Date(until + "T23:59:59Z").getTime() / 1000);

  const search = document.getElementById("notes-q-search").value.trim();
  if (search) query.search = search;
  return query;
}

async function notesFetch(endpoint, { append = false } = {}) {
  if (notes.loading) return;
  notes.loading = true;
  try {
    const params = Object.assign({ limit: NOTES_PAGE_LIMIT }, notes.query || {});
    if (append && notes.cursor) params.cursor = notes.cursor;

    const result = await nip86(endpoint, "listevents", [params]);

    // a reply that arrived after the owner moved on is discarded rather than
    // shown against the wrong relay
    if (result.relay && notes.relay && append && result.relay !== notes.relay) return;

    if (!append) {
      notes.events = [];
      notes.byId.clear();
      notes.selection.clear();
    }
    for (const row of result.events || []) {
      if (notes.byId.has(row.id)) continue;
      notes.byId.set(row.id, row);
      notes.events.push(row);
    }
    for (const [pubkey, profile] of Object.entries(result.profiles || {})) {
      // the server hands back the kind 0 content verbatim, so it is parsed here
      // and a broken one is simply skipped
      if (profile && typeof profile === "object") notes.profiles.set(pubkey, profile);
    }

    notes.relay = result.relay || endpoint.key;
    notes.cursor = result.next_cursor || "";
    notes.total = Number(result.total) || 0;
    notes.countedAt = Number(result.counted_at) || 0;
    notes.scanned = Number(result.scanned) || 0;
    notes.complete = result.complete !== false;
    notes.warning = result.warning || "";
    notes.stale = "";
    if (!append) notes.page = 1;
    notesApplyFilters({ resetPage: !append });
  } finally {
    notes.loading = false;
  }
}

//
// client-side filtering — free, instant, and clearly labelled as such
//

function notesMatches(row) {
  if (notes.kinds.size && !notes.kinds.has(row.kind)) return false;
  if (!notes.search) return true;
  const needle = notes.search.toLowerCase();
  if ((row.content || "").toLowerCase().includes(needle)) return true;
  if (row.id.startsWith(needle)) return true;
  const name = noteAuthorName(row.pubkey);
  if (name && name.toLowerCase().includes(needle)) return true;
  return row.pubkey.startsWith(needle);
}

function notesCompare(a, b) {
  switch (notes.sort) {
    case "oldest":
      return a.created_at - b.created_at;
    case "kind":
      return a.kind - b.kind || b.created_at - a.created_at;
    case "size":
      return (b.size || 0) - (a.size || 0);
    default:
      return b.created_at - a.created_at;
  }
}

function notesApplyFilters({ resetPage = true } = {}) {
  notes.view = notes.events.filter(notesMatches).sort(notesCompare);
  if (resetPage) notes.page = 1;
  notesRender();
}

//
// rendering
//

function notesRender() {
  const feed = document.getElementById("notes-feed");
  const tableWrap = document.getElementById("notes-table-wrap");
  const empty = document.getElementById("notes-empty");
  if (!feed) return;

  const table = notes.density === "table";
  feed.hidden = table;
  tableWrap.hidden = !table;

  feed.classList.toggle("is-cards", notes.density === "cards");
  feed.classList.toggle("is-compact", notes.density === "compact");
  feed.classList.toggle("is-selecting", notes.selectMode);

  const shown = table ? notes.view : notes.view.slice(0, notes.page * NOTES_RENDER_STEP);

  if (table) notesRenderTable(shown);
  else notesRenderCards(feed, shown);

  const nothing = notes.view.length === 0;
  empty.hidden = !nothing;
  if (nothing) {
    empty.textContent = notes.events.length
      ? "Nothing in the loaded notes matches. Widen the filter, or ask the relay — that costs one signature."
      : "This relay is not storing anything that matches.";
  }

  notesRenderSummary(shown.length);
  notesRenderKindChips();
  notesRenderPagers(shown.length);
  notesRenderSelection();
  notesRenderStale();
}

function notesRenderCards(feed, rows) {
  clear(feed);
  const fragment = document.createDocumentFragment();
  rows.forEach((row, index) => fragment.appendChild(notesBuildCard(row, index)));
  feed.appendChild(fragment);
}

function notesBuildCard(row, index) {
  const item = noteEl("li", "");
  const card = noteEl("article", "note-card");
  card.tabIndex = -1;
  card.dataset.id = row.id;
  card.dataset.index = String(index);
  card.dataset.selected = notes.selection.has(row.id) ? "true" : "false";
  // the relay's own hue, so a card is visibly from the relay you are looking at
  card.style.borderLeftColor = notesRelayColour(notes.relay);

  const head = noteEl("div", "mb-2 flex items-start gap-2");
  head.appendChild(noteAvatar(row.pubkey, 20));

  const who = noteEl("div", "min-w-0 flex-1");
  const name = noteAuthorName(row.pubkey);
  const nameLine = noteEl("div", "flex flex-wrap items-baseline gap-x-2");
  if (name) nameLine.appendChild(noteEl("span", "text-sm font-semibold text-gray-100", name));
  const npub = noteEl("span", "mono text-xs text-purple-300", shortNpub(row.pubkey));
  npub.title = hexToNpub(row.pubkey);
  nameLine.appendChild(npub);
  who.appendChild(nameLine);

  const meta = noteEl("div", "flex flex-wrap items-center gap-x-2 text-xs text-gray-500");
  const when = noteEl("span", "", formatRelative(row.created_at));
  when.title = formatDate(row.created_at);
  meta.appendChild(when);
  meta.appendChild(noteEl("span", "", "·"));
  meta.appendChild(noteEl("span", "", noteKindLabel(row.kind)));
  if (row.class && row.class !== "regular") {
    meta.appendChild(noteEl("span", "rounded bg-gray-900 px-1.5 text-[10px] text-gray-400", row.class));
  }
  if (row.banned) {
    meta.appendChild(noteEl("span", "rounded bg-amber-900 px-1.5 text-[10px] text-amber-200", "banned"));
  }
  who.appendChild(meta);
  head.appendChild(who);

  const check = noteEl("label", "note-check shrink-0 place-items-center");
  const box = noteEl("input", "");
  box.type = "checkbox";
  box.checked = notes.selection.has(row.id);
  box.addEventListener("click", (event) => event.stopPropagation());
  box.addEventListener("change", () => notesToggle(row.id));
  check.appendChild(box);
  head.appendChild(check);
  card.appendChild(head);

  noteRenderBody(card, row, { rerender: () => notesRender() });

  const foot = noteEl("div", "mt-2 flex flex-wrap items-center gap-x-2 border-t border-gray-700/60 pt-2 text-xs text-gray-500");
  foot.appendChild(noteEl("span", "", `${row.tag_count || 0} ${row.tag_count === 1 ? "tag" : "tags"}`));
  foot.appendChild(noteEl("span", "", "·"));
  foot.appendChild(noteEl("span", "mono", formatBytes(row.size || 0)));
  const details = button("Details", "ml-auto rounded border border-gray-600 px-2 py-0.5 text-xs", () =>
    notesOpenDetail(row.id)
  );
  foot.appendChild(details);
  card.appendChild(foot);

  card.addEventListener("click", (event) => {
    if (event.target.closest("button, a, input, label, video, audio")) return;
    if (notes.selectMode) notesToggle(row.id);
    else notesOpenDetail(row.id);
  });

  item.appendChild(card);
  return item;
}

function notesRelayColour(key) {
  // one fixed hue per relay, assigned by key and never by position, so filtering
  // a relay out of a chart or switching to another never repaints the rest
  return { outbox: "#8b5cf6", private: "#0891b2", chat: "#16a34a", inbox: "#e04c9a" }[key] || "#6b7280";
}

function notesRenderTable(rows) {
  const body = document.getElementById("notes-rows");
  clear(body);
  for (const row of rows) {
    const tr = noteEl("tr", "");
    tr.appendChild(noteEl("td", "text-sm", noteKindLabel(row.kind)));
    const author = noteEl("td", "mono text-xs text-purple-300", shortNpub(row.pubkey));
    author.title = hexToNpub(row.pubkey);
    tr.appendChild(author);
    const when = noteEl("td", "text-xs text-gray-400", formatRelative(row.created_at));
    when.title = formatDate(row.created_at);
    tr.appendChild(when);
    const preview = noteIsEncrypted(row) ? "🔒 encrypted" : (row.content || "").slice(0, 80).replace(/\s+/g, " ");
    tr.appendChild(noteEl("td", "text-xs text-gray-300", preview));
    const actions = noteEl("td", "text-right");
    actions.appendChild(button("Details", "rounded border border-gray-600 px-2 py-0.5 text-xs", () => notesOpenDetail(row.id)));
    tr.appendChild(actions);
    body.appendChild(tr);
  }
}

function notesRenderSummary(showing) {
  const node = document.getElementById("notes-summary");
  if (!node) return;
  const parts = [];
  parts.push(`showing ${showing.toLocaleString()} of ${notes.view.length.toLocaleString()} loaded`);
  if (notes.total) parts.push(`${notes.total.toLocaleString()} on this relay`);
  parts.push(notes.sort === "newest" ? "newest first" : `sorted ${notes.sort}`);
  if (notes.query && Object.keys(notes.query).length) parts.push("relay filter active");
  if (notes.countedAt) parts.push(`counted ${formatRelative(notes.countedAt)}`);
  node.textContent = parts.join(" · ");

  // a bounded search that stopped quietly would be a lie, so it says where it got to
  if (!notes.complete && notes.query && notes.query.search) {
    node.appendChild(document.createTextNode(" "));
    node.appendChild(
      noteEl(
        "span",
        "text-amber-300",
        `— searched the newest ${notes.scanned.toLocaleString()} events only; there may be older matches.`
      )
    );
  }
  if (notes.warning) {
    node.appendChild(document.createTextNode(" "));
    node.appendChild(noteEl("span", "text-amber-300", `— ${notes.warning}`));
  }
}

function notesRenderKindChips() {
  const host = document.getElementById("notes-kind-filters");
  if (!host) return;
  clear(host);
  const counts = new Map();
  for (const row of notes.events) counts.set(row.kind, (counts.get(row.kind) || 0) + 1);
  const kinds = [...counts.entries()].sort((a, b) => b[1] - a[1]).slice(0, 8);
  for (const [kind, count] of kinds) {
    const chip = noteEl("button", "note-chip rounded border border-gray-600 px-2 py-0.5 text-xs text-gray-300");
    chip.type = "button";
    chip.setAttribute("aria-pressed", notes.kinds.has(kind) ? "true" : "false");
    chip.textContent = `${noteKindName(kind) || "kind " + kind} ${count.toLocaleString()}`;
    chip.addEventListener("click", () => {
      if (notes.kinds.has(kind)) notes.kinds.delete(kind);
      else notes.kinds.add(kind);
      notesApplyFilters();
    });
    host.appendChild(chip);
  }
}

function notesRenderPagers(showing) {
  const showMore = document.getElementById("notes-show-more");
  const loadMore = document.getElementById("notes-load-more");
  if (!showMore || !loadMore) return;

  const remaining = notes.view.length - showing;
  showMore.hidden = notes.density === "table" || remaining <= 0;
  if (!showMore.hidden) {
    showMore.textContent = `Show ${Math.min(remaining, NOTES_RENDER_STEP).toLocaleString()} more · already loaded`;
  }

  // the two buttons are never merged: one is free and one is a signature, and
  // the labels are the only place the difference is visible
  loadMore.hidden = !notes.cursor;
  if (!loadMore.hidden) {
    loadMore.textContent = `Load ${NOTES_PAGE_LIMIT} more from this relay · one signature`;
  }
}

function notesRenderStale() {
  const node = document.getElementById("notes-stale");
  if (!node) return;
  node.hidden = !notes.stale;
  if (!notes.stale) return;
  clear(node);
  node.appendChild(document.createTextNode(notes.stale + " "));
  node.appendChild(
    button("Load this relay · one signature", "underline", () => refreshPanel("notes", true))
  );
}

//
// selection and delete
//

function notesToggle(id) {
  if (notes.selection.has(id)) notes.selection.delete(id);
  else notes.selection.add(id);
  if (!notes.selectMode && notes.selection.size) notesSetSelectMode(true);
  else notesRender();
}

function notesSetSelectMode(on) {
  notes.selectMode = on;
  const button = document.getElementById("notes-select-mode");
  if (button) button.setAttribute("aria-pressed", on ? "true" : "false");
  if (!on) notes.selection.clear();
  notesRender();
}

function notesRenderSelection() {
  const bar = document.getElementById("notes-selection");
  if (!bar) return;
  const count = notes.selection.size;
  bar.hidden = !notes.selectMode || count === 0;
  if (bar.hidden) return;

  const visible = new Set(notes.view.map((row) => row.id));
  const hidden = [...notes.selection].filter((id) => !visible.has(id)).length;

  const bytes = [...notes.selection].reduce((n, id) => n + ((notes.byId.get(id) || {}).size || 0), 0);
  document.getElementById("notes-selection-count").textContent =
    `${count.toLocaleString()} selected · ${formatBytes(bytes)}`;

  const hiddenNode = document.getElementById("notes-selection-hidden");
  hiddenNode.hidden = hidden === 0;
  if (hidden) hiddenNode.textContent = `${hidden.toLocaleString()} hidden by the current filter`;

  document.getElementById("notes-selection-all").textContent = `Select all ${notes.view.length.toLocaleString()}`;
  document.getElementById("notes-selection-delete").textContent = `Delete ${count.toLocaleString()}`;
}

function notesForget(ids) {
  const gone = new Set(ids);
  for (const id of gone) {
    notes.selection.delete(id);
    notes.byId.delete(id);
  }
  notes.events = notes.events.filter((row) => !gone.has(row.id));
  if (notes.total) notes.total = Math.max(0, notes.total - gone.size);
  notesApplyFilters({ resetPage: false });
}

async function notesDelete(ids) {
  const rows = ids.map((id) => notes.byId.get(id)).filter(Boolean);
  if (!rows.length) return;

  const relayLabel = (RELAYS.find((r) => r.key === notes.relay) || {}).label || notes.relay;
  const replaceable = rows.filter((row) => row.class === "replaceable" || row.class === "addressable").length;
  const calls = Math.ceil(ids.length / NOTES_MAX_DELETE);

  const detail = [`${rows.length.toLocaleString()} events`, `${calls} signature${calls === 1 ? "" : "s"}`, notes.relay];
  if (replaceable) {
    detail.push(`${replaceable} replaceable — the author's next update republishes them`);
  }

  const ok = await confirmActionWithOptions({
    title: `Delete ${rows.length.toLocaleString()} ${rows.length === 1 ? "event" : "events"} from ${relayLabel}?`,
    // the honest description of delete-only. Anything vaguer would read as a
    // takedown, and this is not one.
    body:
      "The stored copies are removed from this relay's database. Nothing is published — HAVEN holds no key, so no delete request goes out and other relays keep their copies. The author, or anyone else holding these events, can publish them here again. To keep an event out for good, ban it under Moderation → Banned events.",
    detail: detail.join(" · "),
    danger: `Delete ${rows.length.toLocaleString()}`,
    requireTyped: rows.length > NOTES_TYPED_GUARD ? "DELETE" : "",
  });
  if (!ok) return;

  const panel = panels.notes;
  if (panel) setStatus(panel.node, "info", `deleting ${rows.length.toLocaleString()}…`);
  try {
    for (const batch of chunk(ids, NOTES_MAX_DELETE)) {
      const result = await nip86(selected, "deleteevents", [batch]);
      // local state is mutated rather than reloaded: a reload here would be
      // another signature, which is the same bargain the media browser makes
      notesForget(batch);
      if (result && result.warning) notes.warning = result.warning;
    }
    if (panel) setStatus(panel.node, "ok", "done");
    notesRender();
  } catch (error) {
    if (panel) setStatus(panel.node, "error", describeError(error));
    notesSetStale("Some of those may not have been deleted, so this list may no longer match the relay.");
  }
}

function notesSetStale(reason) {
  notes.stale = reason;
  notesRenderStale();
}

// notesInvalidate is called when the relay switcher moves. It does not fetch:
// that would spend a signature on a feed the owner may not even be looking at.
function notesInvalidate(key) {
  if (!notes.relay || notes.relay === key) return;
  const label = (RELAYS.find((r) => r.key === key) || {}).label || key;
  const from = (RELAYS.find((r) => r.key === notes.relay) || {}).label || notes.relay;
  notesSetStale(`These notes are from ${from}, not ${label}.`);
}

function notesSetActive(on) {
  notes.active = on;
  const label = document.getElementById("notes-relay-label");
  if (label) label.textContent = (RELAYS.find((r) => r.key === selected.key) || {}).label || selected.key;
}

//
// the drawer
//

function notesOpenDetail(id) {
  const row = notes.byId.get(id);
  if (!row) return;
  notes.openIndex = notes.view.findIndex((candidate) => candidate.id === id);

  const dialog = document.getElementById("note-dialog");
  document.getElementById("note-dialog-kind").textContent = noteKindLabel(row.kind);
  const title = document.getElementById("note-dialog-title");
  title.textContent = hexToNote(row.id);
  title.title = row.id;

  const body = document.getElementById("note-dialog-body");
  clear(body);

  const who = noteEl("div", "mb-3 flex items-center gap-2");
  who.appendChild(noteAvatar(row.pubkey, 28));
  const stack = noteEl("div", "min-w-0");
  const name = noteAuthorName(row.pubkey);
  if (name) stack.appendChild(noteEl("p", "text-sm font-semibold text-gray-100", name));
  const npub = noteEl("p", "mono break-all text-xs text-purple-300", hexToNpub(row.pubkey));
  stack.appendChild(npub);
  who.appendChild(stack);
  body.appendChild(who);

  const when = noteEl("p", "mb-3 text-xs text-gray-500", `${formatDate(row.created_at)} · ${formatRelative(row.created_at)}`);
  body.appendChild(when);

  noteRenderBody(body, row, { full: true, rerender: () => notesOpenDetail(id) });

  if (row.content_truncated) {
    body.appendChild(
      noteEl(
        "p",
        "mt-2 rounded border border-gray-700 px-3 py-2 text-xs text-gray-400",
        `The feed carries the first ${row.content.length.toLocaleString()} of ${row.content_size.toLocaleString()} characters. Fetching the rest costs one signature.`
      )
    );
  }

  if ((row.tags || []).length) {
    const table = noteEl("table", "mt-4 w-full text-xs");
    const tbody = noteEl("tbody", "");
    for (const tag of row.tags) {
      const tr = noteEl("tr", "");
      tr.appendChild(noteEl("td", "mono w-12 align-top text-gray-500", tag[0]));
      tr.appendChild(noteEl("td", "mono break-all text-gray-300", tag.slice(1).join(" ")));
      tbody.appendChild(tr);
    }
    table.appendChild(tbody);
    body.appendChild(noteEl("p", "mt-4 text-xs uppercase tracking-wide text-gray-500", "Tags"));
    body.appendChild(table);
    if (row.tags_truncated) {
      body.appendChild(
        noteEl("p", "mt-1 text-xs text-amber-300", `showing ${row.tags.length} of ${row.tag_count} tags`)
      );
    }
  }

  document.getElementById("note-dialog-json").textContent = JSON.stringify(row, null, 2);
  document.getElementById("note-delete").textContent = `Delete from ${(RELAYS.find((r) => r.key === notes.relay) || {}).label || notes.relay}`;

  if (typeof dialog.showModal === "function") dialog.showModal();
  else dialog.setAttribute("open", "");
  document.getElementById("note-dialog-close").focus();
}

function notesStep(delta) {
  if (notes.openIndex < 0) return;
  const next = notes.openIndex + delta;
  if (next < 0 || next >= notes.view.length) return;
  notesOpenDetail(notes.view[next].id);
}

//
// wiring
//

function notesWire() {
  definePanel("notes", {
    endpoint: notesEndpoint,
    load: async (endpoint) => {
      notes.query = notesBuildQuery();
      await notesFetch(endpoint, { append: false });
    },
  });

  const on = (id, event, handler) => {
    const node = document.getElementById(id);
    if (node) node.addEventListener(event, handler);
  };

  let debounce;
  on("notes-search", "input", (event) => {
    clearTimeout(debounce);
    const value = event.target.value;
    debounce = setTimeout(() => {
      notes.search = value.trim();
      notesApplyFilters();
    }, 120);
  });
  // Enter commits the local filter and moves to the relay search, so the
  // deliberate two step is what spends a signature and a single Enter never does
  on("notes-search", "keydown", (event) => {
    if (event.key !== "Enter") return;
    event.preventDefault();
    document.getElementById("notes-q-search").focus();
  });

  on("notes-sort", "change", (event) => {
    notes.sort = event.target.value;
    notesApplyFilters();
  });

  const density = document.getElementById("notes-density");
  if (density) {
    density.addEventListener("click", (event) => {
      const button = event.target.closest("[data-density]");
      if (!button) return;
      notes.density = button.dataset.density;
      for (const other of density.querySelectorAll("[data-density]")) {
        other.setAttribute("aria-pressed", other === button ? "true" : "false");
      }
      notesRender();
    });
  }

  on("notes-select-mode", "click", () => notesSetSelectMode(!notes.selectMode));
  on("notes-selection-clear", "click", () => notesSetSelectMode(false));
  on("notes-selection-all", "click", () => {
    for (const row of notes.view) notes.selection.add(row.id);
    notesRender();
  });
  on("notes-selection-delete", "click", () => notesDelete([...notes.selection]));

  on("notes-remote-media", "change", (event) => {
    notes.remoteMedia = event.target.checked;
    notesRender();
  });

  on("notes-show-more", "click", () => {
    notes.page++;
    notesRender();
  });
  on("notes-load-more", "click", async () => {
    const panel = panels.notes;
    if (panel) setStatus(panel.node, "info", "loading…");
    try {
      await notesFetch(selected, { append: true });
      if (panel) setStatus(panel.node, "info", "");
    } catch (error) {
      if (panel) setStatus(panel.node, "error", describeError(error));
    }
  });

  on("notes-query", "click", () => refreshPanel("notes", true));
  on("notes-query-clear", "click", () => {
    for (const id of ["notes-q-kind", "notes-q-author", "notes-q-since", "notes-q-until", "notes-q-search"]) {
      const node = document.getElementById(id);
      if (node) node.value = "";
    }
  });

  on("note-dialog-close", "click", () => document.getElementById("note-dialog").close());
  on("note-prev", "click", () => notesStep(-1));
  on("note-next", "click", () => notesStep(1));
  on("note-copy-id", "click", () => {
    const row = notes.view[notes.openIndex];
    if (row) navigator.clipboard.writeText(row.id);
  });
  on("note-copy-npub", "click", () => {
    const row = notes.view[notes.openIndex];
    if (row) navigator.clipboard.writeText(hexToNpub(row.pubkey));
  });
  on("note-copy-json", "click", () => {
    const row = notes.view[notes.openIndex];
    if (row) navigator.clipboard.writeText(JSON.stringify(row, null, 2));
  });
  on("note-delete", "click", async () => {
    const row = notes.view[notes.openIndex];
    if (!row) return;
    document.getElementById("note-dialog").close();
    await notesDelete([row.id]);
  });

  // focus goes back to the card the drawer was opened from, not the top of a
  // feed of several hundred
  const dialog = document.getElementById("note-dialog");
  if (dialog) {
    dialog.addEventListener("close", () => {
      const row = notes.view[notes.openIndex];
      if (!row) return;
      const card = document.querySelector(`.note-card[data-id="${row.id}"]`);
      if (card) card.focus();
    });
  }

  const feed = document.getElementById("notes-feed");
  if (feed) feed.addEventListener("keydown", notesOnKeydown);
}

function notesOnKeydown(event) {
  const card = event.target.closest(".note-card");
  if (!card) return;
  const index = Number(card.dataset.index);
  const cards = [...document.querySelectorAll(".note-card")];
  let next = -1;
  if (event.key === "ArrowDown") next = index + 1;
  else if (event.key === "ArrowUp") next = index - 1;
  else if (event.key === "PageDown") next = index + 8;
  else if (event.key === "PageUp") next = index - 8;
  else if (event.key === "Home") next = 0;
  else if (event.key === "End") next = cards.length - 1;
  else if (event.key === "Enter" || event.key === "o") {
    event.preventDefault();
    notesOpenDetail(card.dataset.id);
    return;
  } else if (event.key === " " || event.key === "x") {
    event.preventDefault();
    notesToggle(card.dataset.id);
    return;
  } else return;

  event.preventDefault();
  const target = cards[Math.max(0, Math.min(cards.length - 1, next))];
  if (target) target.focus();
}
