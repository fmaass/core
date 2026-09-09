import { derived, writable } from 'svelte/store';
import { forgetSessionChoice, rememberSessionChoice } from './sessionRefresh.js';
import { setAPIRequestSessionKey } from '../api/core.js';
import { api } from '../api.js';
import { clearStores, getStoreValue } from './storeUtils.js';

function sessionChoiceStorage() {
  return typeof localStorage === 'undefined' ? null : localStorage;
}

function policyValue(error, field) {
  return error?.[field] ?? error?.body?.[field];
}

function hasPolicyFlag(error, field) {
  return policyValue(error, field) === true;
}

function createAuthStore() {
  /** @type {import('svelte/store').Writable<{id: string, email: string, name: string, language: string, avatar_url: string, role: string, is_system_admin?: boolean, [key: string]: any} | null>} */
  const user = writable(null);
  const session = writable(null);
  const isAuthenticated = writable(false);
  const loading = writable(false);
  const error = writable(null);

  // Create a combined derived store for easy subscription
  const combined = derived(
    [user, session, isAuthenticated, loading, error],
    ([$user, $session, $isAuthenticated, $loading, $error]) => ({
      user: $user,
      currentUser: $user,
      session: $session,
      isAuthenticated: $isAuthenticated,
      loading: $loading,
      error: $error,
    })
  );

  // Keep API request coalescing isolated to one authenticated session. The
  // server does not expose the cookie token, so user id + session creation
  // time is the stable public session marker; tests/legacy responses may
  // provide an explicit id instead.
  combined.subscribe((state) => {
    if (!state.isAuthenticated || state.user?.id == null) {
      setAPIRequestSessionKey(null);
      return;
    }
    const marker =
      state.session?.id || state.session?.created_at || state.session?.expires_at || 'active';
    setAPIRequestSessionKey(`auth:${state.user.id}:${marker}`);
  });

  return {
    // Subscribe to combined state
    subscribe: combined.subscribe,

    // Convenience getters for backwards compatibility with direct property access
    get currentUser() {
      return getStoreValue(user);
    },

    get isAuthenticated() {
      return getStoreValue(isAuthenticated);
    },

    get loading() {
      return getStoreValue(loading);
    },

    get error() {
      return getStoreValue(error);
    },

    // Initialize auth state by checking current session
    async init(options) {
      loading.set(true);

      try {
        const response = await api.auth.getCurrentUser(options);
        if (response.session?.auth_pending_type === 'passkey_verification') {
          // A refreshed password+passkey browser session is still only the
          // first factor. Return to login rather than rendering the app as if
          // authentication had completed; the user can restart the short flow.
          clearStores(user, session);
          isAuthenticated.set(false);
          loading.set(false);
          error.set(null);
          return { status: 'unauthenticated' };
        }
        user.set(response.user);
        session.set(response.session);
        isAuthenticated.set(true);
        loading.set(false);
        error.set(null);
        // An existing browser session picked up at bootstrap — an SSO return
        // among them. Nothing here chose a lifetime, so the sliding refresh
        // falls back to reading the session's own window (INFRA-92).
        forgetSessionChoice(sessionChoiceStorage());
        return { status: 'authenticated' };
      } catch (err) {
        loading.set(false);
        if (err?.status === 401) {
          // Only an explicit unauthenticated response proves that the browser
          // session is invalid. A transport failure leaves the prior state
          // intact and lets the bootstrap UI offer a meaningful retry.
          user.set(null);
          session.set(null);
          isAuthenticated.set(false);
          error.set(null);
          return { status: 'unauthenticated' };
        }
        error.set(err?.message || 'Unable to verify your session.');
        return { status: 'error', error: err };
      }
    },

    // Login with credentials
    async login(credentials) {
      loading.set(true);
      error.set(null);

      try {
        const response = await api.auth.login(credentials);

        // Handle policy-related responses
        if (response.sso_required) {
          clearStores(user, session);
          isAuthenticated.set(false);
          loading.set(false);
          error.set(response.policy_message || 'SSO login required');
          return {
            success: false,
            sso_required: true,
            policy_message:
              response.policy_message || 'Password login is disabled. Please use SSO.',
          };
        }

        if (response.passkey_required) {
          clearStores(user, session);
          isAuthenticated.set(false);
          loading.set(false);
          error.set(null);
          return {
            success: false,
            passkey_required: true,
            policy_message: response.policy_message,
          };
        }

        if (response.success) {
          // The session the server describes carries no "remember me" flag, and
          // inferring one from its window stops being reliable once the window
          // has been slid, so the choice is recorded where it was made — the
          // sliding refresh (INFRA-92) asks for the lifetime that was chosen
          // here rather than silently shortening a 30-day session to 24 hours.
          rememberSessionChoice(credentials?.remember_me === true, sessionChoiceStorage());

          // Get session details before marking the store authenticated so a
          // follow-up failure cannot leave user/isAuthenticated inconsistent.
          const sessionResponse = await api.auth.getCurrentUser();
          user.set(sessionResponse.user || response.user);
          session.set(sessionResponse.session);
          isAuthenticated.set(true);
          loading.set(false);
          error.set(null);

          // Return enrollment status if required
          return {
            success: true,
            enrollment_required: response.enrollment_required || false,
            policy_message: response.policy_message,
          };
        } else {
          clearStores(user, session);
          isAuthenticated.set(false);
          loading.set(false);
          error.set(response.message || 'Login failed');
          return { success: false, message: response.message || 'Login failed' };
        }
      } catch (err) {
        // Check if error response contains policy info
        if (hasPolicyFlag(err, 'passkey_required')) {
          clearStores(user, session);
          isAuthenticated.set(false);
          loading.set(false);
          error.set(null);
          return {
            success: false,
            passkey_required: true,
            policy_message: policyValue(err, 'policy_message'),
          };
        }

        if (hasPolicyFlag(err, 'sso_required')) {
          clearStores(user, session);
          isAuthenticated.set(false);
          loading.set(false);
          const policyMessage = policyValue(err, 'policy_message');
          error.set(policyMessage || 'SSO login required');
          return {
            success: false,
            sso_required: true,
            policy_message: policyMessage || 'Password login is disabled. Please use SSO.',
          };
        }

        clearStores(user, session);
        isAuthenticated.set(false);
        loading.set(false);
        error.set(err.message || 'Login failed');
        return { success: false, message: err.message || 'Login failed' };
      }
    },

    // Logout
    async logout() {
      loading.set(true);

      try {
        await api.auth.logout();
      } catch (err) {
        console.warn('Logout API call failed:', err);
      }

      clearStores(user, session, error);
      isAuthenticated.set(false);
      loading.set(false);
      forgetSessionChoice(sessionChoiceStorage());
    },

    // Logout from all sessions
    async logoutAll() {
      loading.set(true);

      try {
        await api.auth.logoutAll();
      } catch (err) {
        console.warn('Logout all API call failed:', err);
      }

      clearStores(user, session, error);
      isAuthenticated.set(false);
      loading.set(false);
      forgetSessionChoice(sessionChoiceStorage());
    },

    // Refresh session
    async refreshSession(rememberMe = false) {
      try {
        await api.auth.refreshSession({ remember_me: rememberMe });

        // Update session info
        const response = await api.auth.getCurrentUser();
        session.set(response.session);

        return true;
      } catch (err) {
        console.warn('Session refresh failed:', err);
        return false;
      }
    },

    // Change password
    async changePassword(passwordData) {
      loading.set(true);
      error.set(null);

      try {
        const response = await api.auth.changePassword(passwordData);
        loading.set(false);
        error.set(null);

        return { success: true, message: response.message || 'Password changed successfully' };
      } catch (err) {
        loading.set(false);
        error.set(err.message || 'Failed to change password');
        return { success: false, message: err.message || 'Failed to change password' };
      }
    },

    // Clear authentication (called on 401 errors)
    clearAuth() {
      clearStores(user, session);
      isAuthenticated.set(false);
      loading.set(false);
      error.set('Session expired. Please log in again.');
      forgetSessionChoice(sessionChoiceStorage());
    },

    // Confirm that a WebAuthn completion created/elevated the browser session,
    // then populate canonical user and session metadata from /auth/me.
    async completePasskeyLogin(fallbackUser = null) {
      loading.set(true);
      error.set(null);
      try {
        const response = await api.auth.getCurrentUser();
        user.set(response.user || fallbackUser);
        session.set(response.session);
        isAuthenticated.set(true);
        loading.set(false);
        // A WebAuthn completion carries no remember-me choice of its own, so any
        // choice a previous password login recorded must not be inherited.
        forgetSessionChoice(sessionChoiceStorage());
        return true;
      } catch (err) {
        clearStores(user, session);
        isAuthenticated.set(false);
        loading.set(false);
        error.set(err.message || 'Failed to complete passkey authentication');
        throw err;
      }
    },

    // Set authentication data directly for callers that already hold canonical
    // session metadata.
    setAuthData(userData, sessionData) {
      user.set(userData);
      session.set(sessionData);
      isAuthenticated.set(true);
      loading.set(false);
      error.set(null);
      // Same as the passkey completion: no lifetime was chosen here.
      forgetSessionChoice(sessionChoiceStorage());
    },

    // Clear error
    clearError() {
      error.set(null);
    },
  };
}

export const authStore = createAuthStore();
