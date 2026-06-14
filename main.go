// main.go
package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/PaulSonOfLars/gotgbot/v2"
	"github.com/PaulSonOfLars/gotgbot/v2/ext"
	"github.com/PaulSonOfLars/gotgbot/v2/ext/handlers"
	"github.com/PaulSonOfLars/gotgbot/v2/ext/handlers/filters/message"
)

// --- GLOBAL VARIABLES ---
var (
	immichClient *ImmichClient
	// Map for quick lookup of admin IDs: map[UserID]exists
	adminIDs = make(map[int64]bool)
)

// --- AUTHORIZED CHATS CACHE ---
// To avoid checking admin presence in chat for every photo
type AuthCacheStruct struct {
	sync.RWMutex
	// map[ChatID]ExpiryTime
	authorizedChats map[int64]time.Time
}

var authCache = AuthCacheStruct{
	authorizedChats: make(map[int64]time.Time),
}

// Check returns true if the chat is valid and the check hasn't expired
func (ac *AuthCacheStruct) Check(chatID int64) bool {
	ac.RLock()
	defer ac.RUnlock()
	expiry, exists := ac.authorizedChats[chatID]
	if !exists {
		return false
	}
	return time.Now().Before(expiry)
}

// Add adds a chat to the "whitelist" for 1 hour
func (ac *AuthCacheStruct) Add(chatID int64) {
	ac.Lock()
	defer ac.Unlock()
	ac.authorizedChats[chatID] = time.Now().Add(1 * time.Hour)
}

// --- TELEGRAM ALBUMS CACHE ---

type GroupCacheStruct struct {
	sync.RWMutex
	data map[string]string
}

var groupCache = GroupCacheStruct{
	data: make(map[string]string),
}

func (c *GroupCacheStruct) Set(groupID, folder string) {
	c.Lock()
	defer c.Unlock()
	c.data[groupID] = folder

	go func(id string) {
		time.Sleep(2 * time.Minute)
		c.Lock()
		delete(c.data, id)
		c.Unlock()
	}(groupID)
}

func (c *GroupCacheStruct) Get(groupID string) (string, bool) {
	c.RLock()
	defer c.RUnlock()
	val, ok := c.data[groupID]
	return val, ok
}

// --- MAIN ---

func main() {
	// 1. Load configuration
	token := os.Getenv("TELEGRAM_TOKEN")
	if token == "" {
		log.Fatal("TELEGRAM_TOKEN is not set")
	}
	immichURL := os.Getenv("IMMICH_URL")
	if immichURL == "" {
		log.Fatal("IMMICH_URL is not set")
	}
	immichAPIKey := os.Getenv("IMMICH_API_KEY")
	if immichAPIKey == "" {
		log.Fatal("IMMICH_API_KEY is not set")
	}

	// 2. Parse admins
	adminsEnv := os.Getenv("TELEGRAM_ADMINS")
	if adminsEnv == "" {
		log.Fatal("TELEGRAM_ADMINS is not set (provide IDs separated by commas)")
	}
	loadAdmins(adminsEnv)

	// 3. Initialize Immich client
	immichClient = NewImmichClient(strings.TrimRight(immichURL, "/"), immichAPIKey)

	if err := immichClient.Ping(); err != nil {
		log.Fatalf("FATAL: Failed to connect to Immich API. Check URL and Key.\nDetails: %v", err)
	}

	b, err := gotgbot.NewBot(token, nil)
	if err != nil {
		log.Fatal(err)
	}

	// Initialize upload queue with persistence
	uploadQueue, err = NewUploadQueue(b, "queue.db")
	if err != nil {
		log.Fatalf("Failed to initialize queue: %v", err)
	}
	defer uploadQueue.Close()
	uploadQueue.Start()

	dispatcher := ext.NewDispatcher(&ext.DispatcherOpts{
		Error: func(b *gotgbot.Bot, ctx *ext.Context, err error) ext.DispatcherAction {
			log.Println("Handler error:", err)
			return ext.DispatcherActionNoop
		},
		MaxRoutines: 20,
	})
	updater := ext.NewUpdater(dispatcher, nil)

	// Handlers
	dispatcher.AddHandler(handlers.NewMessage(message.Photo, handleMedia))
	dispatcher.AddHandler(handlers.NewMessage(message.Video, handleMedia))
	dispatcher.AddHandler(handlers.NewMessage(message.Document, handleMedia))

	err = updater.StartPolling(b, &ext.PollingOpts{
		DropPendingUpdates: false,
		GetUpdatesOpts: &gotgbot.GetUpdatesOpts{
			Timeout: 60,
			RequestOpts: &gotgbot.RequestOpts{
				Timeout: time.Second * 90,
			},
		},
	})
	if err != nil {
		log.Fatal("Startup error: " + err.Error())
	}

	log.Printf("Bot %s started in restricted access mode. AdminIDs: %d", b.User.Username, len(adminIDs))
	updater.Idle()
}

func loadAdmins(env string) {
	parts := strings.Split(env, ",")
	for _, p := range parts {
		trimmed := strings.TrimSpace(p)
		if trimmed == "" {
			continue
		}
		id, err := strconv.ParseInt(trimmed, 10, 64)
		if err != nil {
			log.Printf("WARN: Invalid admin ID: %s", p)
			continue
		}
		adminIDs[id] = true
	}
	if len(adminIDs) == 0 {
		log.Fatal("No valid IDs found in TELEGRAM_ADMINS")
	}
}

// handleMedia parses the message and places the file in the upload queue
func handleMedia(b *gotgbot.Bot, ctx *ext.Context) error {
	msg := ctx.EffectiveMessage
	chat := ctx.EffectiveChat
	user := ctx.EffectiveSender.User

	if !checkPermission(b, chat, user) {
		return nil
	}

	var fileID, fileName string
	fileDate := time.Unix(msg.Date, 0)

	if len(msg.Photo) > 0 {
		fileID = msg.Photo[len(msg.Photo)-1].FileId
	} else if msg.Video != nil {
		fileID = msg.Video.FileId
		fileName = msg.Video.FileName
	} else if msg.Document != nil {
		mime := msg.Document.MimeType
		if !strings.HasPrefix(mime, "image/") && !strings.HasPrefix(mime, "video/") {
			return nil
		}
		fileID = msg.Document.FileId
		fileName = msg.Document.FileName
	} else {
		return nil
	}

	// Determine album name here (before queueing), since the media group cache
	// lives only 2 minutes and will expire while the job waits for a retry.
	albumName := resolveTargetAlbumName(ctx, msg.MediaGroupId, msg.Caption)

	uploadQueue.Enqueue(UploadJob{
		Key:        fmt.Sprintf("tg-%d-%d", chat.Id, msg.MessageId),
		FileID:     fileID,
		CustomName: fileName,
		AlbumName:  albumName,
		FileDate:   fileDate,
		ChatID:     chat.Id,
		MessageID:  msg.MessageId,
	})

	return nil
}

// checkPermission decides whether to process files from this chat
func checkPermission(b *gotgbot.Bot, chat *gotgbot.Chat, user *gotgbot.User) bool {
	if chat.Type == "private" {
		return adminIDs[user.Id]
	}

	if adminIDs[user.Id] {
		authCache.Add(chat.Id)
		return true
	}

	if authCache.Check(chat.Id) {
		return true
	}

	for adminID := range adminIDs {
		member, err := b.GetChatMember(chat.Id, adminID, &gotgbot.GetChatMemberOpts{
			RequestOpts: &gotgbot.RequestOpts{Timeout: 10 * time.Second},
		})
		if err != nil {
			continue
		}

		status := member.GetStatus()
		if status == "creator" || status == "administrator" || status == "member" {
			log.Printf("Chat '%s' (%d) authorized by admin %d presence", chat.Title, chat.Id, adminID)
			authCache.Add(chat.Id)
			return true
		}
	}

	return false
}

// resolveTargetAlbumName determines the album name for Immich
func resolveTargetAlbumName(ctx *ext.Context, groupID, caption string) string {
	const trigger = "!папка"

	getDefaultChatAlbum := func() string {
		chat := ctx.EffectiveChat
		rawName := chat.Title
		if rawName == "" {
			rawName = strings.TrimSpace(chat.FirstName + " " + chat.LastName)
		}
		if rawName == "" {
			rawName = chat.Username
		}
		if rawName == "" {
			rawName = fmt.Sprintf("Chat_%d", chat.Id)
		}
		return rawName
	}

	folderName := parseFolderFromCaption(caption, trigger)
	if folderName != "" {
		if groupID != "" {
			groupCache.Set(groupID, folderName)
		}
		return folderName
	}

	if groupID != "" {
		if cachedAlbum, found := groupCache.Get(groupID); found {
			return cachedAlbum
		}
		for i := 0; i < 5; i++ {
			time.Sleep(200 * time.Millisecond)
			if cachedAlbum, found := groupCache.Get(groupID); found {
				return cachedAlbum
			}
		}
	}

	return getDefaultChatAlbum()
}

func parseFolderFromCaption(caption, trigger string) string {
	if caption == "" || !strings.Contains(caption, trigger) {
		return ""
	}
	parts := strings.SplitN(caption, trigger, 2)
	if len(parts) < 2 {
		return ""
	}
	raw := parts[1]
	if idx := strings.Index(raw, "\n"); idx != -1 {
		raw = raw[:idx]
	}
	return strings.TrimSpace(raw)
}

// uploadToImmich performs the entire process: from downloading from Telegram to uploading to Immich
func uploadToImmich(b *gotgbot.Bot, ctx *ext.Context, fileID, customName, albumName string, fileDate time.Time) error {
	log.Printf("Processing: %s (Album: %s)", fileID, albumName)

	albumID, err := immichClient.GetOrCreateAlbum(albumName)
	if err != nil {
		log.Printf("ERROR with album: %v", err)
		return err
	}

	// CHANGED: Added timeout for fetching file info
	tgFile, err := b.GetFile(fileID, &gotgbot.GetFileOpts{
		RequestOpts: &gotgbot.RequestOpts{Timeout: 15 * time.Second},
	})
	if err != nil {
		log.Printf("ERROR GetFile from TG: %v", err)
		return err
	}

	dlURL := tgFile.URL(b, &gotgbot.RequestOpts{Timeout: 60 * time.Second})

	// CHANGED: Using a client with a hard timeout instead of the hanging http.Get
	// 5 minutes to download a file from TG - should be enough even on a bad network for videos.
	tgClient := &http.Client{
		Timeout: 5 * time.Minute,
	}

	resp, err := tgClient.Get(dlURL)
	if err != nil {
		log.Printf("ERROR downloading file from TG: %v", err)
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("TG download error: %s", resp.Status)
	}

	var finalName string
	if customName != "" {
		finalName = customName
	} else {
		finalName = filepath.Base(tgFile.FilePath)
	}

	deviceAssetID := fmt.Sprintf("tg-%d-%d", ctx.EffectiveChat.Id, ctx.EffectiveMessage.MessageId)

	uploadResult, err := immichClient.UploadAsset(finalName, resp.Body, fileDate, deviceAssetID)
	if err != nil {
		log.Printf("ERROR UploadAsset (file %s): %v", finalName, err)
		return err
	}

	if uploadResult.ID == "" {
		if uploadResult.Duplicate {
			log.Printf("File '%s' already exists (duplicate).", finalName)
			_, _ = b.SetMessageReaction(ctx.EffectiveChat.Id, ctx.EffectiveMessage.MessageId, &gotgbot.SetMessageReactionOpts{
				Reaction:    []gotgbot.ReactionType{gotgbot.ReactionTypeEmoji{Emoji: "👀"}},
				RequestOpts: &gotgbot.RequestOpts{Timeout: 10 * time.Second}, // Added timeout
			})
			return nil
		}
		return fmt.Errorf("file uploaded but no ID received")
	}

	err = immichClient.AddAssetToAlbum(albumID, uploadResult.ID)
	if err != nil {
		log.Printf("Uploaded but not added to album: %v", err)
	}

	_, _ = b.SetMessageReaction(ctx.EffectiveChat.Id, ctx.EffectiveMessage.MessageId, &gotgbot.SetMessageReactionOpts{
		Reaction: []gotgbot.ReactionType{
			gotgbot.ReactionTypeEmoji{Emoji: "👌"},
		},
		RequestOpts: &gotgbot.RequestOpts{Timeout: 10 * time.Second},
	})

	log.Printf("OK: %s -> Album %s", finalName, albumName)
	return nil
}
