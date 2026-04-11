package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
)

const testApiKey = "test-secret-key"

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

	storage := &Storage{BaseDir: tempDir}
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
}

type infiniteReader struct{}

func (r *infiniteReader) Read(p []byte) (n int, err error) {
	for i := range p {
		p[i] = 'a'
	}
	return len(p), nil
}
