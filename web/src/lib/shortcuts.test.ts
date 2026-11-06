import { describe, expect, it } from 'vitest';
import { adjacentClip, commandFor, type KeyLike } from './shortcuts';

const ev = (key: string, over: Partial<KeyLike> = {}): KeyLike => ({
  key,
  ctrlKey: false,
  metaKey: false,
  altKey: false,
  defaultPrevented: false,
  target: null,
  ...over,
});
const el = (tag: string): EventTarget =>
  ({ tagName: tag.toUpperCase(), isContentEditable: false }) as unknown as EventTarget;

describe('commandFor', () => {
  it('maps the ui-spec §4.7 keys', () => {
    expect(commandFor(ev(' '))).toBe('toggle');
    expect(commandFor(ev('ArrowLeft'))).toBe('back10');
    expect(commandFor(ev('ArrowRight'))).toBe('fwd10');
    expect(commandFor(ev('j'))).toBe('prev');
    expect(commandFor(ev('J'))).toBe('prev');
    expect(commandFor(ev('k'))).toBe('next');
    expect(commandFor(ev('K'))).toBe('next');
    expect(commandFor(ev('['))).toBe('zoomOut');
    expect(commandFor(ev(']'))).toBe('zoomIn');
    expect(commandFor(ev('t'))).toBe('now');
    expect(commandFor(ev('T'))).toBe('now');
  });
  it('ignores other keys and modified keys', () => {
    expect(commandFor(ev('x'))).toBeNull();
    expect(commandFor(ev('j', { ctrlKey: true }))).toBeNull();
    expect(commandFor(ev('j', { metaKey: true }))).toBeNull();
    expect(commandFor(ev('j', { altKey: true }))).toBeNull();
  });
  it('leaves typing in form fields alone', () => {
    expect(commandFor(ev('j', { target: el('input') }))).toBeNull();
    expect(commandFor(ev('k', { target: el('textarea') }))).toBeNull();
    expect(commandFor(ev('ArrowLeft', { target: el('select') }))).toBeNull();
  });
  it('leaves Space to a focused button or link', () => {
    expect(commandFor(ev(' ', { target: el('button') }))).toBeNull();
    expect(commandFor(ev(' ', { target: el('a') }))).toBeNull();
    expect(commandFor(ev('j', { target: el('button') }))).toBe('prev');
  });
  it('leaves keys another handler already used', () => {
    expect(commandFor(ev('ArrowLeft', { defaultPrevented: true }))).toBeNull();
  });
});

describe('adjacentClip', () => {
  const clips = [
    { id: 'b', startMs: 200 },
    { id: 'a', startMs: 100 },
    { id: 'c', startMs: 300 },
  ];
  it('J is the previous clip in time and K the next, whatever the array order', () => {
    expect(adjacentClip(clips, 'b', 'prev')).toBe('a');
    expect(adjacentClip(clips, 'b', 'next')).toBe('c');
  });
  it('stays on the first and last clip instead of wrapping', () => {
    expect(adjacentClip(clips, 'a', 'prev')).toBe('a');
    expect(adjacentClip(clips, 'c', 'next')).toBe('c');
  });
  it('starts from the newest clip when nothing is selected', () => {
    expect(adjacentClip(clips, null, 'prev')).toBe('c');
    expect(adjacentClip(clips, null, 'next')).toBe('c');
  });
  it('starts from the newest clip when the selection is not in the list', () => {
    expect(adjacentClip(clips, 'zzz', 'next')).toBe('c');
  });
  it('is null for an empty list', () => {
    expect(adjacentClip([], null, 'next')).toBeNull();
  });
});
