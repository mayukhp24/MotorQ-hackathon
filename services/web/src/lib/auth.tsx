import { createContext, useCallback, useContext, useEffect, useMemo, useState, type ReactNode } from "react";
import { api, login as apiLogin, setToken, setUnauthorizedHandler, getToken, type Me } from "../api/client";

interface AuthState {
  me: Me | null;
  loading: boolean;
  login: (email: string, password: string) => Promise<void>;
  logout: () => Promise<void>;
  can: (perm: string) => boolean;
}

const Ctx = createContext<AuthState | null>(null);

export function AuthProvider({ children }: { children: ReactNode }) {
  const [me, setMe] = useState<Me | null>(null);
  const [loading, setLoading] = useState(!!getToken());

  const clear = useCallback(() => {
    setToken(null);
    setMe(null);
  }, []);

  useEffect(() => {
    setUnauthorizedHandler(clear);
    if (!getToken()) return;
    api<Me>("/auth/me")
      .then(setMe)
      .catch(clear)
      .finally(() => setLoading(false));
  }, [clear]);

  const value = useMemo<AuthState>(
    () => ({
      me,
      loading,
      login: async (email, password) => {
        const res = await apiLogin(email, password);
        setToken(res.access_token);
        setMe(await api<Me>("/auth/me"));
      },
      logout: async () => {
        try {
          await api("/auth/logout", { method: "POST" });
        } catch {
          /* token may already be invalid */
        }
        clear();
      },
      can: (perm) => !!me?.permissions.includes(perm),
    }),
    [me, loading, clear],
  );
  return <Ctx.Provider value={value}>{children}</Ctx.Provider>;
}

export function useAuth() {
  const c = useContext(Ctx);
  if (!c) throw new Error("useAuth outside provider");
  return c;
}
