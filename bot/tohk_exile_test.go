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

// 実際のログ (2026-10-23 15:50:47 タスク開始 → 15:51:01 に死亡済み 3 人の再送と追放者の DIED が同時に届く)
func TestTohkExileBurstAfterResend(t *testing.T) {
	tr, now := newTestExileTracker()
	tr.TasksStarted("C")
	*now = now.Add(14 * time.Second)
	if tr.Resend("C") != "" {
		t.Fatal("resend alone must not trigger")
	}
	*now = now.Add(2 * time.Millisecond)
	if !tr.NewDeath("C", "u1") {
		t.Fatal("new death in the same burst should be treated as an exile")
	}
}

func TestTohkExileNewDeathBeforeResend(t *testing.T) {
	tr, now := newTestExileTracker()
	tr.TasksStarted("C")
	*now = now.Add(14 * time.Second)
	if tr.NewDeath("C", "u1") {
		t.Fatal("cannot decide before a resend arrives")
	}
	*now = now.Add(5 * time.Millisecond)
	if got := tr.Resend("C"); got != "u1" {
		t.Fatalf("resend right after a pending death should return that user, got %q", got)
	}
	*now = now.Add(5 * time.Millisecond)
	if got := tr.Resend("C"); got != "" {
		t.Fatalf("the pending death must be consumed once, got %q", got)
	}
}

func TestTohkExileRealKillStaysHidden(t *testing.T) {
	tr, now := newTestExileTracker()
	tr.TasksStarted("C")
	*now = now.Add(14 * time.Second)
	tr.Resend("C")
	*now = now.Add(5 * time.Second) // 再送から 5 秒後の単独の DIED = 本物のキル
	if tr.NewDeath("C", "u2") {
		t.Fatal("a lone kill must stay hidden")
	}
	*now = now.Add(20 * time.Second)
	if got := tr.Resend("C"); got != "" {
		t.Fatalf("outside the window nothing triggers, got %q", got)
	}
}

func TestTohkExileOutsideWindowAndForget(t *testing.T) {
	tr, now := newTestExileTracker()
	tr.TasksStarted("C")
	*now = now.Add(2 * time.Second) // タスク開始直後 (まだ再送の時間ではない)
	tr.Resend("C")
	if tr.NewDeath("C", "u1") {
		t.Fatal("too early after tasks start")
	}
	tr.Forget("C")
	if _, ok := tr.byCode["C"]; ok {
		t.Fatal("forget should drop the game")
	}
	if (&tohkExileTracker{byCode: map[string]*tohkExileState{}, now: time.Now}).NewDeath("unknown", "u") {
		t.Fatal("no tasks start recorded")
	}
}
