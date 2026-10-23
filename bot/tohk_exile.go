package bot

// fork: TOH-K の追放を、本家 AmongUsCapture のイベントから見分ける。
//
// TOH-K は AntiBlackout のため、会議中はほかの人の画面で死者を一時的に生き返らせ、会議が終わって
// 十数秒後に全員を死亡に戻す。本家 capture はそのとき「死亡済みの人の DIED 再送」と、追放された人の
// DIED を同じ瞬間にまとめて送ってくる (EXILED は来ない)。AutoMuteUs はタスク中の DIED を
// キル隠しのためにすぐ反映しないので、追放された人は次の会議までミュート+スピーカーミュートのままになる。
//
// 対策: タスク開始から一定時間内に「死亡済みの人の再送」と同じ瞬間 (前後 tohkBurstWindow) に届いた
// 新しい DIED は追放とみなし、その 1 人だけをタスク中の死者のミュート状態にする。全員の再計算や
// ボードの書き換えはしない (同じタスク中に隠しているキルまで出てしまうため)。本物のキルは単独で届くので、
// そのまま隠す。会議前に死者が 1 人もいない (再送が無い) ときは見分けられないので、上流と同じ動きになる。
// BranMuteUs (自前の AutoLobby) は投票終了の時点で EXILED を送り、DIED の再送もしないので、この判定は働かない

import (
	"log"
	"sync"
	"time"

	"github.com/automuteus/automuteus/v8/pkg/settings"
)

const (
	tohkResendEarliest = 5 * time.Second  // タスク開始からこの時間より前の再送は見ない
	tohkResendLatest   = 30 * time.Second // これより後の DIED は追放とみなさない
	tohkBurstWindow    = time.Second
)

type tohkExileState struct {
	tasksStart  time.Time
	lastResend  time.Time
	pendingAt   time.Time // 再送より先に届いた新しい DIED (まだ反映していない)
	pendingUser string
}

type tohkExileTracker struct {
	mu     sync.Mutex
	byCode map[string]*tohkExileState
	now    func() time.Time
}

var tohkExile = &tohkExileTracker{byCode: map[string]*tohkExileState{}, now: time.Now}

func (t *tohkExileTracker) get(code string) *tohkExileState {
	s, ok := t.byCode[code]
	if !ok {
		s = &tohkExileState{}
		t.byCode[code] = s
	}
	return s
}

// TasksStarted: タスク段階に入ったとき (会議明け・試合開始)
func (t *tohkExileTracker) TasksStarted(code string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.byCode[code] = &tohkExileState{tasksStart: t.now()}
}

// Forget: ロビーに戻ったとき (試合が終わった)
func (t *tohkExileTracker) Forget(code string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.byCode, code)
}

func (s *tohkExileState) inWindow(now time.Time) bool {
	if s.tasksStart.IsZero() {
		return false
	}
	d := now.Sub(s.tasksStart)
	return d >= tohkResendEarliest && d <= tohkResendLatest
}

// NewDeath: タスク中に新しく死んだ人の DIED。追放とみなして今すぐ反映するなら true。
// まだ決められない (再送が来ていない) ときは保留し、直後の Resend が userID を返す
func (t *tohkExileTracker) NewDeath(code, userID string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	s := t.get(code)
	if !s.inWindow(now) {
		return false
	}
	if !s.lastResend.IsZero() && now.Sub(s.lastResend) <= tohkBurstWindow {
		return true
	}
	s.pendingAt, s.pendingUser = now, userID
	return false
}

// Resend: 死亡済みの人の DIED 再送。直前に保留した新しい DIED があれば、追放とみなすべき人の userID を返す
func (t *tohkExileTracker) Resend(code string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	s := t.get(code)
	if !s.inWindow(now) {
		return ""
	}
	s.lastResend = now
	if !s.pendingAt.IsZero() && now.Sub(s.pendingAt) <= tohkBurstWindow {
		uid := s.pendingUser
		s.pendingAt, s.pendingUser = time.Time{}, ""
		return uid
	}
	return ""
}

// tohkApplyExile: 追放とみなした 1 人だけ、今の段階の死者のミュート状態にする (ほかの人には触らない)。
// 呼び出し側 (processPlayer) が dgs のロックを持っていて、関数を抜けるときに dgs を保存する
func (bot *Bot) tohkApplyExile(dgs *GameState, sett *settings.GuildSettings, userID string) {
	if userID == "" {
		return
	}
	ud, ok := dgs.UserData[userID]
	if !ok {
		return
	}
	mute, deaf := sett.GetVoiceState(false, true, dgs.GameData.GetPhase())
	ud.SetShouldBeMuteDeaf(mute, deaf)
	dgs.UpdateUserData(userID, ud)
	if err := bot.applyToSingle(dgs, userID, mute, deaf); err != nil {
		log.Println("tohk: applying the exile voice state failed:", err)
	}
}
