package tokenprovider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"github.com/automuteus/automuteus/v8/pkg/premium"
	"github.com/automuteus/automuteus/v8/pkg/rediskey"
	"github.com/automuteus/automuteus/v8/pkg/task"
	"github.com/automuteus/automuteus/v8/pkg/token"
	"github.com/bsm/redislock"
	"github.com/bwmarrin/discordgo"
	"github.com/go-redis/redis/v8"
	"golang.org/x/exp/constraints"
	"log"
	"os"
	"sort"
	"strconv"
	"sync"
	"time"
)

var PremiumBotConstraints = map[premium.Tier]int{
	0: 0,
	1: 0,   // Free and Bronze have no premium bots
	2: 1,   // Silver has 1 bot
	3: 3,   // Gold has 3 bots
	4: 10,  // Platinum (TBD)
	5: 100, // Selfhost; 100 bots(!)
}

type TokenProvider struct {
	client         *redis.Client
	primarySession *discordgo.Session

	// maps hashed tokens to active discord sessions
	activeSessions      map[string]*discordgo.Session
	maxRequests5Seconds int64
	sessionLock         sync.RWMutex
	taskTimeoutMs       time.Duration
}

func NewTokenProvider(client *redis.Client, sess *discordgo.Session, taskTimeout time.Duration, maxReq int64) *TokenProvider {
	return &TokenProvider{
		client:              client,
		primarySession:      sess,
		activeSessions:      make(map[string]*discordgo.Session),
		maxRequests5Seconds: maxReq,
		sessionLock:         sync.RWMutex{},
		taskTimeoutMs:       taskTimeout,
	}
}

func (tp *TokenProvider) Init(client *redis.Client, sess *discordgo.Session) {
	tp.client = client
	tp.primarySession = sess
}

//func rateLimitEventCallback(sess *discordgo.Session, rl *discordgo.RateLimit) {
//	log.Println(rl.Message)
//}

func (tokenProvider *TokenProvider) PopulateAndStartSessions(tokens []string) {
	for _, v := range tokens {
		tokenProvider.openAndStartSessionWithToken(v)
	}
}

func (tokenProvider *TokenProvider) openAndStartSessionWithToken(botToken string) bool {
	k := hashToken(botToken)
	tokenProvider.sessionLock.Lock()
	defer tokenProvider.sessionLock.Unlock()

	if _, ok := tokenProvider.activeSessions[k]; !ok {
		token.WaitForToken(tokenProvider.client, botToken)
		token.LockForToken(tokenProvider.client, botToken)
		sess, err := discordgo.New("Bot " + botToken)
		if err != nil {
			log.Println(err)
			return false
		}
		sess.Identify.Intents = discordgo.MakeIntent(discordgo.IntentsGuilds)
		err = sess.Open()
		if err != nil {
			log.Println(err)
			return false
		}
		// associates the guilds with this token to be used for requests
		sess.AddHandler(tokenProvider.newGuild)
		log.Println("Opened session on startup for " + k)
		tokenProvider.activeSessions[k] = sess
		return true
	}
	return false
}

func (tokenProvider *TokenProvider) getSession(guildID string, hTokenSubset map[string]struct{}) (*discordgo.Session, string) {
	tokenProvider.sessionLock.RLock()
	defer tokenProvider.sessionLock.RUnlock()

	for hToken, sess := range tokenProvider.activeSessions {
		// if we have already used this token successfully, or haven't set any restrictions
		if hTokenSubset == nil || mapHasEntry(hTokenSubset, hToken) {
			// if this token isn't potentially rate-limited
			if tokenProvider.IncrAndTestGuildTokenComboLock(guildID, hToken) {
				return sess, hToken
			} else {
				log.Println("Secondary token is potentially rate-limited. Skipping")
			}
		}
	}

	return nil, ""
}

func mapHasEntry[T constraints.Ordered, K any](dict map[T]K, key T) bool {
	if dict == nil {
		return false
	}
	_, ok := dict[key]
	return ok
}

func (tokenProvider *TokenProvider) IncrAndTestGuildTokenComboLock(guildID, hashToken string) bool {
	i, err := tokenProvider.client.Incr(context.Background(), rediskey.GuildTokenLock(guildID, hashToken)).Result()
	if err != nil {
		log.Println(err)
	}
	usable := i < tokenProvider.maxRequests5Seconds
	log.Printf("Token/capture %s on guild %s is at count %d. Using?: %v", hashToken, guildID, i, usable)
	if !usable {
		return false
	}

	// set the expiry only if the mute/deafen was successful, because we want to preserve any existing blacklist expiries
	err = tokenProvider.client.Expire(context.Background(), rediskey.GuildTokenLock(guildID, hashToken), time.Second*5).Err()
	if err != nil {
		log.Println(err)
	}

	return true
}

// BlacklistTokenForDuration sets a guild token (or connect code ala capture bot) to the maximum value allowed before
// attempting other non-rate-limited mute/deafen methods.
// NOTE: this will manifest as the capture/token in question appearing like it "has been used <maxnum> times" in logs,
// even if this is not technically accurate. A more accurate approach would probably use a totally separate Redis key,
// as opposed to this approach, which simply uses the ratelimiting counter key(s) to achieve blacklisting
func (tokenProvider *TokenProvider) BlacklistTokenForDuration(guildID, hashToken string, duration time.Duration) error {
	return tokenProvider.client.Set(context.Background(), rediskey.GuildTokenLock(guildID, hashToken), tokenProvider.maxRequests5Seconds, duration).Err()
}

const DefaultMaxWorkers = 8

var UnresponsiveCaptureBlacklistDuration = time.Minute * time.Duration(5)

func (tokenProvider *TokenProvider) ModifyUsers(guildID, connectCode string, request task.UserModifyRequest, voicelock *redislock.Lock) error {
	if voicelock != nil {
		defer voicelock.Release(context.Background())
	}

	gid, gerr := strconv.ParseUint(guildID, 10, 64)
	if gerr != nil {
		return gerr
	}
	limit := PremiumBotConstraints[request.Premium]
	if limit > 0 && os.Getenv("MUTE_LANES") != "0" {
		return tokenProvider.modifyUsersByLane(guildID, request, limit)
	}

	tasksChannel := make(chan task.UserModify, len(request.Users))
	wg := sync.WaitGroup{}

	mdsc := task.MuteDeafenSuccessCounts{
		Worker:    0,
		Capture:   0,
		Official:  0,
		RateLimit: 0,
	}
	uniqueTokensUsed := make(map[string]struct{})
	lock := sync.Mutex{}
	tokenLock := sync.RWMutex{}

	var latestErr error
	// start a handful of workers to handle the tasks
	for i := 0; i < DefaultMaxWorkers; i++ {
		go func() {
			for req := range tasksChannel {
				userIDStr := strconv.FormatUint(req.UserID, 10)
				hToken := ""
				if limit > 0 {
					tokenLock.RLock()
					if len(uniqueTokensUsed) >= limit {
						hToken = tokenProvider.attemptOnSecondaryTokens(guildID, userIDStr, uniqueTokensUsed, req)
						tokenLock.RUnlock()
					} else {
						tokenLock.RUnlock()
						hToken = tokenProvider.attemptOnSecondaryTokens(guildID, userIDStr, nil, req)
					}
				}
				if hToken != "" {
					lock.Lock()
					mdsc.Worker++
					lock.Unlock()

					tokenLock.Lock()
					uniqueTokensUsed[hToken] = struct{}{}
					tokenLock.Unlock()
				} else {
					success := tokenProvider.attemptOnCaptureBot(guildID, connectCode, gid, req)
					if success {
						lock.Lock()
						mdsc.Capture++
						lock.Unlock()
					} else {
						log.Printf("Applying mute=%v, deaf=%v using primary bot\n", req.Mute, req.Deaf)
						err := task.ApplyMuteDeaf(tokenProvider.primarySession, guildID, userIDStr, req.Mute, req.Deaf)
						if err != nil {
							lock.Lock()
							latestErr = err
							lock.Unlock()
							log.Println("Error on primary bot:")
							log.Println(err)
						} else {
							lock.Lock()
							mdsc.Official++
							lock.Unlock()
						}
					}
				}
				wg.Done()
			}
		}()
	}

	for _, modifyReq := range request.Users {
		wg.Add(1)
		tasksChannel <- modifyReq
	}
	wg.Wait()
	close(tasksChannel)

	RecordDiscordRequestsByCounts(tokenProvider.client, mdsc)

	// note, this should probably be more systematic on startup, not when a mute/deafen task comes in. But this is a
	// context in which we already have the guildID, successful tokens, AND the premium limit...
	go tokenProvider.verifyBotMembership(guildID, limit, uniqueTokensUsed)

	return latestErr
}

func (tokenProvider *TokenProvider) rateLimitEventCallback(sess *discordgo.Session, rl *discordgo.RateLimit) {
	log.Println(rl.Message)
}

func (tokenProvider *TokenProvider) waitForAck(pubsub *redis.PubSub, result chan<- bool) {
	t := time.NewTimer(tokenProvider.taskTimeoutMs)
	defer pubsub.Close()
	channel := pubsub.Channel()

	for {
		select {
		case <-t.C:
			t.Stop()
			result <- false
			return
		case val := <-channel:
			t.Stop()
			result <- val.Payload == "true"
			return
		}
	}
}

func hashToken(token string) string {
	h := sha256.New()
	h.Write([]byte(token))
	return hex.EncodeToString(h.Sum(nil))
}

func (tokenProvider *TokenProvider) Close() {
	tokenProvider.sessionLock.Lock()
	for _, v := range tokenProvider.activeSessions {
		v.Close()
	}

	tokenProvider.activeSessions = map[string]*discordgo.Session{}
	tokenProvider.sessionLock.Unlock()
	tokenProvider.primarySession.Close()
}

func (tokenProvider *TokenProvider) newGuild(s *discordgo.Session, m *discordgo.GuildCreate) {
	log.Println("added to " + m.ID)
}

// fork: ミュートの送り先を「レーン」(作業用 bot ごと + 本体 bot) に均等に振り分ける (MUTE_LANES=0 で上流の動き)。
// discordgo はセッションごとにメンバー PATCH を 1 件ずつしか送らない (リクエスト中は bucket をロックする) ので、
// 上流の「1 つ目の bot を上限まで使ってから次」だと実際は 1〜2 本しか並ばない。
// 並び順 (優先する人が先頭) はレーンの中でも保たれる。作業用 bot が使えないときはその分を本体 bot で送る
type muteLane struct {
	hToken string // 空なら本体 bot
	sess   *discordgo.Session
}

func (tokenProvider *TokenProvider) muteLanes(limit int) []muteLane {
	tokenProvider.sessionLock.RLock()
	defer tokenProvider.sessionLock.RUnlock()
	keys := make([]string, 0, len(tokenProvider.activeSessions))
	for k := range tokenProvider.activeSessions {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) > limit {
		keys = keys[:limit]
	}
	lanes := make([]muteLane, 0, len(keys)+1)
	for _, k := range keys {
		lanes = append(lanes, muteLane{hToken: k, sess: tokenProvider.activeSessions[k]})
	}
	if tokenProvider.primarySession != nil {
		lanes = append(lanes, muteLane{sess: tokenProvider.primarySession})
	}
	return lanes
}

func (tokenProvider *TokenProvider) modifyUsersByLane(guildID string, request task.UserModifyRequest, limit int) error {
	lanes := tokenProvider.muteLanes(limit)
	if len(lanes) == 0 {
		return errors.New("no discord session to apply mutes")
	}
	perLane := make([][]task.UserModify, len(lanes))
	for i, u := range request.Users {
		perLane[i%len(lanes)] = append(perLane[i%len(lanes)], u)
	}
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		latestErr error
	)
	mdsc := task.MuteDeafenSuccessCounts{}
	used := make(map[string]struct{})
	for i, l := range lanes {
		if len(perLane[i]) == 0 {
			continue
		}
		wg.Add(1)
		go func(l muteLane, users []task.UserModify) {
			defer wg.Done()
			for _, req := range users {
				uid := strconv.FormatUint(req.UserID, 10)
				if l.hToken != "" && tokenProvider.IncrAndTestGuildTokenComboLock(guildID, l.hToken) {
					err := task.ApplyMuteDeaf(l.sess, guildID, uid, req.Mute, req.Deaf)
					if err == nil {
						log.Printf("Successfully applied mute=%v, deaf=%v to User %d using secondary bot: %s\n", req.Mute, req.Deaf, req.UserID, l.hToken)
						mu.Lock()
						mdsc.Worker++
						used[l.hToken] = struct{}{}
						mu.Unlock()
						continue
					}
					log.Println("Failed to apply mute to player with error:", err)
					// この bot が使えない (権限なし・トークン無効など 401/403) ときだけ 5 分使わない。
					// 相手が VC を抜けた等の 400 で bot ごと止めると、その間ほかの人も本体 bot に寄る
					var re *discordgo.RESTError
					if errors.As(err, &re) && re.Response != nil && (re.Response.StatusCode == 401 || re.Response.StatusCode == 403) {
						if err := tokenProvider.BlacklistTokenForDuration(guildID, l.hToken, UnresponsiveCaptureBlacklistDuration); err != nil {
							log.Println(err)
						}
					}
				}
				// 本体 bot のレーン、または作業用 bot が使えなかった (上限・未参加・エラー) とき
				err := task.ApplyMuteDeaf(tokenProvider.primarySession, guildID, uid, req.Mute, req.Deaf)
				mu.Lock()
				if err != nil {
					latestErr = err
					log.Println("Error on primary bot:", err)
				} else {
					mdsc.Official++
					log.Printf("Successfully applied mute=%v, deaf=%v to User %d using primary bot\n", req.Mute, req.Deaf, req.UserID)
				}
				mu.Unlock()
			}
		}(l, perLane[i])
	}
	wg.Wait()

	RecordDiscordRequestsByCounts(tokenProvider.client, mdsc)
	go tokenProvider.verifyBotMembership(guildID, limit, used)
	return latestErr
}
