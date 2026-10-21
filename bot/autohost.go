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

	"github.com/automuteus/automuteus/v8/pkg/discord"
	"github.com/automuteus/automuteus/v8/pkg/game"
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
		State     string `json:"state"`
		Code      string `json:"code"`
		// 数値は AutoLobby/Python 側で float になっても読めるよう float64 で受ける
		Ping      *float64 `json:"ping"`
		Players   *float64 `json:"players"`
		Recreates *float64 `json:"recreates"`
		Announced *bool    `json:"announced"` // false の間は ping ゲートで部屋ガチャ中
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
	// Notice: 操作の結果ではない出来事 (放置で部屋が閉じられた など)。新しい部屋が確定すると消える
	Notice *struct {
		Text string `json:"text"`
		At   int64  `json:"at"`
	} `json:"notice"`
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

// inLobby: 部屋が確定してロビーで待機中。部屋ガチャ中 (announced=false) は含めない
func (st *autohostStatus) inLobby() bool {
	return st != nil && st.Busy == nil && st.Host.GameRunning && st.Lobby.State == "lobby" && !st.rolling()
}

// rolling: ping ゲートで部屋ガチャ中 (AutoLobby が自分で立て直している)
func (st *autohostStatus) rolling() bool {
	return st != nil && st.Lobby.State == "lobby" && st.Lobby.Announced != nil && !*st.Lobby.Announced
}

// usedElsewhere: 自動ホストが別のサーバーのゲームに紐づいたまま稼働している
func (st *autohostStatus) usedElsewhere(guildID string) bool {
	return st != nil && st.Link != nil && st.Link.GuildID != guildID && st.Host.GameRunning
}

// autohostComponents: ボタン行。状態に応じて押せないボタンは disabled にする
func autohostComponents(st *autohostStatus, guildID string) discordgo.ActionsRow {
	ready := st != nil && st.Busy == nil && !st.usedElsewhere(guildID)
	running := ready && st.Host.GameRunning
	lobby := ready && st.inLobby()
	// 部屋ボタンは 1 つ: 停止中 → ホストを起動して部屋を立てる / メニュー → 部屋建てだけ / ロビー → 立て直し
	// それ以外 (試合中・準備中・起動中) は押せない。どの操作になるかは押した時点の状態で決める (handleAutohostButton)
	startEnabled := ready && (!running || st.Lobby.State == "menu" || lobby)
	roomLabel, roomEmoji := "部屋を立てる", "▶️"
	if lobby {
		roomLabel, roomEmoji = "部屋を立て直す", "🔄"
	}
	// ホスト終了は /end も兼ねるので、操作中 (起動中・停止中) 以外はいつでも押せる
	// (エージェントが応答しない・部屋が閉じられた後でも、BranMuteUs のゲームは終わらせられる)
	stopEnabled := st == nil || st.Busy == nil
	// discordgo v0.27.1 の Button.Emoji は値型 (omitempty 無し) で、未指定だと "emoji":{} が送られて
	// Discord が COMPONENT_INVALID_EMOJI で弾く。絵文字は必ず Emoji に入れる
	return discordgo.ActionsRow{Components: []discordgo.MessageComponent{
		// よく押す試合開始とホスト終了を離すため、試合開始を左端に置く
		discordgo.Button{Label: "試合開始", Emoji: discordgo.ComponentEmoji{Name: "🎮"}, Style: discordgo.SuccessButton,
			CustomID: autohostBtnGameStart, Disabled: !lobby},
		discordgo.Button{Label: roomLabel, Emoji: discordgo.ComponentEmoji{Name: roomEmoji}, Style: discordgo.PrimaryButton,
			CustomID: autohostBtnStart, Disabled: !startEnabled},
		// ホスト終了: Bragame の自動ホスト用 Among Us を終了する (部屋は消える。設定の同期も再開する)
		discordgo.Button{Label: "ホスト終了", Emoji: discordgo.ComponentEmoji{Name: "⏹️"}, Style: discordgo.DangerButton,
			CustomID: autohostBtnStop, Disabled: !stopEnabled},
	}}
}

// gameStateComponents: game state メッセージの部品 (色の紐づけ + 自動ホストのボタン)。作成と編集の両方で使う
func gameStateComponents(guildID string) []discordgo.MessageComponent {
	placeholder := "Select your in-game color" // upstream はロケールを通さず英語固定
	if autohostEnabledFor(guildID) {
		placeholder = "ゲームで使っている色を選んでください"
	}
	components := []discordgo.MessageComponent{
		discordgo.ActionsRow{
			Components: []discordgo.MessageComponent{
				discordgo.SelectMenu{
					CustomID:    colorSelectID,
					Placeholder: placeholder,
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

// autohostStateLine: 自動ホストの今の段階を 1〜2 行で。Bragame の画面が見えないので、段階ごとに細かく出す
func autohostStateLine(st *autohostStatus, guildID string) string {
	switch {
	case st == nil:
		return "⚪ 自動ホスト bot に接続できません"
	case st.usedElsewhere(guildID):
		return "⚠️ 別のサーバーで使用中です"
	case st.Busy != nil && *st.Busy == "starting":
		return "⏳ **起動中** — Steam をホスト用アカウントに切り替えて TOH-K を起動しています (最大 3 分)"
	case st.Busy != nil:
		return "⏳ **停止中** — ホストを終了しています"
	case !st.AgentOK:
		return "⚪ Bragame のエージェントが応答しません (Bragame が未ログオン?)"
	case !st.Host.GameRunning:
		return "⏹ **停止中** — 「▶️ 部屋を立てる」で起動します"
	}
	switch st.Lobby.State {
	case "lobby":
		if st.Lobby.Announced != nil && !*st.Lobby.Announced {
			n := 1
			if st.Lobby.Recreates != nil {
				n = int(*st.Lobby.Recreates) + 1
			}
			s := fmt.Sprintf("🎲 **部屋ガチャ中** (%d部屋目)", n)
			if st.Lobby.Ping != nil {
				s += fmt.Sprintf(" — ping %.0fms", *st.Lobby.Ping)
			}
			return s + "\n東京サーバーの部屋を引くまで立て直しています"
		}
		return "🟢 **ロビーで待機中** — 人がそろったら「🎮 試合開始」"
	case "ingame":
		return "🎮 **試合中**"
	case "menu":
		return "⚪ **部屋が接続されていません** — 「▶️ 部屋を立てる」で部屋を作ります"
	case "signing-in":
		return "⏳ Among Us にサインインしています…"
	case "updating":
		return "⏳ TOH-K を自動更新しています… (終わると自動で再起動します)"
	case "creating":
		return "⏳ 部屋を作成しています…"
	case "booting", "":
		return "⏳ TOH-K を起動しています… (起動直後は 1〜2 分かかります)"
	}
	return fmt.Sprintf("⏳ 準備中 (%s)", st.Lobby.State)
}

func autohostTrim(s string) string {
	if len([]rune(s)) > autohostFieldMax {
		return string([]rune(s)[:autohostFieldMax]) + "…"
	}
	if s == "" {
		return "​"
	}
	return s
}

// autohostEmbedFields: 「自動ホスト」タイル。AutoMuteUs の Host / Voice Channel と同じ並び (inline の欄) で
// 部屋コード・ping・人数を大きく見せ、レギュレーションは別の欄にする。ValidFields で弾かれないよう Name/Value は必ず非空
func (bot *Bot) autohostEmbedFields(dgs *GameState) []*discordgo.MessageEmbedField {
	st := bot.autohostReadStatus()
	head := autohostStateLine(st, dgs.GuildID)
	// 成功した操作の結果は状態の行で十分なので出さない。失敗だけ一定時間出す
	if st != nil && st.LastAction != nil && !st.LastAction.OK && time.Since(time.Unix(st.LastAction.At, 0)) < autohostLastActionTTL {
		head += "\n⚠️ " + st.LastAction.Detail
	}
	if st != nil && st.Notice != nil && st.Notice.Text != "" && !st.inLobby() {
		head += "\nℹ️ " + st.Notice.Text
	}
	fields := []*discordgo.MessageEmbedField{{Name: "🤖 自動ホスト", Value: autohostTrim(head)}}

	// ping・参加は部屋が確定してから (部屋ガチャ中は出さない)。部屋コードは確定すると AutoMuteUs 自身が
	// 「🔒 ルームコード」で上に出すので、ここでは出さない
	if st.inLobby() && !st.usedElsewhere(dgs.GuildID) {
		if st.Lobby.Ping != nil {
			fields = append(fields, &discordgo.MessageEmbedField{Name: "📶 ping", Value: fmt.Sprintf("**%.0f** ms", *st.Lobby.Ping), Inline: true})
		}
		if st.Lobby.Players != nil {
			// 自動ホスト自身 (GM) を除いた人数
			fields = append(fields, &discordgo.MessageEmbedField{Name: "👥 参加", Value: fmt.Sprintf("**%d** 人", maxInt(int(*st.Lobby.Players)-1, 0)), Inline: true})
		}
	}

	if st != nil && st.Regulation != nil && st.Host.GameRunning && st.Lobby.State != "ingame" {
		r := st.Regulation
		name := "📋 レギュレーション"
		switch {
		case r.Name != "":
			name += " — " + r.Name
		case r.Preset != nil:
			name += fmt.Sprintf(" — プリセット%.0f", *r.Preset)
		}
		var b strings.Builder
		if r.ForPlay != nil {
			fmt.Fprintf(&b, "%.0f人で試合開始した場合\n", *r.ForPlay)
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
		b.WriteString(strings.Join(roles, " / "))
		opts := make([]string, 0, len(r.Options))
		for _, o := range r.Options {
			opts = append(opts, o.Name+" "+o.Value)
		}
		if len(opts) > 0 {
			b.WriteString("\n" + strings.Join(opts, " / "))
		}
		fields = append(fields, &discordgo.MessageEmbedField{Name: name, Value: autohostTrim(b.String())})
	}
	return fields
}

// connectionPayloadLinked: galactus の ConnectionJob の payload が「接続」か。この AutoMuteUs (8.3.x) は
// "true" しか想定していないが、galactus:latest は {"connected":true} を送るため、両方を受け付ける
// (これが無いと capture が繋がっていても「No capture linked」のままになる)
func connectionPayloadLinked(payload interface{}) bool {
	s, ok := payload.(string)
	if !ok {
		return false
	}
	if s == "true" {
		return true
	}
	var v struct {
		Connected bool `json:"connected"`
	}
	if err := json.Unmarshal([]byte(s), &v); err == nil {
		return v.Connected
	}
	return false
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// withAutohostField: game state の embed に自動ホストのタイルを足す (field 上限 25 を超えない範囲で)。
// capture 接続前の「No capture linked」とメニュー画面の英語の案内は、自動ホストでは誤解を招くので差し替える
func (bot *Bot) withAutohostField(me *discordgo.MessageEmbed, dgs *GameState) *discordgo.MessageEmbed {
	if me == nil || dgs == nil || !autohostEnabledFor(dgs.GuildID) {
		return me
	}
	if !dgs.Linked {
		me.Description = "⏳ 自動ホストとの接続を準備しています…"
		me.Color = discord.GREEN
	}
	if dgs.GameData.Phase == game.MENU && me.Footer != nil {
		// 「ロビーに参加してください」は手動ホスト向けの案内なので差し替える
		me.Footer.Text = "自動ホストが部屋を作ると、ここにプレイヤーが表示されます"
	}
	for _, f := range bot.autohostEmbedFields(dgs) {
		if len(me.Fields) >= 25 {
			break
		}
		me.Fields = append(me.Fields, f)
	}
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

// autohostLink: /new で発行した connect code を自動ホスト bot に渡す。autostart=true なら、ホストが止まっていて
// /new した人に操作権限があるとき、自動ホスト bot がそのままホストを起動する
func (bot *Bot) autohostLink(guildID, connectCode, userID string, allowed bool) {
	if autohostEnabledFor(guildID) {
		go bot.autohostPublish(autohostChLink, map[string]interface{}{
			"guild_id": guildID, "connect_code": connectCode,
			"autostart": true, "user_id": userID, "allowed": allowed,
		})
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
	action := strings.TrimPrefix(customID, autohostButtonPrefix)
	st := bot.autohostReadStatus()
	if action == "stop" && (st == nil || st.usedElsewhere(i.GuildID)) {
		// 自動ホスト bot に届かない・別サーバーで使用中でも、このサーバーの BranMuteUs のゲームは /end と同じように終わらせる
		go bot.autohostEndGame(GameStateRequest{GuildID: i.GuildID, TextChannel: i.ChannelID})
		if st == nil {
			go bot.autohostPublish(autohostChActions, map[string]interface{}{
				"id": i.ID, "action": action, "guild_id": i.GuildID, "user_id": i.Member.User.ID, "allowed": allowed,
			})
			return reply("受け付けました: BranMuteUs のゲームを終了します (自動ホスト bot に届かないため、ホストの終了は反映されないかもしれません)")
		}
		return reply("受け付けました: BranMuteUs のゲームを終了します (自動ホストは別のサーバーで使用中のため止めません)")
	}
	if st == nil {
		return reply("⚪ 自動ホスト bot に接続できません。")
	}
	if st.usedElsewhere(i.GuildID) {
		return reply("⚠️ 自動ホストは別のサーバーで使用中です。")
	}

	if action == "start" {
		switch {
		case st.inLobby():
			action = "recreate" // ロビーに居るとき: 立て直し
		case st.Host.GameRunning:
			action = "room" // 起動済みでメニューに居るとき: 部屋建てだけ
		}
	}
	label := map[string]string{"start": "部屋を立てる", "room": "部屋を立てる", "gamestart": "試合開始",
		"recreate": "部屋を立て直す", "stop": "ホスト終了 (BranMuteUs のゲームも終了します)"}[action]
	if action == "stop" {
		// ホスト終了では AutoMuteUs のゲームも /end と同じように終わらせる (パネルのメッセージも消える)
		gsr := GameStateRequest{GuildID: i.GuildID, TextChannel: i.ChannelID}
		if st.Link != nil && st.Link.GuildID == i.GuildID {
			gsr.ConnectCode = st.Link.ConnectCode
		}
		go bot.autohostEndGame(gsr)
	}
	// redis が詰まっても 2 秒の応答期限を守れるよう、publish は待たない
	go bot.autohostPublish(autohostChActions, map[string]interface{}{
		"id": i.ID, "action": action, "guild_id": i.GuildID, "user_id": i.Member.User.ID, "allowed": allowed,
	})
	return reply(fmt.Sprintf("受け付けました: %s (結果は上のメッセージに出ます)", label))
}

// autohostNewResponse: 自動ホスト連携が有効なサーバーの /new の返信。AmongUsCapture のリンクは
// 自動ホストでは使わず、AutoMuteUs を使い慣れた人ほど混乱するので出さない (本人だけに見える)
func (bot *Bot) autohostNewResponse(guildID string, allowed bool) *discordgo.InteractionResponse {
	st := bot.autohostReadStatus()
	var msg string
	switch {
	case st != nil && !st.usedElsewhere(guildID) && st.Busy == nil && !st.Host.GameRunning && st.AgentOK && allowed:
		// /new の link で自動ホスト bot が起動を始める (autohostLink の autostart)
		msg = "⏳ 自動ホストを起動しています。このまま待つと部屋が立ちます (1〜3 分)。\n" +
			"部屋コードとプレイヤーは下のメッセージに表示されます。AmongUsCapture は不要です。"
	case st != nil && !st.Host.GameRunning && st.Busy == nil && !allowed:
		msg = "🤖 自動ホストと連携しました。ホストを起動する権限がないため、権限のある人に「▶️ 部屋を立てる」を押してもらってください。"
	case st == nil:
		msg = "🤖 自動ホスト連携は有効ですが、自動ホスト bot に接続できません。Bragame の状態を確認してください。"
	case st.usedElsewhere(guildID):
		msg = "⚠️ 自動ホストは別のサーバーで使用中です。そちらで停止してから、もう一度 /new してください。"
	case st.Busy != nil && *st.Busy == "starting":
		msg = "⏳ 自動ホストを起動しています。このまま待つと部屋が立ち、下のメッセージに部屋コードが表示されます。"
	case st.inLobby():
		msg = fmt.Sprintf("🟢 自動ホストの部屋 `%s` が立っています。人がそろったら、下のメッセージの「🎮 試合開始」を押してください。", st.Lobby.Code)
	case st.Host.GameRunning:
		msg = "⏳ 自動ホストは起動中です。部屋が立つと、下のメッセージに部屋コードが表示されます。"
	default:
		msg = "🤖 自動ホストと連携しました。\n下のメッセージの「▶️ 部屋を立てる」を押すと、Bragame でホストが起動して部屋が立ちます (1〜3 分)。" +
			"部屋コードとプレイヤーは下のメッセージに表示されます。AmongUsCapture は不要です。"
	}
	return &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Flags: discordgo.MessageFlagsEphemeral, Content: msg},
	}
}

// autohostEndGame: 「ホスト終了」で AutoMuteUs のゲームも /end と同じ手順で終わらせる
// (購読 goroutine に kill を送る → forceEndGame がメッセージを消して autohost に unlink を送る → 全員のミュート解除)
func (bot *Bot) autohostEndGame(gsr GameStateRequest) {
	dgs := bot.RedisInterface.GetReadOnlyDiscordGameState(gsr)
	if dgs == nil || !dgs.GameStateMsg.Exists() {
		return
	}
	bot.ChannelsMapLock.Lock()
	v, ok := bot.EndGameChannels[dgs.ConnectCode]
	delete(bot.EndGameChannels, dgs.ConnectCode)
	bot.ChannelsMapLock.Unlock()
	if ok {
		v <- true
	} else {
		bot.autohostUnlink(dgs.GuildID, dgs.ConnectCode)
	}
	if err := bot.applyToAll(dgs, false, false); err != nil {
		log.Println("autohost: unmute on host end failed:", err)
	}
	log.Printf("autohost: ended game %s in guild %s by host end button\n", dgs.ConnectCode, dgs.GuildID)
}

var (
	autohostRecreateLock sync.Mutex
	autohostLastRecreate = map[string]time.Time{}
)

const autohostRecreateInterval = 10 * time.Minute

func autohostRecreateAllowed(guildID string) bool {
	autohostRecreateLock.Lock()
	defer autohostRecreateLock.Unlock()
	if time.Since(autohostLastRecreate[guildID]) < autohostRecreateInterval {
		return false
	}
	autohostLastRecreate[guildID] = time.Now()
	return true
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
				// 1 時間を過ぎたメッセージは DispatchRefreshOrEdit が作り直す (削除+送信)。自動ホストの状態変化の
				// たびに作り直すとメッセージが飛び回るので、自動ホスト由来の作り直しは同じサーバーで 10 分に 1 回まで
				if dgs.shouldRefresh() && !autohostRecreateAllowed(req.GuildID) {
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
