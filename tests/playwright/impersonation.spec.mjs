// impersonation.spec.mjs
//
// End-to-end browser test for the impersonation banner flow.
//
// The original bug: index.html defined stopImpersonating inside an IIFE, but
// the banner button used an inline `onclick="stopImpersonating()"` handler
// which looks up the function on `window`. Clicking the button did nothing.
//
// This test:
//   1. Loads the home page as the admin user (real session, no impersonation)
//      and verifies the impersonation banner is NOT shown.
//   2. Loads the home page as the admin user WITH the impersonate cookie
//      set to bob, and verifies the banner IS shown.
//   3. Clicks the "Stop impersonating" button - this is the actual code
//      path the user originally hit. The bug would be: the click does
//      nothing and the banner stays.
//   4. Verifies that after the click, the page reloads and the banner is
//      gone.
//
// Screenshots are taken at each step into E2E_SCREENSHOT_DIR.

import { mkdir } from 'node:fs/promises';
import { join } from 'node:path';

const BASE = process.env.E2E_BASE_URL;
const ADMIN = process.env.E2E_ADMIN_TOKEN;
const USER = process.env.E2E_USER_TOKEN;
const ADMIN_EMAIL = process.env.E2E_ADMIN_EMAIL;
const USER_EMAIL = process.env.E2E_USER_EMAIL;
const SHOTS = process.env.E2E_SCREENSHOT_DIR;

function assert(cond, msg) {
  if (!cond) throw new Error(`assertion failed: ${msg}`);
}

async function shot(page, name) {
  const path = join(SHOTS, `${name}.png`);
  await page.screenshot({ path, fullPage: true });
  console.log(`  📸 ${path}`);
  return path;
}

export async function run({ playwright, baseURL, tokens }) {
  assert(BASE && ADMIN && USER, 'env vars E2E_BASE_URL/E2E_ADMIN_TOKEN/E2E_USER_TOKEN must be set');
  await mkdir(SHOTS, { recursive: true });

  const { chromium } = playwright;
  const browser = await chromium.launch({ headless: true });
  let failed = false;
  const results = [];

  try {
    // -----------------------------------------------------------------------
    // Scenario 1: admin is signed in, no impersonation -> no banner.
    // -----------------------------------------------------------------------
    {
      const context = await browser.newContext();
      await context.addCookies([
        { name: 'session', value: ADMIN, domain: '127.0.0.1', path: '/', httpOnly: true, sameSite: 'Lax' },
      ]);
      const page = await context.newPage();
      const resp = await page.goto(baseURL + '/', { waitUntil: 'networkidle' });
      assert(resp && resp.status() === 200, `expected 200, got ${resp && resp.status()}`);

      // The admin UI panel / banner should not be present.
      const bannerCount = await page.locator('#impersonateBanner').count();
      assert(bannerCount === 0, `admin without impersonation should not show the banner (found ${bannerCount})`);

      await shot(page, '01-admin-no-impersonation');
      await context.close();
      results.push({ name: 'admin no impersonation → no banner', ok: true });
    }

    // -----------------------------------------------------------------------
    // Scenario 2: admin is impersonating bob -> banner IS shown.
    // -----------------------------------------------------------------------
    {
      const context = await browser.newContext();
      await context.addCookies([
        { name: 'session',      value: ADMIN,                                       domain: '127.0.0.1', path: '/', httpOnly: true, sameSite: 'Lax' },
        { name: 'impersonate',  value: `user-2:${USER_EMAIL}`,                       domain: '127.0.0.1', path: '/', httpOnly: true, sameSite: 'Lax' },
      ]);
      const page = await context.newPage();
      const resp = await page.goto(baseURL + '/', { waitUntil: 'networkidle' });
      assert(resp && resp.status() === 200, `expected 200, got ${resp && resp.status()}`);

      // The banner must be present.
      const banner = page.locator('#impersonateBanner');
      await banner.waitFor({ state: 'visible', timeout: 5000 });
      const bannerText = await banner.textContent();
      assert(/stop impersonating/i.test(bannerText), `banner should mention "Stop impersonating"; got: ${bannerText}`);

      // /api/me should report we are bob.
      const me = await page.evaluate(async () => (await fetch('/api/me')).json());
      assert(me.email === USER_EMAIL, `me.email = ${me.email}, want ${USER_EMAIL}`);
      assert(me.impersonating === true, `me.impersonating = ${me.impersonating}, want true`);
      assert(me.is_admin === true, `me.is_admin = ${me.is_admin}, want true (real user is admin)`);
      assert(me.real_email === ADMIN_EMAIL, `me.real_email = ${me.real_email}, want ${ADMIN_EMAIL}`);

      await shot(page, '02-admin-impersonating-banner-visible');
      await context.close();
      results.push({ name: 'admin impersonating → banner visible, /api/me reports bob', ok: true });
    }

    // -----------------------------------------------------------------------
    // Scenario 3: THE BUG REGRESSION. Click the button. Banner must clear.
    //
    // In the original bug, `stopImpersonating` was defined inside an IIFE and
    // not exposed on `window`. The inline `onclick="stopImpersonating()"`
    // resolved to a ReferenceError at click time, and the user saw nothing
    // happen. The fix is `window.stopImpersonating = stopImpersonating;`
    // near the end of the IIFE.
    // -----------------------------------------------------------------------
    {
      const context = await browser.newContext();
      await context.addCookies([
        {name: 'session',      value: ADMIN,                                       domain: '127.0.0.1', path: '/', httpOnly: true, sameSite: 'Lax' },
        { name: 'impersonate',  value: `user-2:${USER_EMAIL}`,                       domain: '127.0.0.1', path: '/', httpOnly: true, sameSite: 'Lax' },
      ]);
      const page = await context.newPage();

      // Wire up console + page error listeners so a JS error inside the
      // click handler (which is the original symptom) is loud.
      const errors = [];
      page.on('pageerror', (err) => errors.push(`pageerror: ${err.message}`));
      page.on('console', (msg) => {
        if (msg.type() === 'error') errors.push(`console.error: ${msg.text()}`);
      });

      await page.goto(baseURL + '/', { waitUntil: 'networkidle' });
      await page.locator('#impersonateBanner').waitFor({ state: 'visible', timeout: 5000 });
      await shot(page, '03-before-click');

      // Click the actual button. Use Promise.all to wait for the fetch and
      // the subsequent reload in parallel - the JS calls fetch then
      // window.location.reload().
      const stopBtn = page.locator('#impersonateBanner button', { hasText: 'Stop impersonating' });
      assert((await stopBtn.count()) > 0, 'banner should contain a Stop impersonating button');

      // The button's onclick references the global stopImpersonating.
      // Verify it is defined before clicking - this is a direct check on
      // the bug fix. If the function isn't on window, the click would
      // throw ReferenceError.
      const fnExists = await page.evaluate(() => typeof window.stopImpersonating === 'function');
      assert(fnExists, 'window.stopImpersonating is not a function - the IIFE-scoping bug is back!');

      // Capture all network responses to the stop-impersonate endpoint so we
      // can confirm the button actually fired the request.
      const stopResponses = [];
      page.on('response', (r) => {
        if (r.url().endsWith('/api/admin/stop-impersonate')) {
          stopResponses.push({ status: r.status(), url: r.url() });
        }
      });

      await stopBtn.click();
      // Wait for the reload to complete.
      await page.waitForLoadState('networkidle');

      // The browser must have actually POSTed to the stop endpoint.
      assert(stopResponses.length > 0, 'click should trigger POST to /api/admin/stop-impersonate');
      assert(stopResponses[0].status === 200, `stop-impersonate status = ${stopResponses[0].status}, want 200`);

      // The banner must be gone now.
      const bannerCount = await page.locator('#impersonateBanner').count();
      assert(bannerCount === 0, `after clicking Stop, banner should be gone; still found ${bannerCount}`);

      // /api/me must report the admin (not bob), with impersonating=false.
      const me = await page.evaluate(async () => (await fetch('/api/me')).json());
      assert(me.email === ADMIN_EMAIL, `after click, me.email = ${me.email}, want ${ADMIN_EMAIL}`);
      assert(me.impersonating === false, `after click, me.impersonating = ${me.impersonating}, want false`);

      if (errors.length) {
        throw new Error(`JS errors during click: ${errors.join(' | ')}`);
      }

      await shot(page, '04-after-click-banner-gone');
      await context.close();
      results.push({ name: 'click Stop impersonating → banner clears, /api/me reports admin', ok: true });
    }

    // -----------------------------------------------------------------------
    // Scenario 4: cross-check that the regular user (non-admin) does NOT
    // see the banner even if a stale impersonate cookie is lying around.
    // -----------------------------------------------------------------------
    {
      const context = await browser.newContext();
      await context.addCookies([
        { name: 'session',     value: USER,                                        domain: '127.0.0.1', path: '/', httpOnly: true, sameSite: 'Lax' },
        { name: 'impersonate', value: 'user-2:bob@example.com',                    domain: '127.0.0.1', path: '/', httpOnly: true, sameSite: 'Lax' },
      ]);
      const page = await context.newPage();
      await page.goto(baseURL + '/', { waitUntil: 'networkidle' });

      const me = await page.evaluate(async () => (await fetch('/api/me')).json());
      assert(me.email === USER_EMAIL, `non-admin with stale impersonate cookie: me.email = ${me.email}, want ${USER_EMAIL}`);
      assert(me.is_admin === false, `non-admin: me.is_admin = ${me.is_admin}, want false`);
      assert(me.impersonating === false, `non-admin: me.impersonating = ${me.impersonating}, want false`);

      const bannerCount = await page.locator('#impersonateBanner').count();
      assert(bannerCount === 0, `non-admin should not see impersonation banner; found ${bannerCount}`);

      await shot(page, '05-non-admin-no-banner');
      await context.close();
      results.push({ name: 'non-admin never sees the banner (impersonation is admin-only)', ok: true });
    }

    // -----------------------------------------------------------------------
    // All passed.
    // -----------------------------------------------------------------------
    console.log('\n[impersonation] ✅ All scenarios passed:');
    for (const r of results) console.log('  ✓', r.name);
    return 0;
  } catch (e) {
    failed = true;
    console.error('\n[impersonation] ❌', e.message);
    return 1;
  } finally {
    await browser.close();
  }
}
