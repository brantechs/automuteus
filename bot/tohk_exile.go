package bot

// fork: TOH-K の追放を、本家 AmongUsCapture のイベントから見分ける。
//
// TOH-K は AntiBlackout のため、会議中はほかの人の画面で死者を一時的に生き返らせ、会議が終わって
// 十数秒後に全員を死亡に戻す。本家 capture はそのとき「死亡済みの人の DIED 再送」と、その会議で新しく
// 死んだ人 (追放・道連れなど) の DIED を同じ瞬間にまとめて送ってくる (EXILED は来ない)。AutoMuteUs は
// タスク中の DIED をキル隠しのためにすぐ反映しないので、追放された人は次の会議までミュート+スピーカーミュートのままになる。
//
// 対策: タスク開始から一定時間内に届いた再送のまとまり (バースト) が落ち着いたら、その中の新しい DIED が
// ちょうど 1 人なら追放とみなし、その 1 人だけをタスク中の死者のミュート状態にする。
// - 新しい DIED が 2 人以上 (追放 + 黒猫の道連れなど) のときはどれが追放か見分けられないので、全員そのまま
//   (上流と同じ。道連れされた人はそのターン中ミュートのままが正解: 黒猫がバレないように)
// - 全員の再計算やボードの書き換えはしない (同じタスク中に隠しているキルまで出てしまうため)
// - 本物のキルは単独で届くので、そのまま隠す。会議前に死者が 1 人もいない (再送が無い) ときも上流と同じ
// BranMuteUs (自前の AutoLobby) は投票終了の時点で EXILED を送り、DIED の再送もしないので、この判定は働かない

import (
	"log"
	"sync"
	"time"

	"github.com/automuteus/automuteus/v8/pkg/game"
	"github.com/automuteus/automuteus/v8/pkg/settings"
)

const (
	tohkResendEarliest = 5 * time.Second  // タスク開始からこの時間より前の DIED は見ない
	tohkResendLatest   = 30 * time.Second // これより後の DIED も見ない
	tohkBurstWindow    = time.Second      // 前のイベントからこの時間以内なら同じバースト
)

type tohkBurst struct {
	last    time.Time
	resends int
	deaths  []string // 新しく死んだ人の userID (リンクされていなければ "")
}

type tohkExileState struct {
	tasksStart time.Time
	burst      *tohkBurst
}

type tohkExileTracker struct {
	mu     sync.Mutex
	byCode map[string]*tohkExileState
	now    func() time.Time
}

var tohkExile = &tohkExileTracker{byCode: map[string]*tohkExileState{}, now: time.Now}

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

// note: タスク中の DIED を記録する。新しいバーストの最初のイベントなら true (呼び出し側が Settle の待ちを始める)
func (t *tohkExileTracker) note(code, userID string, resend bool) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	s, ok := t.byCode[code]
	if !ok || !s.inWindow(now) {
		return false
	}
	started := false
	if s.burst == nil || now.Sub(s.burst.last) > tohkBurstWindow {
		s.burst = &tohkBurst{}
		started = true
	}
	s.burst.last = now
	if resend {
		s.burst.resends++
	} else {
		s.burst.deaths = append(s.burst.deaths, userID)
	}
	return started
}

// NewDeath: タスク中に新しく死んだ人の DIED
func (t *tohkExileTracker) NewDeath(code, userID string) bool { return t.note(code, userID, false) }

// Resend: 死亡済みの人の DIED 再送 (TOH-K の AntiBlackout)
func (t *tohkExileTracker) Resend(code string) bool { return t.note(code, "", true) }

// Settle: バーストが落ち着いていれば締めて、追放とみなす 1 人 (無ければ "") と内訳を返す。
// まだ続いているなら done=false
func (t *tohkExileTracker) Settle(code string) (exiled string, resends, deaths int, done bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s, ok := t.byCode[code]
	if !ok || s.burst == nil {
		return "", 0, 0, true
	}
	b := s.burst
	if t.now().Sub(b.last) <= tohkBurstWindow {
		return "", 0, 0, false
	}
	s.burst = nil
	if b.resends > 0 && len(b.deaths) == 1 {
		return b.deaths[0], b.resends, 1, true
	}
	return "", b.resends, len(b.deaths), true
}

// tohkSettleBurst: バーストが落ち着くのを待ってから判定し、追放とみなした 1 人だけ反映する
func (bot *Bot) tohkSettleBurst(code string, gsr GameStateRequest, sett *settings.GuildSettings) {
	for i := 0; i < 30; i++ {
		time.Sleep(tohkBurstWindow + 200*time.Millisecond)
		exiled, resends, deaths, done := tohkExile.Settle(code)
		if !done {
			continue
		}
		if exiled == "" {
			if deaths > 0 {
				log.Printf("tohk: resend burst with %d resends and %d new deaths; not treating any as an exile\n", resends, deaths)
			}
			return
		}
		lock, dgs := bot.RedisInterface.GetDiscordGameStateAndLockRetries(gsr, 10)
		if lock == nil || dgs == nil {
			log.Println("tohk: could not lock the game state to apply the exile")
			return
		}
		if dgs.GameData.GetPhase() != game.TASKS {
			bot.RedisInterface.SetDiscordGameState(dgs, lock)
			return
		}
		log.Printf("tohk: user %s was the only new death in the AntiBlackout resend burst; treating it as an exile\n", exiled)
		bot.tohkApplyExile(dgs, sett, exiled)
		bot.RedisInterface.SetDiscordGameState(dgs, lock)
		return
	}
}

// tohkApplyExile: 追放とみなした 1 人だけ、今の段階の死者のミュート状態にする (ほかの人には触らない)。
// 呼び出し側が dgs のロックを持っていて、あとで dgs を保存する
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
