import { LANGUAGES, isLanguage, loadLanguage, storedLanguage } from './i18n';

describe('language selection', () => {
  beforeEach(() => window.localStorage.clear());

  it('defaults to English when nothing was chosen', () => {
    expect(storedLanguage()).toBe('en');
  });

  it('uses the remembered language', () => {
    window.localStorage.setItem('hfproxy.language', 'fr');
    expect(storedLanguage()).toBe('fr');
  });

  it('ignores a remembered language that is not offered', () => {
    window.localStorage.setItem('hfproxy.language', 'xx');
    expect(storedLanguage()).toBe('en');
    expect(isLanguage('xx')).toBe(false);
  });

  it('lists each language once, English first', () => {
    const codes = LANGUAGES.map((language) => language.code);
    expect(codes[0]).toBe('en');
    expect(new Set(codes).size).toBe(codes.length);
  });

  it('keeps the page language in step with the chosen one', async () => {
    expect(await loadLanguage()).toBe('en');
    expect(document.documentElement.lang).toBe('en');
  });
});
