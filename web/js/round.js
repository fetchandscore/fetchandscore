// The scoring screen.
//
// The tap must feel instant. Everything here is optimistic: the score updates
// on the tap and the write goes into the queue, because a scorekeeper cannot
// wait on a round trip while a dog is already running the next throw.

import { ApiError, api } from './api.js';
import { newClientId, WriteQueue } from './queue.js';
import { pointsFor, ZONE_BUTTONS } from './scoring-table.js';
import { Connection, watchSession } from './stream.js';
import { CueAudio, formatClock, GRACE_SECONDS, Phase, RoundTimer, ScreenLock } from './timer.js';

export function roundScreen() {
  // These live in the closure, not on the returned object, and deliberately so.
  //
  // Alpine wraps component state in a reactive Proxy, and JavaScript private
  // fields (#persist, #frame) throw "Receiver must be an instance of" when
  // touched through one. Keeping the machinery out here sidesteps that, and is
  // honest besides: none of it is view state. Anything the template needs is
  // copied onto reactive properties by the callbacks below.
  let queue = null;
  let timer = null;
  let audio = null;
  let lock = null;
  let stream = null;

  return {
    // --- state ---
    loading: true,
    error: null,
    session: null,
    team: null,
    round: null,
    throws: [],
    connection: Connection.CONNECTING,
    pending: 0,
    queueError: null,
    dropped: 0,
    muted: false,
    audioReady: false,
    clock: '0:00',
    phase: Phase.READY,
    prerollCount: null,
    urgent: false,
    zones: ZONE_BUTTONS,

    // --- internals ---
    roundId: null,

    async init() {
      const params = new URLSearchParams(location.search);
      this.roundId = params.get('round');
      const sessionId = params.get('session');

      if (!this.roundId || !sessionId) {
        this.error = 'This link is missing a round.';
        this.loading = false;
        return;
      }

      try {
        await this.load(sessionId);
      } catch (err) {
        if (err instanceof ApiError && err.isAuth) {
          location.href = `auth.html?next=${encodeURIComponent(location.pathname + location.search)}`;
          return;
        }
        this.error = err.message;
        this.loading = false;
        return;
      }

      this.setupQueue();
      this.setupTimer();
      this.setupStream(sessionId);
      this.loading = false;

      // Draining on reconnect covers the case that matters most: the phone
      // went into a tunnel mid-round and came back with taps still queued.
      addEventListener('online', () => void queue.drain());
    },

    async load(sessionId) {
      const session = await api.session(sessionId);
      this.session = session;

      for (const team of session.teams) {
        const found = team.rounds.find((r) => String(r.id) === String(this.roundId));
        if (found) {
          this.team = team;
          this.round = found;
          break;
        }
      }
      if (!this.round) throw new Error('That round is not part of this session.');
    },

    setupQueue() {
      queue = new WriteQueue(this.roundId, async (item) => {
        if (item.kind === 'void') {
          await api.voidThrow(this.roundId, item.throwId);
        } else {
          const result = await api.addThrow(this.roundId, {
            client_id: item.client_id,
            zone: item.zone,
            air: item.air,
            recorded_at: item.recorded_at,
          });
          // The server's total is authoritative; the optimistic one was a
          // guess made without knowing about a 90:/5 cap or a tiny cap.
          this.round.total_points = result.round.total_points;
          const local = this.throws.find((t) => t.client_id === item.client_id);
          if (local) local.id = result.throw.id;
        }
      });

      queue.onChange = (q) => {
        this.pending = q.pending;
        this.queueError = q.lastError?.message ?? null;
        this.dropped = q.dropped.length;
      };
      this.pending = queue.pending;
      void queue.drain();
    },

    setupTimer() {
      audio = new CueAudio('audio');
      this.muted = audio.muted;

      timer = new RoundTimer({
        roundSeconds: this.session.format.round_seconds,
        cueSeconds: this.session.format.cue_seconds,
        audio: audio,
      });

      timer.onTick = (t) => {
        this.clock = formatClock(t.remaining);
        this.prerollCount = t.prerollCount;
        // The last ten seconds get a pulsing clock, visible from the sideline
        // without having to read it.
        this.urgent = t.phase === Phase.RUNNING && t.remaining <= 10;
      };

      timer.onPhase = (phase) => {
        this.phase = phase;
        if (phase === Phase.GRACE) {
          // The clock is done but the rules count a throw released before the
          // "T" in TIME, so entry stays open until a human confirms.
          void api.graceRound(this.roundId).catch(() => {});
        }
      };

      lock = new ScreenLock();

      // A round already running on another device: adopt its clock so a
      // spectator's screen tracks the scorekeeper's.
      if (this.round.status === 'running' && this.round.started_at) {
        this.adoptRunningRound();
      } else if (this.round.status === 'confirmed') {
        this.phase = Phase.DONE;
      }
    },

    adoptRunningRound() {
      const startedAtMs = Date.parse(this.round.started_at);
      const elapsedMs = Date.now() - startedAtMs;
      // RoundTimer measures from its own monotonic origin, so rebase its start
      // into the past by however long the round has already been going.
      timer.startedAt = performance.now() - elapsedMs;
      timer.phase = Phase.RUNNING;
      this.phase = Phase.RUNNING;
      timer.stopLoop();
      timer.start = () => {};
      timer.onTick(timer);
    },

    setupStream(sessionId) {
      stream = watchSession(
        sessionId,
        {
          'throw.added': (payload) => this.applyRemote(payload),
          'throw.voided': (payload) => this.applyRemote(payload),
          'round.started': (payload) => this.applyRemote(payload),
          'round.confirmed': (payload) => this.applyRemote(payload),
          'round.reset': (payload) => this.applyRemote(payload),
        },
        (state) => {
          this.connection = state;
        },
      );

      addEventListener('pagehide', () => stream?.close());
    },

    /** Applies an event about this round from another device. */
    applyRemote(payload) {
      const round = payload?.round;
      if (!round || String(round.id) !== String(this.roundId)) return;

      // Only trust the remote total when nothing local is still in flight;
      // otherwise it would briefly undo the optimistic score on screen.
      if (this.pending === 0) {
        this.round.total_points = round.total_points;
      }
      this.round.status = round.status;
      if (round.status === 'confirmed') {
        this.phase = Phase.DONE;
        timer?.finish();
      }
    },

    // --- actions ---

    get canScore() {
      return this.phase === Phase.RUNNING || this.phase === Phase.GRACE;
    },

    get graceRemaining() {
      if (this.phase !== Phase.GRACE || !timer) return 0;
      return Math.max(0, Math.ceil(GRACE_SECONDS + timer.remaining));
    },

    async start() {
      // Audio must be unlocked inside the tap. Any later and the browser has
      // already decided this page does not have permission to make noise.
      this.audioReady = await audio.unlock();

      await api.startRound(this.roundId).catch((err) => {
        this.queueError = err.message;
      });

      this.round.status = 'running';
      timer.start();
      void lock.acquire();
    },

    record(zone, air) {
      if (!this.canScore) return;

      const entry = {
        client_id: newClientId(),
        zone,
        air,
        recorded_at: new Date().toISOString(),
      };

      // Optimistic: the tally and the strip update now, the write catches up.
      this.throws.push({ ...entry, id: null, void: false });
      this.round.total_points += pointsFor(zone, air, {
        tiny: this.team.tiny,
        division: this.team.division,
      });

      queue.enqueue(entry);
      this.buzz();
    },

    undo() {
      const last = [...this.throws].reverse().find((t) => !t.void);
      if (!last || !this.canScore) return;

      last.void = true;
      this.round.total_points -= pointsFor(last.zone, last.air, {
        tiny: this.team.tiny,
        division: this.team.division,
      });

      // A throw with no server id yet has not landed, so there is nothing to
      // void remotely; the queued write is simply dropped instead.
      if (last.id) {
        queue.enqueue({ kind: 'void', throwId: last.id });
      }
      this.buzz();
    },

    /** A short haptic tick, so a tap is felt as well as seen. */
    buzz() {
      navigator.vibrate?.(12);
    },

    async falseStart() {
      if (!confirm('Reset this round? Every throw recorded so far is discarded.')) return;

      timer.reset();
      queue.clear();
      this.throws = [];
      this.round.total_points = 0;
      this.round.status = 'ready';
      this.phase = Phase.READY;
      this.clock = formatClock(this.session.format.round_seconds);

      await api.resetRound(this.roundId).catch((err) => {
        this.queueError = err.message;
      });
    },

    async confirm() {
      // Every queued throw must land before the round locks, or the server
      // would reject the stragglers against a confirmed round.
      await queue.drain();

      if (queue.pending > 0) {
        this.queueError = 'Still saving throws. Check your connection and try again.';
        return;
      }

      try {
        const result = await api.confirmRound(this.roundId);
        this.round = { ...this.round, ...result.round };
        this.phase = Phase.DONE;
        timer.finish();
        await lock.release();
      } catch (err) {
        this.queueError = err.message;
      }
    },

    toggleMute() {
      this.muted = !this.muted;
      audio.setMuted(this.muted);
    },

    // --- display helpers ---

    get liveThrows() {
      return this.throws.filter((t) => !t.void);
    },

    get scoreText() {
      const value = this.round?.total_points ?? 0;
      // Half points are real here: 5.5 must not render as 6.
      return Number.isInteger(value) ? String(value) : value.toFixed(1);
    },

    /** What a zone button is worth, formatted for display. */
    zonePoints(zone, air) {
      const value = pointsFor(zone, air, {
        tiny: this.team.tiny,
        division: this.team.division,
      });
      return Number.isInteger(value) ? String(value) : value.toFixed(1);
    },

    shortLabel(t) {
      if (t.zone === 'miss') return 'X';
      const points = pointsFor(t.zone, t.air, {
        tiny: this.team.tiny,
        division: this.team.division,
      });
      return Number.isInteger(points) ? String(points) : points.toFixed(1);
    },

    get statusLabel() {
      return {
        [Phase.READY]: 'Ready',
        [Phase.PREROLL]: 'Get set',
        [Phase.RUNNING]: 'Running',
        [Phase.GRACE]: 'Time — last throw counts',
        [Phase.DONE]: 'Confirmed',
      }[this.phase];
    },
  };
}
