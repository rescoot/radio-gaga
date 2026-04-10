package commands

import (
	"archive/tar"
	"compress/gzip"
	"context"
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

// HandleFetchLogsCommand collects a log bundle from this scooter (MDB plus
// DBC if reachable) and uploads the resulting tarball to sunshine. The bundle
// layout is defined in the logcollect package.
func HandleFetchLogsCommand(client CommandHandlerClient, redisClient *redis.Client, ctx context.Context, config *models.Config, params map[string]interface{}, requestID string) error {
	since := "24h"
	if s, ok := params["since"].(string); ok && s != "" {
		since = s
	}

	bundleName := fmt.Sprintf("scooter-logs-%s", requestID)
	outputDir := filepath.Join("/tmp", bundleName)
	archivePath := outputDir + ".tar.gz"
	defer os.RemoveAll(outputDir)
	defer os.Remove(archivePath)

	log.Printf("fetch_logs: collecting bundle %s (since=%s)", bundleName, since)

	collectCtx, cancel := context.WithTimeout(ctx, 6*time.Minute)
	defer cancel()

	bundleMeta, err := logcollect.Collect(collectCtx, redisClient, outputDir, logcollect.Options{
		Since:     since,
		RequestID: requestID,
	})
	if err != nil {
		return fmt.Errorf("collecting logs: %w", err)
	}
	log.Printf("fetch_logs: collected hosts=%v", bundleMeta.Hosts)

	if err := createTarball(outputDir, archivePath, bundleName); err != nil {
		return fmt.Errorf("packing archive: %w", err)
	}

	timeout := 60 * time.Second
	if config.API.Timeout != "" {
		if d, err := time.ParseDuration(config.API.Timeout); err == nil {
			timeout = d
		}
	}

	uploader := sync.NewLogUploader(config.API.BaseURL, config.API.ScooterID, config.Scooter.Token, timeout)
	uploadCtx, uploadCancel := context.WithTimeout(context.Background(), timeout)
	defer uploadCancel()

	if err := uploader.Upload(uploadCtx, archivePath, requestID); err != nil {
		log.Printf("fetch_logs: upload failed, retrying: %v", err)
		time.Sleep(2 * time.Second)
		if err := uploader.Upload(uploadCtx, archivePath, requestID); err != nil {
			return fmt.Errorf("log upload failed after retry: %w", err)
		}
	}
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
