import { parseInline, parseMarkdown } from './markdown';

describe('parseMarkdown', () => {
  it('reads headings, paragraphs and rules', () => {
    const blocks = parseMarkdown('# Title\n\nSome *text*\nover two lines.\n\n---\n## Next');
    expect(blocks.map((b) => b.type)).toEqual(['heading', 'paragraph', 'rule', 'heading']);
    expect(blocks[0]).toMatchObject({ type: 'heading', level: 1 });
    expect(blocks[1]).toMatchObject({ type: 'paragraph' });
    const paragraph = blocks[1] as { spans: { text: string; italic?: boolean }[] };
    expect(paragraph.spans.map((s) => s.text).join('')).toBe('Some text over two lines.');
    expect(paragraph.spans.find((s) => s.italic)?.text).toBe('text');
  });

  it('keeps code blocks as they are', () => {
    const blocks = parseMarkdown('```python\nprint("<b>hi</b>")  # *not emphasis*\n```\nafter');
    expect(blocks[0]).toEqual({
      type: 'code',
      language: 'python',
      text: 'print("<b>hi</b>")  # *not emphasis*',
    });
    expect(blocks[1].type).toBe('paragraph');
  });

  it('reads ordered and unordered lists and block quotes', () => {
    const blocks = parseMarkdown('- one\n- two\n\n1. first\n2. second\n\n> quoted\n> text');
    expect(blocks[0]).toMatchObject({ type: 'list', ordered: false });
    expect((blocks[0] as { items: unknown[] }).items).toHaveLength(2);
    expect(blocks[1]).toMatchObject({ type: 'list', ordered: true });
    expect(blocks[2]).toMatchObject({ type: 'quote' });
  });

  it('reads tables', () => {
    const blocks = parseMarkdown('| name | score |\n| --- | ---: |\n| a | 1 |\n| b | 2 |\n');
    expect(blocks[0]).toMatchObject({ type: 'table' });
    const table = blocks[0] as { header: unknown[]; rows: unknown[][] };
    expect(table.header).toHaveLength(2);
    expect(table.rows).toHaveLength(2);
  });

  it('never produces markup: HTML stays text', () => {
    const blocks = parseMarkdown('<script>alert(1)</script> <img src=x onerror=alert(1)>');
    expect(JSON.stringify(blocks)).toContain('<script>');
    expect(blocks[0].type).toBe('paragraph');
  });
});

describe('parseInline', () => {
  it('reads emphasis, code and nested bold italic', () => {
    expect(parseInline('**bold *both* done** and `code`')).toEqual([
      { text: 'bold ', bold: true },
      { text: 'both', bold: true, italic: true },
      { text: ' done', bold: true },
      { text: ' and ' },
      { text: 'code', code: true },
    ]);
  });

  it('does not take snake_case for emphasis', () => {
    expect(parseInline('use model_name_or_path here')).toEqual([
      { text: 'use model_name_or_path here' },
    ]);
  });

  it('keeps only http, https and mailto links', () => {
    expect(
      parseInline(
        '[ok](https://example.org/x) [mail](mailto:a@b.c) [bad](javascript:alert) [rel](../x)',
      ),
    ).toEqual([
      { text: 'ok', href: 'https://example.org/x' },
      { text: ' ' },
      { text: 'mail', href: 'mailto:a@b.c' },
      { text: ' ' },
      { text: 'bad' },
      { text: ' ' },
      { text: 'rel' },
    ]);
  });

  it('shows images as text and links bare addresses', () => {
    expect(parseInline('![logo](https://x/y.png) see https://example.org/a.')).toEqual([
      { text: '[logo]' },
      { text: ' see ' },
      { text: 'https://example.org/a.', href: 'https://example.org/a.' },
    ]);
  });

  it('honours backslash escapes and unmatched markers', () => {
    expect(parseInline('2 \\* 3 and a * b')).toEqual([{ text: '2 * 3 and a * b' }]);
  });
});
