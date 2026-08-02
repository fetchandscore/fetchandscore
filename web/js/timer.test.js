import test from 'node:test';
import assert from 'node:assert/strict';

import { buildCueSchedule, cueForSecond, formatClock, PREROLL_SECONDS } from './timer.js';

test('the preroll speaks ready, set, go before the clock starts', () => {
  const schedule = buildCueSchedule(60, []);
  const preroll = schedule.filter((s) => s.at < PREROLL_SECONDS);

  assert.deepEqual(
    preroll.map((s) => s.cue),
    ['ready', 'set', 'go'],
  );
  assert.equal(preroll[2].at, 2, 'GO must land exactly as the clock starts');
});

test('the official 60-second calls land at the right moments', () => {
  const schedule = buildCueSchedule(60, [30, 10, 5, 4, 3, 2, 1]);
  const at = Object.fromEntries(schedule.map((s) => [s.cue, s.at]));

  // Offsets are from START, so each is the preroll plus elapsed round time.
  assert.equal(at.thirty, PREROLL_SECONDS + 30, '"thirty seconds" must be at 30 remaining');
  assert.equal(at.ten, PREROLL_SECONDS + 50);
  assert.equal(at.five, PREROLL_SECONDS + 55);
  assert.equal(at.one, PREROLL_SECONDS + 59);
  assert.equal(at.time, PREROLL_SECONDS + 60, 'TIME must land as the clock hits zero');
});

test('a 90-second round gets the extra sixty-second call', () => {
  const schedule = buildCueSchedule(90, [60, 30, 10, 5, 4, 3, 2, 1]);
  const at = Object.fromEntries(schedule.map((s) => [s.cue, s.at]));

  assert.equal(at.sixty, PREROLL_SECONDS + 30);
  assert.equal(at.time, PREROLL_SECONDS + 90);
});

test('a custom two-minute format can warn at thirty and fifteen', () => {
  const schedule = buildCueSchedule(120, [30, 15, 5, 4, 3, 2, 1]);
  const at = Object.fromEntries(schedule.map((s) => [s.cue, s.at]));

  assert.equal(at.thirty, PREROLL_SECONDS + 90);
  assert.equal(at.fifteen, PREROLL_SECONDS + 105);
  assert.equal(at.time, PREROLL_SECONDS + 120);
});

test('a cue mark at or beyond the round length is dropped', () => {
  // A 60-second mark in a 60-second round would fire during the preroll.
  const schedule = buildCueSchedule(60, [60, 90, 30]);
  const cues = schedule.map((s) => s.cue);

  assert.ok(!cues.includes('sixty'), 'a mark equal to the round length must not be scheduled');
  assert.ok(cues.includes('thirty'));
});

test('the schedule is ordered, so cues can be scheduled in one pass', () => {
  const schedule = buildCueSchedule(90, [60, 30, 10, 5, 4, 3, 2, 1]);
  const offsets = schedule.map((s) => s.at);

  assert.deepEqual(offsets, [...offsets].sort((a, b) => a - b));
});

test('only the marks with recorded audio are spoken', () => {
  assert.equal(cueForSecond(30), 'thirty');
  assert.equal(cueForSecond(1), 'one');
  assert.equal(cueForSecond(47), null, 'an unrecorded mark must not produce a missing cue');
});

test('the clock reads as minutes and padded seconds', () => {
  assert.equal(formatClock(90), '1:30');
  assert.equal(formatClock(60), '1:00');
  assert.equal(formatClock(9), '0:09');
  assert.equal(formatClock(0), '0:00');
});

test('the clock never shows a negative time during grace', () => {
  assert.equal(formatClock(-5), '0:00');
});

test('the clock rounds up, so it shows 1:00 until a full second has passed', () => {
  // Showing 0:59 the instant a 60-second round starts would look broken.
  assert.equal(formatClock(59.4), '1:00');
  assert.equal(formatClock(58.9), '0:59');
});
