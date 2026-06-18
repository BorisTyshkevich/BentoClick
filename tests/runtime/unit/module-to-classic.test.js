// Integration regression for the iframe boot path: charts.js + dash.js
// bundled, stripped via moduleToClassic, evaluated as a classic <script>.
//
// dash.js + charts.js are loaded via vitest's ESM resolver in every
// other test file in this directory, but production loads them by
// fetching the source as text, running it through moduleToClassic,
// and inlining the result as a *classic* <script> in the dashboard
// iframe srcdoc (see spa.js:synthesizeSpecWrapper). PR #1's split
// introduced an `import { ... } from './charts.js'` that survived
// the original strip-export pass and crashed the iframe boot with
// SyntaxError — that's the regression this file pins.
//
// After the runtime split, the production "bundle" is the topological
// concat that scripts/install.sh and scripts/update.sh produce
// (core/*.js + panels/*.js + dash.js for the dash bundle; charts/*.js +
// charts.js for charts). This test assembles the same order and confirms
// moduleToClassic still yields a parseable, self-contained classic script
// with the window.DASH assignment intact.
//
// Unit-level coverage for moduleToClassic itself lives in
// spa.helpers.test.js (the helper now exports from spa-helpers.js).

import { describe, it, expect } from 'vitest';
import { readFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { moduleToClassic } from '../../../runtime/v1/spa-helpers.js';

const here = dirname(fileURLToPath(import.meta.url));
const runtimeDir = join(here, '../../../runtime/v1');

// Topological order must match the canonical bundle lists in
// scripts/lib.sh (DASH_BUNDLE / CHARTS_BUNDLE), shared by install.sh and
// update.sh. Keep this list in sync — a missing file here means the
// bundle is shipping less than tests cover.
const CHARTS_BUNDLE = [
  'charts/palette.js',
  'charts/scales.js',
  'charts/svg.js',
  'charts.js',
];

const DASH_BUNDLE = [
  'core/fmt.js',
  'core/interpolate.js',
  'core/run-state.js',
  'core/markdown.js',
  'core/ledger.js',
  'core/badge.js',
  'core/csv.js',
  'panels/_shared.js',
  'panels/chart-helpers.js',
  'panels/kpi-strip.js',
  'panels/table.js',
  'panels/bars.js',
  'panels/markdown.js',
  'panels/hero.js',
  'panels/callouts.js',
  'panels/html.js',
  'panels/script.js',
  'panels/line.js',
  'panels/combo.js',
  'panels/chart.js',
  'panels/dataset.js',
  'dash.js',
];

function cat(files) {
  return files.map((f) => readFileSync(join(runtimeDir, f), 'utf8')).join('\n');
}

describe('moduleToClassic — real runtime bundle', () => {
  it('produces parseable classic script for charts + dash bundle', () => {
    const bundled = moduleToClassic([cat(CHARTS_BUNDLE), cat(DASH_BUNDLE)].join('\n'));
    expect(bundled).not.toMatch(/^import\b/m);
    expect(bundled).not.toMatch(/^export\b/m);
    expect(() => new Function(bundled)).not.toThrow();
  });

  it('exposes the panel renderers + chart primitives in the bundled scope', () => {
    const bundled = moduleToClassic([cat(CHARTS_BUNDLE), cat(DASH_BUNDLE)].join('\n'));
    // Spot-check identifiers that must be present after the strip:
    // a chart primitive, a panel renderer, the PANELS dispatch, and
    // the SpecRuntime / renderSpec entry point.
    [
      'chartPalette', 'svgRoot', 'linearScale',
      'renderKpiStrip', 'renderTable', 'renderChart',
      'PANELS', 'SpecRuntime', 'renderSpec',
    ].forEach((id) => { expect(bundled).toContain(id); });
  });

  it('leaves the runtime\'s `window.DASH = …` assignment intact', () => {
    const bundled = moduleToClassic([cat(CHARTS_BUNDLE), cat(DASH_BUNDLE)].join('\n'));
    expect(bundled).toMatch(/window\.DASH\s*=/);
  });

  it('executes in a flat scope without ReferenceError — concat order is correct', () => {
    // new Function(code)() mirrors exactly how the iframe boot runs the bundle:
    // no ESM resolver, every symbol must be defined before its first use.
    // A const/let referenced before its definition throws ReferenceError here
    // but passes the parse-only `new Function(code)` check above.
    const bundled = moduleToClassic([cat(CHARTS_BUNDLE), cat(DASH_BUNDLE)].join('\n'));
    expect(() => new Function(bundled)()).not.toThrow();
    // Verify key symbols are reachable in the flat scope after execution.
    // new Function scope doesn't get window, so use the return trick instead.
    const types = new Function(bundled + '\nreturn { renderSpec: typeof renderSpec, PANELS: typeof PANELS, chartPalette: typeof chartPalette }')();
    expect(types.renderSpec).toBe('function');
    expect(types.PANELS).toBe('object');
    expect(types.chartPalette).toBe('object');
  });
});
