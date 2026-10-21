package main

import (
	_ "embed"
	"errors"
	"fmt"
	"github.com/automuteus/automuteus/v8/bot/command"
	"github.com/automuteus/automuteus/v8/bot/tokenprovider"
	"github.com/automuteus/automuteus/v8/internal/server"
	"github.com/automuteus/automuteus/v8/pkg/capture"
	"github.com/automuteus/automuteus/v8/pkg/locale"
	storage2 "github.com/automuteus/automuteus/v8/pkg/storage"
	"github.com/bwmarrin/discordgo"
	"io"
	"log"
	"math/rand"
	"os"
	"os/signal"
	"path"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/automuteus/automuteus/v8/storage"

	"github.com/automuteus/automuteus/v8/bot"
)

var (
	version = "v8.1.0"
	commit  = "none"
	date    = "unknown"
)

//go:embed storage/postgres.sql
var postgresFileContents string

const (
	DefaultURL                   = "http://localhost:8123"
	DefaultMaxRequests5Sec int64 = 7
)

type registeredCommand struct {
	GuildID            string
	ApplicationCommand *discordgo.ApplicationCommand
}

func main() {
	// seed the rand generator (used for making connection codes)
	rand.Seed(time.Now().Unix())
	err := discordMainWrapper()
	if err != nil {
		log.Println("Program exited with the following error:")
		log.Println(err)
		return
	}
}

func discordMainWrapper() error {
	var isOfficial = os.Getenv("AUTOMUTEUS_OFFICIAL") != ""

	discordToken := os.Getenv("DISCORD_BOT_TOKEN")
	if discordToken == "" {
		return errors.New("no DISCORD_BOT_TOKEN provided")
	}
	logPath := os.Getenv("LOG_PATH")
	if logPath == "" {
		logPath = "./"
	}

	logEntry := os.Getenv("DISABLE_LOG_FILE")
	if logEntry == "" {
		file, err := os.Create(path.Join(logPath, "logs.txt"))
		if err != nil {
			return err
		}
		mw := io.MultiWriter(os.Stdout, file)
		log.SetOutput(mw)
	}

	emojiGuildID := os.Getenv("EMOJI_GUILD_ID")

	log.Println(version + "-" + commit)

	numShardsStr := os.Getenv("NUM_SHARDS")
	numShards, err := strconv.Atoi(numShardsStr)
	if err != nil {
		log.Println("No NUM_SHARDS specified; defaulting to 1")
		numShards = 1
	}

	shardIDStr := os.Getenv("SHARD_ID")
	if shardIDStr != "" {
		return errors.New("SHARD_ID is no longer supported! Please use SHARDS instead")
	}

	var shards shards
	shardsStr := os.Getenv("SHARDS")
	if shardsStr == "" {
		log.Println("No SHARDS specified, defaulting to 0")
		shards = defaultShard()
	} else {
		shards, err = parseShards(shardsStr, numShards)
		if err != nil {
			return err
		}
	}

	url := os.Getenv("HOST")
	if url == "" {
		log.Printf("[Info] No valid HOST provided. Defaulting to %s\n", DefaultURL)
		url = DefaultURL
	}

	var redisClient bot.RedisInterface
	var storageInterface storage.StorageInterface

	redisAddr := os.Getenv("REDIS_ADDR")
	redisPassword := os.Getenv("REDIS_PASS")
	if redisAddr != "" {
		err := redisClient.Init(storage.RedisParameters{
			Addr:     redisAddr,
			Username: "",
			Password: redisPassword,
		})
		if err != nil {
			log.Println(err)
		}
		err = storageInterface.Init(storage.RedisParameters{
			Addr:     redisAddr,
			Username: "",
			Password: redisPassword,
		})
		if err != nil {
			log.Println(err)
		}
	} else {
		return errors.New("no REDIS_ADDR specified; exiting")
	}

	locale.InitLang(os.Getenv("LOCALE_PATH"), os.Getenv("BOT_LANG"))

	psql := storage2.PsqlInterface{}
	pAddr := os.Getenv("POSTGRES_ADDR")
	if pAddr == "" {
		return errors.New("no POSTGRES_ADDR specified; exiting")
	}

	pUser := os.Getenv("POSTGRES_USER")
	if pUser == "" {
		return errors.New("no POSTGRES_USER specified; exiting")
	}

	pPass := os.Getenv("POSTGRES_PASS")
	if pPass == "" {
		return errors.New("no POSTGRES_PASS specified; exiting")
	}

	err = psql.Init(storage2.ConstructPsqlConnectURL(pAddr, pUser, pPass))
	if err != nil {
		return err
	}

	if !isOfficial {
		go func() {
			err := psql.ExecFromString(postgresFileContents)
			if err != nil {
				log.Println("Exiting with fatal error when attempting to execute postgres.sql:")
				log.Fatal(err)
			}
		}()
	}

	log.Println("Bot is now running.  Press CTRL-C to exit.")
	sc := make(chan os.Signal, 1)
	signal.Notify(sc, syscall.SIGINT, syscall.SIGTERM, os.Interrupt)

	go server.StartHealthCheckServer("8080")

	topGGToken := os.Getenv("TOP_GG_TOKEN")

	taskTimeoutms := capture.DefaultCaptureBotTimeout

	taskTimeoutmsStr := os.Getenv("ACK_TIMEOUT_MS")
	num, err := strconv.ParseInt(taskTimeoutmsStr, 10, 64)
	if err == nil {
		log.Printf("Read from env; using ACK_TIMEOUT_MS=%d\n", num)
		taskTimeoutms = time.Millisecond * time.Duration(num)
	}

	maxReq5Sec := os.Getenv("MAX_REQ_5_SEC")
	maxReq := DefaultMaxRequests5Sec
	num, err = strconv.ParseInt(maxReq5Sec, 10, 64)
	if err == nil {
		maxReq = num
	}

	tokenProvider := tokenprovider.NewTokenProvider(nil, nil, taskTimeoutms, maxReq)
	var extraTokens []string
	extraTokenStr := strings.ReplaceAll(os.Getenv("WORKER_BOT_TOKENS"), " ", "")
	if extraTokenStr != "" {
		extraTokens = strings.Split(extraTokenStr, ",")
	}

	bots := make([]*bot.Bot, len(shards))
	for i, shard := range shards {
		bots[i] = bot.MakeAndStartBot(version, commit, discordToken, topGGToken, url, emojiGuildID, numShards, int(shard), &redisClient, &storageInterface, &psql, logPath)
		if bots[i] == nil {
			log.Fatalf("bot %d failed to initialize; did you provide a valid Discord Bot Token?", shard)
		}
	}

	// initialize the token provider using the first shard's redis client and primary session
	bots[0].InitTokenProvider(tokenProvider)
	for i := 0; i < len(shards); i++ {
		bots[i].TokenProvider = tokenProvider
	}
	tokenProvider.PopulateAndStartSessions(extraTokens)
	// indicate to Kubernetes that we're ready to start receiving traffic
	server.GlobalReady = true

	go bots[0].StartMetricsServer(os.Getenv("SCW_NODE_ID"))

	go bots[0].StartAPIServer("5000")

	// empty string entry = global
	slashCommandGuildIds := []string{""}
	slashCommandGuildIdStr := strings.ReplaceAll(os.Getenv("SLASH_COMMAND_GUILD_IDS"), " ", "")
	if slashCommandGuildIdStr != "" {
		slashCommandGuildIds = strings.Split(slashCommandGuildIdStr, ",")
	}

	// only register commands if we're not the official bot, OR we're the primary/main shard
	var registeredCommands []registeredCommand
	if !isOfficial || shards.isPrimaryShard() {
		// fork: 1 件ずつの create + 終了時の delete だと、再起動のたびに「新規作成」になって
		// Discord の 1 日 200 件の作成上限にすぐ達する (上限に達すると /new などが消えたままになる)。
		// bulk overwrite なら既にあるコマンドは更新扱いで上限を消費しない。終了時の削除も既定では行わない
		for _, guild := range slashCommandGuildIds {
			if guild == "" {
				log.Printf("Registering %d commands GLOBALLY (bulk overwrite)\n", len(command.All))
			} else {
				log.Printf("Registering %d commands in guild %s (bulk overwrite)\n", len(command.All), guild)
			}
			cmds, err := registerSlashCommands(bots[0].PrimarySession, guild)
			// 作成上限で作れなかったコマンドは、上限が空き次第 1 件ずつ作る (/new を優先)
			go ensureSlashCommands(bots[0].PrimarySession, guild)
			if err != nil {
				log.Printf("Cannot register commands in guild %q: %v", guild, err)
				continue
			}
			for _, c := range cmds {
				registeredCommands = append(registeredCommands, registeredCommand{GuildID: guild, ApplicationCommand: c})
			}
		}
		log.Println("Finishing registering all commands!")
	}

	<-sc
	log.Printf("Received Sigterm or Kill signal. Bot will terminate in 1 second")
	time.Sleep(time.Second)

	// only delete the slash commands if we're not the official bot, AND we're the primary/"master" shard
	if !isOfficial && shards.isPrimaryShard() && os.Getenv("SLASH_COMMAND_DELETE_ON_EXIT") == "1" {
		log.Println("Deleting slash commands")
		for _, v := range registeredCommands {
			if v.GuildID == "" {
				log.Printf("Deleting command %s GLOBALLY\n", v.ApplicationCommand.Name)
			} else {
				log.Printf("Deleting command %s on guild %s\n", v.ApplicationCommand.Name, v.GuildID)
			}
			err = bots[0].PrimarySession.ApplicationCommandDelete(v.ApplicationCommand.ApplicationID, v.GuildID, v.ApplicationCommand.ID)
			if err != nil {
				log.Println(err)
			}
		}
		log.Println("Finished deleting all commands")
	}

	for _, v := range bots {
		v.Close()
	}
	tokenProvider.Close()
	return nil
}

type shards []uint8

func defaultShard() shards {
	return []uint8{0}
}

// isPrimaryShard ensures that the FIRST shard running is the 0th/primary shard.
// This prevents performing additional work when shard instances may overlap
// (for example, an instance running 0,1, and another running 1,0)
func (sr shards) isPrimaryShard() bool {
	return len(sr) > 0 && sr[0] == 0
}

func parseShards(str string, maxShards int) (shards, error) {
	var shards shards

	tokens := strings.Split(strings.ReplaceAll(str, " ", ""), ",")
	for _, token := range tokens {
		v, err := strconv.ParseUint(token, 10, 64)
		if err != nil {
			return shards, err
		}
		if v >= uint64(maxShards) {
			return shards, fmt.Errorf("shard: %d is greater or equal to the total max shards: %d", v, maxShards)
		}
		shards = append(shards, uint8(v))
	}
	return shards, nil
}

// registerSlashCommands: guild のコマンドを command.All で bulk overwrite する。
// 本体のセッションは 429 を無限に待ち直すので (作成上限の 429 だと登録が止まったままになる)、
// 再試行しない REST 専用セッションを使う。作成上限に達していたら、既にあるコマンドだけ更新する
func registerSlashCommands(primary *discordgo.Session, guild string) ([]*discordgo.ApplicationCommand, error) {
	rest := func() *discordgo.Session {
		s, _ := discordgo.New(primary.Token)
		s.ShouldRetryOnRateLimit = false
		return s
	}
	appID := primary.State.User.ID
	cmds, err := rest().ApplicationCommandBulkOverwrite(appID, guild, command.All)
	if err == nil {
		return cmds, nil
	}
	existing, err2 := rest().ApplicationCommands(appID, guild)
	if err2 != nil {
		return nil, fmt.Errorf("%v (and listing existing commands failed: %v)", err, err2)
	}
	have := map[string]bool{}
	for _, c := range existing {
		have[c.Name] = true
	}
	var subset []*discordgo.ApplicationCommand
	var missing []string
	for _, c := range command.All {
		if have[c.Name] {
			subset = append(subset, c)
		} else {
			missing = append(missing, c.Name)
		}
	}
	if len(subset) == 0 {
		return nil, fmt.Errorf("%v (no existing commands to update)", err)
	}
	cmds, err2 = rest().ApplicationCommandBulkOverwrite(appID, guild, subset)
	if err2 != nil {
		return nil, fmt.Errorf("%v (and updating existing commands failed: %v)", err, err2)
	}
	log.Printf("guild %s: could not create %v (%v); updated the %d existing commands only", guild, missing, err, len(subset))
	return cmds, nil
}

// slashCommandPriority: 作成上限で一部しか作れないときに先に作るもの
var slashCommandPriority = []string{"new", "end", "refresh", "link", "unlink", "pause", "help", "stats", "settings", "map", "info", "privacy"}

// ensureSlashCommands: guild に無いコマンドを 1 件ずつ作る。作成上限 (429) なら Discord の指示した時間
// (1〜30 分に丸める) 待ってからやり直す。全部そろったら終わる
func ensureSlashCommands(primary *discordgo.Session, guild string) {
	rest := func() *discordgo.Session {
		s, _ := discordgo.New(primary.Token)
		s.ShouldRetryOnRateLimit = false
		return s
	}
	appID := primary.State.User.ID
	rank := map[string]int{}
	for i, n := range slashCommandPriority {
		rank[n] = i + 1
	}
	order := func(name string) int {
		if r, ok := rank[name]; ok {
			return r
		}
		return len(slashCommandPriority) + 1
	}
	wait := func(err error) {
		d := 10 * time.Minute
		var rl *discordgo.RateLimitError
		if errors.As(err, &rl) && rl.RateLimit != nil && rl.TooManyRequests != nil {
			d = rl.TooManyRequests.RetryAfter + 5*time.Second
		}
		if d < time.Minute {
			d = time.Minute
		} else if d > 30*time.Minute {
			d = 30 * time.Minute
		}
		time.Sleep(d)
	}
	for attempt := 0; attempt < 500; attempt++ {
		existing, err := rest().ApplicationCommands(appID, guild)
		if err != nil {
			wait(err)
			continue
		}
		have := map[string]bool{}
		for _, c := range existing {
			have[c.Name] = true
		}
		var missing []*discordgo.ApplicationCommand
		for _, c := range command.All {
			if !have[c.Name] {
				missing = append(missing, c)
			}
		}
		if len(missing) == 0 {
			if attempt > 0 {
				log.Printf("guild %s: all slash commands are registered now", guild)
			}
			return
		}
		sort.SliceStable(missing, func(i, j int) bool { return order(missing[i].Name) < order(missing[j].Name) })
		c := missing[0]
		if _, err := rest().ApplicationCommandCreate(appID, guild, c); err != nil {
			log.Printf("guild %s: creating /%s failed (%d missing), will retry: %v", guild, c.Name, len(missing), err)
			wait(err)
			continue
		}
		log.Printf("guild %s: created /%s (%d more missing)", guild, c.Name, len(missing)-1)
	}
}
