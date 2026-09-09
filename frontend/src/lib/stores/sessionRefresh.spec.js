import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  attachSessionActivity,
  createSessionRefresher,
  forgetSessionChoice,
  readSessionChoice,
  rememberSessionChoice,
  resolveRememberMe,
  sessionIsMoreThanHalfSpent,
} from './sessionRefresh.js';

const HOUR = 60 * 60 * 1000;
const DAY = 24 * HOUR;
const T0 = Date.parse('2026-09-10T00:00:00.000Z');

/** A 24 h session created at T0. */
function session(createdOffsetMs = 0, windowMs = DAY) {
  return {
    id: 1,
    created_at: new Date(T0 + createdOffsetMs).toISOString(),
    expires_at: new Date(T0 + createdOffsetMs + windowMs).toISOString(),
  };
}

function memoryStorage() {
  const map = new Map();
  return {
    getItem: (k) => (map.has(k) ? map.get(k) : null),
    setItem: (k, v) => map.set(k, v),
    removeItem: (k) => map.delete(k),
  };
}

describe('sessionIsMoreThanHalfSpent', () => {
  it('is false while less than half the window is gone', () => {
    expect(sessionIsMoreThanHalfSpent(session(), T0 + 11 * HOUR)).toBe(false);
  });

  it('is true once more than half the window is gone', () => {
    expect(sessionIsMoreThanHalfSpent(session(), T0 + 13 * HOUR)).toBe(true);
  });

  it('is false for an already-expired session', () => {
    expect(sessionIsMoreThanHalfSpent(session(), T0 + 25 * HOUR)).toBe(false);
  });

  it('claims nothing without both timestamps', () => {
    expect(sessionIsMoreThanHalfSpent({ expires_at: new Date(T0).toISOString() }, T0)).toBe(false);
    expect(sessionIsMoreThanHalfSpent(null, T0)).toBe(false);
  });
});

describe('resolveRememberMe', () => {
  it('uses the recorded login choice above everything else', () => {
    expect(resolveRememberMe(session(0, 30 * DAY), false)).toBe(false);
    expect(resolveRememberMe(session(0, DAY), true)).toBe(true);
  });

  it('falls back to the session window when no choice was recorded', () => {
    expect(resolveRememberMe(session(0, DAY), null)).toBe(false);
    expect(resolveRememberMe(session(0, 30 * DAY), null)).toBe(true);
  });

  it('round-trips through storage', () => {
    const storage = memoryStorage();
    rememberSessionChoice(true, storage);
    expect(readSessionChoice(storage)).toBe(true);
    rememberSessionChoice(false, storage);
    expect(readSessionChoice(storage)).toBe(false);
  });

  it('reads unrecorded and unreachable storage as "never recorded"', () => {
    expect(readSessionChoice(memoryStorage())).toBe(null);
    expect(
      readSessionChoice({
        getItem() {
          throw new Error('SecurityError');
        },
      })
    ).toBe(null);
  });
});

describe('createSessionRefresher', () => {
  let clock;
  let refresh;
  let current;

  beforeEach(() => {
    vi.useFakeTimers();
    vi.setSystemTime(T0);
    clock = T0;
    refresh = vi.fn().mockResolvedValue(true);
    current = session();
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  function build(overrides = {}) {
    return createSessionRefresher({
      getSession: () => current,
      refresh,
      now: () => clock,
      minIntervalMs: 5 * 60 * 1000,
      ...overrides,
    });
  }

  it('does nothing while the session is young, however much activity there is', async () => {
    const refresher = build();
    for (let i = 0; i < 50; i++) {
      clock = T0 + i * 60 * 1000;
      expect(await refresher.noteActivity()).toBe('not-due');
    }
    expect(refresh).not.toHaveBeenCalled();
  });

  it('refreshes once the session is more than half spent', async () => {
    const refresher = build();
    clock = T0 + 13 * HOUR;
    expect(await refresher.noteActivity()).toBe('refreshed');
    expect(refresh).toHaveBeenCalledTimes(1);
    expect(refresh).toHaveBeenCalledWith(false);
  });

  it('makes at most one call per activity window however busy the user is', async () => {
    const refresher = build();
    clock = T0 + 13 * HOUR;
    expect(await refresher.noteActivity()).toBe('refreshed');
    for (let i = 1; i <= 60; i++) {
      clock = T0 + 13 * HOUR + i * 1000; // a click a second for a minute
      expect(await refresher.noteActivity()).toBe('throttled');
    }
    expect(refresh).toHaveBeenCalledTimes(1);

    clock = T0 + 13 * HOUR + 6 * 60 * 1000; // past the window
    expect(await refresher.noteActivity()).toBe('refreshed');
    expect(refresh).toHaveBeenCalledTimes(2);
  });

  it('never fires on its own: an idle session reaches its deadline unrefreshed', async () => {
    build();
    await vi.advanceTimersByTimeAsync(2 * DAY);
    expect(refresh).not.toHaveBeenCalled();
  });

  it('asks for the long lifetime when the login recorded "remember me"', async () => {
    const refresher = build({ readChoice: () => true });
    clock = T0 + 13 * HOUR;
    await refresher.noteActivity();
    expect(refresh).toHaveBeenCalledWith(true);
  });

  it('does nothing without a session', async () => {
    current = null;
    const refresher = build();
    clock = T0 + 13 * HOUR;
    expect(await refresher.noteActivity()).toBe('no-session');
    expect(refresh).not.toHaveBeenCalled();
  });

  it('a failed refresh is reported, not thrown, and retries in the next window', async () => {
    refresh.mockRejectedValueOnce(new Error('network'));
    const refresher = build();
    clock = T0 + 13 * HOUR;
    expect(await refresher.noteActivity()).toBe('failed');
    clock = T0 + 13 * HOUR + 6 * 60 * 1000;
    expect(await refresher.noteActivity()).toBe('refreshed');
    expect(refresh).toHaveBeenCalledTimes(2);
  });

  it('negative control: with no refresher wired at all — the state before this change — an active user gets nothing', async () => {
    // The pre-fix frontend had zero call sites for authStore.refreshSession, so
    // activity could not slide anything. Reproduced by simply never calling the
    // refresher over a session's whole window.
    clock = T0 + 23 * HOUR;
    expect(refresh).not.toHaveBeenCalled();
  });
});

// ---------------------------------------------------------------------------
// Review findings, INFRA-92 round 2.
// ---------------------------------------------------------------------------

describe('mount is not activity (finding 1)', () => {
  const HALF_SPENT = {
    id: 1,
    created_at: new Date(T0 - 13 * HOUR).toISOString(),
    expires_at: new Date(T0 - 13 * HOUR + DAY).toISOString(),
  };

  let refresh;
  let refresher;
  let target;

  beforeEach(() => {
    vi.useFakeTimers();
    vi.setSystemTime(T0);
    refresh = vi.fn().mockResolvedValue(true);
    refresher = createSessionRefresher({
      getSession: () => HALF_SPENT,
      refresh,
      now: () => Date.now(),
    });
    target = new EventTarget();
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  /** A Svelte store hands a new subscriber the current value at once. */
  function routeStore(initial) {
    let value = initial;
    const subscribers = new Set();
    return {
      subscribe(fn) {
        subscribers.add(fn);
        fn(value);
        return () => subscribers.delete(fn);
      },
      navigate(next) {
        value = next;
        for (const fn of subscribers) fn(value);
      },
    };
  }

  it('an already half-spent session is NOT refreshed by mounting', async () => {
    const route = routeStore({ path: '/workspaces/1' });
    attachSessionActivity({ refresher, target, subscribeRoute: route.subscribe });
    await vi.advanceTimersByTimeAsync(30 * 60 * 1000);
    expect(refresh).not.toHaveBeenCalled();
  });

  it('one keydown after mount refreshes exactly once', async () => {
    const route = routeStore({ path: '/workspaces/1' });
    attachSessionActivity({ refresher, target, subscribeRoute: route.subscribe });
    await vi.advanceTimersByTimeAsync(30 * 60 * 1000);
    expect(refresh).not.toHaveBeenCalled();

    target.dispatchEvent(new Event('keydown'));
    await vi.advanceTimersByTimeAsync(0);
    expect(refresh).toHaveBeenCalledTimes(1);
  });

  it('the bootstrap redirect to the default view does not count', async () => {
    // What the running app does with no input at all: /workspaces/1 resolves,
    // then the shell redirects to the workspace's default view.
    const route = routeStore({ path: '/workspaces/1' });
    attachSessionActivity({ refresher, target, subscribeRoute: route.subscribe });
    route.navigate({ path: '/workspaces/1/board' });
    await vi.advanceTimersByTimeAsync(5000);
    expect(refresh).not.toHaveBeenCalled();
  });

  it('a navigation the user makes counts, once they have touched something', async () => {
    const route = routeStore({ path: '/workspaces/1' });
    attachSessionActivity({ refresher, target, subscribeRoute: route.subscribe });
    route.navigate({ path: '/workspaces/1/board' }); // still bootstrap
    await vi.advanceTimersByTimeAsync(0);
    expect(refresh).not.toHaveBeenCalled();

    target.dispatchEvent(new Event('pointerdown')); // the click on the link
    await vi.advanceTimersByTimeAsync(0);
    expect(refresh).toHaveBeenCalledTimes(1);

    // The route change that follows is inside the throttle window, so it costs
    // nothing — but it is now eligible, which is what the next window relies on.
    route.navigate({ path: '/workspaces/1/items/7' });
    await vi.advanceTimersByTimeAsync(0);
    expect(refresh).toHaveBeenCalledTimes(1);
  });

  it('a re-emit of the same route is not a navigation', async () => {
    const route = routeStore({ path: '/a' });
    attachSessionActivity({ refresher, target, subscribeRoute: route.subscribe });
    target.dispatchEvent(new Event('keydown'));
    await vi.advanceTimersByTimeAsync(0);
    expect(refresh).toHaveBeenCalledTimes(1);
    refresh.mockClear();

    route.navigate({ path: '/a' });
    await vi.advanceTimersByTimeAsync(0);
    expect(refresh).not.toHaveBeenCalled();
  });

  it('detaching stops the listeners', async () => {
    const route = routeStore({ path: '/a' });
    const detach = attachSessionActivity({ refresher, target, subscribeRoute: route.subscribe });
    detach();
    target.dispatchEvent(new Event('pointerdown'));
    route.navigate({ path: '/b' });
    await vi.advanceTimersByTimeAsync(0);
    expect(refresh).not.toHaveBeenCalled();
  });
});

describe('the refreshed window is what the next decision reads (finding 2)', () => {
  let clock;
  let current;
  let refresh;

  beforeEach(() => {
    clock = T0 + 13 * HOUR;
    current = session(); // created T0, expires T0 + 24h
    // What authStore.refreshSession does: refresh, then re-read /auth/me and put
    // the NEW session in the store. Without that write the refresher keeps
    // reading the original deadline.
    refresh = vi.fn(async () => {
      current = {
        id: 1,
        created_at: current.created_at,
        expires_at: new Date(clock + DAY).toISOString(),
      };
      return true;
    });
  });

  function build() {
    return createSessionRefresher({
      getSession: () => current,
      refresh,
      now: () => clock,
      minIntervalMs: 5 * 60 * 1000,
    });
  }

  it('advances the stored expiry and then reads the new window, not the old one', async () => {
    const refresher = build();
    expect(await refresher.noteActivity()).toBe('refreshed');
    expect(Date.parse(current.expires_at)).toBe(clock + DAY);

    // 6 minutes later: past the throttle, but the NEW window is barely spent.
    clock += 6 * 60 * 1000;
    expect(await refresher.noteActivity()).toBe('not-due');
    expect(refresh).toHaveBeenCalledTimes(1);

    // Past the original deadline, and past half of the new window.
    clock += 20 * HOUR;
    expect(clock).toBeGreaterThan(Date.parse(session().expires_at));
    expect(await refresher.noteActivity()).toBe('refreshed');
    expect(refresh).toHaveBeenCalledTimes(2);
  });

  it('a refresh that answers false is a failure, not a silent success', async () => {
    refresh = vi.fn().mockResolvedValue(false);
    const refresher = build();
    expect(await refresher.noteActivity()).toBe('failed');
  });
});

describe('the recorded choice does not outlive its session (finding 3)', () => {
  it('forgetSessionChoice returns the reader to the window fallback', () => {
    const storage = memoryStorage();
    rememberSessionChoice(true, storage);
    expect(readSessionChoice(storage)).toBe(true);
    forgetSessionChoice(storage);
    expect(readSessionChoice(storage)).toBe(null);
    // With no recorded choice a 24 h session is not treated as "remember me".
    expect(resolveRememberMe(session(0, DAY), readSessionChoice(storage))).toBe(false);
  });

  it('survives storage that refuses to remove', () => {
    expect(() =>
      forgetSessionChoice({
        removeItem() {
          throw new Error('SecurityError');
        },
      })
    ).not.toThrow();
  });

  // The entry points in auth.svelte.js, each reproduced as the call it makes:
  // login() records, and init() / completePasskeyLogin() / setAuthData() /
  // logout() / logoutAll() / clearAuth() forget.
  const RECORDS = [['login (password)', (storage, choice) => rememberSessionChoice(choice, storage)]];
  const FORGETS = [
    ['init (existing session, SSO return included)'],
    ['completePasskeyLogin'],
    ['setAuthData'],
    ['logout'],
    ['logoutAll'],
    ['clearAuth'],
  ];

  it.each(RECORDS)('%s records the choice it made', (_name, record) => {
    const storage = memoryStorage();
    record(storage, true);
    expect(readSessionChoice(storage)).toBe(true);
  });

  it.each(FORGETS)('%s leaves no choice behind for the next session', (_name) => {
    const storage = memoryStorage();
    rememberSessionChoice(true, storage); // a previous password login
    forgetSessionChoice(storage);
    expect(readSessionChoice(storage)).toBe(null);
  });
});
