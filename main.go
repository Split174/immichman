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

// --- ГЛОБАЛЬНЫЕ ПЕРЕМЕННЫЕ ---
var (
	immichClient *ImmichClient
	// Map для быстрого поиска ID админов: map[UserID]exists
	adminIDs = make(map[int64]bool)
)

// --- КЭШ АВТОРИЗОВАННЫХ ЧАТОВ ---
// Чтобы не проверять присутствие админа в чате при каждом фото
type AuthCacheStruct struct {
	sync.RWMutex
	// map[ChatID]ExpiryTime
	authorizedChats map[int64]time.Time
}

var authCache = AuthCacheStruct{
	authorizedChats: make(map[int64]time.Time),
}

// Check возвращает true, если чат валиден и срок проверки не истек
func (ac *AuthCacheStruct) Check(chatID int64) bool {
	ac.RLock()
	defer ac.RUnlock()
	expiry, exists := ac.authorizedChats[chatID]
	if !exists {
		return false
	}
	return time.Now().Before(expiry)
}

// Add добавляет чат в "белый список" на 1 час
func (ac *AuthCacheStruct) Add(chatID int64) {
	ac.Lock()
	defer ac.Unlock()
	ac.authorizedChats[chatID] = time.Now().Add(1 * time.Hour)
}

// --- КЭШ ДЛЯ АЛЬБОМОВ TELEGRAM ---

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
	// 1. Загрузка конфигурации
	token := os.Getenv("TELEGRAM_TOKEN")
	if token == "" {
		log.Fatal("TELEGRAM_TOKEN не установлен")
	}
	immichURL := os.Getenv("IMMICH_URL")
	if immichURL == "" {
		log.Fatal("IMMICH_URL не установлен")
	}
	immichAPIKey := os.Getenv("IMMICH_API_KEY")
	if immichAPIKey == "" {
		log.Fatal("IMMICH_API_KEY не установлен")
	}

	// 2. Парсинг админов
	adminsEnv := os.Getenv("TELEGRAM_ADMINS")
	if adminsEnv == "" {
		log.Fatal("TELEGRAM_ADMINS не установлен (укажите ID через запятую)")
	}
	loadAdmins(adminsEnv)

	// 3. Инициализация клиента Immich
	immichClient = NewImmichClient(strings.TrimRight(immichURL, "/"), immichAPIKey)

	if err := immichClient.Ping(); err != nil {
		log.Fatalf("FATAL: Не удалось подключиться к Immich API. Проверьте URL и Key.\nПодробности: %v", err)
	}

	b, err := gotgbot.NewBot(token, nil)
	if err != nil {
		log.Fatal(err)
	}

	dispatcher := ext.NewDispatcher(&ext.DispatcherOpts{
		Error: func(b *gotgbot.Bot, ctx *ext.Context, err error) ext.DispatcherAction {
			log.Println("Ошибка обработчика:", err)
			return ext.DispatcherActionNoop
		},
		MaxRoutines: 20,
	})
	updater := ext.NewUpdater(dispatcher, nil)

	// Хендлеры
	dispatcher.AddHandler(handlers.NewMessage(message.Photo, handleMedia))
	dispatcher.AddHandler(handlers.NewMessage(message.Video, handleMedia))
	dispatcher.AddHandler(handlers.NewMessage(message.Document, handleMedia))

	err = updater.StartPolling(b, &ext.PollingOpts{
		DropPendingUpdates: false,
		GetUpdatesOpts: &gotgbot.GetUpdatesOpts{
			Timeout: 9,
			RequestOpts: &gotgbot.RequestOpts{
				Timeout: time.Second * 10,
			},
		},
	})
	if err != nil {
		log.Fatal("Ошибка запуска: " + err.Error())
	}

	log.Printf("Бот %s запущен в режиме ограниченного доступа. AdminIDs: %d", b.User.Username, len(adminIDs))
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
			log.Printf("WARN: Некорректный ID админа: %s", p)
			continue
		}
		adminIDs[id] = true
	}
	if len(adminIDs) == 0 {
		log.Fatal("Не найдено ни одного корректного ID в TELEGRAM_ADMINS")
	}
}

// handleMedia разбирает сообщение и запускает выгрузку в Immich
func handleMedia(b *gotgbot.Bot, ctx *ext.Context) error {
	msg := ctx.EffectiveMessage
	chat := ctx.EffectiveChat
	user := ctx.EffectiveSender.User

	// --- 1. ПРОВЕРКА ПРАВ ДОСТУПА ---
	if !checkPermission(b, chat, user) {
		// Для дебага можно раскомментировать, но в продакшене будет спамить
		// log.Printf("Игнор: чат %s (%d), юзер %s (%d)", chat.Title, chat.Id, user.FirstName, user.Id)
		return nil
	}
	// --------------------------------

	var fileID, fileName string
	var fileDate = time.Unix(msg.Date, 0)

	if len(msg.Photo) > 0 {
		best := msg.Photo[len(msg.Photo)-1]
		fileID = best.FileId
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

	// Определяем имя альбома, в который будем загружать
	albumName := resolveTargetAlbumName(ctx, msg.MediaGroupId, msg.Caption)

	return uploadToImmich(b, ctx, fileID, fileName, albumName, fileDate)
}

// checkPermission решает, обрабатывать ли файлы из этого чата
func checkPermission(b *gotgbot.Bot, chat *gotgbot.Chat, user *gotgbot.User) bool {
	// 1. Если это личка — проверяем, админ ли пишет
	if chat.Type == "private" {
		return adminIDs[user.Id]
	}

	// 2. Если пишет сам админ в любой группе — разрешаем и запоминаем группу
	// (Это экономит API вызовы)
	if adminIDs[user.Id] {
		authCache.Add(chat.Id)
		return true
	}

	// 3. Проверяем кэш авторизованных групп
	if authCache.Check(chat.Id) {
		return true
	}

	// 4. Тяжелая проверка: перебираем всех админов из конфига и спрашиваем Telegram,
	// состоят ли они в этом чате.
	for adminID := range adminIDs {
		member, err := b.GetChatMember(chat.Id, adminID, nil)
		if err != nil {
			// Ошибка запроса (например, бот кикнут или нет прав видеть админов)
			continue
		}

		status := member.GetStatus()
		// Статусы: creator, administrator, member - считаем, что админ "присутствует"
		if status == "creator" || status == "administrator" || status == "member" {
			log.Printf("Чат '%s' (%d) авторизован по присутствию админа %d", chat.Title, chat.Id, adminID)
			authCache.Add(chat.Id)
			return true
		}
	}

	return false
}

// resolveTargetAlbumName определяет имя альбома для Immich
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

// uploadToImmich выполняет весь процесс: от скачивания из Telegram до загрузки в Immich
func uploadToImmich(b *gotgbot.Bot, ctx *ext.Context, fileID, customName, albumName string, fileDate time.Time) error {
	log.Printf("Обработка: %s (Альбом: %s)", fileID, albumName)

	albumID, err := immichClient.GetOrCreateAlbum(albumName)
	if err != nil {
		log.Printf("ОШИБКА с альбомом: %v", err)
		return err
	}

	tgFile, err := b.GetFile(fileID, nil)
	if err != nil {
		return err
	}

	dlURL := tgFile.URL(b, &gotgbot.RequestOpts{Timeout: 60 * time.Second})
	resp, err := http.Get(dlURL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ошибка скачивания TG: %s", resp.Status)
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
		log.Printf("ОШИБКА UploadAsset: %v", err)
		return err
	}

	if uploadResult.ID == "" {
		if uploadResult.Duplicate {
			log.Printf("Файл '%s' уже существует (дубликат).", finalName)
			_, _ = b.SetMessageReaction(ctx.EffectiveChat.Id, ctx.EffectiveMessage.MessageId, &gotgbot.SetMessageReactionOpts{
				Reaction: []gotgbot.ReactionType{gotgbot.ReactionTypeEmoji{Emoji: "👀"}},
			})
			return nil
		}
		return fmt.Errorf("файл загружен, но ID не получен")
	}

	err = immichClient.AddAssetToAlbum(albumID, uploadResult.ID)
	if err != nil {
		log.Printf("Загружен, но не добавлен в альбом: %v", err)
	}

	_, _ = b.SetMessageReaction(ctx.EffectiveChat.Id, ctx.EffectiveMessage.MessageId, &gotgbot.SetMessageReactionOpts{
		Reaction: []gotgbot.ReactionType{
			gotgbot.ReactionTypeEmoji{Emoji: "👌"},
		},
	})

	log.Printf("ОК: %s -> Альбом %s", finalName, albumName)
	return nil
}
