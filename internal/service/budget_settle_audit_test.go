package service

// r53 ① regressions (2026-10-21) for the r47 residue forensics: 28 deferred
// team/org settles (17 requests) received enqueueSettleRetry=true at the
// Redis-unpause boundary and then vanished with NO terminal log — no
// RECOVERED, no EXPIRED, no LOST — while 700 sibling entries recovered in
// one burst at 09:08:10.24. Whatever the trigger (consumer stall/death at
// the handoff), the CONTRACT these tests lock in is: every deferred settle
// task ends in exactly one auditable terminal state, and no removal from
// the ledger or the memory queue is ever unlogged.
//
// T9  consumer panic is contained, the task is re-persisted, the consumer
//     continues (a dead consumer used to silence every later task);
// T10 malformed ledger entries are dropped LOUDLY (counter + member bytes);
// T11 a memory-queue task becomes Redis-persistent BEFORE its first settle
//     attempt (consumer death after that point cannot orphan it), and the
//     upsert is removed at the terminal state;
// T12 accounting invariant: every deferred task reaches exactly one
//     terminal state — settledmark present, ledger empty, channel drained.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"
)

func newAuditBudget(t *testing.T) (*BudgetService, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	return &BudgetService{rdb: rdb, markerTTL: defaultMarkerTTL}, rdb
}

func auditTask(scope, reqID string, deadline time.Time) settleRetryTask {
	return settleRetryTask{
		scope: scope, targetID: "902", period: "monthly", pk: "2026-10",
		reqID: reqID, cost: 0.002992, deadline: deadline,
	}
}

// T9: a task whose processing panics must not kill the consumer; the panic
// path re-persists the task to the ledger so the drain worker owns it, and
// the next task proceeds normally.
func TestSettleAudit_T9_ConsumerPanicContainedAndTaskRepersisted(t *testing.T) {
	s, rdb := newAuditBudget(t)
	panics := 0
	settleTaskProbe = func(t2 settleRetryTask) {
		if t2.reqID == "panic1" {
			panics++
			panic("injected consumer fault")
		}
	}
	t.Cleanup(func() { settleTaskProbe = nil })

	// The panicking task: consumeSettleTask must recover, log loudly and
	// re-persist it to the ledger.
	s.consumeSettleTask(auditTask("org", "panic1", time.Now().Add(10*time.Minute)))
	if panics != 1 {
		t.Fatalf("probe fired %d times, want 1", panics)
	}
	if n := rdb.ZCard(context.Background(), settleLedgerZSet).Val(); n != 1 {
		t.Fatalf("after panic, ledger holds %d entries, want 1 (task re-persisted)", n)
	}
	// The ledger drain then recovers it to a terminal state.
	s.drainSettleLedgerOnce()
	if n := rdb.ZCard(context.Background(), settleLedgerZSet).Val(); n != 0 {
		t.Fatalf("ledger after drain = %d, want 0", n)
	}
	if rdb.Exists(context.Background(), settledMarkKey("org", "902", "panic1")).Val() != 1 {
		t.Fatal("panicking task was never settled by the ledger drain")
	}

	// The consumer continues with the next task (no goroutine death).
	s.consumeSettleTask(auditTask("team", "after1", time.Now().Add(10*time.Minute)))
	if rdb.Exists(context.Background(), settledMarkKey("team", "902", "after1")).Val() != 1 {
		t.Fatal("consumer did not survive the panic (next task unsettled)")
	}
	if n := rdb.ZCard(context.Background(), settleLedgerZSet).Val(); n != 0 {
		t.Fatalf("terminal state must remove the ledger upsert, left %d", n)
	}
}

// T10: malformed ledger members are dropped LOUDLY — counter increments and
// the drop is auditable (this was the only unlogged ledger removal path).
func TestSettleAudit_T10_MalformedLedgerEntryDroppedLoudly(t *testing.T) {
	s, rdb := newAuditBudget(t)
	rdb.ZAdd(context.Background(), settleLedgerZSet, redis.Z{
		Score: float64(time.Now().Unix()), Member: "garbage-not-a-member",
	})
	rdb.ZAdd(context.Background(), settleLedgerZSet, redis.Z{
		Score: float64(time.Now().Unix()), Member: ledgerMember(auditTask("org", "good1", time.Now().Add(10*time.Minute))),
	})

	before := testutil.ToFloat64(settleLedgerMalformedTotal)
	s.drainSettleLedgerOnce()
	if got := testutil.ToFloat64(settleLedgerMalformedTotal) - before; got != 1 {
		t.Fatalf("malformed counter delta = %v, want 1", got)
	}
	if n := rdb.ZCard(context.Background(), settleLedgerZSet).Val(); n != 0 {
		t.Fatalf("ledger = %d after drain, want 0 (garbage dropped, good entry recovered)", n)
	}
}

// T11: a Tier2 (memory-queue) task must be Redis-persistent BEFORE its
// first settle attempt. The probe (which runs at the top of the task,
// before any settle) asserts the ledger already holds the entry; a consumer
// death at ANY later point leaves the task in the ledger.
func TestSettleAudit_T11_Tier2TaskRepersistedBeforeFirstAttempt(t *testing.T) {
	s, rdb := newAuditBudget(t)
	seen := make(chan int64, 1)
	settleTaskProbe = func(t2 settleRetryTask) {
		seen <- rdb.ZCard(context.Background(), settleLedgerZSet).Val()
	}
	t.Cleanup(func() { settleTaskProbe = nil })

	s.processSettleTask(auditTask("org", "persist1", time.Now().Add(10*time.Minute)))
	if n := <-seen; n != 1 {
		t.Fatalf("ledger depth at task start = %d, want 1 (re-persist BEFORE first attempt)", n)
	}
	if n := rdb.ZCard(context.Background(), settleLedgerZSet).Val(); n != 0 {
		t.Fatalf("terminal state must remove the upsert, left %d", n)
	}
}

// T12: the r47 accounting invariant — every deferred task reaches exactly
// one terminal state. Deferred during an outage (Tier1 down → Tier2), then
// recovered: settledmark present for every task, ledger empty, channel
// drained.
func TestSettleAudit_T12_EveryDeferredTaskReachesTerminalState(t *testing.T) {
	mr := miniredis.RunT(t)
	deadAddr := mr.Addr()
	mr.Close() // simulate the outage: dial fails, Tier1 ZAdd errors

	rdbDead := redis.NewClient(&redis.Options{Addr: deadAddr})
	sDead := &BudgetService{rdb: rdbDead, markerTTL: defaultMarkerTTL}

	const n = 8
	dl := time.Now().Add(10 * time.Minute)
	for i := 0; i < n; i++ {
		ok := sDead.enqueueSettleRetry(auditTask("org", auditID(i), dl))
		if !ok {
			t.Fatalf("enqueue %d failed — Tier2 must absorb during the outage", i)
		}
	}
	// The Once-armed background consumer may already be draining (it shares
	// sDead, so it converges after the rdb swap below) — only sanity-bound
	// the depth here; the terminal asserts after the drain are the contract.
	if got := settleChannelDepthN.Load(); got < 1 || got > n {
		t.Fatalf("channel depth = %d after enqueue, want 1..%d", got, n)
	}

	// Recovery: Redis is back (a live server on a fresh instance); drain the
	// channel through the REAL consume path. A background consumer may also
	// have been armed by the enqueue above — both paths are idempotent, so
	// drain opportunistically until the depth hits 0.
	mr2 := miniredis.RunT(t)
	rdbLive := redis.NewClient(&redis.Options{Addr: mr2.Addr()})
	t.Cleanup(func() { rdbLive.Close() })
	sDead.rdb = rdbLive

	drainDeadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(drainDeadline) {
		for {
			var t2 settleRetryTask
			var ok bool
			select {
			case t2 = <-settleRetryQ:
				ok = true
			default:
			}
			if !ok {
				break
			}
			settleChannelDepthN.Add(-1)
			settleChannelDepth.Set(float64(settleChannelDepthN.Load()))
			sDead.consumeSettleTask(t2)
		}
		if settleChannelDepthN.Load() == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := settleChannelDepthN.Load(); got != 0 {
		t.Fatalf("channel depth after drain = %d, want 0", got)
	}
	// Terminal states strip their ledger upserts; a background consumer may
	// still be mid-task (backoff sleeps before its first live attempt), so
	// poll briefly for BOTH the empty ledger and every settledmark.
	settled := func() bool {
		if rdbLive.ZCard(context.Background(), settleLedgerZSet).Val() != 0 {
			return false
		}
		for i := 0; i < n; i++ {
			if rdbLive.Exists(context.Background(), settledMarkKey("org", "902", auditID(i))).Val() != 1 {
				return false
			}
		}
		return true
	}
	ok := false
	for i := 0; i < 200 && !ok; i++ {
		if settled() {
			ok = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !ok {
		for i := 0; i < n; i++ {
			if rdbLive.Exists(context.Background(), settledMarkKey("org", "902", auditID(i))).Val() != 1 {
				t.Fatalf("task %s has no settledmark — terminal state not reached (r47 vanish class)", auditID(i))
			}
		}
		t.Fatalf("ledger non-empty after all terminals = %d, want 0",
			rdbLive.ZCard(context.Background(), settleLedgerZSet).Val())
	}
}

func auditID(i int) string {
	return strings.Repeat("0", 7) + string(rune('a'+i))
}

// T13 (r58 review): task conservation at the handoff chain — when the
// ledger refuses AND the channel is full, the task reaches the LOUD LOST
// terminal (counter + ERROR), never a silent drop; when the ledger refuses
// but the channel has room, the task is re-enqueued (depth rises) and later
// settles.
func TestSettleAudit_T13_HandoffConservation(t *testing.T) {
	mr := miniredis.RunT(t)
	deadAddr := mr.Addr()
	mr.Close()
	rdbDead := redis.NewClient(&redis.Options{Addr: deadAddr})
	sDead := &BudgetService{rdb: rdbDead, markerTTL: defaultMarkerTTL}

	// Channel-full + ledger-refused: LOUD LOST terminal.
	oldQ := settleRetryQ
	settleRetryQ = make(chan settleRetryTask, 1)
	settleRetryQ <- auditTask("org", "occupier", time.Now().Add(time.Minute)) // full
	lostBefore := testutil.ToFloat64(settleLostTotal)
	sDead.handoffTask(auditTask("org", "conserv1", time.Now().Add(time.Minute)))
	if got := testutil.ToFloat64(settleLostTotal) - lostBefore; got != 1 {
		t.Fatalf("handoff with full channel + dead ledger must count exactly one LOST, got %v", got)
	}
	<-settleRetryQ // release the occupier

	// Ledger-refused but channel has room: re-enqueue (conserved), then
	// settle through the live path.
	mr2 := miniredis.RunT(t)
	rdbLive := redis.NewClient(&redis.Options{Addr: mr2.Addr()})
	t.Cleanup(func() { rdbLive.Close() })
	depthBefore := settleChannelDepthN.Load()
	sDead.handoffTask(auditTask("team", "conserv2", time.Now().Add(time.Minute)))
	if got := settleChannelDepthN.Load() - depthBefore; got != 1 {
		t.Fatalf("handoff with room must re-enqueue (depth +1), got %d", got)
	}
	sDead.rdb = rdbLive
	t2 := <-settleRetryQ
	settleChannelDepthN.Add(-1)
	settleChannelDepth.Set(float64(settleChannelDepthN.Load()))
	sDead.consumeSettleTask(t2)
	if rdbLive.Exists(context.Background(), settledMarkKey("team", "902", "conserv2")).Val() != 1 {
		t.Fatal("re-enqueued task was not settled (conservation broken)")
	}
	if rdbLive.ZCard(context.Background(), settleLedgerZSet).Val() != 0 {
		t.Fatal("terminal state must strip the ledger upsert")
	}
	settleRetryQ = oldQ
}

// T14 (r58 review): the persist-failure branch — a task whose ledger
// upsert fails on the first attempt (Redis down at that instant) but whose
// settle succeeds right after (client swapped to live between the upsert
// and the first attempt, via the probe) still reaches its RECOVERED
// terminal: not lost, ledger clean, settledmark present.
func TestSettleAudit_T14_PersistFailureStillSettlesAndConserves(t *testing.T) {
	_, rdb := newAuditBudget(t) // live client
	dead := miniredis.RunT(t)
	deadAddr := dead.Addr()
	dead.Close()
	deadClient := redis.NewClient(&redis.Options{Addr: deadAddr})
	sd := &BudgetService{rdb: deadClient, markerTTL: defaultMarkerTTL}

	swapped := make(chan struct{})
	settleTaskProbe = func(t2 settleRetryTask) {
		// Runs AFTER the first persist attempt (dead → fails) and BEFORE
		// the first settle attempt: swap to the live client so only the
		// upsert failed.
		select {
		case <-swapped:
		default:
			close(swapped)
			sd.rdb = rdb
		}
	}
	t.Cleanup(func() { settleTaskProbe = nil })

	lostBefore := testutil.ToFloat64(settleLostTotal)
	sd.processSettleTask(auditTask("org", "persist-fail-1", time.Now().Add(10*time.Minute)))
	if got := testutil.ToFloat64(settleLostTotal) - lostBefore; got != 0 {
		t.Fatalf("persist-failure path must not lose the task, LOST delta %v", got)
	}
	if rdb.Exists(context.Background(), settledMarkKey("org", "902", "persist-fail-1")).Val() != 1 {
		t.Fatal("task with failed upsert still settled (conservation broken)")
	}
	if rdb.ZCard(context.Background(), settleLedgerZSet).Val() != 0 {
		t.Fatal("terminal must leave the ledger clean")
	}
	select {
	case <-swapped:
	default:
		t.Fatal("probe never ran —test setup broken")
	}
}

// T16 (request-id collision fix): two INDEPENDENT requests on the same
// scope/target — even with byte-identical client-supplied ids in their
// transport layer — carry distinct internal billing ids and are each
// billed exactly once; a replay of one event settles once (settledmark NX)
// and never touches the other's markers.
func TestSettleAudit_T16_IndependentRequestsEachBilledOnce(t *testing.T) {
	s, rdb := newAuditBudget(t)
	a := auditTask("org", "11111111-2222-4333-8444-555555555555", time.Now().Add(10*time.Minute))
	b := auditTask("org", "99999999-8888-4777-9666-555555555555", time.Now().Add(10*time.Minute))
	for _, task := range []settleRetryTask{a, b} {
		if err := rdb.Set(context.Background(), resvMarkKey(task.scope, task.targetID, task.reqID), "0.002992000|2026-10", 0).Err(); err != nil {
			t.Fatalf("seed resvmark: %v", err)
		}
	}
	for i := 0; i < 2; i++ { // replay each settle twice
		s.processSettleTask(a)
		s.processSettleTask(b)
	}
	for _, task := range []settleRetryTask{a, b} {
		if rdb.Exists(context.Background(), settledMarkKey(task.scope, task.targetID, task.reqID)).Val() != 1 {
			t.Fatalf("request %s not settled", task.reqID)
		}
		if rdb.Exists(context.Background(), resvMarkKey(task.scope, task.targetID, task.reqID)).Val() != 0 {
			t.Fatalf("request %s mark not released", task.reqID)
		}
	}
	settled := rdb.Get(context.Background(), budgetSettledKey("org", "902", "2026-10")).Val()
	if settled != "0.005984" { // 2 x 0.002992 — each request counted EXACTLY once despite double settle
		t.Fatalf("settled = %q, want 0.005984 (each independent request billed once)", settled)
	}
}


