package bot

// Among Us 自動ホスト (amongus-autohost-setup) との連携。AUTOHOST_ENABLE=1 のときだけ有効。
//
// /new の game state メッセージに「自動ホスト」タイルと操作ボタンを足し、ボタン操作・/new・/end を
// この AutoMuteUs 自身の redis を介して自動ホスト bot に伝える (同じホストで動く前提)。
// 仕様: amongus-autohost-setup/docs/automuteus-panel-design.md
//
//	autohost:status  (string)  自動ホスト bot が書く状態 JSON。タイルとボタンの有効/無効に使う
//	autohost:refresh (pub/sub) 自動ホスト bot → ここ。{guild_id, connect_code} のメッセージを更新する
//	autohost:actions (pub/sub) ここ → bot。ボタン操作 {id, action, guild_id, user_id, allowed}
//	autohost:link    (pub/sub) ここ → bot。/new で発行した connect code {guild_id, connect_code}
//	autohost:unlink  (pub/sub) ここ → bot。/end {guild_id, connect_code}

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/automuteus/automuteus/v8/pkg/settings"
	"github.com/bwmarrin/discordgo"
)

const (
	autohostStatusKey    = "autohost:status"
	autohostChRefresh    = "autohost:refresh"
	autohostChActions    = "autohost:actions"
	autohostChLink       = "autohost:link"
	autohostChUnlink     = "autohost:unlink"
	autohostButtonPrefix = "autohost:"

	autohostBtnStart     = "autohost:start"
	autohostBtnGameStart = "autohost:gamestart"
	autohostBtnRecreate  = "autohost:recreate"
	autohostBtnStop      = "autohost:stop"

	// Discord の field value 上限は 1024 文字
	autohostFieldMax = 1000
	// 直近の操作結果をタイルに出す期間
	autohostLastActionTTL = 10 * time.Minute
)

var (
	autohostEnabled = os.Getenv("AUTOHOST_ENABLE") == "1"
	autohostGuilds  = autohostIDSet(os.Getenv("AUTOHOST_GUILD_IDS"))
	autohostRoles   = autohostIDSet(os.Getenv("AUTOHOST_ALLOWED_ROLE_IDS"))

	autohostPressLock sync.Mutex
	autohostPressed   = map[string]time.Time{}

	// game state メッセージの作成/編集 (GameState のメソッドで Bot を持たない) から状態を読むため
	autohostBot *Bot
)

func autohostIDSet(s string) map[string]bool {
	set := map[string]bool{}
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			set[v] = true
		}
	}
	return set
}

// autohostEnabledFor: このサーバーで自動ホスト連携を使うか (AUTOHOST_GUILD_IDS が空なら全サーバー)
func autohostEnabledFor(guildID string) bool {
	return autohostEnabled && (len(autohostGuilds) == 0 || autohostGuilds[guildID])
}

type autohostStatus struct {
	Busy    *string `json:"busy"`
	AgentOK bool    `json:"agent_ok"`
	Host    struct {
		GameRunning        bool   `json:"game_running"`
		SteamIsHostAccount bool   `json:"steam_is_host_account"`
		SteamPersona       string `json:"steam_persona"`
	} `json:"host"`
	Lobby struct {
		State string `json:"state"`
		Code  string `json:"code"`
		// 数値は AutoLobby/Python 側で float になっても読めるよう float64 で受ける
		Ping      *float64 `json:"ping"`
		Players   *float64 `json:"players"`
		Recreates *float64 `json:"recreates"`
	} `json:"lobby"`
	Regulation *struct {
		Name    string   `json:"name"`
		Preset  *float64 `json:"preset"`
		ForPlay *float64 `json:"for_players"`
		Roles   []struct {
			Name   string  `json:"name"`
			Side   string  `json:"side"`
			Count  float64 `json:"count"`
			Chance float64 `json:"chance"`
		} `json:"roles"`
		Options []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"options"`
	} `json:"regulation"`
	Link *struct {
		GuildID     string `json:"guild_id"`
		ConnectCode string `json:"connect_code"`
	} `json:"link"`
	LastAction *struct {
		Action string `json:"action"`
		OK     bool   `json:"ok"`
		Detail string `json:"detail"`
		At     int64  `json:"at"`
		By     string `json:"by"`
	} `json:"last_action"`
	UpdatedAt int64 `json:"updated_at"`
}

// autohostReadStatus は描画パスから呼ばれるので、redis が詰まっても 300ms で諦める
func (bot *Bot) autohostReadStatus() *autohostStatus {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	raw, err := bot.RedisInterface.client.Get(ctx, autohostStatusKey).Result()
	if err != nil {
		return nil
	}
	var st autohostStatus
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		// 型の食い違いはタイルとボタンを全部止めてしまうので、原因が分かるよう生の JSON も出す (1 分に 1 回まで)
		autohostLogLock.Lock()
		if time.Since(autohostLastBadJSONLog) > time.Minute {
			autohostLastBadJSONLog = time.Now()
			log.Printf("autohost: bad status json: %v raw=%.500s", err, raw)
		}
		autohostLogLock.Unlock()
		return nil
	}
	return &st
}

var (
	autohostLogLock        sync.Mutex
	autohostLastBadJSONLog time.Time
)

func (st *autohostStatus) inLobby() bool {
	return st != nil && st.Busy == nil && st.Host.GameRunning && st.Lobby.State == "lobby"
}

// usedElsewhere: 自動ホストが別のサーバーのゲームに紐づいたまま稼働している
func (st *autohostStatus) usedElsewhere(guildID string) bool {
	return st != nil && st.Link != nil && st.Link.GuildID != guildID && st.Host.GameRunning
}

// autohostComponents: ボタン行。状態に応じて押せないボタンは disabled にする
func autohostComponents(st *autohostStatus, guildID string) discordgo.ActionsRow {
	ready := st != nil && st.Busy == nil && !st.usedElsewhere(guildID)
	running := ready && st.Host.GameRunning
	// 停止中: ホストを起動して部屋を立てる / 起動済みでメニューに居る: 部屋建てだけ / それ以外 (ロビー・試合中・準備中): 押せない
	startEnabled := ready && (!running || st.Lobby.State == "menu")
	return discordgo.ActionsRow{Components: []discordgo.MessageComponent{
		discordgo.Button{Label: "▶ 部屋を立てる", Style: discordgo.PrimaryButton, CustomID: autohostBtnStart,
			Disabled: !startEnabled},
		discordgo.Button{Label: "🎮 ゲーム開始", Style: discordgo.SuccessButton, CustomID: autohostBtnGameStart,
			Disabled: !st.inLobby() || !ready},
		discordgo.Button{Label: "🔄 立て直し", Style: discordgo.SecondaryButton, CustomID: autohostBtnRecreate,
			Disabled: !st.inLobby() || !ready},
		discordgo.Button{Label: "⏹ 停止", Style: discordgo.DangerButton, CustomID: autohostBtnStop,
			Disabled: !running},
	}}
}

// gameStateComponents: game state メッセージの部品 (色の紐づけ + 自動ホストのボタン)。作成と編集の両方で使う
func gameStateComponents(guildID string) []discordgo.MessageComponent {
	components := []discordgo.MessageComponent{
		discordgo.ActionsRow{
			Components: []discordgo.MessageComponent{
				discordgo.SelectMenu{
					CustomID:    colorSelectID,
					Placeholder: "Select your in-game color",
					Options:     EmojisToSelectMenuOptions(GlobalAlivenessEmojis[true], X),
				},
			},
		},
	}
	if autohostEnabledFor(guildID) && autohostBot != nil {
		components = append(components, autohostComponents(autohostBot.autohostReadStatus(), guildID))
	}
	return components
}

// editMessageEmbedComponents: editMessageEmbed の部品付き版。ボタンの disabled を切り替えるには編集のたびに渡す必要がある
func editMessageEmbedComponents(s *discordgo.Session, channelID, messageID string, me *discordgo.MessageEmbed, components []discordgo.MessageComponent) *discordgo.Message {
	edit := discordgo.NewMessageEdit(channelID, messageID).SetEmbed(me)
	edit.Components = components
	msg, err := s.ChannelMessageEditComplex(edit)
	if err != nil {
		log.Println("autohost: error when editing game state message with components", err)
	}
	return msg
}

// autohostEmbedField: 「自動ホスト」タイル。ValidFields で弾かれないよう Name/Value は必ず非空
func (bot *Bot) autohostEmbedField(guildID string) *discordgo.MessageEmbedField {
	st := bot.autohostReadStatus()
	var b strings.Builder
	switch {
	case st == nil:
		b.WriteString("⚪ 自動ホスト bot に接続できません")
	case st.usedElsewhere(guildID):
		b.WriteString("⚠️ 別のサーバーで使用中です")
	case st.Busy != nil && *st.Busy == "starting":
		b.WriteString("⏳ ホストを起動しています… (Steam の切り替え込みで最大 3 分)")
	case st.Busy != nil:
		b.WriteString("⏳ ホストを停止しています…")
	case !st.AgentOK:
		b.WriteString("⚪ Bragame のエージェントが応答しません (未ログオン?)")
	case !st.Host.GameRunning:
		b.WriteString("⏹ 停止中 — 「▶ 部屋を立てる」で起動します")
	case st.Lobby.State == "lobby":
		b.WriteString("🟢 ロビー")
		if st.Lobby.Code != "" {
			fmt.Fprintf(&b, " `%s`", st.Lobby.Code)
		}
		if st.Lobby.Ping != nil {
			fmt.Fprintf(&b, " / ping %.0fms", *st.Lobby.Ping)
		}
		if st.Lobby.Players != nil {
			// 自動ホスト自身 (GM) を除いた人数
			fmt.Fprintf(&b, " / 参加 %d人", maxInt(int(*st.Lobby.Players)-1, 0))
		}
	case st.Lobby.State == "ingame":
		b.WriteString("🎮 試合中")
	default:
		fmt.Fprintf(&b, "⏳ 部屋を準備中 (%s)", st.Lobby.State)
	}

	if st != nil && st.Regulation != nil && st.Host.GameRunning {
		r := st.Regulation
		b.WriteString("\n📋 ")
		if r.Name != "" {
			b.WriteString(r.Name)
		} else if r.Preset != nil {
			fmt.Fprintf(&b, "プリセット%.0f", *r.Preset)
		}
		if r.ForPlay != nil {
			fmt.Fprintf(&b, " (%.0f人で開始した場合)", *r.ForPlay)
		}
		roles := make([]string, 0, len(r.Roles))
		for _, role := range r.Roles {
			s := role.Name
			if role.Count > 1 {
				s += fmt.Sprintf("×%.0f", role.Count)
			}
			if role.Chance > 0 && role.Chance < 100 {
				s += fmt.Sprintf("(%.0f%%)", role.Chance)
			}
			roles = append(roles, s)
		}
		if len(roles) > 0 {
			b.WriteString("\n" + strings.Join(roles, " / "))
		}
		opts := make([]string, 0, len(r.Options))
		for _, o := range r.Options {
			opts = append(opts, o.Name+" "+o.Value)
		}
		if len(opts) > 0 {
			b.WriteString("\n" + strings.Join(opts, " / "))
		}
	}

	if st != nil && st.LastAction != nil && time.Since(time.Unix(st.LastAction.At, 0)) < autohostLastActionTTL {
		mark := "✅"
		if !st.LastAction.OK {
			mark = "⚠️"
		}
		fmt.Fprintf(&b, "\n%s %s", mark, st.LastAction.Detail)
	}

	value := b.String()
	if len([]rune(value)) > autohostFieldMax {
		value = string([]rune(value)[:autohostFieldMax]) + "…"
	}
	return &discordgo.MessageEmbedField{Name: "🤖 自動ホスト", Value: value, Inline: false}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// withAutohostField: game state の embed に自動ホストのタイルを足す (field 上限 25 を超えない範囲で)
func (bot *Bot) withAutohostField(me *discordgo.MessageEmbed, guildID string) *discordgo.MessageEmbed {
	if me == nil || !autohostEnabledFor(guildID) || len(me.Fields) >= 25 {
		return me
	}
	me.Fields = append(me.Fields, bot.autohostEmbedField(guildID))
	return me
}

func (bot *Bot) autohostPublish(channel string, payload map[string]interface{}) {
	b, err := json.Marshal(payload)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := bot.RedisInterface.client.Publish(ctx, channel, string(b)).Err(); err != nil {
		log.Println("autohost: publish", channel, "failed:", err)
	}
}

func (bot *Bot) autohostLink(guildID, connectCode string) {
	if autohostEnabledFor(guildID) {
		go bot.autohostPublish(autohostChLink, map[string]interface{}{"guild_id": guildID, "connect_code": connectCode})
	}
}

func (bot *Bot) autohostUnlink(guildID, connectCode string) {
	if autohostEnabledFor(guildID) {
		go bot.autohostPublish(autohostChUnlink, map[string]interface{}{"guild_id": guildID, "connect_code": connectCode})
	}
}

// autohostAllowed: サーバー管理者・オーナー・AUTOHOST_ALLOWED_ROLE_IDS・AutoMuteUs の権限ロールのどれか
func (bot *Bot) autohostAllowed(i *discordgo.InteractionCreate, sett *settings.GuildSettings) bool {
	m := i.Member
	if m.Permissions&discordgo.PermissionAdministrator != 0 {
		return true
	}
	if g, err := bot.PrimarySession.State.Guild(i.GuildID); err == nil && g.OwnerID == m.User.ID {
		return true
	}
	if len(sett.AdminUserIDs) > 0 && sett.HasAdminPerms(m.User) {
		return true
	}
	for _, r := range m.Roles {
		if autohostRoles[r] {
			return true
		}
		for _, pr := range sett.PermissionRoleIDs {
			if pr == r {
				return true
			}
		}
	}
	return false
}

// handleAutohostButton: ボタンは 2 秒以内に必ず応答する (実際の操作は自動ホスト bot が非同期で行い、結果はタイルに出る)。
// 既存のレート制限 (超過でソフトBAN) は通さず、同じ人の同じボタンの 1.5 秒以内の連打だけ無視する
func (bot *Bot) handleAutohostButton(i *discordgo.InteractionCreate, sett *settings.GuildSettings) *discordgo.InteractionResponse {
	reply := func(msg string) *discordgo.InteractionResponse {
		return &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{Flags: 1 << 6, Content: msg},
		}
	}
	customID := i.MessageComponentData().CustomID
	if !autohostEnabledFor(i.GuildID) {
		return reply("このサーバーでは自動ホスト連携が無効です。")
	}
	key := i.Member.User.ID + "|" + customID
	autohostPressLock.Lock()
	last, seen := autohostPressed[key]
	if seen && time.Since(last) < 1500*time.Millisecond {
		autohostPressLock.Unlock()
		return reply("⏳ 処理中です。")
	}
	autohostPressed[key] = time.Now()
	if len(autohostPressed) > 200 {
		for k, t := range autohostPressed {
			if time.Since(t) > time.Minute {
				delete(autohostPressed, k)
			}
		}
	}
	autohostPressLock.Unlock()

	allowed := bot.autohostAllowed(i, sett)
	if !allowed {
		return reply("⚠️ 自動ホストを操作する権限がありません。")
	}
	st := bot.autohostReadStatus()
	if st == nil {
		return reply("⚪ 自動ホスト bot に接続できません。")
	}
	if st.usedElsewhere(i.GuildID) {
		return reply("⚠️ 自動ホストは別のサーバーで使用中です。")
	}

	action := strings.TrimPrefix(customID, autohostButtonPrefix)
	label := map[string]string{"start": "部屋を立てる", "gamestart": "ゲーム開始", "recreate": "立て直し", "stop": "停止"}[action]
	if action == "start" && st.Host.GameRunning {
		action = "room" // 起動済みでメニューに居るとき: 部屋建てだけ
	}
	// redis が詰まっても 2 秒の応答期限を守れるよう、publish は待たない
	go bot.autohostPublish(autohostChActions, map[string]interface{}{
		"id": i.ID, "action": action, "guild_id": i.GuildID, "user_id": i.Member.User.ID, "allowed": allowed,
	})
	return reply(fmt.Sprintf("受け付けました: %s (結果は上のメッセージに出ます)", label))
}

var autohostSubscribeOnce sync.Once

// InitAutohost: ハンドラ登録/Open より前に呼ぶ (起動直後に作られるメッセージにもボタンを付けるため)
func (bot *Bot) InitAutohost() {
	if !autohostEnabled {
		return
	}
	autohostBot = bot
	bot.StartAutohostSubscriber()
}

// StartAutohostSubscriber: 自動ホスト bot からの更新依頼を受けて game state メッセージを更新する (何度呼んでも購読は 1 本)
func (bot *Bot) StartAutohostSubscriber() {
	if !autohostEnabled {
		return
	}
	autohostSubscribeOnce.Do(func() { go bot.autohostRefreshLoop() })
}

func (bot *Bot) autohostRefreshLoop() {
	{
		for {
			sub := bot.RedisInterface.client.Subscribe(context.Background(), autohostChRefresh)
			log.Println("autohost: subscribed to", autohostChRefresh)
			for msg := range sub.Channel() {
				var req struct {
					GuildID     string `json:"guild_id"`
					ConnectCode string `json:"connect_code"`
				}
				if err := json.Unmarshal([]byte(msg.Payload), &req); err != nil || req.GuildID == "" || req.ConnectCode == "" {
					continue
				}
				if !autohostEnabledFor(req.GuildID) {
					continue
				}
				gsr := GameStateRequest{GuildID: req.GuildID, ConnectCode: req.ConnectCode}
				dgs := bot.RedisInterface.GetReadOnlyDiscordGameState(gsr)
				if dgs == nil || !dgs.GameStateMsg.Exists() {
					continue
				}
				// 1 時間を過ぎたメッセージは編集できず、DispatchRefreshOrEdit が作り直す (削除+送信)。
				// 自動ホストの状態変化のたびに作り直すとメッセージが飛び回るので、作り直しはゲームイベント側に任せる
				if dgs.shouldRefresh() {
					continue
				}
				go bot.DispatchRefreshOrEdit(dgs, gsr, bot.StorageInterface.GetGuildSettings(req.GuildID))
			}
			_ = sub.Close()
			log.Println("autohost: refresh subscription closed; retrying in 5s")
			time.Sleep(5 * time.Second)
		}
	}
}
