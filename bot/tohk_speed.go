package bot

// fork: TOH-K のタスク速度・アーソニストの塗り速度の集計 (/tohk stats)。
// 「ターン」は TOH-K の日 (試合開始が 1 日目、会議が終わるたびに +1。tohk_games.days は試合終了時に
// もう 1 つ進んでいるので、最後のタスク時間は days-1 日目)。
// - 対象: タスクを持つクルー陣営。シェリフとマッドメイトは対象外 (ユーザー指定)
// - 生存中: 生きている間に終えたタスク数 / 生きていたターン数。全部終えたターンで数えるのを止める
// - 天界: 死んだ後に終えたタスク数 / 死んでからのターン数。守護天使 (幽霊役職) は別に集計する
// - 試合終了時: 終えていたタスク数 / 持っていたタスク数

import (
	"context"
	"fmt"
	"strings"

	"github.com/bwmarrin/discordgo"
)

type tohkTaskEvt struct {
	Day   int
	Done  int
	Alive bool
}

type tohkTaskGame struct {
	Role, Team, GhostRole string
	SubRoles              []string
	HasTasks              bool
	TasksTotal, TasksDone int
	Dead                  bool
	DeathDay, Days        int
	Events                []tohkTaskEvt
}

type tohkSpeed struct {
	Games                    int // 対象の試合数
	EventGames               int // タスク完了の記録がある試合数 (記録は AutoLobby 0.6 から)
	AliveTasks, AliveTurns   int
	HeavenTasks, HeavenTurns int
	GATasks, GATurns         int
	EndDone, EndTotal        int
}

func tohkTaskEligible(g tohkTaskGame) bool {
	if !g.HasTasks || g.TasksTotal <= 0 || g.Team != "Crewmate" || g.Role == "Sheriff" {
		return false
	}
	for _, r := range g.SubRoles {
		if strings.Contains(r, "Madmate") {
			return false
		}
	}
	return true
}

func (s *tohkSpeed) add(g tohkTaskGame) {
	if !tohkTaskEligible(g) {
		return
	}
	s.Games++
	s.EndDone += g.TasksDone
	s.EndTotal += g.TasksTotal
	if len(g.Events) == 0 {
		return
	}
	s.EventGames++
	lastDay := g.Days - 1
	if lastDay < 1 {
		lastDay = 1
	}
	finishDay, finishedAlive := 0, false
	aliveTasks, heavenTasks := 0, 0
	for _, e := range g.Events {
		if e.Alive {
			aliveTasks++
		} else {
			heavenTasks++
		}
		if e.Done >= g.TasksTotal && finishDay == 0 {
			finishDay, finishedAlive = e.Day, e.Alive
		}
	}
	// 生存中のターン: 生きているうちに全部終えた日 / 死んだ日 / 最後の日
	aliveTurns := lastDay
	switch {
	case finishDay > 0 && finishedAlive:
		aliveTurns = finishDay
	case g.Dead && g.DeathDay > 0:
		aliveTurns = g.DeathDay
	}
	s.AliveTasks += aliveTasks
	s.AliveTurns += aliveTurns
	// 天界のターン: 死んだ日から、天界で全部終えた日か最後の日まで
	if g.Dead && g.DeathDay > 0 && !(finishDay > 0 && finishedAlive) {
		end := lastDay
		if finishDay > 0 {
			end = finishDay
		}
		turns := end - g.DeathDay
		if turns < 1 && heavenTasks > 0 {
			turns = 1
		}
		if turns > 0 {
			if g.GhostRole == "GuardianAngel" {
				s.GATasks += heavenTasks
				s.GATurns += turns
			} else {
				s.HeavenTasks += heavenTasks
				s.HeavenTurns += turns
			}
		}
	}
}

// tohkArsonist: アーソニストの塗り (何ターンで何人)
type tohkArsonist struct {
	Games, Douses, Turns int
}

func tohkPerTurn(n, turns int) string {
	if turns <= 0 {
		return "-"
	}
	return fmt.Sprintf("%.1f", float64(n)/float64(turns))
}

// tohkSpeedFields: /tohk stats にタスク速度とアーソニストの塗り速度の欄を足す
func (bot *Bot) tohkSpeedFields(ctx context.Context, e *discordgo.MessageEmbed, guildID, userID string) error {
	pool := bot.PostgresInterface.Pool
	rows, err := pool.Query(ctx, `SELECT p.result_id, p.role, p.team, p.ghost_role, p.sub_roles,
        coalesce(p.has_tasks, p.tasks_total > 0), p.tasks_total, p.tasks_done, p.dead, p.death_day, g.days
        FROM tohk_players p JOIN tohk_games g USING (result_id)
        WHERE g.guild_id = $1::numeric AND p.user_id = $2::numeric AND p.role <> 'GM'`, guildID, userID)
	if err != nil {
		return err
	}
	games := map[string]*tohkTaskGame{}
	for rows.Next() {
		var rid string
		var g tohkTaskGame
		if err := rows.Scan(&rid, &g.Role, &g.Team, &g.GhostRole, &g.SubRoles, &g.HasTasks, &g.TasksTotal, &g.TasksDone,
			&g.Dead, &g.DeathDay, &g.Days); err != nil {
			rows.Close()
			return err
		}
		games[rid] = &g
	}
	rows.Close()
	if len(games) == 0 {
		return nil
	}

	rows, err = pool.Query(ctx, `SELECT e.result_id, e.kind, e.day, e.alive, e.done
        FROM tohk_events e
        JOIN tohk_players p ON p.result_id = e.result_id AND p.player_id = e.player_id
        JOIN tohk_games g ON g.result_id = e.result_id
        WHERE g.guild_id = $1::numeric AND p.user_id = $2::numeric ORDER BY e.result_id, e.seq`, guildID, userID)
	if err != nil {
		return err
	}
	douses := map[string]int{}
	for rows.Next() {
		var rid, kind string
		var ev tohkTaskEvt
		if err := rows.Scan(&rid, &kind, &ev.Day, &ev.Alive, &ev.Done); err != nil {
			rows.Close()
			return err
		}
		g, ok := games[rid]
		if !ok {
			continue
		}
		switch kind {
		case "task":
			g.Events = append(g.Events, ev)
		case "douse":
			douses[rid]++
		}
	}
	rows.Close()
	// 記録 (tohk_events) がある試合かどうか。アーソニストは 1 人も塗らなかった試合もあるので、試合単位で見る
	recorded := map[string]bool{}
	rows, err = pool.Query(ctx, `SELECT DISTINCT e.result_id FROM tohk_events e JOIN tohk_games g USING (result_id)
        WHERE g.guild_id = $1::numeric`, guildID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var rid string
		if rows.Scan(&rid) == nil {
			recorded[rid] = true
		}
	}
	rows.Close()

	var s tohkSpeed
	var a tohkArsonist
	for rid, g := range games {
		s.add(*g)
		if g.Role == "Arsonist" && recorded[rid] {
			last := g.Days - 1
			if last < 1 {
				last = 1
			}
			a.Games++
			a.Turns += last
			a.Douses += douses[rid]
		}
	}
	if s.Games > 0 {
		var lines []string
		lines = append(lines, fmt.Sprintf("試合終了時: 平均 %.1f / %.1f 個 (完了率 %s)",
			float64(s.EndDone)/float64(s.Games), float64(s.EndTotal)/float64(s.Games), tohkPct(s.EndDone, s.EndTotal)))
		if s.EventGames > 0 {
			lines = append(lines, fmt.Sprintf("生存中: %d 個 / %d ターン (1ターン %s 個)", s.AliveTasks, s.AliveTurns, tohkPerTurn(s.AliveTasks, s.AliveTurns)))
			if s.HeavenTurns > 0 {
				lines = append(lines, fmt.Sprintf("天界: %d 個 / %d ターン (1ターン %s 個)", s.HeavenTasks, s.HeavenTurns, tohkPerTurn(s.HeavenTasks, s.HeavenTurns)))
			}
			if s.GATurns > 0 {
				lines = append(lines, fmt.Sprintf("天界 (守護天使): %d 個 / %d ターン (1ターン %s 個)", s.GATasks, s.GATurns, tohkPerTurn(s.GATasks, s.GATurns)))
			}
		}
		if s.EventGames < s.Games {
			lines = append(lines, fmt.Sprintf("※ターンごとの記録は %d / %d 試合 (2026-10-27 の AutoLobby 更新後の試合から)", s.EventGames, s.Games))
		}
		e.Fields = append(e.Fields, &discordgo.MessageEmbedField{
			Name:  fmt.Sprintf("タスク速度 (タスク持ちクルー %d 試合)", s.Games),
			Value: strings.Join(lines, "\n"),
		})
	}
	if a.Games > 0 {
		e.Fields = append(e.Fields, &discordgo.MessageEmbedField{
			Name:  fmt.Sprintf("アーソニスト (%d 試合)", a.Games),
			Value: fmt.Sprintf("%d 人 / %d ターン (1ターン %s 人)", a.Douses, a.Turns, tohkPerTurn(a.Douses, a.Turns)),
		})
	}
	return nil
}
