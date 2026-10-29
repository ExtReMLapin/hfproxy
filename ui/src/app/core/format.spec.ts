import { formatBytes } from './format';

describe('formatBytes', () => {
  it('uses binary units with one decimal', () => {
    expect(formatBytes(0)).toBe('0 B');
    expect(formatBytes(1023)).toBe('1023 B');
    expect(formatBytes(1536)).toBe('1.5 KiB');
    expect(formatBytes(5 * 1024 ** 3)).toBe('5.0 GiB');
    expect(formatBytes(3000 * 1024 ** 4)).toBe('3000.0 TiB');
  });
  it('copes with values that are not sizes', () => {
    expect(formatBytes(NaN)).toBe('0 B');
    expect(formatBytes(-5)).toBe('0 B');
  });
});
