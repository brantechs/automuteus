package bot

import (
	"testing"
	"time"
)

func newTestExileTracker() (*tohkExileTracker, *time.Time) {
	now := time.Unix(1_800_000_000, 0)
	t := &tohkExileTracker{byCode: map[string]*tohkExileState{}, now: func() time.Time { return now }}
	return t, &now
}

func settleAfter(tr *tohkExileTracker, now *time.Time) (string, int, int, bool) {
	*now = now.Add(tohkBurstWindow + 200*time.Millisecond)
	return tr.Settle("C")
}

// 実際のログ (2026-10-23 15:50:47 タスク開始 → 15:51:01 に死亡済み 3 人の再送と追放者の DIED が同時に届く)
func TestTohkExileSingleNewDeathInBurst(t *testing.T) {
	tr, now := newTestExileTracker()
	tr.TasksStarted("C")
	*now = now.Add(14 * time.Second)
	if !tr.Resend("C") {
		t.Fatal("first event of a burst should ask the caller to settle")
	}
	*now = now.Add(time.Millisecond)
	if tr.NewDeath("C", "u1") {
		t.Fatal("only the first event starts the settle wait")
	}
	*now = now.Add(time.Millisecond)
	tr.Resend("C")
	if _, _, _, done := tr.Settle("C"); done {
		t.Fatal("the burst is still going")
	}
	exiled, resends, deaths, done := settleAfter(tr, now)
	if !done || exiled != "u1" || resends != 2 || deaths != 1 {
		t.Fatalf("got exiled=%q resends=%d deaths=%d done=%v", exiled, resends, deaths, done)
	}
}

// 追放 + 黒猫の道連れ: どちらが追放か分からないので、どちらにも触らない
func TestTohkExileTwoNewDeathsStayMuted(t *testing.T) {
	tr, now := newTestExileTracker()
	tr.TasksStarted("C")
	*now = now.Add(14 * time.Second)
	tr.NewDeath("C", "cat")
	tr.Resend("C")
	tr.NewDeath("C", "victim")
	exiled, _, deaths, done := settleAfter(tr, now)
	if !done || exiled != "" || deaths != 2 {
		t.Fatalf("got exiled=%q deaths=%d done=%v", exiled, deaths, done)
	}
}

func TestTohkExileRealKillStaysHidden(t *testing.T) {
	tr, now := newTestExileTracker()
	tr.TasksStarted("C")
	*now = now.Add(14 * time.Second)
	tr.Resend("C")
	settleAfter(tr, now)
	*now = now.Add(5 * time.Second) // 再送から数秒後の単独の DIED = 本物のキル
	if !tr.NewDeath("C", "u2") {
		t.Fatal("a lone death starts its own burst")
	}
	exiled, resends, _, done := settleAfter(tr, now)
	if !done || exiled != "" || resends != 0 {
		t.Fatalf("a lone kill must stay hidden, got exiled=%q resends=%d", exiled, resends)
	}
}

func TestTohkExileOutsideWindowAndForget(t *testing.T) {
	tr, now := newTestExileTracker()
	tr.TasksStarted("C")
	*now = now.Add(2 * time.Second) // タスク開始直後 (まだ再送の時間ではない)
	if tr.Resend("C") || tr.NewDeath("C", "u1") {
		t.Fatal("too early after tasks start")
	}
	tr.Forget("C")
	if _, ok := tr.byCode["C"]; ok {
		t.Fatal("forget should drop the game")
	}
	if tr.NewDeath("unknown", "u") {
		t.Fatal("no tasks start recorded")
	}
}
