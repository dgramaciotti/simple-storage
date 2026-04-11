package main

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func TestQuotaAndMemoryFixes(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "storage-fix-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(tempDir) }()

	apiKey := "secret"
	if err := os.Setenv("STORAGE_API_KEY", apiKey); err != nil {
		t.Fatalf("failed to set env: %v", err)
	}
	defer func() {
		if err := os.Unsetenv("STORAGE_API_KEY"); err != nil {
			t.Logf("failed to unset env: %v", err)
		}
	}()

	// macOS trick: /var is a symlink to /private/var
	tempDir, _ = filepath.EvalSymlinks(tempDir)

	storage := &Storage{
		BaseDir:            tempDir,
		MaxBucketSizeBytes: 1000,
		inFlightBytes:      0,
	}
	storage.calculateInitialSizes()

	server := httptest.NewServer(storage)
	defer server.Close()

	t.Run("Quota desync on overwrite", func(t *testing.T) {
		bucket := "quota-fix"
		fileUrl := server.URL + "/" + bucket + "/file.txt"

		// 1. Initial upload (10 bytes)
		req, _ := http.NewRequest(http.MethodPut, fileUrl, bytes.NewReader([]byte("1234567890")))
		req.Header.Set("Authorization", "Bearer "+apiKey)
		resp, _ := http.DefaultClient.Do(req)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("initial upload failed: %d", resp.StatusCode)
		}

		sizePtr := storage.getBucketSizePtr(bucket)
		if atomic.LoadInt64(sizePtr) != 10 {
			t.Errorf("expected size 10, got %d", atomic.LoadInt64(sizePtr))
		}

		// 2. Overwrite with 20 bytes.
		// New total should be exactly 20, NOT 30 (which would happen with a leak).
		req, _ = http.NewRequest(http.MethodPut, fileUrl, bytes.NewReader([]byte("12345678901234567890")))
		req.Header.Set("Authorization", "Bearer "+apiKey)
		resp, _ = http.DefaultClient.Do(req)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("overwrite failed: %d", resp.StatusCode)
		}

		if atomic.LoadInt64(sizePtr) != 20 {
			t.Errorf("expected size 20 after overwrite, got %d (QUOTA LEAK DETECTED)", atomic.LoadInt64(sizePtr))
		}
	})

	t.Run("Lock map memory leak fix", func(t *testing.T) {
		bucket := "lock-fix"

		// Create many unique files
		for i := 0; i < 50; i++ {
			filename := fmt.Sprintf("file-%d.txt", i)
			fileUrl := server.URL + "/" + bucket + "/" + filename
			req, _ := http.NewRequest(http.MethodPut, fileUrl, bytes.NewReader([]byte("data")))
			req.Header.Set("Authorization", "Bearer "+apiKey)
			resp, _ := http.DefaultClient.Do(req)
			if resp.StatusCode != http.StatusCreated {
				t.Fatalf("upload %d failed", i)
			}
		}

		// Count entries in locks sync.Map
		count := 0
		storage.locks.Range(func(key, value interface{}) bool {
			count++
			return true
		})

		// Since we release and delete when refCount drops to 0,
		// the map should be empty (or nearly empty if something is still processing, but here it's serial).
		if count > 0 {
			t.Errorf("expected 0 locks in map after uploads, got %d (MEMORY LEAK DETECTED)", count)
		}
	})

	t.Run("In-flight protection", func(t *testing.T) {
		bucket := "inflight-test"
		// Quota is 100.
		// We simulate an in-flight upload of 100 bytes by spoofing Content-Length.

		body := make([]byte, 1001)
		req, _ := http.NewRequest(http.MethodPut, server.URL+"/"+bucket+"/huge.txt", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+apiKey)

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer func() {
			_ = resp.Body.Close()
		}()
		if resp.StatusCode != http.StatusInsufficientStorage {
			t.Errorf("expected 507 Insufficient Storage due to in-flight spoofing, got %d", resp.StatusCode)
		}
	})
}
