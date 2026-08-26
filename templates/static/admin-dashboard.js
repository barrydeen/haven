"use strict";

//
// the dashboard
//
// One signed call brings back everything: live counters, all three time ranges,
// and the aggregates. Switching between 24h, 7d and 30d after that is client side
// and costs nothing, which is the whole reason the server sends all three at once.
//
// Bindings are prefixed dash for the same reason every other file here prefixes
// its own: these are classic scripts sharing one global scope.
//

const DASH_RANGES = ["24h", "7d", "30d"];

const dash = {
  active: false,
  data: null,
  range: "24h",
};

function dashEndpoint() {
  return GLOBAL;
}

function dashSetActive(on) {
  dash.active = on;
}

async function dashLoad(endpoint) {
  dash.data = await nip86(endpoint, "dashboard", []);
  dashRender();
}

//
// helpers over the payload
//

function dashRange() {
  const ranges = (dash.data && dash.data.ranges) || {};
  return ranges[dash.range] || { buckets: [], series: {}, bucket_seconds: 3600 };
}

function dashSeries(name) {
  const range = dashRange();
  return (range.series && range.series[name]) || range.buckets.map(() => null);
}

// dashSum adds a series up, treating a null as "no record" rather than as zero —
// the sum of a window with gaps is still the sum of what was recorded.
function dashSum(values) {
  let total = 0;
  for (const value of values) if (value !== null && value !== undefined) total += value;
  return total;
}

function dashBucketLabels() {
  const range = dashRange();
  const daily = range.bucket_seconds >= 86400;
  return range.buckets.map((at) => {
    const d = new Date(at * 1000);
    if (daily) return d.toLocaleDateString(undefined, { day: "numeric", month: "short" });
    if (dash.range === "7d") return d.toLocaleDateString(undefined, { weekday: "short", hour: "numeric" });
    return d.toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit" });
  });
}

// dashPerRelay rebuilds one counter as a series per relay.
//
// The payload's ranges are summed across relays, because every chart that shows a
// total wants them that way. The stacked chart wants the split, and the only
// honest source for it is each relay's own bucket history — which the payload
// does not carry per relay to keep it small. So the stack is drawn from the
// summed total with the per-relay share taken from the stored corpus, and the
// chart says so rather than implying a per-relay history it does not have.
function dashRelayShare() {
  const live = (dash.data && dash.data.live) || [];
  const shares = [];
  let total = 0;
  for (const row of live) {
    const events = row.corpus ? row.corpus.events : 0;
    total += events;
    shares.push({ relay: row.relay, label: row.label, events });
  }
  return { shares, total };
}

//
// rendering
//

function dashRender() {
  if (!dash.data) return;
  dashRenderFreshness();
  dashRenderCoverage();
  dashRenderKPIs();
  dashRenderCharts();
  for (const button of document.querySelectorAll("#dash-range [data-range]")) {
    button.setAttribute("aria-pressed", button.dataset.range === dash.range ? "true" : "false");
  }
}

function dashRenderFreshness() {
  const node = document.getElementById("dash-freshness");
  if (!node) return;
  const info = dash.data.analytics || {};
  const parts = [];
  if (dash.data.counted_at) parts.push(`counted ${formatRelative(dash.data.counted_at)}`);
  if (info.aggregates_at) parts.push(`scanned ${formatRelative(info.aggregates_at)}`);
  else parts.push("first scan still running");
  node.textContent = parts.join(" · ");
}

function dashRenderCoverage() {
  const node = document.getElementById("dash-coverage");
  if (!node) return;
  const info = dash.data.analytics || {};
  const messages = [];
  if (info.note) messages.push(info.note);
  if (dash.data.warning) messages.push(dash.data.warning);
  // history starts where it starts; drawing a flat line back to the edge of the
  // window would say "quiet" where the truth is "not recorded"
  if (info.first_bucket) {
    const days = Math.floor((Date.now() / 1000 - info.first_bucket) / 86400);
    const window = dash.range === "24h" ? 1 : dash.range === "7d" ? 7 : 30;
    if (days < window) {
      messages.push(
        `Traffic has only been recorded for ${days === 0 ? "less than a day" : days + (days === 1 ? " day" : " days")}, so most of this window has no data rather than no traffic.`
      );
    }
  }
  node.hidden = messages.length === 0;
  node.textContent = messages.join(" ");
}

function dashRenderKPIs() {
  const host = document.getElementById("dash-kpis");
  if (!host) return;
  clear(host);

  const events = dash.data.events || {};
  const relays = dash.data.relays || [];
  const storedTotal = relays.reduce((n, name) => n + (events[name] || 0), 0);

  const live = (dash.data.live || []).reduce((n, row) => n + (row.live_connections || 0), 0);
  const attempts = dashSum(dashSeries("event_attempts"));
  const passed = dashSum(dashSeries("event_passed"));
  const rejected = Math.max(0, attempts - passed);
  const rate = attempts ? ((rejected / attempts) * 100).toFixed(1) : "0.0";

  const corpusBytes = (dash.data.live || []).reduce((n, row) => n + (row.corpus ? row.corpus.bytes : 0), 0);
  const blobBytes = dash.data.blobs ? dash.data.blobs.disk_bytes : 0;

  statTile(host, {
    label: "Events stored",
    value: storedTotal.toLocaleString(),
    sub: `across ${relays.length} relays`,
    hero: true,
  });
  statTile(host, {
    label: "Accepted",
    value: compactNumber(passed),
    sub: attempts ? `${rate}% rejected` : "no writes in this window",
    tone: attempts && rejected / attempts > 0.5 ? "bad" : "good",
  });
  statTile(host, {
    label: "Connected now",
    value: live.toLocaleString(),
    sub: `${compactNumber(dashSum(dashSeries("conn_opened")))} opened in this window`,
  });
  statTile(host, {
    label: "Stored bytes",
    value: formatBytes(corpusBytes + blobBytes),
    // growth is not a fault, so this is never painted red
    sub: `${formatBytes(corpusBytes)} events · ${formatBytes(blobBytes)} media`,
  });
  statTile(host, {
    label: "Uptime",
    value: dashUptime(dash.data.uptime_seconds || 0),
    sub: dash.data.version || "",
  });
}

function dashUptime(seconds) {
  const days = Math.floor(seconds / 86400);
  const hours = Math.floor((seconds % 86400) / 3600);
  if (days) return `${days}d ${hours}h`;
  const minutes = Math.floor((seconds % 3600) / 60);
  return hours ? `${hours}h ${minutes}m` : `${minutes}m`;
}

function dashRenderCharts() {
  const host = document.getElementById("dash-charts");
  if (!host) return;
  clear(host);

  const labels = dashBucketLabels();
  const stored = dashSeries("event_stored");
  const attempts = dashSeries("event_attempts");
  const passed = dashSeries("event_passed");
  const reqs = dashSeries("req_filters");
  const conns = dashSeries("conn_opened");

  dashChartStored(host, labels, stored);
  dashChartAcceptedRejected(host, labels, attempts, passed);
  dashChartLine(host, "Requests per bucket", "REQ filters the relay was asked to answer", labels, reqs);
  dashChartLine(host, "Connections opened", "websocket connections accepted", labels, conns);
  dashChartKinds(host);
  dashChartAuthors(host);
  dashChartStorage(host);
}

function dashChartStored(host, labels, stored) {
  const frame = chartFrame(host, {
    title: "Events stored",
    subtitle: `written to the databases · ${dash.range}`,
    height: 170,
    span: "32rem",
  });
  if (!dashSum(stored)) {
    chartEmpty(frame, "No events recorded in this window.", "Counters start when the relay does.");
    return;
  }
  const { shares, total } = dashRelayShare();
  // the stack is the recorded total split by each relay's share of what is
  // stored, and the subtitle says so — the payload carries no per relay history,
  // and inventing one would be worse than saying what this is
  const series = shares
    .filter((s) => s.events > 0)
    .map((s) => ({
      label: s.label,
      values: stored.map((v) => (v === null || v === undefined ? 0 : Math.round((v * s.events) / (total || 1)))),
    }));
  const colours = shares.filter((s) => s.events > 0).map((s) => CHART_RELAY_COLOURS[s.relay] || CHART_MUTED);

  if (!series.length) {
    // no per relay share yet, because the first pass over the databases has not
    // finished. The recorded total is still true, so it is drawn as one series
    // rather than left blank — and the note below says the split is missing.
    chartStackedColumns(frame, {
      buckets: labels,
      series: [{ label: "all relays", values: stored.map((v) => (v === null || v === undefined ? 0 : v)) }],
      colours: [CHART_DEFAULT],
      labels,
    });
    chartAxisX(frame, dashAxisLabels(frame, labels));
    frame.card.appendChild(
      el("p", "mt-2 text-xs text-gray-500", "The per-relay split appears once the first pass over the databases finishes.")
    );
    return;
  }

  chartStackedColumns(frame, { buckets: labels, series, colours, labels });
  chartAxisX(frame, dashAxisLabels(frame, labels));
  chartLegend(frame, series.map((s, i) => ({ label: s.label, colour: colours[i] })), "rect");
  chartTableTwin(
    frame,
    ["bucket", ...series.map((s) => s.label), "total"],
    labels.map((label, i) => [
      label,
      ...series.map((s) => s.values[i].toLocaleString()),
      stored[i] === null || stored[i] === undefined ? "no record" : stored[i].toLocaleString(),
    ])
  );
  frame.card.appendChild(
    el("p", "mt-2 text-xs text-gray-500", "Split by each relay's share of what it currently stores.")
  );
}

function dashChartAcceptedRejected(host, labels, attempts, passed) {
  const frame = chartFrame(host, {
    title: "Accepted and rejected",
    subtitle: `events offered to the relay · ${dash.range}`,
    height: 150,
    span: "26rem",
  });
  if (!dashSum(attempts)) {
    chartEmpty(frame, "Nothing was offered in this window.");
    return;
  }
  const accepted = passed.map((v) => (v === null || v === undefined ? 0 : v));
  // attempts and passed are incremented microseconds apart by different hooks, so
  // an event landing on an hour boundary can be an attempt in one bucket and a
  // pass in the next. Clamping at zero is more honest than shipping a negative.
  const rejected = attempts.map((v, i) => Math.max(0, (v === null || v === undefined ? 0 : v) - accepted[i]));

  chartStackedColumns(frame, {
    buckets: labels,
    series: [
      { label: "accepted", values: accepted },
      { label: "rejected", values: rejected },
    ],
    colours: [CHART_GOOD, CHART_BAD],
    labels,
  });
  chartAxisX(frame, dashAxisLabels(frame, labels));
  // the glyphs are the secondary channel: status is never colour alone
  chartLegend(
    frame,
    [
      { label: "✓ accepted", colour: CHART_GOOD, value: dashSum(accepted).toLocaleString() },
      { label: "✕ rejected", colour: CHART_BAD, value: dashSum(rejected).toLocaleString() },
    ],
    "rect"
  );
  chartTableTwin(
    frame,
    ["bucket", "accepted", "rejected"],
    labels.map((label, i) => [label, accepted[i].toLocaleString(), rejected[i].toLocaleString()])
  );
}

function dashChartLine(host, title, subtitle, labels, values) {
  const frame = chartFrame(host, { title, subtitle: `${subtitle} · ${dash.range}`, height: 130, span: "24rem" });
  if (!dashSum(values)) {
    chartEmpty(frame, "Nothing recorded in this window.");
    return;
  }
  chartLine(frame, { values, colour: CHART_DEFAULT, labels });
  chartAxisX(frame, dashAxisLabels(frame, labels));
  chartTableTwin(
    frame,
    ["bucket", "count"],
    labels.map((label, i) => [label, values[i] === null || values[i] === undefined ? "no record" : values[i].toLocaleString()])
  );
}

function dashChartKinds(host) {
  const totals = new Map();
  for (const row of dash.data.live || []) {
    const kinds = (row.corpus && row.corpus.kinds) || {};
    for (const [kind, n] of Object.entries(kinds)) {
      totals.set(Number(kind), (totals.get(Number(kind)) || 0) + n);
    }
  }
  const frame = chartFrame(host, {
    title: "Events by kind",
    subtitle: "everything currently stored, all relays",
    height: 170,
    span: "26rem",
  });
  if (!totals.size) {
    chartEmpty(frame, "The first pass over the databases has not finished.");
    return;
  }
  const ranked = [...totals.entries()].sort((a, b) => b[1] - a[1]);
  const top = ranked.slice(0, 8);
  const rest = ranked.slice(8).reduce((n, [, v]) => n + v, 0);
  // deliberately bars rather than a donut: relay kind counts are often close, and
  // a donut for close values cannot be read
  const rows = top.map(([kind, value]) => ({
    label: (typeof NOTE_KIND_NAMES !== "undefined" && NOTE_KIND_NAMES[kind]) || `kind ${kind}`,
    value,
  }));
  if (rest) rows.push({ label: `${ranked.length - 8} other kinds`, value: rest, colour: CHART_MUTED });

  chartBars(frame, rows, CHART_DEFAULT);
  chartTableTwin(frame, ["kind", "events"], rows.map((r) => [r.label, r.value.toLocaleString()]));
}

function dashChartAuthors(host) {
  const totals = new Map();
  for (const row of dash.data.live || []) {
    for (const author of (row.corpus && row.corpus.authors) || []) {
      const current = totals.get(author.pubkey) || { events: 0, name: author.name };
      current.events += author.events;
      totals.set(author.pubkey, current);
    }
  }
  const frame = chartFrame(host, {
    title: "Top authors",
    subtitle: "by events currently stored",
    height: 170,
    span: "26rem",
  });
  if (!totals.size) {
    chartEmpty(frame, "The first pass over the databases has not finished.");
    return;
  }
  const rows = [...totals.entries()]
    .sort((a, b) => b[1].events - a[1].events)
    .slice(0, 10)
    // names come from kind 0 metadata, which is a stranger's string: svgText puts
    // it in with textContent, and it is capped here
    .map(([pubkey, info]) => ({
      label: (info.name || shortNpub(pubkey)).slice(0, 22),
      value: info.events,
    }));

  chartBars(frame, rows, CHART_DEFAULT);
  chartTableTwin(frame, ["author", "events"], rows.map((r) => [r.label, r.value.toLocaleString()]));
}

function dashChartStorage(host) {
  const card = el("figure", "chart-card m-0");
  card.style.flex = "1 1 26rem";
  const head = el("figcaption", "mb-2");
  head.appendChild(el("h4", "text-sm font-semibold text-gray-200", "Storage"));
  head.appendChild(el("p", "text-xs text-gray-500", "summed size of the events and blobs held"));
  card.appendChild(head);

  const parts = [];
  for (const row of dash.data.live || []) {
    if (!row.corpus || !row.corpus.bytes) continue;
    parts.push({ label: row.label, value: row.corpus.bytes, colour: CHART_RELAY_COLOURS[row.relay] || CHART_MUTED });
  }
  if (dash.data.blobs && dash.data.blobs.disk_bytes) {
    // blossom is not a relay, so it does not take a relay hue
    parts.push({ label: "Blossom media", value: dash.data.blobs.disk_bytes, colour: CHART_MUTED });
  }
  const total = parts.reduce((n, p) => n + p.value, 0);

  if (!total) {
    card.appendChild(el("p", "py-6 text-center text-sm text-gray-400", "Nothing stored yet, or the first scan is still running."));
  } else {
    chartComposition(card, parts, total);
    card.appendChild(
      el(
        "p",
        "mt-2 text-xs text-gray-500",
        "Event bytes are the summed size of the events themselves, not the size of the database directory — badger preallocates sparse files, so that would read as gigabytes on an empty relay."
      )
    );
  }
  host.appendChild(card);
}

// dashAxisLabels thins the x labels to at most six, so they never collide.
function dashAxisLabels(frame, labels) {
  const out = [];
  if (!labels.length) return out;
  const every = Math.max(1, Math.ceil(labels.length / 6));
  const step = frame.plot.w / Math.max(1, labels.length - 1);
  labels.forEach((text, i) => {
    if (i % every !== 0 && i !== labels.length - 1) return;
    out.push({ x: frame.plot.x0 + i * step, text });
  });
  return out;
}

//
// wiring
//

function dashWire() {
  definePanel("dashboard", { endpoint: dashEndpoint, load: dashLoad });

  const range = document.getElementById("dash-range");
  if (range) {
    range.addEventListener("click", (event) => {
      const button = event.target.closest("[data-range]");
      if (!button || !DASH_RANGES.includes(button.dataset.range)) return;
      dash.range = button.dataset.range;
      // no fetch: every range came back with the first call, which is the whole
      // reason the server sends all three together
      dashRender();
    });
  }

  const reload = document.getElementById("dash-reload");
  if (reload) reload.addEventListener("click", () => refreshPanel("dashboard", true));

  // charts are laid out from a fixed logical width chosen by media query, so they
  // only need rebuilding when a breakpoint is actually crossed — not on every
  // pixel of a window drag
  try {
    for (const query of ["(min-width: 40rem)", "(min-width: 64rem)"]) {
      window.matchMedia(query).addEventListener("change", () => {
        if (dash.data) dashRender();
      });
    }
  } catch (e) {
    /* no matchMedia: the charts stay at whatever size they were built for */
  }
}
