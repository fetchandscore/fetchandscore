// The round timer and its spoken cues.
//
// Two things have to be right, and they are not the same thing:
//
//   1. The displayed clock, which is read by a human and can drift a frame.
//   2. The audio cues, which must land on the second. A late "3" during a
//      5-4-3-2-1 countdown is worse than no countdown at all.
//
// So the display is driven by requestAnimationFrame against a wall-clock
// delta, while every cue for the whole round is scheduled up front on the
// WebAudio clock the moment the round starts. That clock runs on the audio
// thread, so it is immune to a garbage collection pause or a busy main thread.

/** The sprite and its offset map, fetched once and decoded once. */
export class CueAudio {
  constructor(basePath = 'audio') {
    this.basePath = basePath.replace(/\/$/, '');
    this.context = null;
    this.buffer = null;
    this.offsets = null;
    this.muted = loadMuted();
    this.scheduled = [];
  }

  get ready() {
    return this.buffer !== null;
  }

  /**
   * Prepares audio. Must be called from a user gesture: browsers will not let
   * a page make noise otherwise, and on iOS the context stays suspended until
   * a real tap resumes it.
   */
  async unlock() {
    if (!this.context) {
      const Ctx = globalThis.AudioContext || globalThis.webkitAudioContext;
      if (!Ctx) return false;
      this.context = new Ctx();
    }
    if (this.context.state === 'suspended') {
      await this.context.resume();
    }
    if (!this.buffer) {
      await this.#load();
    }
    return this.ready;
  }

  async #load() {
    const [offsets, encoded] = await Promise.all([
      fetch(`${this.basePath}/offsets.json`).then((r) => r.json()),
      fetch(`${this.basePath}/${this.#preferredFile()}`).then((r) => r.arrayBuffer()),
    ]);
    this.offsets = offsets.cues;
    this.buffer = await this.context.decodeAudioData(encoded);
  }

  // Opus in WebM everywhere it works; AAC for Safari.
  #preferredFile() {
    const probe = document.createElement('audio');
    if (probe.canPlayType('audio/webm; codecs=opus')) return 'cues.webm';
    return 'cues.m4a';
  }

  setMuted(muted) {
    this.muted = muted;
    saveMuted(muted);
    if (muted) this.cancelScheduled();
  }

  /**
   * Schedules one cue to play `delaySeconds` from now.
   *
   * Scheduling on the audio clock rather than via setTimeout is the entire
   * point: setTimeout in a background or busy tab is throttled to whole
   * seconds or worse, which would scatter a countdown.
   */
  play(key, delaySeconds = 0) {
    if (this.muted || !this.ready) return;
    const cue = this.offsets?.[key];
    if (!cue) return;

    const source = this.context.createBufferSource();
    source.buffer = this.buffer;
    source.connect(this.context.destination);
    source.start(this.context.currentTime + Math.max(0, delaySeconds), cue.offset, cue.duration);

    this.scheduled.push(source);
    source.onended = () => {
      this.scheduled = this.scheduled.filter((s) => s !== source);
    };
  }

  /** Stops everything pending. Used on a false start or a manual stop. */
  cancelScheduled() {
    for (const source of this.scheduled) {
      try {
        source.stop();
      } catch {
        // Already finished; nothing to stop.
      }
    }
    this.scheduled = [];
  }
}

const MUTE_KEY = 'fns.muted';

function loadMuted() {
  try {
    // Default to unmuted on the device doing the scoring. Spectators are
    // muted explicitly by the page that knows it is only watching.
    return globalThis.localStorage?.getItem(MUTE_KEY) === '1';
  } catch {
    return false;
  }
}

function saveMuted(muted) {
  try {
    globalThis.localStorage?.setItem(MUTE_KEY, muted ? '1' : '0');
  } catch {
    // Nothing to do; the setting simply will not persist.
  }
}

/** Cue key for a remaining-seconds mark, or null if nothing is spoken then. */
export function cueForSecond(second) {
  return (
    {
      60: 'sixty',
      30: 'thirty',
      15: 'fifteen',
      10: 'ten',
      5: 'five',
      4: 'four',
      3: 'three',
      2: 'two',
      1: 'one',
    }[second] ?? null
  );
}

/** How long the "Ready, set, GO" preamble runs before the clock starts. */
export const PREROLL_SECONDS = 3;

/**
 * Builds the full schedule for a round, as offsets in seconds from the moment
 * START is tapped.
 *
 * The preroll occupies the first three seconds; the clock begins after it.
 *
 * @param {number} roundSeconds
 * @param {number[]} cueSeconds   remaining-time marks that should be spoken
 */
export function buildCueSchedule(roundSeconds, cueSeconds) {
  const schedule = [
    { at: 0, cue: 'ready' },
    { at: 1, cue: 'set' },
    { at: 2, cue: 'go' },
  ];

  for (const second of cueSeconds ?? []) {
    // A mark at or beyond the round length would fire during the preroll.
    if (second <= 0 || second >= roundSeconds) continue;
    const cue = cueForSecond(second);
    if (cue) schedule.push({ at: PREROLL_SECONDS + (roundSeconds - second), cue });
  }

  schedule.push({ at: PREROLL_SECONDS + roundSeconds, cue: 'time' });
  return schedule.sort((a, b) => a.at - b.at);
}

/** Phases of a round, in order. */
export const Phase = Object.freeze({
  READY: 'ready',
  PREROLL: 'preroll',
  RUNNING: 'running',
  GRACE: 'grace',
  DONE: 'done',
});

/**
 * How long entry stays open after the clock hits zero.
 *
 * The rules count a throw released before the "T" in TIME, so there is almost
 * always one more catch to record. Entry closes when a human confirms, not
 * when the clock does.
 */
export const GRACE_SECONDS = 15;

/**
 * Drives one round's clock.
 *
 * Deliberately owns no DOM: it exposes state and calls back, so it can be
 * tested against a fake clock.
 */
export class RoundTimer {
  /**
   * @param {object} options
   * @param {number} options.roundSeconds
   * @param {number[]} options.cueSeconds
   * @param {CueAudio} [options.audio]
   * @param {() => number} [options.now]  monotonic milliseconds
   */
  constructor({ roundSeconds, cueSeconds, audio, now }) {
    this.roundSeconds = roundSeconds;
    this.cueSeconds = cueSeconds ?? [];
    this.audio = audio ?? null;
    // performance.now is monotonic: it does not jump when the device syncs its
    // wall clock, which Date.now can do mid-round.
    this.now = now ?? (() => performance.now());

    this.phase = Phase.READY;
    this.startedAt = null;
    this.onTick = () => {};
    this.onPhase = () => {};
    this.#frame = null;
  }

  #frame;

  /** Seconds elapsed since START, preroll included. */
  get elapsed() {
    if (this.startedAt === null) return 0;
    return (this.now() - this.startedAt) / 1000;
  }

  /** Seconds left on the round clock, negative once in grace. */
  get remaining() {
    if (this.startedAt === null) return this.roundSeconds;
    return this.roundSeconds - Math.max(0, this.elapsed - PREROLL_SECONDS);
  }

  /** Countdown shown during the preroll, 3 to 1, else null. */
  get prerollCount() {
    if (this.phase !== Phase.PREROLL) return null;
    return Math.max(1, Math.ceil(PREROLL_SECONDS - this.elapsed));
  }

  /**
   * Starts the round.
   *
   * Every cue is scheduled here, in one pass, rather than being fired as the
   * clock passes each mark. That is what makes the countdown accurate.
   */
  start() {
    this.startedAt = this.now();
    this.#setPhase(Phase.PREROLL);

    if (this.audio?.ready) {
      for (const { at, cue } of buildCueSchedule(this.roundSeconds, this.cueSeconds)) {
        this.audio.play(cue, at);
      }
    }

    this.#loop();
  }

  /** Abandons the round: a false start, or a manual stop. */
  reset() {
    this.stopLoop();
    this.audio?.cancelScheduled();
    this.startedAt = null;
    this.#setPhase(Phase.READY);
  }

  /** Closes the round for entry. */
  finish() {
    this.stopLoop();
    this.audio?.cancelScheduled();
    this.#setPhase(Phase.DONE);
  }

  stopLoop() {
    if (this.#frame !== null) {
      cancelAnimationFrame(this.#frame);
      this.#frame = null;
    }
  }

  #loop() {
    const step = () => {
      const elapsed = this.elapsed;

      if (this.phase === Phase.PREROLL && elapsed >= PREROLL_SECONDS) {
        this.#setPhase(Phase.RUNNING);
      }
      if (this.phase === Phase.RUNNING && this.remaining <= 0) {
        this.#setPhase(Phase.GRACE);
      }
      if (this.phase === Phase.GRACE && this.remaining <= -GRACE_SECONDS) {
        // Entry stays open; this only stops the display counting further into
        // the negative. Only a human confirms the round.
        this.stopLoop();
        this.onTick(this);
        return;
      }

      this.onTick(this);
      this.#frame = requestAnimationFrame(step);
    };
    this.#frame = requestAnimationFrame(step);
  }

  #setPhase(phase) {
    if (this.phase === phase) return;
    this.phase = phase;
    this.onPhase(phase, this);
  }
}

/** Formats seconds as m:ss for the clock. */
export function formatClock(seconds) {
  const clamped = Math.max(0, Math.ceil(seconds));
  const m = Math.floor(clamped / 60);
  const s = clamped % 60;
  return `${m}:${String(s).padStart(2, '0')}`;
}

/**
 * Keeps the screen awake during a round.
 *
 * A phone that sleeps thirty seconds into a round is the single most annoying
 * thing this app could do.
 */
export class ScreenLock {
  constructor() {
    this.sentinel = null;
  }

  async acquire() {
    try {
      this.sentinel = await navigator.wakeLock?.request('screen');
      // The lock is dropped whenever the tab is hidden, so it has to be
      // retaken when the user comes back.
      document.addEventListener('visibilitychange', this.#reacquire);
    } catch {
      // Unsupported, or refused because the battery is critical. The round
      // still works; the screen may just dim.
    }
  }

  #reacquire = async () => {
    if (document.visibilityState === 'visible' && this.sentinel?.released !== false) {
      try {
        this.sentinel = await navigator.wakeLock?.request('screen');
      } catch {
        // Same as above.
      }
    }
  };

  async release() {
    document.removeEventListener('visibilitychange', this.#reacquire);
    try {
      await this.sentinel?.release();
    } catch {
      // Already gone.
    }
    this.sentinel = null;
  }
}
