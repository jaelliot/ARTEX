const TOKEN_KEY = "artex_token";
const SESSION_HINT_KEY = "artex_session";
const COOKIE_MAX_AGE = 7 * 24 * 60 * 60; // 7 days (seconds)

export interface CurrentUser {
  id: string;
  name: string;
  username: string;
  email: string;
  avatar: string;
  role: string;
}

function syncSessionHint(hasToken: boolean): void {
  // Remove the legacy credential cookie; cookies are shared across ports on a host.
  document.cookie = `${TOKEN_KEY}=; path=/; max-age=0; SameSite=Lax`;
  document.cookie = hasToken
    ? `${SESSION_HINT_KEY}=1; path=/; max-age=${COOKIE_MAX_AGE}; SameSite=Lax`
    : `${SESSION_HINT_KEY}=; path=/; max-age=0; SameSite=Lax`;
}

export const auth = {
  getToken(): string | null {
    if (typeof window === "undefined") return null;
    // Mock demo: no real login; allow every page (the client-side auth guard also allows through).
    const token = localStorage.getItem(TOKEN_KEY) ?? (process.env.NEXT_PUBLIC_MOCK === "1" ? "mock-demo" : null);
    syncSessionHint(Boolean(token));
    return token;
  },

  setToken(token: string): void {
    localStorage.setItem(TOKEN_KEY, token);
    syncSessionHint(Boolean(token));
  },

  clearToken(): void {
    localStorage.removeItem(TOKEN_KEY);
    syncSessionHint(false);
  },

  // Parse the current user from the JWT payload's sub field; display only, no signature verification.
  getCurrentUser(): CurrentUser | null {
    const token = this.getToken();
    if (!token) return null;
    try {
      const parts = token.split(".");
      if (parts.length !== 3) return null;
      // base64url → base64
      const payload = JSON.parse(atob(parts[1].replace(/-/g, "+").replace(/_/g, "/")));
      const username: string = payload.sub ?? "ARTEX";
      return { id: "1", name: username, username, email: "", avatar: "", role: "operator" };
    } catch {
      return null;
    }
  },
};
