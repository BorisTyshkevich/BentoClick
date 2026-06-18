import { describe, it, expect, beforeEach } from 'vitest';
import { PANELS, fmt } from '../../../runtime/v1/dash.js';
import { colorFor } from '../../../runtime/v1/charts.js';

function makeSpecStub(panels) {
  const handlers = {};
  return {
    panels,
    on(ev, fn) { (handlers[ev] = handlers[ev] || []).push(fn); },
    _emit(ev, payload) { (handlers[ev] || []).forEach((f) => f(payload)); },
  };
}

function ctx(specStub) { return { api: { fmt }, spec: specStub || null }; }
function makeState() { return { id: 'c', rows: [], update: () => {} }; }

describe('renderCombo', () => {
  beforeEach(() => { document.body.innerHTML = ''; });

  const basic = {
    type: 'combo',
    title: 'Combo',
    x_key: 'year',
    bars: { key: 'flights', color_by: 'who', label: 'Flights' },
    line: { key: 'margin', axis: 'right', label: 'Margin' },
  };

  it('renders title, bars and a line path', () => {
    const state = makeState();
    const el = PANELS.combo(basic, state, ctx());
    state.update([
      { year: 2000, flights: 100, margin: 10, who: 'WN' },
      { year: 2001, flights: 110, margin: 20, who: 'DL' },
      { year: 2002, flights: 90, margin: 5, who: 'WN' },
    ]);
    expect(el.querySelector('h2').textContent).toBe('Combo');
    expect(el.querySelectorAll('rect.chart-bar').length).toBe(3);
    expect(el.querySelectorAll('path.chart-line').length).toBe(1);
    expect(el.querySelectorAll('circle.chart-point').length).toBe(3);
  });

  it('shows empty_text on no data', () => {
    const state = makeState();
    const el = PANELS.combo({ ...basic, empty_text: 'nothing' }, state, ctx());
    state.update([]);
    expect(el.textContent).toContain('nothing');
  });

  it('colors bars via color_by stably across re-renders', () => {
    const state = makeState();
    const el = PANELS.combo(basic, state, ctx());
    state.update([
      { year: 2000, flights: 1, margin: 1, who: 'WN' },
      { year: 2001, flights: 1, margin: 1, who: 'WN' },
    ]);
    const fills1 = Array.from(el.querySelectorAll('rect.chart-bar')).map((r) => r.getAttribute('fill'));
    expect(fills1[0]).toBe(fills1[1]); // same airline → same color
    expect(fills1[0]).toBe(colorFor('WN'));
    // Re-render with the same data and confirm color stability.
    state.update([{ year: 2000, flights: 1, margin: 1, who: 'WN' }]);
    const fill2 = el.querySelector('rect.chart-bar').getAttribute('fill');
    expect(fill2).toBe(fills1[0]);
  });

  it('renders a dual axis when line.axis is right', () => {
    const state = makeState();
    const el = PANELS.combo(basic, state, ctx());
    state.update([
      { year: 2000, flights: 1000, margin: 0.05, who: 'WN' },
    ]);
    expect(el.querySelector('.chart-axis-right')).not.toBeNull();
  });

  it('with bars.color_by, legend enumerates each unique category', () => {
    const state = makeState();
    const el = PANELS.combo(basic, state, ctx());
    state.update([
      { year: 2000, flights: 1, margin: 1, who: 'WN' },
      { year: 2001, flights: 2, margin: 1, who: 'DL' },
      { year: 2002, flights: 1, margin: 1, who: 'WN' },
    ]);
    const legend = el.querySelector('.chart-legend');
    expect(legend).not.toBeNull();
    const items = legend.querySelectorAll('.item');
    // Two unique bar categories + the line series.
    expect(items.length).toBe(3);
    expect(items[0].textContent).toBe('WN');
    expect(items[1].textContent).toBe('DL');
    expect(items[2].textContent).toContain('Margin');
    // Line gets the 2px-strip swatch variant; bars get the square swatch.
    expect(items[0].querySelector('.sw').classList.contains('line')).toBe(false);
    expect(items[2].querySelector('.sw').classList.contains('line')).toBe(true);
  });

  it('without bars.color_by, legend collapses to a single bar-series swatch', () => {
    const state = makeState();
    const noColor = { ...basic, bars: { key: 'flights', label: 'Flights' } };
    const el = PANELS.combo(noColor, state, ctx());
    state.update([{ year: 2000, flights: 1, margin: 1 }]);
    const items = el.querySelectorAll('.chart-legend .item');
    expect(items.length).toBe(2);
    expect(items[0].textContent).toBe('Flights');
    expect(items[1].textContent).toContain('Margin');
  });

  it('falls back to the palette default when bars.color_by is absent', () => {
    const state = makeState();
    const noColor = { ...basic, bars: { key: 'flights', label: 'Flights' } };
    const el = PANELS.combo(noColor, state, ctx());
    state.update([
      { year: 2000, flights: 1, margin: 1, who: 'WN' },
      { year: 2001, flights: 1, margin: 1, who: 'DL' },
    ]);
    const fills = Array.from(el.querySelectorAll('rect.chart-bar')).map((r) => r.getAttribute('fill'));
    expect(new Set(fills).size).toBe(1); // all the same colour, not per-row
  });

  it('draws slim bars at the approved 60% band width (40% gap)', () => {
    const state = makeState();
    const el = PANELS.combo({ ...basic, bars: { key: 'flights', label: 'Flights' } }, state, ctx());
    // 24 bars (an hourly series) keeps bandwidth under the 32px cap, so the
    // band padding governs the width. The bar should fill 60% of the band —
    // matching the design's comboChart (`bw = (iw/n)*0.6`); it was 85% (a
    // 0.15 pad) before, which read as too-wide, near-touching bars.
    const rows = Array.from({ length: 24 }, (_, i) => ({ year: 2000 + i, flights: 100 + i, margin: i }));
    state.update(rows);
    const bars = Array.from(el.querySelectorAll('rect.chart-bar'));
    expect(bars.length).toBe(24);
    const center = (r) => parseFloat(r.getAttribute('x')) + parseFloat(r.getAttribute('width')) / 2;
    const width = parseFloat(bars[0].getAttribute('width'));
    const step = center(bars[1]) - center(bars[0]); // band step (adjacent bar centers)
    expect(width).toBeGreaterThan(0);
    expect(width).toBeLessThan(32); // cap not binding at this density
    expect(width / step).toBeCloseTo(0.6, 2); // 60% band → 40% gap
  });

  it('survives null series values without emitting NaN in the line path', () => {
    const state = makeState();
    const el = PANELS.combo(basic, state, ctx());
    state.update([
      { year: 2000, flights: 10,   margin: null, who: 'WN' },
      { year: 2001, flights: null, margin: 5,    who: 'DL' },
      { year: 2002, flights: 30,   margin: 8,    who: 'WN' },
    ]);
    const d = el.querySelector('path.chart-line').getAttribute('d');
    expect(d).not.toMatch(/NaN/);
  });

  it('hover handle.destroy() is callable + empty rows short-circuits the install', async () => {
    // Empty-rows path returns a no-op destroy() — exercise it directly.
    const helpers = await import('../../../runtime/v1/panels/chart-helpers.js');
    const h = helpers.installHoverCrosshair(
      { getBoundingClientRect: () => ({ left:0, top:0, width:0, height:0 }) },
      document.createElement('div'), document.createElement('div'),
      { xs: [], xScale: () => 0, ih: 0, iw: 0, series: [] },
    );
    expect(typeof h.destroy).toBe('function');
    expect(() => h.destroy()).not.toThrow();
  });

  it('hover crosshair + tooltip show on mousemove, hide on mouseleave', () => {
    const state = makeState();
    const el = PANELS.combo(basic, state, ctx());
    document.body.appendChild(el);
    state.update([
      { year: 2000, flights: 100, margin: 10, who: 'WN' },
      { year: 2001, flights: 110, margin: 20, who: 'DL' },
      { year: 2002, flights: 90,  margin: 5,  who: 'WN' },
    ]);
    // Overlay + hover line exist post-render.
    const overlay = el.querySelector('rect.chart-hover-overlay');
    const hoverLine = el.querySelector('line.chart-hover-line');
    const tip = el.querySelector('.chart-tooltip');
    expect(overlay).not.toBeNull();
    expect(hoverLine).not.toBeNull();
    expect(tip).not.toBeNull();
    expect(tip.classList.contains('on')).toBe(false);
    // Stub getBoundingClientRect on the svg so the fallback x conversion
    // resolves; happy-dom returns zeros without this.
    const svg = el.querySelector('svg.chart-svg');
    svg.getBoundingClientRect = () => ({ left: 0, top: 0, width: 880, height: 300 });
    overlay.dispatchEvent(new MouseEvent('mousemove', { clientX: 400, clientY: 100 }));
    expect(tip.classList.contains('on')).toBe(true);
    // Tooltip renders the x label and both series formatted values.
    expect(tip.querySelector('.tt-x')).not.toBeNull();
    expect(tip.querySelectorAll('.tt-row').length).toBe(2);
    overlay.dispatchEvent(new MouseEvent('mouseleave'));
    expect(tip.classList.contains('on')).toBe(false);
  });

  it('clicking a legend item fades it and hides matching SVG elements', () => {
    const state = makeState();
    const el = PANELS.combo(basic, state, ctx());
    state.update([
      { year: 2000, flights: 1, margin: 1, who: 'WN' },
      { year: 2001, flights: 2, margin: 1, who: 'DL' },
      { year: 2002, flights: 3, margin: 1, who: 'WN' },
    ]);
    const wnItem = el.querySelector('.chart-legend .item[data-legend-key="bar:WN"]');
    expect(wnItem).not.toBeNull();
    // 2 WN bars + 1 DL bar present initially.
    expect(el.querySelectorAll('[data-legend-key="bar:WN"]').length).toBe(3); // 2 rects + 1 legend item
    wnItem.click();
    expect(wnItem.classList.contains('off')).toBe(true);
    // Bars matching bar:WN are display:none; the legend item itself is not hidden.
    const wnBars = Array.from(el.querySelectorAll('rect[data-legend-key="bar:WN"]'));
    wnBars.forEach((b) => expect(b.style.display).toBe('none'));
    expect(wnItem.style.display).toBe('');
    // Click again restores.
    wnItem.click();
    expect(wnItem.classList.contains('off')).toBe(false);
    wnBars.forEach((b) => expect(b.style.display).toBe(''));
  });

  it('overlays annotations from a sibling panel', () => {
    const stub = makeSpecStub({
      flips: { rows: [{ year: 2001, who: 'WN→DL' }] },
    });
    const state = makeState();
    const panel = {
      ...basic,
      annotations: { source: 'flips', x_key: 'year', label_key: 'who' },
    };
    const el = PANELS.combo(panel, state, ctx(stub));
    state.update([
      { year: 2000, flights: 1, margin: 1, who: 'WN' },
      { year: 2001, flights: 2, margin: 1, who: 'DL' },
    ]);
    expect(el.querySelectorAll('.chart-annotation').length).toBe(1);
    expect(el.querySelector('.chart-annotation-label').textContent).toBe('WN→DL');
  });
});
