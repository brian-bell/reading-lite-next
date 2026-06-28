// @vitest-environment jsdom

import { beforeEach, describe, expect, it } from 'vitest';

import { clearToken, loadToken, saveToken, TOKEN_STORAGE_KEY } from './tokenStorage';

describe('tokenStorage', () => {
  beforeEach(() => {
    localStorage.clear();
  });

  it('loads and saves the bearer token under one exported key', () => {
    expect(TOKEN_STORAGE_KEY).toBe('reading-lite.token');

    saveToken('  secret-token  ');

    expect(localStorage.getItem(TOKEN_STORAGE_KEY)).toBe('secret-token');
    expect(loadToken()).toBe('secret-token');
  });

  it('clears the stored token', () => {
    localStorage.setItem(TOKEN_STORAGE_KEY, 'secret-token');

    clearToken();

    expect(loadToken()).toBe('');
    expect(localStorage.getItem(TOKEN_STORAGE_KEY)).toBeNull();
  });
});
