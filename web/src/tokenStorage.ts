export const TOKEN_STORAGE_KEY = 'reading-lite.token';

export function loadToken(): string {
  return localStorage.getItem(TOKEN_STORAGE_KEY) ?? '';
}

export function saveToken(token: string): void {
  localStorage.setItem(TOKEN_STORAGE_KEY, token.trim());
}

export function clearToken(): void {
  localStorage.removeItem(TOKEN_STORAGE_KEY);
}
