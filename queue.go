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

// --- QUEUE CONFIG ---

const (
	maxAttempts = 5
	queueSize   = 1000
	numWorkers  = 3
)

var bucketName = []byte("jobs")

// --- JOB ---

type UploadJob struct {
	Key        string    `json:"key"` // = deviceAssetID, unique job key
	FileID     string    `json:"file_id"`
	CustomName string    `json:"custom_name"`
	AlbumName  string    `json:"album_name"`
	FileDate   time.Time `json:"file_date"`
	ChatID     int64     `json:"chat_id"`
	MessageID  int64     `json:"message_id"`
	Attempts   int       `json:"attempts"`
}

// --- ERRORS ---

// permanentError wraps errors that shouldn't be retried
type permanentError struct{ err error }

func (e permanentError) Error() string { return e.err.Error() }
func permanent(err error) error        { return permanentError{err} }

func isPermanent(err error) bool {
	_, ok := err.(permanentError)
	return ok
}

// --- QUEUE ---

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
		return nil, fmt.Errorf("failed to open queue DB: %w", err)
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

	// Restore unfinished jobs after restart
	q.restore()

	// Periodic queue size log
	go func() {
		ticker := time.NewTicker(1 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			if l := len(q.jobs); l > 0 {
				log.Printf("Queue: %d jobs pending", l)
			}
		}
	}()
}

func (q *UploadQueue) Close() {
	if q.db != nil {
		_ = q.db.Close()
	}
}

// Enqueue saves the job to DB and puts it in the channel
func (q *UploadQueue) Enqueue(job UploadJob) {
	if err := q.save(job); err != nil {
		log.Printf("WARNING: failed to save job %s to DB: %v", job.Key, err)
	}

	select {
	case q.jobs <- job:
	default:
		// Channel is full — put it blocking in a separate goroutine,
		// so as not to slow down the Telegram update handler
		log.Printf("WARNING: queue channel is full, job %s is waiting for space", job.Key)
		go func(j UploadJob) { q.jobs <- j }(job)
	}
}

// --- DB OPERATIONS ---

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
		log.Printf("WARNING: failed to delete job %s from DB: %v", key, err)
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
		log.Printf("WARNING: failed to restore jobs from DB: %v", err)
		return
	}

	if len(jobs) > 0 {
		log.Printf("Restored %d unfinished jobs from DB", len(jobs))
	}

	for _, j := range jobs {
		go func(job UploadJob) { q.jobs <- job }(j)
	}
}

// --- WORKERS ---

func (q *UploadQueue) worker(id int) {
	defer q.wg.Done()
	for job := range q.jobs {
		q.process(job)
	}
}

func (q *UploadQueue) process(job UploadJob) {
	err := q.processUpload(job)
	if err == nil {
		q.delete(job.Key) // success — remove from DB
		return
	}

	if isPermanent(err) {
		log.Printf("REJECTED (permanent): job %s: %v", job.Key, err)
		q.markFailed(job)
		q.delete(job.Key)
		return
	}

	job.Attempts++
	if job.Attempts >= maxAttempts {
		log.Printf("REJECTED: job %s failed after %d attempts: %v", job.Key, job.Attempts, err)
		q.markFailed(job)
		q.delete(job.Key)
		return
	}

	delay := backoffWithJitter(job.Attempts)
	log.Printf("Retrying job %s (attempt %d) in %s. Reason: %v",
		job.Key, job.Attempts, delay.Round(time.Second), err)

	// Update attempt counter in DB in case of a crash during the wait
	if err := q.save(job); err != nil {
		log.Printf("WARNING: failed to update job %s in DB: %v", job.Key, err)
	}

	// Retry in a separate goroutine so as not to block the worker
	go func(j UploadJob, d time.Duration) {
		time.Sleep(d)
		q.jobs <- j
	}(job, delay)
}

// backoffWithJitter: 5s, 15s, 45s, 135s... ±20% random jitter
func backoffWithJitter(attempt int) time.Duration {
	base := 5 * time.Second * time.Duration(pow(3, attempt-1))
	jitter := time.Duration(rand.Int63n(int64(base) / 5)) // up to 20%
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

// --- UPLOAD LOGIC ---

func (q *UploadQueue) processUpload(job UploadJob) error {
	log.Printf("Processing: %s (Album: %s, attempt %d)", job.FileID, job.AlbumName, job.Attempts+1)

	albumID, err := immichClient.GetOrCreateAlbum(job.AlbumName)
	if err != nil {
		log.Printf("ERROR with album: %v", err)
		return err // network/immich error — retry
	}

	tgFile, err := q.bot.GetFile(job.FileID, &gotgbot.GetFileOpts{
		RequestOpts: &gotgbot.RequestOpts{Timeout: 15 * time.Second},
	})
	if err != nil {
		log.Printf("ERROR GetFile from TG: %v", err)
		return err
	}

	dlURL := tgFile.URL(q.bot, &gotgbot.RequestOpts{Timeout: 60 * time.Second})

	tgClient := &http.Client{Timeout: 5 * time.Minute}

	resp, err := tgClient.Get(dlURL)
	if err != nil {
		log.Printf("ERROR downloading file from TG: %v", err)
		return err // network — retry
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// 4xx — the file is likely expired/unavailable, retry is useless
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			return permanent(fmt.Errorf("TG download error (permanent): %s", resp.Status))
		}
		return fmt.Errorf("TG download error: %s", resp.Status)
	}

	finalName := job.CustomName
	if finalName == "" {
		finalName = filepath.Base(tgFile.FilePath)
	}

	uploadResult, err := immichClient.UploadAsset(finalName, resp.Body, job.FileDate, job.Key)
	if err != nil {
		log.Printf("ERROR UploadAsset (file %s): %v", finalName, err)
		return err
	}

	if uploadResult.ID == "" {
		if uploadResult.Duplicate {
			log.Printf("File '%s' already exists (duplicate).", finalName)
			q.setReaction(job, "👀")
			return nil
		}
		return permanent(fmt.Errorf("file uploaded but no ID received"))
	}

	if err := immichClient.AddAssetToAlbum(albumID, uploadResult.ID); err != nil {
		// File is already uploaded. If we retry the whole thing — Immich will discard
		// the duplicate by deviceAssetID, but it still won't be in the album. So we retry
		// only the album addition.
		log.Printf("Uploaded but not added to album: %v", err)
		return fmt.Errorf("add to album error: %w", err)
	}

	q.setReaction(job, "👌")
	log.Printf("OK: %s -> Album %s", finalName, job.AlbumName)
	return nil
}
