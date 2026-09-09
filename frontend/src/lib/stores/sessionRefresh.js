// Sliding session expiry (INFRA-92).
//
// `POST /auth/refresh` has existed end to end since the session manager was
// written, but nothing in the frontend ever called it, so the TTL fixed at login
// was a hard ceiling even for a daily-active user: 24 h without "remember me"
// forced a daily login and 30 d with it forced a monthly one, however much the
// person used the app.
//
// The rule here is deliberately activity-driven rather than timed: nothing runs
// on its own, so an IDLE session still expires at exactly the deadline its login
// set. A refresh happens only when the user does something AND the session is
// more than half spent AND at least `minIntervalMs` has passed since the last
// one — a busy user therefore costs one request per window, never one per click.

export const REMEMBER_ME_STORAGE_KEY = 'windshift-session-remember-me';
export const DEFAULT_MIN_REFRESH_INTERVAL_MS = 5 * 60 * 1000;
export const DEFAULT_SPENT_RATIO = 0.5;

// A session whose original window was longer than this was a "remember me"
// login: the two server-side durations are 24 h and 30 d, so any threshold
// between them separates them without the client hardcoding either.
const LONG_SESSION_MS = 7 * 24 * 60 * 60 * 1000;

function toMs(value) {
  if (!value) return null;
  const parsed = Date.parse(value);
  return Number.isNaN(parsed) ? null : parsed;
}

/**
 * Record the "remember me" box as it was ticked at login. The session the server
 * reports carries no such flag, and inferring it from the session's own window
 * stops being reliable once the window has been slid, so the choice is kept
 * where it was made. A wrong value can only ever change the length of the
 * reader's own session.
 */
export function rememberSessionChoice(rememberMe, storage) {
  try {
    storage?.setItem(REMEMBER_ME_STORAGE_KEY, rememberMe ? 'true' : 'false');
  } catch {
    // Private mode and quota-exhausted storage throw; the fallback below applies.
  }
}

/**
 * Forget the recorded choice, so the window fallback decides again. Called
 * wherever a session ENDS and wherever one BEGINS through an entry point that
 * made no such choice — otherwise a password login's "remember me" outlives its
 * session and is applied to whatever signs in next (INFRA-92 review finding 3).
 */
export function forgetSessionChoice(storage) {
  try {
    storage?.removeItem(REMEMBER_ME_STORAGE_KEY);
  } catch {
    // Private mode and quota-exhausted storage throw; "never recorded" is the
    // same outcome the read below produces anyway.
  }
}

/** @returns {boolean|null} null when the choice was never recorded. */
export function readSessionChoice(storage) {
  try {
    const stored = storage?.getItem(REMEMBER_ME_STORAGE_KEY);
    if (stored === 'true') return true;
    if (stored === 'false') return false;
  } catch {
    // Same as above — treated as "never recorded".
  }
  return null;
}

/**
 * Which lifetime a refresh should ask for. The recorded login choice wins; with
 * none (a session predating this code, or an SSO return that never passed
 * through the login dialog) the session's own window is the only evidence there
 * is, and only a window longer than a week is read as "remember me".
 */
export function resolveRememberMe(session, storedChoice) {
  if (storedChoice === true || storedChoice === false) return storedChoice;
  const createdAt = toMs(session?.created_at);
  const expiresAt = toMs(session?.expires_at);
  if (createdAt === null || expiresAt === null) return false;
  return expiresAt - createdAt > LONG_SESSION_MS;
}

/**
 * True when more than `ratio` of the session's window has been spent. Without
 * both timestamps nothing can be judged, and nothing is claimed: the session is
 * left alone rather than refreshed on a guess.
 */
export function sessionIsMoreThanHalfSpent(session, now, ratio = DEFAULT_SPENT_RATIO) {
  const createdAt = toMs(session?.created_at);
  const expiresAt = toMs(session?.expires_at);
  if (createdAt === null || expiresAt === null) return false;
  const window = expiresAt - createdAt;
  if (window <= 0) return false;
  const remaining = expiresAt - now;
  if (remaining <= 0) return false; // already expired: refreshing is the server's call, not ours
  return remaining < window * ratio;
}

/**
 * @param {Object} deps
 * @param {() => Object|null} deps.getSession   current session as the server last described it
 * @param {(rememberMe: boolean) => Promise<any>} deps.refresh  performs POST /auth/refresh
 * @param {() => number} [deps.now]
 * @param {number} [deps.minIntervalMs]
 * @param {number} [deps.spentRatio]
 * @param {() => boolean|null} [deps.readChoice]
 */
export function createSessionRefresher({
  getSession,
  refresh,
  now = Date.now,
  minIntervalMs = DEFAULT_MIN_REFRESH_INTERVAL_MS,
  spentRatio = DEFAULT_SPENT_RATIO,
  readChoice = () => null,
}) {
  let lastRefreshAt = null;
  let inFlight = false;

  return {
    /**
     * Called on navigation and on user input. Returns why it did or did not act,
     * which is what the tests assert on.
     * @returns {Promise<'no-session'|'throttled'|'not-due'|'in-flight'|'refreshed'|'failed'>}
     */
    async noteActivity() {
      const session = getSession();
      if (!session) return 'no-session';

      const at = now();
      if (lastRefreshAt !== null && at - lastRefreshAt < minIntervalMs) return 'throttled';
      if (!sessionIsMoreThanHalfSpent(session, at, spentRatio)) return 'not-due';
      if (inFlight) return 'in-flight';

      inFlight = true;
      lastRefreshAt = at;
      try {
        // authStore.refreshSession() catches its own transport errors and
        // answers false, so a falsy result is a failure like a rejection is.
        const ok = await refresh(resolveRememberMe(session, readChoice()));
        return ok === false ? 'failed' : 'refreshed';
      } catch {
        // A failed refresh is not a logout: the session keeps the deadline it
        // had, and the next activity window tries again.
        return 'failed';
      } finally {
        inFlight = false;
      }
    },

    /** Forget the throttle — used when the identity changes (login, logout). */
    reset() {
      lastRefreshAt = null;
    },
  };
}

/**
 * Drive a refresher from real activity, and from nothing else.
 *
 * MOUNT IS NOT ACTIVITY (INFRA-92 review finding 1). Two things at startup look
 * like navigation and are not: a Svelte store hands a new subscriber the CURRENT
 * route immediately, and the shell then redirects to the workspace's default
 * view (observed in the running app: `/workspaces/1` → `/workspaces/1/board`
 * with no input at all). Loading an already half-spent session would otherwise
 * slide it on bootstrap alone.
 *
 * So a route change counts only once a real input event has been seen. A
 * navigation the PERSON makes is always preceded by one — the click on the link
 * or the key that triggered it — while a programmatic redirect is not, which is
 * exactly the line the invariant draws. Pointer and key events themselves cannot
 * arrive before the person produces one, so they are passed through as they are;
 * the click that starts a navigation and the route change that follows it are
 * absorbed by the refresher's own throttle.
 *
 * @param {Object} deps
 * @param {{noteActivity: () => Promise<any>}} deps.refresher
 * @param {EventTarget|null} [deps.target]        where input is listened for
 * @param {(fn: (route: any) => void) => (() => void)} [deps.subscribeRoute]
 * @param {(route: any) => string} [deps.routeKey]
 * @returns {() => void} detach
 */
export function attachSessionActivity({
  refresher,
  target = typeof window === 'undefined' ? null : window,
  subscribeRoute = null,
  routeKey = (route) => route?.path ?? route?.view ?? String(route),
}) {
  let sawInput = false;
  let lastRouteKey;

  const note = () => {
    void refresher.noteActivity();
  };

  const onInput = () => {
    sawInput = true;
    note();
  };

  const unsubscribeRoute = subscribeRoute
    ? subscribeRoute((route) => {
        const key = routeKey(route);
        const changed = lastRouteKey !== undefined && key !== lastRouteKey;
        lastRouteKey = key;
        if (changed && sawInput) note();
      })
    : null;

  if (target) {
    target.addEventListener('pointerdown', onInput, { passive: true });
    target.addEventListener('keydown', onInput, { passive: true });
  }

  return () => {
    unsubscribeRoute?.();
    if (target) {
      target.removeEventListener('pointerdown', onInput);
      target.removeEventListener('keydown', onInput);
    }
  };
}
