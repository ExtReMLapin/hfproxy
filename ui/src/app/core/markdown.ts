/**
 * A small Markdown reader for model and dataset cards.
 *
 * It produces a tree of plain data (blocks of spans) that a template renders;
 * nothing is ever turned into HTML, so a card cannot inject markup or script.
 * It covers what cards use: headings, paragraphs, lists, quotes, code, tables,
 * emphasis, inline code and links. Images are shown as their text, and only
 * http, https and mailto links are kept as links.
 */

export interface MdSpan {
  text: string;
  bold?: boolean;
  italic?: boolean;
  code?: boolean;
  href?: string;
}

export type MdBlock =
  | { type: 'heading'; level: number; spans: MdSpan[] }
  | { type: 'paragraph'; spans: MdSpan[] }
  | { type: 'code'; language: string; text: string }
  | { type: 'list'; ordered: boolean; items: MdSpan[][] }
  | { type: 'quote'; blocks: MdBlock[] }
  | { type: 'table'; header: MdSpan[][]; rows: MdSpan[][][] }
  | { type: 'rule' };

const SAFE_LINK = /^(https?:\/\/|mailto:)/i;
const MAX_DEPTH = 4;

export function parseMarkdown(source: string, depth = 0): MdBlock[] {
  const lines = source.replace(/\r\n?/g, '\n').split('\n');
  const blocks: MdBlock[] = [];
  let i = 0;
  while (i < lines.length) {
    const line = lines[i];
    if (line.trim() === '') {
      i++;
      continue;
    }
    const fence = /^\s*(```+|~~~+)\s*([\w+-]*)/.exec(line);
    if (fence) {
      const body: string[] = [];
      i++;
      while (i < lines.length && !lines[i].trim().startsWith(fence[1])) body.push(lines[i++]);
      i++;
      blocks.push({ type: 'code', language: fence[2], text: body.join('\n') });
      continue;
    }
    const heading = /^(#{1,6})\s+(.*?)\s*#*\s*$/.exec(line);
    if (heading) {
      blocks.push({ type: 'heading', level: heading[1].length, spans: parseInline(heading[2]) });
      i++;
      continue;
    }
    if (/^\s{0,3}([-*_])(\s*\1){2,}\s*$/.test(line)) {
      blocks.push({ type: 'rule' });
      i++;
      continue;
    }
    if (line.startsWith('>')) {
      const quoted: string[] = [];
      while (i < lines.length && lines[i].startsWith('>'))
        quoted.push(lines[i++].replace(/^>\s?/, ''));
      blocks.push({
        type: 'quote',
        blocks: depth < MAX_DEPTH ? parseMarkdown(quoted.join('\n'), depth + 1) : [],
      });
      continue;
    }
    if (isTableStart(lines, i)) {
      const header = splitRow(lines[i]);
      i += 2;
      const rows: MdSpan[][][] = [];
      while (i < lines.length && lines[i].includes('|') && lines[i].trim() !== '')
        rows.push(splitRow(lines[i++]).map(parseInline));
      blocks.push({ type: 'table', header: header.map(parseInline), rows });
      continue;
    }
    const item = /^(\s*)([-*+]|\d+[.)])\s+(.*)$/.exec(line);
    if (item) {
      const ordered = /\d/.test(item[2]);
      const items: MdSpan[][] = [];
      while (i < lines.length) {
        const next = /^(\s*)([-*+]|\d+[.)])\s+(.*)$/.exec(lines[i]);
        if (next) {
          items.push(parseInline(next[3]));
        } else if (/^\s{2,}\S/.test(lines[i]) && items.length) {
          items[items.length - 1].push({ text: ' ' }, ...parseInline(lines[i].trim()));
        } else break;
        i++;
      }
      blocks.push({ type: 'list', ordered, items });
      continue;
    }
    const paragraph: string[] = [];
    while (i < lines.length && lines[i].trim() !== '' && !startsBlock(lines, i))
      paragraph.push(lines[i++].trim());
    if (!paragraph.length) {
      paragraph.push(lines[i++].trim());
    }
    blocks.push({ type: 'paragraph', spans: parseInline(paragraph.join(' ')) });
  }
  return blocks;
}

function startsBlock(lines: string[], i: number): boolean {
  const line = lines[i];
  return (
    /^\s*(```|~~~)/.test(line) ||
    /^#{1,6}\s/.test(line) ||
    line.startsWith('>') ||
    /^(\s*)([-*+]|\d+[.)])\s+/.test(line) ||
    /^\s{0,3}([-*_])(\s*\1){2,}\s*$/.test(line) ||
    isTableStart(lines, i)
  );
}

function isTableStart(lines: string[], i: number): boolean {
  return (
    i + 1 < lines.length &&
    lines[i].includes('|') &&
    /^\s*\|?\s*:?-+:?\s*(\|\s*:?-+:?\s*)*\|?\s*$/.test(lines[i + 1])
  );
}

function splitRow(line: string): string[] {
  return line
    .trim()
    .replace(/^\|/, '')
    .replace(/\|$/, '')
    .split('|')
    .map((cell) => cell.trim());
}

/** Reads emphasis, inline code and links into flat spans. */
export function parseInline(text: string): MdSpan[] {
  const spans: MdSpan[] = [];
  let bold = false;
  let italic = false;
  let buffer = '';
  const flush = () => {
    if (buffer) spans.push({ text: buffer, ...(bold && { bold }), ...(italic && { italic }) });
    buffer = '';
  };
  let i = 0;
  while (i < text.length) {
    const rest = text.slice(i);
    if (text[i] === '\\' && i + 1 < text.length) {
      buffer += text[i + 1];
      i += 2;
    } else if (text[i] === '`') {
      const end = text.indexOf('`', i + 1);
      if (end > i) {
        flush();
        spans.push({ text: text.slice(i + 1, end), code: true });
        i = end + 1;
      } else {
        buffer += text[i++];
      }
    } else if (text.startsWith('**', i) || text.startsWith('__', i)) {
      flush();
      bold = !bold;
      i += 2;
    } else if ((text[i] === '*' || text[i] === '_') && isEmphasisBoundary(text, i)) {
      flush();
      italic = !italic;
      i++;
    } else if (text[i] === '!' && text[i + 1] === '[') {
      const image = /^!\[([^\]]*)\]\(([^)\s]*)[^)]*\)/.exec(rest);
      if (image) {
        flush();
        spans.push({ text: image[1] ? `[${image[1]}]` : '[image]' });
        i += image[0].length;
      } else buffer += text[i++];
    } else if (text[i] === '[') {
      const link = /^\[([^\]]*)\]\(([^)\s]*)[^)]*\)/.exec(rest);
      if (link) {
        flush();
        const label = link[1] || link[2];
        spans.push(SAFE_LINK.test(link[2]) ? { text: label, href: link[2] } : { text: label });
        i += link[0].length;
      } else buffer += text[i++];
    } else if (SAFE_LINK.test(rest) && /^https?:/i.test(rest)) {
      const url = /^https?:\/\/[^\s<>()]+/i.exec(rest);
      if (url) {
        flush();
        spans.push({ text: url[0], href: url[0] });
        i += url[0].length;
      } else buffer += text[i++];
    } else {
      buffer += text[i++];
    }
  }
  flush();
  return spans;
}

/** `_` inside a word (snake_case) is not emphasis; `*` and `_` around words are. */
function isEmphasisBoundary(text: string, i: number): boolean {
  const marker = text[i];
  const before = i > 0 ? text[i - 1] : ' ';
  const after = i + 1 < text.length ? text[i + 1] : ' ';
  if (marker === '_' && /\w/.test(before) && /\w/.test(after)) return false;
  return !/\s/.test(after) || !/\s/.test(before);
}
