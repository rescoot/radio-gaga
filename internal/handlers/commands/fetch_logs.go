package commands

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-redis/redis/v8"

	"radio-gaga/internal/logcollect"
	"radio-gaga/internal/models"
	"radio-gaga/internal/sync"
)

// Upload tuning for fetch_logs. Sized for degraded cellular links: EDGE tops
// out around 150 kbit/s (~18 KB/s), so a 30 MB bundle needs ~27 min to push.
// The retry schedule gives ~3 min of backoff across 5 attempts before giving
// up, which covers transient signal drops without hammering a failing server.
var (
	uploadAttemptTimeout = 30 * time.Minute
	uploadBackoff        = []time.Duration{5 * time.Second, 15 * time.Second, 45 * time.Second, 2 * time.Minute}
)

// HandleFetchLogsCommand collects a log bundle from this scooter (MDB plus
// DBC if reachable) and uploads the resulting tarball to sunshine. The bundle
// layout is defined in the logcollect package.
//
// Collection and upload run in a background goroutine so the MQTT ack returns
// immediately — fetch_logs can take several minutes, and blocking the command
// handler would stall every other command behind it and exceed the server's
// ack TTL. Sunshine observes success via the arriving upload, not the ack.
func HandleFetchLogsCommand(client CommandHandlerClient, redisClient *redis.Client, ctx context.Context, config *models.Config, params map[string]any, requestID string) error {
	since := "24h"
	if s, ok := params["since"].(string); ok && s != "" {
		since = s
	}

	bundleName := fmt.Sprintf("scooter-logs-%s", requestID)
	outputDir := filepath.Join("/tmp", bundleName)
	archivePath := outputDir + ".tar.gz"

	log.Printf("fetch_logs: accepted %s (since=%s), running in background", bundleName, since)

	go func() {
		defer os.RemoveAll(outputDir)
		defer os.Remove(archivePath)

		collectCtx, cancel := context.WithTimeout(ctx, 6*time.Minute)
		defer cancel()

		bundleMeta, err := logcollect.Collect(collectCtx, redisClient, outputDir, logcollect.Options{
			Since:     since,
			RequestID: requestID,
		})
		if err != nil {
			log.Printf("fetch_logs: collecting logs failed: %v", err)
			return
		}
		log.Printf("fetch_logs: collected hosts=%v", bundleMeta.Hosts)

		if err := createTarball(outputDir, archivePath, bundleName); err != nil {
			log.Printf("fetch_logs: packing archive failed: %v", err)
			return
		}

		uploader := sync.NewLogUploader(config.API.BaseURL, config.Scooter.Token, uploadAttemptTimeout)

		var uploadErr error
		for attempt := 1; attempt <= len(uploadBackoff)+1; attempt++ {
			uploadCtx, uploadCancel := context.WithTimeout(ctx, uploadAttemptTimeout)
			uploadErr = uploader.Upload(uploadCtx, archivePath, requestID)
			uploadCancel()
			if uploadErr == nil {
				break
			}
			var rejected *sync.UploadRejectedError
			if errors.As(uploadErr, &rejected) {
				log.Printf("fetch_logs: upload rejected, not retrying: %v", uploadErr)
				return
			}
			if attempt > len(uploadBackoff) {
				break
			}
			backoff := uploadBackoff[attempt-1]
			log.Printf("fetch_logs: upload attempt %d failed, retrying in %s: %v", attempt, backoff, uploadErr)
			select {
			case <-ctx.Done():
				log.Printf("fetch_logs: upload aborted: %v", ctx.Err())
				return
			case <-time.After(backoff):
			}
		}
		if uploadErr != nil {
			log.Printf("fetch_logs: upload failed after %d attempts: %v", len(uploadBackoff)+1, uploadErr)
			return
		}
		log.Printf("fetch_logs: upload complete for %s", requestID)
	}()

	return nil
}

// createTarball writes a gzip-compressed tar of sourceDir to tarPath. The
// archive entries are prefixed with rootName so the extracted tree has a
// single predictable top-level directory, which is what sunshine's find_root
// relies on.
func createTarball(sourceDir, tarPath, rootName string) error {
	f, err := os.Create(tarPath)
	if err != nil {
		return err
	}
	defer f.Close()

	gz := gzip.NewWriter(f)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()

	return filepath.Walk(sourceDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(sourceDir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}

		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = strings.TrimPrefix(filepath.ToSlash(filepath.Join(rootName, rel)), "./")
		if err := tw.WriteHeader(header); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}

		src, err := os.Open(path)
		if err != nil {
			return err
		}
		defer src.Close()
		_, err = io.Copy(tw, src)
		return err
	})
}
