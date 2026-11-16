// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// A small, forgiving XML reader and writer for DASH manifests. Service
// workers have no DOMParser, so the background cannot use the browser's.
// Handles elements, attributes, text, CDATA, comments, processing
// instructions, a DOCTYPE without an internal subset, and the predefined
// and numeric entities. Namespace prefixes are kept as part of the name.

export class XmlError extends Error {}

const ENTITIES = { amp: '&', lt: '<', gt: '>', quot: '"', apos: "'" };

export function decodeEntities(s) {
  return s.replace(/&(#x[0-9a-fA-F]+|#[0-9]+|[a-zA-Z]+);/g, (m, e) => {
    if (e[0] === '#') {
      const code = e[1] === 'x' || e[1] === 'X' ? parseInt(e.slice(2), 16) : parseInt(e.slice(1), 10);
      try {
        return String.fromCodePoint(code);
      } catch {
        return m;
      }
    }
    return Object.hasOwn(ENTITIES, e) ? ENTITIES[e] : m;
  });
}

export function escapeText(s) {
  return String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
}

export function escapeAttr(s) {
  return escapeText(s).replace(/"/g, '&quot;');
}

/**
 * Parses XML into { name, attrs: [[name, value]], children: [node] } where
 * text nodes are { text } (CDATA included). Returns the root element.
 * Throws XmlError on malformed input.
 */
export function parseXml(input, { maxDepth = 64 } = {}) {
  const s = String(input).replace(/^\ufeff/, '');
  let i = 0;
  const root = { name: '#document', attrs: [], children: [] };
  const stack = [root];

  const top = () => stack[stack.length - 1];

  while (i < s.length) {
    const lt = s.indexOf('<', i);
    if (lt < 0) {
      pushText(s.slice(i));
      break;
    }
    if (lt > i) pushText(s.slice(i, lt));
    if (s.startsWith('<!--', lt)) {
      const end = s.indexOf('-->', lt + 4);
      if (end < 0) throw new XmlError('unterminated comment');
      i = end + 3;
    } else if (s.startsWith('<![CDATA[', lt)) {
      const end = s.indexOf(']]>', lt + 9);
      if (end < 0) throw new XmlError('unterminated CDATA');
      top().children.push({ text: s.slice(lt + 9, end) });
      i = end + 3;
    } else if (s.startsWith('<?', lt)) {
      const end = s.indexOf('?>', lt + 2);
      if (end < 0) throw new XmlError('unterminated processing instruction');
      i = end + 2;
    } else if (s.startsWith('<!', lt)) {
      const end = s.indexOf('>', lt + 2);
      if (end < 0) throw new XmlError('unterminated declaration');
      if (s.slice(lt, end).includes('[')) throw new XmlError('internal DTD subsets are not supported');
      i = end + 1;
    } else if (s[lt + 1] === '/') {
      const end = s.indexOf('>', lt + 2);
      if (end < 0) throw new XmlError('unterminated end tag');
      const name = s.slice(lt + 2, end).trim();
      const open = top();
      if (stack.length <= 1 || open.name !== name) throw new XmlError(`mismatched end tag </${name}>`);
      stack.pop();
      i = end + 1;
    } else {
      // Start tag: name, attributes, optional "/".
      let j = lt + 1;
      const nameMatch = /^[^\s/>]+/.exec(s.slice(j, j + 256));
      if (!nameMatch) throw new XmlError('bad start tag');
      const name = nameMatch[0];
      j += name.length;
      const attrs = [];
      let selfClosing = false;
      for (;;) {
        while (j < s.length && /\s/.test(s[j])) j++;
        if (j >= s.length) throw new XmlError('unterminated start tag');
        if (s[j] === '>') {
          j++;
          break;
        }
        if (s[j] === '/' && s[j + 1] === '>') {
          selfClosing = true;
          j += 2;
          break;
        }
        const an = /^[^\s=/>]+/.exec(s.slice(j, j + 256));
        if (!an) throw new XmlError('bad attribute');
        j += an[0].length;
        while (j < s.length && /\s/.test(s[j])) j++;
        if (s[j] !== '=') throw new XmlError(`attribute ${an[0]} without value`);
        j++;
        while (j < s.length && /\s/.test(s[j])) j++;
        const q = s[j];
        if (q !== '"' && q !== "'") throw new XmlError('unquoted attribute value');
        const end = s.indexOf(q, j + 1);
        if (end < 0) throw new XmlError('unterminated attribute value');
        attrs.push([an[0], decodeEntities(s.slice(j + 1, end))]);
        j = end + 1;
      }
      const el = { name, attrs, children: [] };
      top().children.push(el);
      if (!selfClosing) {
        if (stack.length > maxDepth) throw new XmlError('too deep');
        stack.push(el);
      }
      i = j;
    }
  }
  if (stack.length !== 1) throw new XmlError(`unclosed element <${top().name}>`);
  const elements = root.children.filter((c) => c.name);
  if (elements.length !== 1) throw new XmlError('expected exactly one root element');
  return elements[0];

  function pushText(raw) {
    if (stack.length === 1) {
      if (raw.trim() !== '') throw new XmlError('text outside the root element');
      return;
    }
    top().children.push({ text: decodeEntities(raw) });
  }
}

/** Local name without a namespace prefix. */
export function localName(name) {
  const c = name.indexOf(':');
  return c < 0 ? name : name.slice(c + 1);
}

export function getAttr(el, name) {
  for (const [k, v] of el.attrs) if (k === name) return v;
  return undefined;
}

export function childElements(el, local) {
  return el.children.filter((c) => c.name && (local === undefined || localName(c.name) === local));
}

export function textOf(el) {
  let out = '';
  for (const c of el.children) {
    if (c.text !== undefined) out += c.text;
    else if (c.name) out += textOf(c);
  }
  return out;
}

/** Serialises a tree from parseXml (indent with two spaces). */
export function serializeXml(el, depth = 0) {
  const pad = '  '.repeat(depth);
  const attrs = el.attrs.map(([k, v]) => ` ${k}="${escapeAttr(v)}"`).join('');
  const kids = el.children.filter((c) => c.name || (c.text !== undefined && c.text.trim() !== ''));
  if (kids.length === 0) return `${pad}<${el.name}${attrs}/>`;
  if (kids.length === 1 && kids[0].text !== undefined) {
    return `${pad}<${el.name}${attrs}>${escapeText(kids[0].text.trim())}</${el.name}>`;
  }
  const inner = kids
    .map((c) => (c.name ? serializeXml(c, depth + 1) : `${pad}  ${escapeText(c.text.trim())}`))
    .join('\n');
  return `${pad}<${el.name}${attrs}>\n${inner}\n${pad}</${el.name}>`;
}
