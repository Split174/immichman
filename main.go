// main.go
package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/PaulSonOfLars/gotgbot/v2"
	"github.com/PaulSonOfLars/gotgbot/v2/ext"
	"github.com/PaulSonOfLars/gotgbot/v2/ext/handlers"
	"github.com/PaulSonOfLars/gotgbot/v2/ext/handlers/filters/message"
)

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
var immichClient *ImmichClient

func main() {
	// Загрузка конфигурации
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

	// Инициализация клиента Immich
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
		MaxRoutines: 20, // Можно увеличить количество горутин для параллельной обработки
	})
	updater := ext.NewUpdater(dispatcher, nil)

	// Хендлеры
	dispatcher.AddHandler(handlers.NewMessage(message.Photo, handleMedia))
	dispatcher.AddHandler(handlers.NewMessage(message.Video, handleMedia))
	dispatcher.AddHandler(handlers.NewMessage(message.Document, handleMedia))

	err = updater.StartPolling(b, &ext.PollingOpts{
		DropPendingUpdates: true,
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

	log.Printf("Бот %s запущен. Выгрузка в Immich. Логика: папка чата -> !папка. ОК.\n", b.User.Username)
	updater.Idle()
}

// handleMedia разбирает сообщение и запускает выгрузку в Immich
func handleMedia(b *gotgbot.Bot, ctx *ext.Context) error {
	msg := ctx.EffectiveMessage
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

// resolveTargetAlbumName определяет имя альбома для Immich
func resolveTargetAlbumName(ctx *ext.Context, groupID, caption string) string {
	const trigger = "!папка"

	// Вспомогательная функция для получения имени альбома по умолчанию (Название чата)
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
		// Immich сам обработает любые символы, нет нужды в sanitize
		return rawName
	}

	// 1. Сценарий: Явное указание папки (альбома) в текущем сообщении
	folderName := parseFolderFromCaption(caption, trigger)
	if folderName != "" {
		if groupID != "" {
			groupCache.Set(groupID, folderName)
		}
		return folderName
	}

	// 2. Сценарий: Это альбом Telegram, ищем в кеше
	if groupID != "" {
		if cachedAlbum, found := groupCache.Get(groupID); found {
			return cachedAlbum
		}
		// Ждем, если сообщения пришли асинхронно
		for i := 0; i < 5; i++ {
			time.Sleep(200 * time.Millisecond)
			if cachedAlbum, found := groupCache.Get(groupID); found {
				return cachedAlbum
			}
		}
	}

	// 3. Сценарий: Используем имя чата
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

	// 1. ID альбома
	albumID, err := immichClient.GetOrCreateAlbum(albumName)
	if err != nil {
		log.Printf("ОШИБКА с альбомом: %v", err)
		return err // Прерываем, если не можем найти альбом
	}

	// 2. Инфо о файле Telegram
	tgFile, err := b.GetFile(fileID, nil)
	if err != nil {
		return err
	}

	// 3. Скачивание стримом
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

	// Уникальный ID для дедупликации Immich
	deviceAssetID := fmt.Sprintf("tg-%d-%d", ctx.EffectiveChat.Id, ctx.EffectiveMessage.MessageId)

	// 4. ЗАГРУЗКА ФАЙЛА В IMMICH
	// Обратите внимание: мы убрали albumID из этого вызова
	uploadResult, err := immichClient.UploadAsset(finalName, resp.Body, fileDate, deviceAssetID)
	if err != nil {
		log.Printf("ОШИБКА UploadAsset: %v", err)
		return err
	}

	// Если это дубликат и ID не вернулся, мы не сможем добавить его в альбом.
	// Обычно Immich возвращает ID даже для дубликатов, если включена соответствующая опция,
	// но если ID пустой - выходим.
	if uploadResult.ID == "" {
		if uploadResult.Duplicate {
			log.Printf("Файл '%s' уже существует (дубликат). ID не получен, пропускаем добавление в альбом.", finalName)
			// Ставим реакцию "глаза", типа "вижу, но уже было"
			_, _ = b.SetMessageReaction(ctx.EffectiveChat.Id, ctx.EffectiveMessage.MessageId, &gotgbot.SetMessageReactionOpts{
				Reaction: []gotgbot.ReactionType{gotgbot.ReactionTypeEmoji{Emoji: "👀"}},
			})
			return nil
		}
		return fmt.Errorf("файл загружен, но ID не получен")
	}

	// 5. ЯВНОЕ ДОБАВЛЕНИЕ В АЛЬБОМ
	err = immichClient.AddAssetToAlbum(albumID, uploadResult.ID)
	if err != nil {
		log.Printf("Загружен, но не добавлен в альбом: %v", err)
		// Не "фэйлим" всю функцию, так как файл все-таки сохранился
	}

	_, _ = b.SetMessageReaction(ctx.EffectiveChat.Id, ctx.EffectiveMessage.MessageId, &gotgbot.SetMessageReactionOpts{
		Reaction: []gotgbot.ReactionType{
			gotgbot.ReactionTypeEmoji{Emoji: "👌"},
		},
	})

	log.Printf("ОК: %s -> Альбом %s", finalName, albumName)
	return nil
}
