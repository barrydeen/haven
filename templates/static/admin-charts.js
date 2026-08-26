"use strict";

//
// a small SVG chart kit
//
// Hand rolled because the admin page's CSP allows scripts from 'self' and the
// Tailwind CDN and nowhere else, so there is no charting library to load and
// self-hosting one would be a large new dependency for a handful of charts.
//
// Everything is built with createElementNS and textContent — this page has a
// standing rule against innerHTML, and chart labels are the worst possible place
// to break it: series names here come from event content and profile metadata,
// which is to say from strangers.
//
// Every value inside an <svg> is a presentation attribute rather than a utility
// class, so a chart draws correctly even when the Tailwind CDN is unreachable.
// Only the card around it needs CSS, and the fallback block in admin.html has it.
//
// Bindings are prefixed svg/chart/scale/stat because these are classic scripts
// sharing one global scope: redeclaring a const another file owns throws, and the
// losing file does not execute at all.
//

const SVGNS = "http://www.w3.org/2000/svg";

// The chart palette. One fixed colour per relay, assigned by key and never by
// rank, so filtering one out of a chart never repaints the others.
//
// Validated rather than chosen: run through the dataviz validator in --pairs all
// mode against both the page surface (#111827) and the card surface (#1f2937),
// where it passes the lightness band, the chroma floor, colour-vision separation
// (worst pair ΔE 8.5 under deuteranopia), the normal-vision floor (17.4) and 3:1
// contrast on both. The media browser's own kind colours are NOT reused here:
// they fail all of that as a chart palette — six of them sit above the dark
// lightness band and two are ΔE 0.3 apart under deuteranopia.
const CHART_RELAY_COLOURS = {
  outbox: "#8b5cf6",
  private: "#0891b2",
  chat: "#16a34a",
  inbox: "#e04c9a",
};
const CHART_DEFAULT = "#8b5cf6";
const CHART_MUTED = "#6b7280";

// Status colours are reserved: they mean good and bad, and are never reused as
// "series 3". They always ship with a word or a glyph beside them, never colour
// alone.
const CHART_GOOD = "#0ca30c";
const CHART_BAD = "#d03b3b";

// chrome, one step off the surface and recessive
const CHART_GRID = "#1f2937";
const CHART_AXIS = "#374151";
const CHART_TICK = "#6b7280";
const CHART_INK = "#f9fafb";
const CHART_INK_DIM = "#9ca3af";
const CHART_SURFACE = "#111827";

function svgEl(tag, attrs) {
  const node = document.createElementNS(SVGNS, tag);
  for (const [key, value] of Object.entries(attrs || {})) {
    if (value === null || value === undefined) continue;
    node.setAttribute(key, String(value));
  }
  return node;
}

// svgText always goes through textContent. Series and category names reaching
// this function came off the wire.
function svgText(x, y, text, attrs) {
  const node = svgEl("text", Object.assign({ x, y, fill: CHART_TICK, "font-size": 11 }, attrs));
  node.textContent = String(text);
  return node;
}

// svgTitle is the zero-CSS tooltip. It needs no JavaScript, survives the CDN
// being down, and is read out by screen readers, so every mark gets one whether
// or not a richer hover layer is mounted.
function svgTitle(node, text) {
  const title = document.createElementNS(SVGNS, "title");
  title.textContent = String(text);
  node.appendChild(title);
  return node;
}

//
// scales and ticks
//

function scaleLinear(domain, range) {
  const [d0, d1] = domain;
  const [r0, r1] = range;
  const span = d1 - d0 || 1;
  const fn = (v) => r0 + ((v - d0) / span) * (r1 - r0);
  fn.domain = domain;
  fn.range = range;
  return fn;
}

function scaleBand(count, range, padding) {
  const [r0, r1] = range;
  const step = count > 0 ? (r1 - r0) / count : 0;
  const inner = Math.max(1, step * (1 - (padding === undefined ? 0.2 : padding)));
  const fn = (i) => r0 + i * step + (step - inner) / 2;
  fn.bandwidth = () => inner;
  fn.step = () => step;
  return fn;
}

// niceTicks rounds to numbers a person would choose: 0, 1,000, 2,000 rather than
// 0, 1,143, 2,286.
function niceTicks(max, count) {
  if (!(max > 0)) return [0, 1];
  const rough = max / (count || 4);
  const magnitude = Math.pow(10, Math.floor(Math.log10(rough)));
  const step = [1, 2, 2.5, 5, 10].map((m) => m * magnitude).find((s) => s >= rough) || magnitude * 10;
  const ticks = [];
  // the loop runs until the last tick is at or above max, not until it passes
  // max: stopping early would leave the top of the scale below the tallest bar,
  // and the bar would be drawn above the plot area and clipped
  for (let v = 0; ; v += step) {
    ticks.push(v);
    if (v >= max) break;
    if (ticks.length > 40) break; // a guard, not a limit anyone should reach
  }
  return ticks;
}

function compactNumber(n) {
  const value = Number(n) || 0;
  try {
    return new Intl.NumberFormat(undefined, { notation: "compact", maximumFractionDigits: 1 }).format(value);
  } catch (e) {
    return String(Math.round(value));
  }
}

// chartSizeClass picks one of three fixed logical widths from a media query
// rather than from an element's measured width.
//
// That is what keeps a chart from re-rendering on every pixel of a window drag:
// the viewBox scales the drawing to whatever the card is, and a real re-render
// only happens when a breakpoint is crossed. No ResizeObserver, no debounce.
function chartSizeClass() {
  try {
    if (window.matchMedia("(min-width: 64rem)").matches) return 720;
    if (window.matchMedia("(min-width: 40rem)").matches) return 560;
  } catch (e) {
    /* no matchMedia: the middle size is a safe guess */
  }
  return 360;
}

//
// the frame
//

// chartFrame builds the card, the svg and the layers, and owns the sizing
// contract.
//
// The svg's height includes the x-axis band, so a card can never end up with a
// nested scrollbar around a clipped axis — which is what happens when a fixed
// container height is chosen for the plot alone.
function chartFrame(host, spec) {
  const width = chartSizeClass();
  const pad = Object.assign({ top: 12, right: 12, bottom: 26, left: 44 }, spec.pad);
  const plotHeight = spec.height || 160;
  const height = pad.top + plotHeight + pad.bottom;

  const card = el("figure", "chart-card m-0");
  if (spec.span) card.style.flex = `1 1 ${spec.span}`;

  const head = el("figcaption", "mb-2");
  head.appendChild(el("h4", "text-sm font-semibold text-gray-200", spec.title));
  if (spec.subtitle) head.appendChild(el("p", "text-xs text-gray-500", spec.subtitle));
  card.appendChild(head);

  const svg = svgEl("svg", {
    class: "chart-svg",
    viewBox: `0 0 ${width} ${height}`,
    preserveAspectRatio: "xMidYMid meet",
    // the accessible representation is the table twin below, not ARIA on svg
    // internals, which is unreliable across screen readers
    "aria-hidden": "true",
    role: "img",
  });

  const layers = {
    grid: svgEl("g", {}),
    marks: svgEl("g", {}),
    labels: svgEl("g", {}),
  };
  svg.appendChild(layers.grid);
  svg.appendChild(layers.marks);
  svg.appendChild(layers.labels);

  const plotWrap = el("div", "chart-plot");
  plotWrap.tabIndex = 0;
  plotWrap.appendChild(svg);
  card.appendChild(plotWrap);

  const legendBox = el("div", "chart-legend mt-2");
  card.appendChild(legendBox);

  const foot = el("div", "mt-2 flex items-center gap-2");
  card.appendChild(foot);

  const tableWrap = el("div", "mt-2 overflow-x-auto");
  tableWrap.hidden = true;
  card.appendChild(tableWrap);

  const frame = {
    card,
    svg,
    layers,
    legendBox,
    foot,
    tableWrap,
    width,
    height,
    pad,
    plot: {
      x0: pad.left,
      x1: width - pad.right,
      y0: pad.top,
      y1: pad.top + plotHeight,
      w: width - pad.left - pad.right,
      h: plotHeight,
    },
  };

  if (spec.note) card.appendChild(el("p", "mt-2 text-xs text-gray-500", spec.note));
  if (host) host.appendChild(card);
  return frame;
}

// chartTableTwin is the accessibility twin, and the answer to the contrast
// warning the validator raises on any low-contrast series: every number a chart
// shows stays reachable without hovering, and without colour.
function chartTableTwin(frame, columns, rows) {
  const toggle = button("Table", "rounded border border-gray-600 px-2 py-0.5 text-xs", () => {
    frame.tableWrap.hidden = !frame.tableWrap.hidden;
    toggle.textContent = frame.tableWrap.hidden ? "Table" : "Chart";
  });
  frame.foot.appendChild(toggle);

  const table = el("table", "chart-table w-full text-xs");
  const thead = el("thead", "");
  const hrow = el("tr", "");
  for (const column of columns) hrow.appendChild(el("th", "text-left text-gray-500", column));
  thead.appendChild(hrow);
  table.appendChild(thead);
  const tbody = el("tbody", "");
  for (const row of rows) {
    const tr = el("tr", "");
    for (const cell of row) tr.appendChild(el("td", "text-gray-300", cell));
    tbody.appendChild(tr);
  }
  table.appendChild(tbody);
  clear(frame.tableWrap);
  frame.tableWrap.appendChild(table);
}

// chartLegend is always present for two or more series, because colour must never
// be the only channel carrying identity. A single series gets none: there is one
// colour, and the title already says what is plotted.
function chartLegend(frame, entries, shape) {
  clear(frame.legendBox);
  if (entries.length < 2) return;
  for (const entry of entries) {
    const row = el("span", "inline-flex items-center gap-1.5");
    const swatch = svgEl("svg", { width: 12, height: 12, "aria-hidden": "true" });
    if (shape === "line") {
      swatch.appendChild(svgEl("rect", { x: 0, y: 5, width: 12, height: 2, rx: 1, fill: entry.colour }));
    } else {
      swatch.appendChild(svgEl("rect", { x: 1, y: 2, width: 10, height: 8, rx: 2, fill: entry.colour }));
    }
    row.appendChild(swatch);
    // the label wears a text token, never the series colour
    row.appendChild(el("span", "text-gray-300", entry.label));
    if (entry.value !== undefined) row.appendChild(el("span", "text-gray-500", entry.value));
    frame.legendBox.appendChild(row);
  }
}

function chartEmpty(frame, message, detail) {
  clear(frame.layers.grid);
  clear(frame.layers.marks);
  clear(frame.layers.labels);
  clear(frame.legendBox);
  // an empty chart keeps the card and the title — so the grid does not reflow
  // when data arrives — but not the full plot height, which would otherwise
  // leave a card that is mostly blank and reads as broken rather than as empty
  const short = 64;
  frame.svg.setAttribute("viewBox", `0 0 ${frame.width} ${short}`);
  frame.plot.y0 = 0;
  frame.plot.h = short;
  const mid = short / 2 + 4;
  frame.layers.labels.appendChild(
    svgText(frame.width / 2, mid, message, { fill: CHART_INK_DIM, "text-anchor": "middle", "font-size": 12 })
  );
  if (detail) {
    frame.layers.labels.appendChild(
      svgText(frame.width / 2, mid + 16, detail, { fill: CHART_TICK, "text-anchor": "middle", "font-size": 10 })
    );
  }
}

//
// axes
//

function chartAxisY(frame, scale, ticks) {
  const g = frame.layers.grid;
  for (const value of ticks) {
    const y = Math.round(scale(value)) + 0.5;
    // solid hairlines, never dashed: dashing reads as "projection" or
    // "threshold" when it is only a grid
    g.appendChild(
      svgEl("line", { x1: frame.plot.x0, x2: frame.plot.x1, y1: y, y2: y, stroke: CHART_GRID, "stroke-width": 1 })
    );
    g.appendChild(
      svgText(frame.plot.x0 - 6, y + 4, compactNumber(value), { "text-anchor": "end", "font-variant-numeric": "tabular-nums" })
    );
  }
  g.appendChild(
    svgEl("line", {
      x1: frame.plot.x0, x2: frame.plot.x1, y1: frame.plot.y1 + 0.5, y2: frame.plot.y1 + 0.5,
      stroke: CHART_AXIS, "stroke-width": 1,
    })
  );
}

function chartAxisX(frame, labels) {
  const g = frame.layers.labels;
  for (const { x, text } of labels) {
    g.appendChild(svgText(x, frame.plot.y1 + 16, text, { "text-anchor": "middle" }));
  }
}

//
// marks
//

// chartStackedColumns draws one column per bucket, segmented by series.
//
// Segments are separated by a 2px gap in the surface colour rather than by a
// stroke around each one: a border is data-weight ink that is not data. Columns
// are capped at 24px so a sparse chart does not turn into slabs.
function chartStackedColumns(frame, opts) {
  const { buckets, series, colours, labels } = opts;
  const totals = buckets.map((_, i) => series.reduce((sum, s) => sum + (s.values[i] || 0), 0));
  const max = Math.max(1, ...totals);
  const ticks = niceTicks(max, 4);
  const y = scaleLinear([0, ticks[ticks.length - 1]], [frame.plot.y1, frame.plot.y0]);
  const band = scaleBand(buckets.length, [frame.plot.x0, frame.plot.x1], 0.25);
  const width = Math.min(24, band.bandwidth());

  chartAxisY(frame, y, ticks);

  buckets.forEach((bucket, i) => {
    let base = frame.plot.y1;
    const x = band(i) + (band.bandwidth() - width) / 2;
    series.forEach((s, si) => {
      const value = s.values[i] || 0;
      if (value <= 0) return;
      const h = Math.max(1, frame.plot.y1 - y(value));
      const top = base - h;
      const rect = svgEl("rect", {
        x, y: top, width, height: h,
        fill: colours[si],
        // rounded at the data end, square at the baseline
        rx: si === series.length - 1 ? Math.min(4, width / 2) : 0,
      });
      svgTitle(rect, `${labels[i]} · ${s.label}: ${Number(value).toLocaleString()}`);
      frame.layers.marks.appendChild(rect);
      base = top - 2; // the 2px surface gap between stacked segments
    });
  });

  return { y, band, width };
}

// chartLine draws one series over time, with an optional area wash.
function chartLine(frame, opts) {
  const { values, colour, labels } = opts;
  const real = values.filter((v) => v !== null && v !== undefined);
  const max = Math.max(1, ...real);
  const ticks = niceTicks(max, 3);
  const y = scaleLinear([0, ticks[ticks.length - 1]], [frame.plot.y1, frame.plot.y0]);
  const x = scaleLinear([0, Math.max(1, values.length - 1)], [frame.plot.x0, frame.plot.x1]);

  chartAxisY(frame, y, ticks);

  // a null is a gap, not a zero: it means there is no record for that bucket,
  // which is what an hour before analytics was switched on, or an hour the relay
  // was down, actually is. Drawing it as zero would put a floor under a blackout.
  const runs = [];
  let run = [];
  values.forEach((value, i) => {
    if (value === null || value === undefined) {
      if (run.length) runs.push(run);
      run = [];
      return;
    }
    run.push([x(i), y(value)]);
  });
  if (run.length) runs.push(run);

  for (const points of runs) {
    if (points.length === 1) {
      // a single point makes an invisible line, so draw the dot instead
      const dot = svgEl("circle", { cx: points[0][0], cy: points[0][1], r: 4, fill: colour, stroke: CHART_SURFACE, "stroke-width": 2 });
      frame.layers.marks.appendChild(dot);
      continue;
    }
    const d = points.map((p, i) => `${i ? "L" : "M"}${p[0].toFixed(1)} ${p[1].toFixed(1)}`).join(" ");
    // the area is a wash at ~10% opacity, never a saturated block
    const area = svgEl("path", {
      d: `${d} L${points[points.length - 1][0].toFixed(1)} ${frame.plot.y1} L${points[0][0].toFixed(1)} ${frame.plot.y1} Z`,
      fill: colour, "fill-opacity": 0.12,
    });
    frame.layers.marks.appendChild(area);
    frame.layers.marks.appendChild(
      svgEl("path", { d, fill: "none", stroke: colour, "stroke-width": 2, "stroke-linejoin": "round", "stroke-linecap": "round" })
    );
  }

  // one end dot with a surface ring, and one direct label — selectively, never a
  // number on every point
  const lastIndex = values.length - 1;
  const last = values[lastIndex];
  if (last !== null && last !== undefined) {
    frame.layers.marks.appendChild(
      svgEl("circle", { cx: x(lastIndex), cy: y(last), r: 4, fill: colour, stroke: CHART_SURFACE, "stroke-width": 2 })
    );
    frame.layers.labels.appendChild(
      svgText(x(lastIndex) - 6, y(last) - 8, Number(last).toLocaleString(), { fill: CHART_INK, "text-anchor": "end", "font-size": 11 })
    );
  }

  // an invisible hit rect per bucket, so the native tooltip works anywhere in the
  // column rather than only on a two pixel line
  values.forEach((value, i) => {
    const step = frame.plot.w / Math.max(1, values.length - 1);
    const hit = svgEl("rect", {
      x: x(i) - step / 2, y: frame.plot.y0, width: step, height: frame.plot.h, fill: "transparent",
    });
    svgTitle(hit, `${labels[i]}: ${value === null || value === undefined ? "no record" : Number(value).toLocaleString()}`);
    frame.layers.marks.appendChild(hit);
  });

  return { x, y };
}

// chartBars draws ranked horizontal bars in ONE hue.
//
// One hue on purpose: these are nominal categories, and a value ramp would
// re-encode bar length as colour, spending the identity channel on information
// the bar already carries.
function chartBars(frame, rows, colour) {
  const max = Math.max(1, ...rows.map((r) => r.value));
  const pitch = frame.plot.h / Math.max(1, rows.length);
  const height = Math.min(14, Math.max(6, pitch - 6));
  const labelWidth = 96;
  const x0 = frame.plot.x0 + labelWidth;
  const x = scaleLinear([0, max], [x0, frame.plot.x1 - 44]);

  rows.forEach((row, i) => {
    const y = frame.plot.y0 + i * pitch + (pitch - height) / 2;
    frame.layers.labels.appendChild(
      svgText(frame.plot.x0 - 8, y + height - 2, row.label, { "text-anchor": "start", x: frame.plot.x0 - 40, fill: CHART_INK_DIM })
    );
    const w = Math.max(1, x(row.value) - x0);
    const rect = svgEl("rect", {
      x: x0, y, width: w, height,
      fill: row.colour || colour || CHART_DEFAULT,
      rx: Math.min(4, height / 2),
    });
    svgTitle(rect, `${row.label}: ${Number(row.value).toLocaleString()}`);
    frame.layers.marks.appendChild(rect);
    // the value at the tip, outside the bar, so it can never be clipped by a
    // bar too short to hold it
    frame.layers.labels.appendChild(
      svgText(x0 + w + 6, y + height - 2, compactNumber(row.value), { fill: CHART_INK, "font-variant-numeric": "tabular-nums" })
    );
  });
}

// chartComposition is the slim part-to-whole strip, the same encoding the media
// browser already uses for storage, so the two views agree on what it means.
function chartComposition(host, parts, total) {
  const bar = el("div", "composition-bar rounded");
  for (const part of parts) {
    if (!part.value) continue;
    const segment = el("span", "");
    // through CSSOM, not a style attribute: this page builds no markup strings
    segment.style.width = `${((part.value / (total || 1)) * 100).toFixed(2)}%`;
    segment.style.background = part.colour;
    segment.title = `${part.label}: ${formatBytes(part.value)}`;
    bar.appendChild(segment);
  }
  host.appendChild(bar);

  const legend = el("div", "mt-2 flex flex-wrap gap-x-3 gap-y-1 text-xs");
  for (const part of parts) {
    if (!part.value) continue;
    const row = el("span", "inline-flex items-center gap-1.5");
    const dot = el("span", "inline-block h-2 w-2 rounded-full");
    dot.style.background = part.colour;
    row.appendChild(dot);
    row.appendChild(el("span", "text-gray-300", part.label));
    row.appendChild(el("span", "text-gray-500", formatBytes(part.value)));
    legend.appendChild(row);
  }
  host.appendChild(legend);
}

// statTile is a number that is the whole story, which is what a KPI is. A
// one-bar bar chart would be the anti-pattern here.
function statTile(host, spec) {
  const tile = el("div", "stat-tile");
  tile.appendChild(el("p", "text-xs uppercase tracking-wide text-gray-500", spec.label));
  // proportional figures, not tabular: equal-width digits make a large standalone
  // number look loose
  tile.appendChild(el("p", `stat-value ${spec.hero ? "stat-hero" : ""} text-gray-100`, spec.value));
  if (spec.sub) {
    const tone =
      spec.tone === "good" ? "text-emerald-400" : spec.tone === "bad" ? "text-red-400" : "text-gray-500";
    tile.appendChild(el("p", `stat-sub text-xs ${tone}`, spec.sub));
  }
  host.appendChild(tile);
  return tile;
}
