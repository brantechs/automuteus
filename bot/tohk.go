package bot

// tohk: TOH-K の試合結果を受けて保存し、/tohk で正しい戦績を見せる (fork 独自)。
//
// AutoMuteUs の capture は素の Among Us のフラグしか読めないので、TOH-K では役職も勝敗もずれる
// (users_games はインポスター扱いが約 6 割・クルー勝ち 0 件)。正しい情報はホストの TOH-K だけが
// 持っているので、ホストの AutoLobby (GameResult.cs) が試合終了時に POST /tohk/result へ送ってくる。
//
// - 保存先は統計 DB (cnh-vps の NamaMuteEx と共有) の tohk_games / tohk_players。AutoMuteUs の
//   既存テーブルには書かない
// - Discord ユーザーとの紐づけは、同じ時刻に終わった AutoMuteUs の試合 (games / users_games) の
//   色で行う。users_games は AutoMuteUs が試合終了を処理してから書かれるので、受信後に何度か試す
// - 送信元の確認は接続元 IP だけ (loopback / プライベート / tailnet)。X-Forwarded-For は見ない

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/automuteus/automuteus/v8/bot/command"
	"github.com/automuteus/automuteus/v8/pkg/settings"
	"github.com/bwmarrin/discordgo"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v4"
)

const (
	tohkSchemaVersion  = 1
	tohkMatchWindowSec = 300 // AutoMuteUs の試合の終了時刻とのずれの許容
	tohkMinRankGames   = 10
)

var TohkCommand = discordgo.ApplicationCommand{
	Name:        "tohk",
	Description: "TOH-K の正しい戦績 (役職・勝敗・キル) を見る",
	Options: []*discordgo.ApplicationCommandOption{
		{
			Name:        "stats",
			Description: "プレイヤーの戦績 (省略すると自分)",
			Type:        discordgo.ApplicationCommandOptionSubCommand,
			Options: []*discordgo.ApplicationCommandOption{
				{Name: "user", Description: "戦績を見たい人", Type: discordgo.ApplicationCommandOptionUser},
			},
		},
		{Name: "last", Description: "直近の試合の結果とキルログ", Type: discordgo.ApplicationCommandOptionSubCommand},
		{Name: "ranking", Description: "勝率とキル数のランキング", Type: discordgo.ApplicationCommandOptionSubCommand},
	},
}

func init() {
	command.All = append(command.All, &TohkCommand)
}

type tohkPlayer struct {
	ID           int      `json:"id"`
	Name         string   `json:"name"`
	Color        int      `json:"color"`
	Role         string   `json:"role"`
	RoleName     string   `json:"role_name"`
	Team         string   `json:"team"`
	CountType    string   `json:"count_type"`
	SubRoles     []string `json:"sub_roles"`
	GhostRole    string   `json:"ghost_role"`
	Won          bool     `json:"won"`
	Dead         bool     `json:"dead"`
	DeathReason  string   `json:"death_reason"`
	KillerID     int      `json:"killer_id"`
	DeathAt      int64    `json:"death_at"`
	Kills        int      `json:"kills"`
	TasksDone    int      `json:"tasks_done"`
	TasksTotal   int      `json:"tasks_total"`
	Disconnected bool     `json:"disconnected"`
	DeathDay     int      `json:"death_day"` // AutoLobby 0.6 から (それより前は 0)
	HasTasks     *bool    `json:"has_tasks"` // AutoLobby 0.6 から
}

// tohkEvent: 試合中の出来事 (AutoLobby 0.6 から)。kind = task (タスク完了) / douse (アーソニストの塗り完了)
type tohkEvent struct {
	Kind     string `json:"kind"`
	PlayerID int    `json:"player_id"`
	TargetID *int   `json:"target_id"`
	Day      int    `json:"day"`
	Alive    bool   `json:"alive"`
	Done     int    `json:"done"`
	At       int64  `json:"at"`
}

type tohkResult struct {
	Schema                int          `json:"schema"`
	ID                    string       `json:"id"`
	Source                string       `json:"source"`
	Autohost              bool         `json:"autohost"`
	TohkVersion           string       `json:"tohk_version"`
	LobbyCode             string       `json:"lobby_code"`
	MapID                 int          `json:"map_id"`
	StartedAt             int64        `json:"started_at"`
	EndedAt               int64        `json:"ended_at"`
	Days                  int          `json:"days"`
	WinnerTeam            string       `json:"winner_team"`
	AdditionalWinnerRoles []string     `json:"additional_winner_roles"`
	Players               []tohkPlayer `json:"players"`
	Events                []tohkEvent  `json:"events"`
	Log                   string       `json:"log"`
}

var (
	tohkSchemaMu    sync.Mutex
	tohkSchemaReady bool
)

func (bot *Bot) tohkEnsureSchema(ctx context.Context) error {
	tohkSchemaMu.Lock()
	defer tohkSchemaMu.Unlock()
	if tohkSchemaReady {
		return nil
	}
	_, err := bot.PostgresInterface.Pool.Exec(ctx, `
CREATE TABLE IF NOT EXISTS tohk_games (
    result_id    text PRIMARY KEY,
    guild_id     numeric,
    amu_game_id  bigint,
    source       text NOT NULL DEFAULT '',
    autohost     boolean NOT NULL DEFAULT false,
    tohk_version text NOT NULL DEFAULT '',
    lobby_code   text NOT NULL DEFAULT '',
    map_id       smallint NOT NULL DEFAULT -1,
    started_at   bigint NOT NULL DEFAULT 0,
    ended_at     bigint NOT NULL,
    days         smallint NOT NULL DEFAULT 0,
    winner_team  text NOT NULL DEFAULT '',
    additional_winner_roles text[] NOT NULL DEFAULT '{}',
    log          text NOT NULL DEFAULT '',
    raw          jsonb,
    received_at  bigint NOT NULL
);
CREATE INDEX IF NOT EXISTS tohk_games_guild_ended ON tohk_games (guild_id, ended_at DESC);
CREATE TABLE IF NOT EXISTS tohk_players (
    result_id        text NOT NULL REFERENCES tohk_games(result_id) ON DELETE CASCADE,
    player_id        smallint NOT NULL,
    user_id          numeric,
    name             text NOT NULL DEFAULT '',
    color            smallint NOT NULL DEFAULT -1,
    role             text NOT NULL DEFAULT '',
    role_name        text NOT NULL DEFAULT '',
    team             text NOT NULL DEFAULT '',
    count_type       text NOT NULL DEFAULT '',
    sub_roles        text[] NOT NULL DEFAULT '{}',
    ghost_role       text NOT NULL DEFAULT '',
    won              boolean NOT NULL DEFAULT false,
    dead             boolean NOT NULL DEFAULT false,
    death_reason     text NOT NULL DEFAULT '',
    killer_player_id smallint NOT NULL DEFAULT -1,
    killer_user_id   numeric,
    death_at         bigint NOT NULL DEFAULT 0,
    kills            smallint NOT NULL DEFAULT 0,
    tasks_done       smallint NOT NULL DEFAULT 0,
    tasks_total      smallint NOT NULL DEFAULT 0,
    disconnected     boolean NOT NULL DEFAULT false,
    PRIMARY KEY (result_id, player_id)
);
CREATE INDEX IF NOT EXISTS tohk_players_user ON tohk_players (user_id);
ALTER TABLE tohk_players ADD COLUMN IF NOT EXISTS death_day smallint NOT NULL DEFAULT 0;
ALTER TABLE tohk_players ADD COLUMN IF NOT EXISTS has_tasks boolean;
CREATE TABLE IF NOT EXISTS tohk_events (
    result_id text NOT NULL REFERENCES tohk_games(result_id) ON DELETE CASCADE,
    seq       integer NOT NULL,
    kind      text NOT NULL,
    player_id smallint NOT NULL,
    target_id smallint,
    day       smallint NOT NULL DEFAULT 0,
    alive     boolean NOT NULL DEFAULT false,
    done      smallint NOT NULL DEFAULT 0,
    at        bigint NOT NULL DEFAULT 0,
    PRIMARY KEY (result_id, seq)
);`)
	if err == nil {
		tohkSchemaReady = true
	}
	return err
}

// tohkTrusted: 接続元が loopback / プライベート / tailnet (100.64.0.0/10) か
func tohkTrusted(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	if ip.IsLoopback() || ip.IsPrivate() {
		return true
	}
	_, cgnat, _ := net.ParseCIDR("100.64.0.0/10")
	return cgnat.Contains(ip)
}

func (r *tohkResult) validate() error {
	switch {
	case r.Schema != tohkSchemaVersion:
		return fmt.Errorf("unsupported schema %d", r.Schema)
	case r.ID == "" || len(r.ID) > 64:
		return errors.New("bad id")
	case r.EndedAt <= 0:
		return errors.New("bad ended_at")
	case len(r.Events) > 5000:
		return fmt.Errorf("too many events %d", len(r.Events))
	case len(r.Players) == 0 || len(r.Players) > 20:
		return fmt.Errorf("bad player count %d", len(r.Players))
	}
	return nil
}

func (bot *Bot) handleTohkResult() gin.HandlerFunc {
	return func(c *gin.Context) {
		// gin は全プロキシを信頼する設定なので c.ClientIP() は X-Forwarded-For で偽装できる。実際の接続元を見る
		if !tohkTrusted(c.Request.RemoteAddr) {
			c.AbortWithStatus(http.StatusForbidden)
			return
		}
		body, err := io.ReadAll(io.LimitReader(c.Request.Body, 1<<20))
		if err != nil {
			c.AbortWithStatus(http.StatusBadRequest)
			return
		}
		var res tohkResult
		if err := json.Unmarshal(body, &res); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if err := res.validate(); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		inserted, err := bot.tohkStore(ctx, &res, body)
		if err != nil {
			log.Println("tohk: store failed:", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "store failed"})
			return
		}
		if inserted {
			log.Printf("tohk: stored result %s (%s, %d players, winner %s)", res.ID, res.Source, len(res.Players), res.WinnerTeam)
			go bot.tohkResolveLater(res.ID)
		}
		c.JSON(http.StatusOK, gin.H{"ok": true, "duplicate": !inserted})
	}
}

func (bot *Bot) tohkStore(ctx context.Context, res *tohkResult, raw []byte) (bool, error) {
	if err := bot.tohkEnsureSchema(ctx); err != nil {
		return false, err
	}
	tx, err := bot.PostgresInterface.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	tag, err := tx.Exec(ctx, `INSERT INTO tohk_games (result_id, source, autohost, tohk_version, lobby_code, map_id,
        started_at, ended_at, days, winner_team, additional_winner_roles, log, raw, received_at)
        VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14) ON CONFLICT (result_id) DO NOTHING`,
		res.ID, res.Source, res.Autohost, res.TohkVersion, res.LobbyCode, res.MapID, res.StartedAt, res.EndedAt,
		res.Days, res.WinnerTeam, nonNil(res.AdditionalWinnerRoles), res.Log, raw, time.Now().Unix())
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 0 {
		return false, nil // 再送 (同じ id) は捨てる
	}
	for _, p := range res.Players {
		_, err = tx.Exec(ctx, `INSERT INTO tohk_players (result_id, player_id, name, color, role, role_name, team,
            count_type, sub_roles, ghost_role, won, dead, death_reason, killer_player_id, death_at, kills,
            tasks_done, tasks_total, disconnected, death_day, has_tasks)
            VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21)`,
			res.ID, p.ID, p.Name, p.Color, p.Role, p.RoleName, p.Team, p.CountType, nonNil(p.SubRoles), p.GhostRole,
			p.Won, p.Dead, p.DeathReason, p.KillerID, p.DeathAt, p.Kills, p.TasksDone, p.TasksTotal, p.Disconnected,
			p.DeathDay, p.HasTasks)
		if err != nil {
			return false, err
		}
	}
	for i, e := range res.Events {
		if e.Kind != "task" && e.Kind != "douse" {
			continue
		}
		if _, err = tx.Exec(ctx, `INSERT INTO tohk_events (result_id, seq, kind, player_id, target_id, day, alive, done, at)
            VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`, res.ID, i, e.Kind, e.PlayerID, e.TargetID, e.Day, e.Alive, e.Done, e.At); err != nil {
			return false, err
		}
	}
	return true, tx.Commit(ctx)
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// tohkResolveLater: AutoMuteUs が試合終了を記録し終わるのを待ちながら、何度か紐づけを試す
func (bot *Bot) tohkResolveLater(resultID string) {
	for _, wait := range []time.Duration{15 * time.Second, 45 * time.Second, 2 * time.Minute, 5 * time.Minute} {
		time.Sleep(wait)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		ok, err := bot.tohkResolve(ctx, resultID)
		cancel()
		if err != nil {
			log.Println("tohk: resolve failed:", err)
		}
		if ok {
			return
		}
	}
	log.Printf("tohk: result %s has no matching AutoMuteUs game; players stay unlinked", resultID)
}

// tohkResolve: 同じ時刻に終わった AutoMuteUs の試合を探し、guild と、色が一致するリンク済みユーザーを付ける
func (bot *Bot) tohkResolve(ctx context.Context, resultID string) (bool, error) {
	pool := bot.PostgresInterface.Pool
	var gameID int64
	var guildID string
	err := pool.QueryRow(ctx, `SELECT g.game_id, g.guild_id::text FROM games g, tohk_games t
        WHERE t.result_id = $1 AND g.end_time > 0 AND abs(g.end_time - t.ended_at) <= $2 AND g.start_time <= t.ended_at
          AND (t.started_at = 0 OR abs(g.start_time - t.started_at) <= $2)
          -- 既に別の結果に紐づいた試合は使わない
          AND NOT EXISTS (SELECT 1 FROM tohk_games x WHERE x.amu_game_id = g.game_id AND x.result_id <> t.result_id)
          -- AutoMuteUs 側でリンクされていた人が、TOH-K の結果に色か名前で全員いる試合だけ (別の部屋の試合を拾わない)。
          -- 色は結果に入っていないことがある (AutoLobby 0.5 系は全員 -1 を送っていた) ので名前でも照合する。
          -- users_games.player_name は 10 文字まで。リンク済みの人が 1 人もいない試合は見分けがつかないので使わない
          AND EXISTS (SELECT 1 FROM users_games ug WHERE ug.game_id = g.game_id)
          AND NOT EXISTS (SELECT 1 FROM users_games ug WHERE ug.game_id = g.game_id
              AND ug.player_color NOT IN (SELECT color FROM tohk_players WHERE result_id = t.result_id)
              AND ug.player_name NOT IN (SELECT left(name, 10) FROM tohk_players WHERE result_id = t.result_id))
        ORDER BY abs(g.end_time - t.ended_at) LIMIT 1`, resultID, tohkMatchWindowSec).Scan(&gameID, &guildID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if _, err = pool.Exec(ctx, `UPDATE tohk_games SET amu_game_id = $2, guild_id = $3::numeric WHERE result_id = $1`,
		resultID, gameID, guildID); err != nil {
		return false, err
	}
	// 色で紐づける。同じ色が 2 人いる (途中で色替えなど) ときは紐づけない
	if _, err = pool.Exec(ctx, `UPDATE tohk_players p SET user_id = ug.user_id
        FROM (SELECT player_color, min(user_id) AS user_id FROM users_games WHERE game_id = $2
              GROUP BY player_color HAVING count(*) = 1) ug
        WHERE p.result_id = $1 AND p.color >= 0 AND ug.player_color = p.color`, resultID, gameID); err != nil {
		return false, err
	}
	// 色で付かなかった人は名前で (色が -1 の結果も)。色も AutoMuteUs の記録から埋める
	if _, err = pool.Exec(ctx, `UPDATE tohk_players p SET user_id = ug.user_id,
            color = CASE WHEN p.color < 0 THEN ug.player_color ELSE p.color END
        FROM (SELECT player_name, min(user_id) AS user_id, min(player_color) AS player_color FROM users_games
              WHERE game_id = $2 GROUP BY player_name HAVING count(*) = 1) ug
        WHERE p.result_id = $1 AND p.user_id IS NULL AND left(p.name, 10) = ug.player_name`, resultID, gameID); err != nil {
		return false, err
	}
	if _, err = pool.Exec(ctx, `UPDATE tohk_players p SET killer_user_id = k.user_id FROM tohk_players k
        WHERE p.result_id = $1 AND k.result_id = p.result_id AND k.player_id = p.killer_player_id`, resultID); err != nil {
		return false, err
	}
	log.Printf("tohk: result %s linked to AutoMuteUs game %d (guild %s)", resultID, gameID, guildID)
	return true, nil
}

// ---- /tohk ----

var tohkTeamLabel = map[string]string{
	"Crewmate": "クルー陣営", "Impostor": "インポスター陣営", "Madmate": "マッドメイト", "Neutral": "第三陣営",
}

var tohkDeathLabel = map[string]string{
	"Kill": "キル", "Vote": "追放", "Suicide": "自爆", "Spell": "呪殺", "Bite": "噛み殺し", "Bombed": "爆死",
	"Misfire": "誤射", "Torched": "焼死", "Sniped": "狙撃", "Revenge": "道連れ", "Execution": "処刑",
	"Disconnected": "切断", "Fall": "転落", "Poisoning": "毒殺", "etc": "その他",
}

func tohkLabel(m map[string]string, k string) string {
	if v, ok := m[k]; ok {
		return v
	}
	return k
}

func tohkMention(userID *string, name string) string {
	if userID != nil && *userID != "" {
		return "<@" + *userID + ">"
	}
	return name
}

func (bot *Bot) handleTohkCommand(i *discordgo.InteractionCreate, _ *settings.GuildSettings) *discordgo.InteractionResponse {
	reply := func(content string, embed *discordgo.MessageEmbed) *discordgo.InteractionResponse {
		d := &discordgo.InteractionResponseData{Content: content, AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}}}
		if embed != nil {
			d.Embeds = []*discordgo.MessageEmbed{embed}
		}
		return &discordgo.InteractionResponse{Type: discordgo.InteractionResponseChannelMessageWithSource, Data: d}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := bot.tohkEnsureSchema(ctx); err != nil {
		return reply("⚠️ 統計 DB に接続できません。", nil)
	}
	if i.Member == nil || i.Member.User == nil || i.GuildID == "" {
		return reply("サーバー内で使ってください。", nil)
	}
	opts := i.ApplicationCommandData().Options
	if len(opts) == 0 {
		return reply("サブコマンドを選んでください。", nil)
	}
	var embed *discordgo.MessageEmbed
	var err error
	switch sub := opts[0]; sub.Name {
	case "stats":
		userID := i.Member.User.ID
		for _, o := range sub.Options {
			if o.Name == "user" {
				userID = o.UserValue(nil).ID
			}
		}
		embed, err = bot.tohkStatsEmbed(ctx, i.GuildID, userID)
	case "last":
		embed, err = bot.tohkLastEmbed(ctx, i.GuildID)
	case "ranking":
		embed, err = bot.tohkRankingEmbed(ctx, i.GuildID)
	}
	if err != nil {
		log.Println("tohk: query failed:", err)
		return reply("⚠️ 戦績を読めませんでした。", nil)
	}
	if embed == nil {
		return reply("まだ記録がありません (記録は TOH-K の結果送信を入れたホストの試合から始まります)。", nil)
	}
	return reply("", embed)
}

func tohkPct(won, total int) string {
	if total == 0 {
		return "-"
	}
	return fmt.Sprintf("%.0f%%", float64(won)*100/float64(total))
}

func (bot *Bot) tohkStatsEmbed(ctx context.Context, guildID, userID string) (*discordgo.MessageEmbed, error) {
	pool := bot.PostgresInterface.Pool
	const base = ` FROM tohk_players p JOIN tohk_games g USING (result_id)
        WHERE g.guild_id = $1::numeric AND p.user_id = $2::numeric AND p.role <> 'GM' `
	var games, wins, kills, alive int
	if err := pool.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE p.won), coalesce(sum(p.kills), 0),
        count(*) FILTER (WHERE NOT p.dead)`+base, guildID, userID).Scan(&games, &wins, &kills, &alive); err != nil {
		return nil, err
	}
	if games == 0 {
		return nil, nil
	}
	e := &discordgo.MessageEmbed{
		Title:       "TOH-K 戦績",
		Description: fmt.Sprintf("<@%s>\n**%d 試合 %d 勝 (勝率 %s)**\nキル %d ・ 生存 %d 回 (%s)", userID, games, wins, tohkPct(wins, games), kills, alive, tohkPct(alive, games)),
		Color:       0x8e44ad,
	}

	rows, err := pool.Query(ctx, `SELECT p.team, count(*), count(*) FILTER (WHERE p.won)`+base+` GROUP BY p.team ORDER BY count(*) DESC`, guildID, userID)
	if err != nil {
		return nil, err
	}
	var teams []string
	for rows.Next() {
		var team string
		var n, w int
		if err := rows.Scan(&team, &n, &w); err != nil {
			rows.Close()
			return nil, err
		}
		teams = append(teams, fmt.Sprintf("%s: %d 試合 勝率 %s", tohkLabel(tohkTeamLabel, team), n, tohkPct(w, n)))
	}
	rows.Close()
	if len(teams) > 0 {
		e.Fields = append(e.Fields, &discordgo.MessageEmbedField{Name: "陣営別", Value: strings.Join(teams, "\n")})
	}

	rows, err = pool.Query(ctx, `SELECT p.role_name, count(*), count(*) FILTER (WHERE p.won)`+base+` GROUP BY p.role_name ORDER BY count(*) DESC LIMIT 6`, guildID, userID)
	if err != nil {
		return nil, err
	}
	var roles []string
	for rows.Next() {
		var role string
		var n, w int
		if err := rows.Scan(&role, &n, &w); err != nil {
			rows.Close()
			return nil, err
		}
		roles = append(roles, fmt.Sprintf("%s: %d 回 (勝率 %s)", role, n, tohkPct(w, n)))
	}
	rows.Close()
	if len(roles) > 0 {
		e.Fields = append(e.Fields, &discordgo.MessageEmbedField{Name: "よく引く役職", Value: strings.Join(roles, "\n")})
	}

	pair := func(title, q string) error {
		rows, err := pool.Query(ctx, q, guildID, userID)
		if err != nil {
			return err
		}
		defer rows.Close()
		var lines []string
		for rows.Next() {
			var uid *string
			var name string
			var n int
			if err := rows.Scan(&uid, &name, &n); err != nil {
				return err
			}
			lines = append(lines, fmt.Sprintf("%s: %d 回", tohkMention(uid, name), n))
		}
		if len(lines) > 0 {
			e.Fields = append(e.Fields, &discordgo.MessageEmbedField{Name: title, Value: strings.Join(lines, "\n"), Inline: true})
		}
		return rows.Err()
	}
	// 倒された相手 / 倒した相手 (自爆は除く)
	if err := pair("よく倒される相手", `SELECT k.user_id::text, max(k.name), count(*)
        FROM tohk_players p JOIN tohk_games g USING (result_id)
        JOIN tohk_players k ON k.result_id = p.result_id AND k.player_id = p.killer_player_id
        WHERE g.guild_id = $1::numeric AND p.user_id = $2::numeric AND p.dead AND p.killer_player_id <> p.player_id
        GROUP BY coalesce(k.user_id::text, k.name), k.user_id ORDER BY count(*) DESC LIMIT 3`); err != nil {
		return nil, err
	}
	if err := pair("よく倒す相手", `SELECT v.user_id::text, max(v.name), count(*)
        FROM tohk_players k JOIN tohk_games g USING (result_id)
        JOIN tohk_players v ON v.result_id = k.result_id AND v.killer_player_id = k.player_id
        WHERE g.guild_id = $1::numeric AND k.user_id = $2::numeric AND v.dead AND v.player_id <> k.player_id
        GROUP BY coalesce(v.user_id::text, v.name), v.user_id ORDER BY count(*) DESC LIMIT 3`); err != nil {
		return nil, err
	}
	if err := bot.tohkSpeedFields(ctx, e, guildID, userID); err != nil {
		return nil, err
	}
	e.Footer = &discordgo.MessageEmbedFooter{Text: "TOH-K の結果送信を入れたホストの試合だけを数えています。ターン = 会議と会議の間"}
	return e, nil
}

func (bot *Bot) tohkLastEmbed(ctx context.Context, guildID string) (*discordgo.MessageEmbed, error) {
	pool := bot.PostgresInterface.Pool
	var resultID, winner, logText, code string
	var ended int64
	var days int
	// 紐づけ前 (guild 未確定) の直近の試合も出す
	err := pool.QueryRow(ctx, `SELECT result_id, winner_team, log, lobby_code, ended_at, days FROM tohk_games
        WHERE guild_id = $1::numeric OR (guild_id IS NULL AND received_at > extract(epoch from now()) - 600)
        ORDER BY ended_at DESC LIMIT 1`, guildID).Scan(&resultID, &winner, &logText, &code, &ended, &days)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rows, err := pool.Query(ctx, `SELECT p.player_id, p.user_id::text, p.name, p.role, p.role_name, p.team, p.sub_roles, p.won, p.dead,
        p.death_reason, p.killer_player_id, p.kills FROM tohk_players p WHERE p.result_id = $1 AND p.role <> 'GM'
        ORDER BY p.won DESC, p.dead, p.player_id`, resultID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type pl struct {
		id, killer, kills                 int
		uid                               *string
		name, role, roleName, team, death string
		subs                              []string
		won, dead                         bool
	}
	var ps []pl
	names := map[int]string{}
	winnerLabel := ""
	for rows.Next() {
		var p pl
		if err := rows.Scan(&p.id, &p.uid, &p.name, &p.role, &p.roleName, &p.team, &p.subs, &p.won, &p.dead, &p.death, &p.killer, &p.kills); err != nil {
			return nil, err
		}
		names[p.id] = p.name
		if p.role == winner && winnerLabel == "" {
			winnerLabel = p.roleName
		}
		ps = append(ps, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	switch {
	case winnerLabel != "":
	case winner == "Crewmate" || winner == "Impostor":
		winnerLabel = tohkLabel(tohkTeamLabel, winner)
	case winner == "Draw":
		winnerLabel = "強制終了"
	case winner == "None":
		winnerLabel = "全滅"
	default:
		winnerLabel = winner
	}
	var lines []string
	for _, p := range ps {
		mark := "　"
		if p.won {
			mark = "★"
		}
		role := p.roleName
		if len(p.subs) > 0 {
			role += " +" + strings.Join(p.subs, "+")
		}
		state := "生存"
		if p.dead {
			state = tohkLabel(tohkDeathLabel, p.death)
			if k, ok := names[p.killer]; ok && p.killer != p.id {
				state += " (" + k + ")"
			}
		}
		if p.kills > 0 {
			state += fmt.Sprintf(" ・ %dキル", p.kills)
		}
		lines = append(lines, fmt.Sprintf("%s %s — %s ・ %s", mark, tohkMention(p.uid, p.name), role, state))
	}
	desc := strings.Join(lines, "\n")
	if logText != "" {
		logText = strings.TrimSpace(logText)
		room := 4000 - len([]rune(desc)) - 20
		if r := []rune(logText); room > 100 && len(r) > room {
			logText = "…" + string(r[len(r)-room:])
		}
		if room > 100 {
			desc += "\n\n**キルログ**\n```\n" + logText + "\n```"
		}
	}
	return &discordgo.MessageEmbed{
		Title:       fmt.Sprintf("直近の試合 — %s の勝利", winnerLabel),
		Description: desc,
		Color:       0x8e44ad,
		Footer:      &discordgo.MessageEmbedFooter{Text: fmt.Sprintf("部屋 %s ・ %d 日目まで ・ %s", code, days, time.Unix(ended, 0).In(time.FixedZone("JST", 9*3600)).Format("2006-01-02 15:04"))},
	}, nil
}

func (bot *Bot) tohkRankingEmbed(ctx context.Context, guildID string) (*discordgo.MessageEmbed, error) {
	pool := bot.PostgresInterface.Pool
	rank := func(order string) ([]string, error) {
		rows, err := pool.Query(ctx, `SELECT p.user_id::text, count(*), count(*) FILTER (WHERE p.won), coalesce(sum(p.kills), 0)
            FROM tohk_players p JOIN tohk_games g USING (result_id)
            WHERE g.guild_id = $1::numeric AND p.user_id IS NOT NULL AND p.role <> 'GM'
            GROUP BY p.user_id HAVING count(*) >= $2 ORDER BY `+order+` LIMIT 10`, guildID, tohkMinRankGames)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var lines []string
		for rows.Next() {
			var uid string
			var n, w, k int
			if err := rows.Scan(&uid, &n, &w, &k); err != nil {
				return nil, err
			}
			lines = append(lines, fmt.Sprintf("%d. <@%s> 勝率 %s (%d/%d) ・ %dキル", len(lines)+1, uid, tohkPct(w, n), w, n, k))
		}
		return lines, rows.Err()
	}
	byWin, err := rank(`count(*) FILTER (WHERE p.won)::float / count(*) DESC, count(*) DESC`)
	if err != nil {
		return nil, err
	}
	byKill, err := rank(`coalesce(sum(p.kills), 0) DESC, count(*) DESC`)
	if err != nil {
		return nil, err
	}
	if len(byWin) == 0 {
		return nil, nil
	}
	return &discordgo.MessageEmbed{
		Title: "TOH-K ランキング",
		Color: 0x8e44ad,
		Fields: []*discordgo.MessageEmbedField{
			{Name: "勝率", Value: strings.Join(byWin, "\n")},
			{Name: "キル数", Value: strings.Join(byKill, "\n")},
		},
		Footer: &discordgo.MessageEmbedFooter{Text: fmt.Sprintf("%d 試合以上の人だけ ・ TOH-K の結果送信を入れたホストの試合だけを数えています", tohkMinRankGames)},
	}, nil
}

// tohkResolveUnlinked: 起動時に、まだ AutoMuteUs の試合に紐づいていない最近の結果を照合し直す
// (照合の条件を直したときや、受信直後の再試行の間に AutoMuteUs が試合を記録できなかったとき用)
func (bot *Bot) tohkResolveUnlinked() {
	time.Sleep(10 * time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := bot.tohkEnsureSchema(ctx); err != nil {
		log.Println("tohk: schema check failed:", err)
		return
	}
	rows, err := bot.PostgresInterface.Pool.Query(ctx, `SELECT result_id FROM tohk_games
        WHERE amu_game_id IS NULL AND received_at > extract(epoch from now()) - 30*86400 ORDER BY ended_at`)
	if err != nil {
		log.Println("tohk: listing unlinked results failed:", err)
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	linked := 0
	for _, id := range ids {
		ok, err := bot.tohkResolve(ctx, id)
		if err != nil {
			log.Println("tohk: resolve failed:", err)
		}
		if ok {
			linked++
		}
	}
	if len(ids) > 0 {
		log.Printf("tohk: re-resolved unlinked results: %d of %d linked\n", linked, len(ids))
	}
}
