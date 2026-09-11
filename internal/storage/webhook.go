package storage

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"
)

// WebhookPayload represents the event payload sent to the configured webhook URL.
type WebhookPayload struct {
	EventName string    `json:"eventName"`
	Bucket    string    `json:"bucket"`
	Key       string    `json:"key"`
	Size      int64     `json:"size,omitempty"`
	ETag      string    `json:"etag,omitempty"`
	VersionID string    `json:"versionId,omitempty"`
	Time      time.Time `json:"time"`
}

var webhookClient = &http.Client{
	Timeout: 5 * time.Second,
}

type webhookTask struct {
	url     string
	payload WebhookPayload
}

const webhookQueueSize = 5000
const webhookWorkerCount = 10

func (fs *FilesystemEngine) initWebhookDispatcher() {
	fs.webhookQueue = make(chan webhookTask, webhookQueueSize)
	for i := 0; i < webhookWorkerCount; i++ {
		fs.webhookWG.Add(1)
		go func() {
			defer fs.webhookWG.Done()
			for task := range fs.webhookQueue {
				fs.sendWebhookEvent(task.url, task.payload)
			}
		}()
	}
}

// StopWebhookDispatcher halts webhook processing, flushes remaining notifications, and waits for workers to terminate.
func (fs *FilesystemEngine) StopWebhookDispatcher() {
	fs.webhookMu.Lock()
	if atomic.SwapInt32(&fs.isWebhookShuttingDown, 1) == 1 {
		fs.webhookMu.Unlock()
		return
	}

	q := fs.webhookQueue
	fs.webhookMu.Unlock()

	if q != nil {
		close(q)
		fs.webhookWG.Wait()
	}
}

// triggerWebhook parses the configuration and sends the event asynchronously.
func (fs *FilesystemEngine) triggerWebhook(eventName string, info *ObjectInfo) {
	if fs.webhookURL == "" {
		return
	}

	fs.webhookMu.Lock()
	defer fs.webhookMu.Unlock()

	if atomic.LoadInt32(&fs.isWebhookShuttingDown) == 1 {
		slog.Warn("[Webhook] Webhook dispatcher shutting down, ignoring event", "event", eventName, "bucket", info.Bucket, "key", info.Key)
		return
	}

	fs.webhookOnce.Do(func() {
		fs.initWebhookDispatcher()
	})

	payload := WebhookPayload{
		EventName: "s3:" + eventName,
		Bucket:    info.Bucket,
		Key:       info.Key,
		Size:      info.Size,
		ETag:      info.ETag,
		VersionID: info.VersionID,
		Time:      time.Now().UTC(),
	}

	task := webhookTask{
		url:     fs.webhookURL,
		payload: payload,
	}

	select {
	case fs.webhookQueue <- task:
	default:
		GlobalMetrics.IncWebhookDropped()
		slog.Warn("[Webhook] Webhook queue full, dropping event", "event", eventName, "bucket", info.Bucket, "key", info.Key)
	}
}

const (
	webhookMaxRetries  = 3
	webhookBaseBackoff = time.Second
)

func (fs *FilesystemEngine) sendWebhookEvent(url string, payload WebhookPayload) {
	data, err := json.Marshal(payload)
	if err != nil {
		slog.Error("[Webhook] Failed to marshal payload", "error", err)
		return
	}

	// Sleeping between attempts on the worker goroutine stalled the whole pool:
	// one dead endpoint blocked a worker for the full backoff on every event,
	// and the queue silently overflowed. Retries now run on their own timer,
	// each tracked by fs.webhookWG so StopWebhookDispatcher's Wait() actually
	// blocks until the retry chain finishes instead of reporting a clean
	// shutdown while a scheduled retry (and its outbound HTTP call) is still
	// pending.
	var attempt func(n int)
	attempt = func(n int) {
		if fs.deliverWebhook(url, data) {
			return
		}
		if atomic.LoadInt32(&fs.isWebhookShuttingDown) == 1 {
			slog.Warn("[Webhook] Dispatcher shutting down, abandoning retry", "event", payload.EventName)
			return
		}
		if n >= webhookMaxRetries {
			slog.Error("[Webhook] Failed to deliver event after retries",
				"event", payload.EventName, "retries", webhookMaxRetries)
			return
		}
		backoff := webhookBaseBackoff << n
		fs.webhookWG.Add(1)
		time.AfterFunc(backoff, func() {
			defer fs.webhookWG.Done()
			attempt(n + 1)
		})
	}
	attempt(0)
}

// deliverWebhook performs a single delivery attempt, reporting success.
func (fs *FilesystemEngine) deliverWebhook(url string, data []byte) bool {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		slog.Error("[Webhook] Failed to create request", "error", err)
		return true // Unrecoverable: do not retry a malformed URL.
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Stiva-Webhook-Dispatcher")

	if fs.webhookSecret != "" {
		mac := hmac.New(sha256.New, []byte(fs.webhookSecret))
		_, _ = mac.Write(data)
		req.Header.Set("X-Stiva-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}

	resp, err := webhookClient.Do(req)
	if err != nil {
		slog.Warn("[Webhook] Dispatch failed", "error", err)
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return true
	}
	slog.Warn("[Webhook] Server returned error status", "status", resp.StatusCode)
	return false
}
