package bot

// fork: /tohk history — AutoMuteUs の記録 (games / users_games / game_events) から出す全期間の参加・生存の戦績。
// TOH-K だと AutoMuteUs に役職と勝敗は正しく記録されない (インポスター扱いが約 6 割・クルー勝ち 0 件) ので出さない。
// 死亡は game_events の DIED (2) / EXILED (6) のうち、その試合で最初の 1 回だけを数える
// (TOH-K の AntiBlackout で会議明けに死亡済みの人の DIED が再送されるため)。
// users_games.player_name は 10 文字までなので、game_events の名前も 10 文字に切って照合する

import (
	"context"
	"fmt"
	"time"

	"github.com/bwmarrin/discordgo"
)

const tohkHistoryMinRankGames = 30

const tohkHistoryDeathsCTE = `
deaths AS (
  SELECT e.game_id, left(e.payload->>'Name', 10) AS name, min(e.event_time) AS t
    FROM game_events e JOIN games g ON g.game_id = e.game_id
   WHERE g.guild_id = $1::numeric AND e.event_type = 3 AND (e.payload->>'Action') IN ('2', '6')
   GROUP BY 1, 2),
ranked AS (SELECT game_id, name, t, rank() OVER (PARTITION BY game_id ORDER BY t) AS rk FROM deaths)`

func tohkDuration(secs float64) string {
	d := time.Duration(secs) * time.Second
	return fmt.Sprintf("%d分%02d秒", int(d.Minutes()), int(d.Seconds())%60)
}

func (bot *Bot) tohkHistoryEmbed(ctx context.Context, guildID, userID string) (*discordgo.MessageEmbed, error) {
	pool := bot.PostgresInterface.Pool
	var games, died, firstDeaths int
	var avgRank, avgDeathSecs, avgGameSecs *float64
	var since *time.Time
	err := pool.QueryRow(ctx, `WITH ug AS (
  SELECT ug.game_id, ug.player_name, g.start_time, g.end_time
    FROM users_games ug JOIN games g ON g.game_id = ug.game_id
   WHERE ug.user_id = $2::numeric AND g.guild_id = $1::numeric AND g.end_time > 0),`+tohkHistoryDeathsCTE+`
SELECT count(*), count(r.t), count(*) FILTER (WHERE r.rk = 1),
       avg(r.rk)::float8, avg(r.t - ug.start_time)::float8, avg(ug.end_time - ug.start_time)::float8,
       min(to_timestamp(ug.start_time))
  FROM ug LEFT JOIN ranked r ON r.game_id = ug.game_id AND r.name = ug.player_name`, guildID, userID).
		Scan(&games, &died, &firstDeaths, &avgRank, &avgDeathSecs, &avgGameSecs, &since)
	if err != nil {
		return nil, err
	}
	if games == 0 {
		return nil, nil
	}
	survived := games - died
	desc := fmt.Sprintf("<@%s>\n**%d 試合**", userID, games)
	if since != nil {
		desc += fmt.Sprintf(" (%s から)", since.In(time.FixedZone("JST", 9*3600)).Format("2006-01-02"))
	}
	e := &discordgo.MessageEmbed{Title: "全期間の戦績 (参加・生存)", Description: desc, Color: 0x2980b9}
	e.Fields = append(e.Fields,
		&discordgo.MessageEmbedField{Name: "生存", Value: fmt.Sprintf("%d 回 (生存率 %s)", survived, tohkPct(survived, games)), Inline: true},
		&discordgo.MessageEmbedField{Name: "最初に死んだ", Value: fmt.Sprintf("%d 回 (%s)", firstDeaths, tohkPct(firstDeaths, games)), Inline: true},
	)
	if avgRank != nil {
		e.Fields = append(e.Fields, &discordgo.MessageEmbedField{Name: "死ぬときの順番", Value: fmt.Sprintf("平均 %.1f 人目", *avgRank), Inline: true})
	}
	if avgDeathSecs != nil && avgGameSecs != nil {
		e.Fields = append(e.Fields, &discordgo.MessageEmbedField{Name: "時間",
			Value: fmt.Sprintf("死ぬまで平均 %s ・ 試合は平均 %s", tohkDuration(*avgDeathSecs), tohkDuration(*avgGameSecs))})
	}

	// サーバー内の順位 (一定試合数以上の人の生存率)
	rows, err := pool.Query(ctx, `WITH ug AS (
  SELECT ug.user_id, ug.game_id, ug.player_name
    FROM users_games ug JOIN games g ON g.game_id = ug.game_id
   WHERE g.guild_id = $1::numeric AND g.end_time > 0),`+tohkHistoryDeathsCTE+`
SELECT ug.user_id::text, count(*), count(*) - count(r.rk)
  FROM ug LEFT JOIN ranked r ON r.game_id = ug.game_id AND r.name = ug.player_name
 GROUP BY ug.user_id HAVING count(*) >= $2
 ORDER BY (count(*) - count(r.rk))::float8 / count(*) DESC, count(*) DESC`, guildID, tohkHistoryMinRankGames)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var lines []string
	rank, total := 0, 0
	for rows.Next() {
		var uid string
		var n, s int
		if err := rows.Scan(&uid, &n, &s); err != nil {
			return nil, err
		}
		total++
		if uid == userID {
			rank = total
		}
		if total <= 5 {
			lines = append(lines, fmt.Sprintf("%d. <@%s> %s (%d 試合)", total, uid, tohkPct(s, n), n))
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(lines) > 0 {
		title := fmt.Sprintf("生存率ランキング (%d 試合以上 %d 人)", tohkHistoryMinRankGames, total)
		if rank > 0 {
			title += fmt.Sprintf(" — この人は %d 位", rank)
		}
		e.Fields = append(e.Fields, &discordgo.MessageEmbedField{Name: title, Value: joinLines(lines)})
	}
	e.Footer = &discordgo.MessageEmbedFooter{Text: "AutoMuteUs の記録から。TOH-K の役職・勝敗は AutoMuteUs に正しく残らないので、" +
		"役職別の戦績は /tohk stats (TOH-K の結果送信を入れたホストの試合だけ) で見てください"}
	return e, nil
}

func joinLines(lines []string) string {
	out := ""
	for i, l := range lines {
		if i > 0 {
			out += "\n"
		}
		out += l
	}
	return out
}
