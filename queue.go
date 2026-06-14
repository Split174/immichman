// queue.go
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"path/filepath"
	"sync"
	"time"

	"github.com/PaulSonOfLars/gotgbot/v2"
	bolt "go.etcd.io/bbolt"
)

// --- КОНФИГ ОЧЕРЕДИ ---

const (
	maxAttempts = 5
	queueSize   = 1000
	numWorkers  = 3
)

var bucketName = []byte("jobs")

// --- ЗАДАЧА ---

type UploadJob struct {
	Key        string    `json:"key"` // = deviceAssetID, уникальный ключ задачи
	FileID     string    `json:"file_id"`
	CustomName string    `json:"custom_name"`
	AlbumName  string    `json:"album_name"`
	FileDate   time.Time `json:"file_date"`
	ChatID     int64     `json:"chat_id"`
	MessageID  int64     `json:"message_id"`
	Attempts   int       `json:"attempts"`
}

// --- ОШИБКИ ---

// permanentError оборачивает ошибки, которые не нужно ретраить
type permanentError struct{ err error }

func (e permanentError) Error() string { return e.err.Error() }
func permanent(err error) error        { return permanentError{err} }

func isPermanent(err error) bool {
	_, ok := err.(permanentError)
	return ok
}

// --- ОЧЕРЕДЬ ---

type UploadQueue struct {
	jobs chan UploadJob
	bot  *gotgbot.Bot
	db   *bolt.DB
	wg   sync.WaitGroup
}

var uploadQueue *UploadQueue

func NewUploadQueue(b *gotgbot.Bot, dbPath string) (*UploadQueue, error) {
	db, err := bolt.Open(dbPath, 0600, &bolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("не удалось открыть БД очереди: %w", err)
	}
	err = db.Update(func(tx *bolt.Tx) error {
		_, e := tx.CreateBucketIfNotExists(bucketName)
		return e
	})
	if err != nil {
		return nil, err
	}

	return &UploadQueue{
		jobs: make(chan UploadJob, queueSize),
		bot:  b,
		db:   db,
	}, nil
}

func (q *UploadQueue) Start() {
	for i := 0; i < numWorkers; i++ {
		q.wg.Add(1)
		go q.worker(i)
	}

	// Восстановление незавершённых задач после перезапуска
	q.restore()

	// Периодический лог размера очереди
	go func() {
		ticker := time.NewTicker(1 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			if l := len(q.jobs); l > 0 {
				log.Printf("Очередь: %d задач в ожидании", l)
			}
		}
	}()
}

func (q *UploadQueue) Close() {
	if q.db != nil {
		_ = q.db.Close()
	}
}

// Enqueue сохраняет задачу в БД и кладёт в канал
func (q *UploadQueue) Enqueue(job UploadJob) {
	if err := q.save(job); err != nil {
		log.Printf("ВНИМАНИЕ: не удалось сохранить задачу %s в БД: %v", job.Key, err)
	}

	select {
	case q.jobs <- job:
	default:
		// Канал переполнен — кладём блокирующе в отдельной горутине,
		// чтобы не тормозить обработчик апдейтов Telegram
		log.Printf("ВНИМАНИЕ: канал очереди переполнен, задача %s ждёт места", job.Key)
		go func(j UploadJob) { q.jobs <- j }(job)
	}
}

// --- РАБОТА С БД ---

func (q *UploadQueue) save(job UploadJob) error {
	data, err := json.Marshal(job)
	if err != nil {
		return err
	}
	return q.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketName).Put([]byte(job.Key), data)
	})
}

func (q *UploadQueue) delete(key string) {
	err := q.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketName).Delete([]byte(key))
	})
	if err != nil {
		log.Printf("ВНИМАНИЕ: не удалось удалить задачу %s из БД: %v", key, err)
	}
}

func (q *UploadQueue) restore() {
	var jobs []UploadJob
	err := q.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketName).ForEach(func(k, v []byte) error {
			var j UploadJob
			if err := json.Unmarshal(v, &j); err == nil {
				jobs = append(jobs, j)
			}
			return nil
		})
	})
	if err != nil {
		log.Printf("ВНИМАНИЕ: не удалось восстановить задачи из БД: %v", err)
		return
	}

	if len(jobs) > 0 {
		log.Printf("Восстановлено %d незавершённых задач из БД", len(jobs))
	}

	for _, j := range jobs {
		go func(job UploadJob) { q.jobs <- job }(j)
	}
}

// --- ВОРКЕРЫ ---

func (q *UploadQueue) worker(id int) {
	defer q.wg.Done()
	for job := range q.jobs {
		q.process(job)
	}
}

func (q *UploadQueue) process(job UploadJob) {
	err := q.processUpload(job)
	if err == nil {
		q.delete(job.Key) // успех — убираем из БД
		return
	}

	if isPermanent(err) {
		log.Printf("ОТКАЗ (permanent): задача %s: %v", job.Key, err)
		q.markFailed(job)
		q.delete(job.Key)
		return
	}

	job.Attempts++
	if job.Attempts >= maxAttempts {
		log.Printf("ОТКАЗ: задача %s провалена после %d попыток: %v", job.Key, job.Attempts, err)
		q.markFailed(job)
		q.delete(job.Key)
		return
	}

	delay := backoffWithJitter(job.Attempts)
	log.Printf("Повтор задачи %s (попытка %d) через %s. Причина: %v",
		job.Key, job.Attempts, delay.Round(time.Second), err)

	// Обновляем счётчик попыток в БД на случай краша во время ожидания
	if err := q.save(job); err != nil {
		log.Printf("ВНИМАНИЕ: не удалось обновить задачу %s в БД: %v", job.Key, err)
	}

	// Ретрай в отдельной горутине, чтобы не блокировать воркер
	go func(j UploadJob, d time.Duration) {
		time.Sleep(d)
		q.jobs <- j
	}(job, delay)
}

// backoffWithJitter: 5s, 15s, 45s, 135s... ±20% случайного джиттера
func backoffWithJitter(attempt int) time.Duration {
	base := 5 * time.Second * time.Duration(pow(3, attempt-1))
	jitter := time.Duration(rand.Int63n(int64(base) / 5)) // до 20%
	if rand.Intn(2) == 0 {
		return base - jitter
	}
	return base + jitter
}

func pow(base, exp int) int {
	r := 1
	for i := 0; i < exp; i++ {
		r *= base
	}
	return r
}

func (q *UploadQueue) markFailed(job UploadJob) {
	q.setReaction(job, "😢")
}

func (q *UploadQueue) setReaction(job UploadJob, emoji string) {
	_, _ = q.bot.SetMessageReaction(job.ChatID, job.MessageID, &gotgbot.SetMessageReactionOpts{
		Reaction:    []gotgbot.ReactionType{gotgbot.ReactionTypeEmoji{Emoji: emoji}},
		RequestOpts: &gotgbot.RequestOpts{Timeout: 10 * time.Second},
	})
}

// --- ЛОГИКА ЗАГРУЗКИ ---

func (q *UploadQueue) processUpload(job UploadJob) error {
	log.Printf("Обработка: %s (Альбом: %s, попытка %d)", job.FileID, job.AlbumName, job.Attempts+1)

	albumID, err := immichClient.GetOrCreateAlbum(job.AlbumName)
	if err != nil {
		log.Printf("ОШИБКА с альбомом: %v", err)
		return err // сетевая/immich ошибка — ретраим
	}

	tgFile, err := q.bot.GetFile(job.FileID, &gotgbot.GetFileOpts{
		RequestOpts: &gotgbot.RequestOpts{Timeout: 15 * time.Second},
	})
	if err != nil {
		log.Printf("ОШИБКА GetFile у TG: %v", err)
		return err
	}

	dlURL := tgFile.URL(q.bot, &gotgbot.RequestOpts{Timeout: 60 * time.Second})

	tgClient := &http.Client{Timeout: 5 * time.Minute}

	resp, err := tgClient.Get(dlURL)
	if err != nil {
		log.Printf("ОШИБКА загрузки файла из TG: %v", err)
		return err // сеть — ретраим
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// 4xx — скорее всего файл устарел/недоступен, ретрай бесполезен
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			return permanent(fmt.Errorf("ошибка скачивания TG (permanent): %s", resp.Status))
		}
		return fmt.Errorf("ошибка скачивания TG: %s", resp.Status)
	}

	finalName := job.CustomName
	if finalName == "" {
		finalName = filepath.Base(tgFile.FilePath)
	}

	uploadResult, err := immichClient.UploadAsset(finalName, resp.Body, job.FileDate, job.Key)
	if err != nil {
		log.Printf("ОШИБКА UploadAsset (файл %s): %v", finalName, err)
		return err
	}

	if uploadResult.ID == "" {
		if uploadResult.Duplicate {
			log.Printf("Файл '%s' уже существует (дубликат).", finalName)
			q.setReaction(job, "👀")
			return nil
		}
		return permanent(fmt.Errorf("файл загружен, но ID не получен"))
	}

	if err := immichClient.AddAssetToAlbum(albumID, uploadResult.ID); err != nil {
		// Файл уже залит. Если ретраить целиком — Immich отбросит дубликат
		// по deviceAssetID, но в альбом так и не попадёт. Поэтому ретраим
		// только добавление в альбом.
		log.Printf("Загружен, но не добавлен в альбом: %v", err)
		return fmt.Errorf("ошибка добавления в альбом: %w", err)
	}

	q.setReaction(job, "👌")
	log.Printf("ОК: %s -> Альбом %s", finalName, job.AlbumName)
	return nil
}
