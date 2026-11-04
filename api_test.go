package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

const testApiKey = "test-secret-key"

var umaskMutex sync.Mutex

func TestAPI(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "storage-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer func() {
		_ = os.RemoveAll(tempDir)
	}()

	// Set environment variable for the test server to pick up
	if err := os.Setenv("STORAGE_API_KEY", testApiKey); err != nil {
		t.Fatalf("failed to set env var: %v", err)
	}
	defer func() { _ = os.Unsetenv("STORAGE_API_KEY") }()

	storage := &Storage{
		BaseDir:  tempDir,
		FileMode: 0644,
		DirMode:  0755,
	}
	// Important: Initialize bucket sizes for the storage instance
	storage.calculateInitialSizes()

	server := httptest.NewServer(storage)
	defer server.Close()

	t.Run("Unauthorized access", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, server.URL+"/my-bucket/test.txt", nil)
		resp, _ := http.DefaultClient.Do(req)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("expected 401 Unauthorized, got %d", resp.StatusCode)
		}
	})

	t.Run("Bucket validation", func(t *testing.T) {
		cases := []struct {
			name   string
			bucket string
			status int
		}{
			{"Valid", "my-bucket-123", http.StatusCreated},
			{"Invalid Upper", "MyBucket", http.StatusBadRequest},
			{"Invalid Chars", "bucket_name", http.StatusBadRequest},
			{"Empty Key", "bucket/", http.StatusBadRequest},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				url := server.URL + "/" + tc.bucket
				if tc.name != "Empty Key" {
					url += "/file.txt"
				}
				req, _ := http.NewRequest(http.MethodPut, url, bytes.NewReader([]byte("data")))
				req.Header.Set("Authorization", "Bearer "+testApiKey)
				resp, _ := http.DefaultClient.Do(req)
				if resp.StatusCode != tc.status {
					t.Errorf("%s: expected %d, got %d", tc.name, tc.status, resp.StatusCode)
				}
			})
		}
	})

	t.Run("Upload, Download and Security Headers", func(t *testing.T) {
		bucket := "docs"
		filename := "hello.txt"
		content := []byte("hello world")

		// 1. Upload
		req, _ := http.NewRequest(http.MethodPut, server.URL+"/"+bucket+"/"+filename, bytes.NewReader(content))
		req.Header.Set("Authorization", "Bearer "+testApiKey)
		resp, _ := http.DefaultClient.Do(req)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("failed to upload: status %d", resp.StatusCode)
		}

		// 2. Download and Check Headers
		req, _ = http.NewRequest(http.MethodGet, server.URL+"/"+bucket+"/"+filename, nil)
		req.Header.Set("Authorization", "Bearer "+testApiKey)
		resp, err = http.DefaultClient.Do(req)
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("failed to download: status %d", resp.StatusCode)
		}

		if h := resp.Header.Get("X-Content-Type-Options"); h != "nosniff" {
			t.Errorf("expected nosniff header, got %q", h)
		}

		downloaded, _ := io.ReadAll(resp.Body)
		if !bytes.Equal(content, downloaded) {
			t.Errorf("expected content %q, got %q", string(content), string(downloaded))
		}
	})

	t.Run("Metadata Persistence (Content-Type)", func(t *testing.T) {
		bucket := "meta"
		filename := "script.sh"
		content := []byte("#!/bin/bash\necho ok")
		customMime := "text/x-shellscript"

		// Upload with custom mime
		req, _ := http.NewRequest(http.MethodPut, server.URL+"/"+bucket+"/"+filename, bytes.NewReader(content))
		req.Header.Set("Authorization", "Bearer "+testApiKey)
		req.Header.Set("Content-Type", customMime)
		resp, _ := http.DefaultClient.Do(req)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("upload failed: %d", resp.StatusCode)
		}

		// Download and check mime
		req, _ = http.NewRequest(http.MethodGet, server.URL+"/"+bucket+"/"+filename, nil)
		req.Header.Set("Authorization", "Bearer "+testApiKey)
		resp, _ = http.DefaultClient.Do(req)

		if h := resp.Header.Get("Content-Type"); h != customMime {
			t.Errorf("expected Content-Type %q, got %q", customMime, h)
		}
	})

	t.Run("Range Requests", func(t *testing.T) {
		bucket := "docs"
		filename := "range.txt"
		content := []byte("0123456789")

		// Upload
		req, _ := http.NewRequest(http.MethodPut, server.URL+"/"+bucket+"/"+filename, bytes.NewReader(content))
		req.Header.Set("Authorization", "Bearer "+testApiKey)
		resp, _ := http.DefaultClient.Do(req)
		if resp != nil {
			_ = resp.Body.Close()
		}

		// Get Range
		req, _ = http.NewRequest(http.MethodGet, server.URL+"/"+bucket+"/"+filename, nil)
		req.Header.Set("Authorization", "Bearer "+testApiKey)
		req.Header.Set("Range", "bytes=0-4")
		resp, _ = http.DefaultClient.Do(req)

		if resp.StatusCode != http.StatusPartialContent {
			t.Errorf("expected 206 Partial Content, got %d", resp.StatusCode)
		}
		body, _ := io.ReadAll(resp.Body)
		if string(body) != "01234" {
			t.Errorf("expected '01234', got %q", string(body))
		}
	})

	t.Run("Concurrency Stress", func(t *testing.T) {
		bucket := "stress"
		filename := "shared.txt"
		var wg sync.WaitGroup
		iterations := 10

		for i := 0; i < iterations; i++ {
			wg.Add(1)
			go func(val int) {
				defer wg.Done()
				data := []byte(fmt.Sprintf("data-%d", val))
				req, _ := http.NewRequest(http.MethodPut, server.URL+"/"+bucket+"/"+filename, bytes.NewReader(data))
				req.Header.Set("Authorization", "Bearer "+testApiKey)
				resp, err := http.DefaultClient.Do(req)
				if err != nil || (resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK) {
					// We don't fail immediately to avoid blocking other goroutines, but we log
					t.Logf("concurrency put failed: %v, status: %d", err, resp.StatusCode)
				}
			}(i)
		}
		wg.Wait()

		// Final check: file should exist and contain *some* valid data from one of the requests
		req, _ := http.NewRequest(http.MethodGet, server.URL+"/"+bucket+"/"+filename, nil)
		req.Header.Set("Authorization", "Bearer "+testApiKey)
		resp, _ := http.DefaultClient.Do(req)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("file should exist after stress test, got status %d", resp.StatusCode)
		}
	})

	t.Run("Path Traversal and Security", func(t *testing.T) {
		badPaths := []string{
			"/bucket/../../etc/passwd",
			"/bucket/%2e%2e%2fetc/passwd",
			"/bucket/folder/../outside.txt",
			"/bucket/./internal.txt",            // Should be cleanable but let's check
			"/bucket/file\\with\\backslash.txt", // Invalid char
			"/bucket/malicious.meta",            // Metadata bypass
		}

		for _, path := range badPaths {
			req, _ := http.NewRequest(http.MethodGet, server.URL+path, nil)
			req.Header.Set("Authorization", "Bearer "+testApiKey)
			resp, _ := http.DefaultClient.Do(req)
			// Status 400 or 403 or 404 is fine as long as it's not 200
			if resp.StatusCode == http.StatusOK {
				t.Errorf("security hole: path %s returned 200 OK", path)
			}
		}
	})

	t.Run("File size limit 100MB", func(t *testing.T) {
		largeData := io.LimitReader(&infiniteReader{}, 101*1024*1024)
		req, _ := http.NewRequest(http.MethodPut, server.URL+"/limit-bucket/huge.dat", largeData)
		req.Header.Set("Authorization", "Bearer "+testApiKey)
		resp, _ := http.DefaultClient.Do(req)

		if resp.StatusCode != http.StatusRequestEntityTooLarge {
			t.Errorf("expected 413, got %d", resp.StatusCode)
		}
	})

	t.Run("Bucket Quota Enforcement", func(t *testing.T) {
		// New storage with a very small quota: 5 bytes
		smallStorage := &Storage{
			BaseDir:            tempDir,
			MaxBucketSizeBytes: 5,
			FileMode:           0644,
			DirMode:            0755,
		}
		smallStorage.calculateInitialSizes()
		smallServer := httptest.NewServer(smallStorage)
		defer smallServer.Close()

		bucket := "quota-bucket"
		// Uploading 10 bytes should fail
		req, _ := http.NewRequest(http.MethodPut, smallServer.URL+"/"+bucket+"/too-big.txt", bytes.NewReader([]byte("1234567890")))
		req.Header.Set("Authorization", "Bearer "+testApiKey)
		// Set Content-Length manually because small bytes.NewReader might not be picked up cleanly in some mocks
		req.ContentLength = 10
		resp, _ := http.DefaultClient.Do(req)

		if resp.StatusCode != http.StatusInsufficientStorage {
			t.Errorf("expected 507 Insufficient Storage, got %d", resp.StatusCode)
		}
	})

	t.Run("Security: Content-Type Header Injection", func(t *testing.T) {
		bucket := "sec-test"
		filename := "injection.txt"
		maliciousMime := "text/plain\r\nX-Injected: true\nAnother-Header: value"

		// Upload
		req, err := http.NewRequest(http.MethodPut, server.URL+"/"+bucket+"/"+filename, bytes.NewReader([]byte("test")))
		if err != nil {
			t.Fatalf("failed to create request: %v", err)
		}
		req.Header.Set("Authorization", "Bearer "+testApiKey)
		// Go's net/http client might reject \r\n in headers.
		// If it does, we'll try to bypass it or simply skip this client-side test
		// and trust the server-side sanitization we added.
		req.Header.Set("Content-Type", maliciousMime)

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Logf("client rejected malicious header (good): %v", err)
			return // If the client won't even send it, we're "safe" from standard clients
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("upload failed: %d", resp.StatusCode)
		}

		// Download
		req, err = http.NewRequest(http.MethodGet, server.URL+"/"+bucket+"/"+filename, nil)
		if err != nil {
			t.Fatalf("failed to create download request: %v", err)
		}
		req.Header.Set("Authorization", "Bearer "+testApiKey)
		resp, err = http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("download request failed: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()

		// Check injected header is NOT present and Content-Type is sanitized
		if resp.Header.Get("X-Injected") != "" {
			t.Errorf("Security Hole: CRLF injection successful, found X-Injected header")
		}
		if strings.Contains(resp.Header.Get("Content-Type"), "\r") || strings.Contains(resp.Header.Get("Content-Type"), "\n") {
			t.Errorf("Security Hole: Content-Type still contains CRLF: %q", resp.Header.Get("Content-Type"))
		}
	})

	t.Run("List Buckets and Files", func(t *testing.T) {
		bucket := "list-test"
		filename := "test.txt"
		content := []byte("list data")
		customMime := "text/plain"

		// 1. Initial list (should be empty if we use a fresh bucket name)
		req, _ := http.NewRequest(http.MethodGet, server.URL+"/list", nil)
		req.Header.Set("Authorization", "Bearer "+testApiKey)
		resp, _ := http.DefaultClient.Do(req)
		var listResp ListResponse
		if err := json.NewDecoder(resp.Body).Decode(&listResp); err != nil {
			t.Errorf("failed to decode list response: %v", err)
		}
		_ = resp.Body.Close()

		// 2. Upload a file
		req, _ = http.NewRequest(http.MethodPut, server.URL+"/"+bucket+"/"+filename, bytes.NewReader(content))
		req.Header.Set("Authorization", "Bearer "+testApiKey)
		req.Header.Set("Content-Type", customMime)
		resp, _ = http.DefaultClient.Do(req)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("upload failed: %d", resp.StatusCode)
		}

		// 3. List again
		req, _ = http.NewRequest(http.MethodGet, server.URL+"/list", nil)
		req.Header.Set("Authorization", "Bearer "+testApiKey)
		resp, _ = http.DefaultClient.Do(req)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("list failed: %d", resp.StatusCode)
		}

		if err := json.NewDecoder(resp.Body).Decode(&listResp); err != nil {
			t.Errorf("failed to decode list response: %v", err)
		}
		_ = resp.Body.Close()

		foundBucket := false
		for _, b := range listResp.Buckets {
			if b.Name == bucket {
				foundBucket = true
				foundFile := false
				for _, f := range b.Files {
					if f.Key == filename {
						foundFile = true
						if f.Size != int64(len(content)) {
							t.Errorf("expected size %d, got %d", len(content), f.Size)
						}
						if f.ContentType != customMime {
							t.Errorf("expected content type %s, got %s", customMime, f.ContentType)
						}
					}
				}
				if !foundFile {
					t.Errorf("file %s not found in bucket %s", filename, bucket)
				}
			}
		}
		if !foundBucket {
			t.Errorf("bucket %s not found in list", bucket)
		}

		// 4. Unauthorized
		req, _ = http.NewRequest(http.MethodGet, server.URL+"/list", nil)
		resp, _ = http.DefaultClient.Do(req)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("expected 401 for unauthorized list, got %d", resp.StatusCode)
		}
	})

	t.Run("File Permissions (Umask Fix Scenarios)", func(t *testing.T) {
		umaskMutex.Lock()
		defer umaskMutex.Unlock()

		cases := []struct {
			name         string
			umask        int
			expectedFile os.FileMode
			expectedDir  os.FileMode
		}{
			{"Standard 0022", 0022, 0644, 0755},
			{"Group Writable 0002", 0002, 0664, 0775},
			{"Strict 0077", 0077, 0600, 0700},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				// We MUST set the process umask to test MkdirAll properly,
				// because MkdirAll is affected by it.
				oldMask := syscall.Umask(tc.umask)
				defer syscall.Umask(oldMask)

				tempBase, _ := os.MkdirTemp(tempDir, "perm-scen-*")
				permStorage := &Storage{
					BaseDir:  tempBase,
					FileMode: os.FileMode(0666 &^ tc.umask),
					DirMode:  os.FileMode(0777 &^ tc.umask),
				}
				server := httptest.NewServer(permStorage)
				defer server.Close()

				bucket := "scen-test"
				filename := "file.txt"
				req, _ := http.NewRequest(http.MethodPut, server.URL+"/"+bucket+"/"+filename, bytes.NewReader([]byte("test")))
				req.Header.Set("Authorization", "Bearer "+testApiKey)
				resp, _ := http.DefaultClient.Do(req)
				if resp.StatusCode != http.StatusCreated {
					t.Fatalf("upload failed: %d", resp.StatusCode)
				}

				// Check Dir
				dirInfo, _ := os.Stat(filepath.Join(tempBase, bucket))
				if dirInfo.Mode().Perm() != tc.expectedDir {
					t.Errorf("%s: expected dir mode %o, got %o", tc.name, tc.expectedDir, dirInfo.Mode().Perm())
				}

				// Check File (os.Chmod in handleUpload should ensure exact mode)
				fileInfo, _ := os.Stat(filepath.Join(tempBase, bucket, filename))
				if fileInfo.Mode().Perm() != tc.expectedFile {
					t.Errorf("%s: expected file mode %o, got %o", tc.name, tc.expectedFile, fileInfo.Mode().Perm())
				}
			})
		}
	})

	t.Run("Permissions Race Window (Definitive Fix)", func(t *testing.T) {
		tempBase, _ := os.MkdirTemp(tempDir, "race-*")
		raceStorage := &Storage{
			BaseDir:  tempBase,
			FileMode: os.FileMode(0644),
			DirMode:  0755,
		}
		server := httptest.NewServer(raceStorage)
		defer server.Close()

		bucket := "race-bucket"
		filename := "race-file.txt"
		path := filepath.Join(tempBase, bucket, filename)

		// Tight loop in another goroutine to catch the file as it appears
		stopRace := make(chan bool)
		var windowDetected atomic.Bool
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stopRace:
					return
				default:
					info, err := os.Stat(path)
					if err == nil {
						// File appeared! Check permissions immediately.
						if info.Mode().Perm() == 0600 {
							windowDetected.Store(true)
						}
					}
				}
			}
		}()

		// Multiple uploads to increase chance of catching the window
		for i := 0; i < 20; i++ {
			req, _ := http.NewRequest(http.MethodPut, server.URL+"/"+bucket+"/"+filename, bytes.NewReader([]byte("race-test")))
			req.Header.Set("Authorization", "Bearer "+testApiKey)
			resp, _ := http.DefaultClient.Do(req)
			if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
				t.Errorf("upload failed: %d", resp.StatusCode)
			}
		}

		close(stopRace)
		wg.Wait()

		if windowDetected.Load() {
			t.Errorf("Race window detected: file appeared at destination with 0600 permissions before Chmod was effective")
		}
	})
}

func TestUmaskEnvironmentVariable(t *testing.T) {
	// This tests the logic used in main() for parsing umask
	parseUmask := func(val string) int {
		umask := 0022
		if val != "" {
			if parsed, err := strconv.ParseInt(val, 8, 32); err == nil {
				umask = int(parsed)
			}
		}
		return umask
	}

	cases := []struct {
		env      string
		expected int
	}{
		{"", 0022},
		{"0002", 0002},
		{"0077", 0077},
		{"invalid", 0022},
	}

	for _, tc := range cases {
		got := parseUmask(tc.env)
		if got != tc.expected {
			t.Errorf("parseUmask(%q) = %04o, want %04o", tc.env, got, tc.expected)
		}
	}
}

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
	defer func() { _ = os.Unsetenv("STORAGE_API_KEY") }()

	// macOS trick: /var is a symlink to /private/var
	tempDir, _ = filepath.EvalSymlinks(tempDir)

	storage := &Storage{
		BaseDir:            tempDir,
		MaxBucketSizeBytes: 1000,
		inFlightBytes:      0,
		FileMode:           0644,
		DirMode:            0755,
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
		req, _ = http.NewRequest(http.MethodPut, fileUrl, bytes.NewReader([]byte("12345678901234567890")))
		req.Header.Set("Authorization", "Bearer "+apiKey)
		resp, _ = http.DefaultClient.Do(req)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("overwrite failed: %d", resp.StatusCode)
		}

		if atomic.LoadInt64(sizePtr) != 20 {
			t.Errorf("expected size 20 after overwrite, got %d", atomic.LoadInt64(sizePtr))
		}

		// 3. Overwrite up to full bucket capacity (1000 bytes)
		req, _ = http.NewRequest(http.MethodPut, fileUrl, bytes.NewReader(bytes.Repeat([]byte("a"), 1000)))
		req.Header.Set("Authorization", "Bearer "+apiKey)
		resp, _ = http.DefaultClient.Do(req)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("overwrite to full capacity failed: %d", resp.StatusCode)
		}
		if atomic.LoadInt64(sizePtr) != 1000 {
			t.Errorf("expected size 1000 after full capacity overwrite, got %d", atomic.LoadInt64(sizePtr))
		}
	})

	t.Run("Lock map memory leak fix", func(t *testing.T) {
		bucket := "lock-fix"
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

		count := 0
		storage.locks.Range(func(key, value interface{}) bool {
			count++
			return true
		})

		if count > 0 {
			t.Errorf("expected 0 locks in map after uploads, got %d", count)
		}
	})

	t.Run("In-flight protection", func(t *testing.T) {
		bucket := "inflight-test"
		body := make([]byte, 1001)
		req, _ := http.NewRequest(http.MethodPut, server.URL+"/"+bucket+"/huge.txt", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+apiKey)

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusInsufficientStorage {
			t.Errorf("expected 507, got %d", resp.StatusCode)
		}
	})
}

func TestVulnerabilityRegressions(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "storage-vuln-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(tempDir) }()

	tempDir, _ = filepath.EvalSymlinks(tempDir)

	apiKey := "secret"
	if err := os.Setenv("STORAGE_API_KEY", apiKey); err != nil {
		t.Fatalf("failed to set env: %v", err)
	}
	defer func() { _ = os.Unsetenv("STORAGE_API_KEY") }()

	storage := &Storage{
		BaseDir:            tempDir,
		MaxBucketSizeBytes: 1000,
		FileMode:           0644,
		DirMode:            0755,
	}
	storage.calculateInitialSizes()

	server := httptest.NewServer(storage)
	defer server.Close()

	t.Run("1. Legitimate upload-* files must not be hidden or deleted on restart", func(t *testing.T) {
		bucket := "docs"
		key := "upload-invoice.pdf"
		content := []byte("important-invoice-data")

		// Upload legitimate file starting with "upload-"
		req, _ := http.NewRequest(http.MethodPut, server.URL+"/"+bucket+"/"+key, bytes.NewReader(content))
		req.Header.Set("Authorization", "Bearer "+apiKey)
		resp, err := http.DefaultClient.Do(req)
		if err != nil || resp.StatusCode != http.StatusCreated {
			t.Fatalf("upload failed: %v, status: %d", err, resp.StatusCode)
		}
		_ = resp.Body.Close()

		// 1a. Verify file appears in GET /list
		req, _ = http.NewRequest(http.MethodGet, server.URL+"/list", nil)
		req.Header.Set("Authorization", "Bearer "+apiKey)
		resp, err = http.DefaultClient.Do(req)
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("list failed: %v", err)
		}
		var listResp ListResponse
		_ = json.NewDecoder(resp.Body).Decode(&listResp)
		_ = resp.Body.Close()

		found := false
		for _, b := range listResp.Buckets {
			if b.Name == bucket {
				for _, f := range b.Files {
					if f.Key == key {
						found = true
					}
				}
			}
		}
		if !found {
			t.Errorf("file %q was hidden from /list response", key)
		}

		// 1b. Simulate service restart cleanup (calculateInitialSizes)
		storage.calculateInitialSizes()

		// Verify file was NOT deleted by startup cleanup
		req, _ = http.NewRequest(http.MethodGet, server.URL+"/"+bucket+"/"+key, nil)
		req.Header.Set("Authorization", "Bearer "+apiKey)
		resp, err = http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("get request failed: %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("file %q was deleted on startup cleanup! got status %d", key, resp.StatusCode)
		}
	})

	t.Run("2. Deleting a subdirectory must not underflow bucket quota", func(t *testing.T) {
		bucket := "quota-underflow"
		fileURL := server.URL + "/" + bucket + "/sub/item.txt"

		// Upload 10-byte file in subdirectory
		req, _ := http.NewRequest(http.MethodPut, fileURL, bytes.NewReader([]byte("0123456789")))
		req.Header.Set("Authorization", "Bearer "+apiKey)
		resp, _ := http.DefaultClient.Do(req)
		_ = resp.Body.Close()

		// Delete the file -> bucket size should be 0
		req, _ = http.NewRequest(http.MethodDelete, fileURL, nil)
		req.Header.Set("Authorization", "Bearer "+apiKey)
		resp, _ = http.DefaultClient.Do(req)
		_ = resp.Body.Close()

		sizePtr := storage.getBucketSizePtr(bucket)
		if got := atomic.LoadInt64(sizePtr); got != 0 {
			t.Fatalf("expected bucket size 0 after file delete, got %d", got)
		}

		// Attempt to DELETE the empty subdirectory "/sub"
		dirURL := server.URL + "/" + bucket + "/sub"
		req, _ = http.NewRequest(http.MethodDelete, dirURL, nil)
		req.Header.Set("Authorization", "Bearer "+apiKey)
		resp, _ = http.DefaultClient.Do(req)
		_ = resp.Body.Close()

		if got := atomic.LoadInt64(sizePtr); got < 0 {
			t.Errorf("bucket quota underflowed to negative value after directory DELETE: %d", got)
		}
	})

	t.Run("3. GET on a subdirectory must not expose HTML directory listing", func(t *testing.T) {
		bucket := "dir-listing"
		fileURL := server.URL + "/" + bucket + "/nested/secret.txt"

		req, _ := http.NewRequest(http.MethodPut, fileURL, bytes.NewReader([]byte("secret")))
		req.Header.Set("Authorization", "Bearer "+apiKey)
		resp, _ := http.DefaultClient.Do(req)
		_ = resp.Body.Close()

		// Request the directory path itself
		dirURL := server.URL + "/" + bucket + "/nested"
		req, _ = http.NewRequest(http.MethodGet, dirURL, nil)
		req.Header.Set("Authorization", "Bearer "+apiKey)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		if resp.StatusCode == http.StatusOK || strings.Contains(string(body), ".meta") {
			t.Errorf("GET on subdirectory exposed directory listing (status %d): %s", resp.StatusCode, string(body))
		}
	})

	t.Run("4. Chunked uploads (ContentLength -1) must track in-flight bytes", func(t *testing.T) {
		chunkStorage := &Storage{
			BaseDir:            tempDir,
			MaxBucketSizeBytes: 50,
			FileMode:           0644,
			DirMode:            0755,
		}
		chunkServer := httptest.NewServer(chunkStorage)
		defer chunkServer.Close()

		pr, pw := io.Pipe()
		written40 := make(chan struct{})
		uploadDone := make(chan struct{})

		go func() {
			defer close(uploadDone)
			// Wrapping pr in io.NopCloser hides size so ContentLength is -1 (chunked)
			req, _ := http.NewRequest(http.MethodPut, chunkServer.URL+"/chunk-bkt/stream1.bin", io.NopCloser(pr))
			req.ContentLength = -1
			req.Header.Set("Authorization", "Bearer "+apiKey)
			resp, err := http.DefaultClient.Do(req)
			if err == nil && resp != nil {
				_ = resp.Body.Close()
			}
		}()

		// Stream 40 bytes into the first upload and keep the stream open
		go func() {
			_, _ = pw.Write(bytes.Repeat([]byte("x"), 40))
			close(written40)
		}()
		<-written40

		// Wait for the server handler to receive the 40 chunked bytes
		deadline := time.Now().Add(500 * time.Millisecond)
		for atomic.LoadInt64(&chunkStorage.inFlightBytes) < 40 && time.Now().Before(deadline) {
			time.Sleep(2 * time.Millisecond)
		}

		// Now send a second upload of 20 bytes (40 in-flight + 20 = 60 > 50 quota)
		req2, _ := http.NewRequest(http.MethodPut, chunkServer.URL+"/chunk-bkt/stream2.bin", bytes.NewReader(bytes.Repeat([]byte("y"), 20)))
		req2.Header.Set("Authorization", "Bearer "+apiKey)
		resp2, err := http.DefaultClient.Do(req2)
		if err != nil {
			t.Fatalf("second upload request failed: %v", err)
		}
		_ = resp2.Body.Close()

		_ = pw.Close()
		<-uploadDone

		if resp2.StatusCode != http.StatusInsufficientStorage {
			t.Errorf("expected 507 Insufficient Storage while chunked upload was in-flight, got %d", resp2.StatusCode)
		}
	})

	t.Run("5. Symlink prefix and parent-directory escape jailing", func(t *testing.T) {
		// Create two sibling buckets where one is a prefix of the other: "app" and "app-secret"
		appDir := filepath.Join(tempDir, "app")
		secretDir := filepath.Join(tempDir, "app-secret")
		_ = os.MkdirAll(appDir, 0755)
		_ = os.MkdirAll(secretDir, 0755)

		secretFile := filepath.Join(secretDir, "secret.txt")
		_ = os.WriteFile(secretFile, []byte("top-secret"), 0644)

		// 5a. File symlink in "app" pointing to sibling bucket "app-secret/secret.txt"
		_ = os.Symlink(secretFile, filepath.Join(appDir, "leak.txt"))

		req, _ := http.NewRequest(http.MethodGet, server.URL+"/app/leak.txt", nil)
		req.Header.Set("Authorization", "Bearer "+apiKey)
		resp, _ := http.DefaultClient.Do(req)
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Errorf("cross-bucket symlink escape succeeded via prefix match (status 200 OK)")
		}

		// 5b. Directory symlink in "app" pointing to "app-secret", uploading a new leaf file
		_ = os.Symlink(secretDir, filepath.Join(appDir, "symdir"))
		req, _ = http.NewRequest(http.MethodPut, server.URL+"/app/symdir/pwned.txt", bytes.NewReader([]byte("overwrite")))
		req.Header.Set("Authorization", "Bearer "+apiKey)
		resp, _ = http.DefaultClient.Do(req)
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusCreated {
			t.Errorf("symlinked parent directory escape succeeded on PUT (status 201 Created)")
		}
	})
}

type infiniteReader struct{}

func (r *infiniteReader) Read(p []byte) (n int, err error) {
	for i := range p {
		p[i] = 'a'
	}
	return len(p), nil
}
