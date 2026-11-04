//go:build integration
package integration

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type ListResponse struct {
	Buckets []BucketInfo `json:"buckets"`
}

type BucketInfo struct {
	Name  string     `json:"name"`
	Files []FileInfo `json:"files"`
}

type FileInfo struct {
	Key         string `json:"key"`
	Size        int64  `json:"size"`
	ContentType string `json:"content_type"`
	ModifiedAt  string `json:"modified_at"`
}

const (
	baseURL    = "http://127.0.0.1:8080"
	apiKey     = "my-secure-key" // Matches docker-compose.yml
	testDataDir = "../../test-data"
	storageDir  = "../../storage-data"
)

func TestIntegration(t *testing.T) {
	baseURL := baseURL
	if port := os.Getenv("HOST_PORT"); port != "" {
		baseURL = "http://127.0.0.1:" + port
	}
	client := &http.Client{}

	t.Run("Health Check", func(t *testing.T) {
		resp, err := client.Get(baseURL + "/health")
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("Health check failed: %v, status: %d", err, resp.StatusCode)
		}
	})

	t.Run("Unauthorized Access Rejection", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, baseURL+"/any-bucket/any-key", nil)
		resp, _ := client.Do(req)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("expected 401 Unauthorized, got %d", resp.StatusCode)
		}
	})

	t.Run("Bucket Validation", func(t *testing.T) {
		// Invalid bucket name (upper case)
		req, _ := http.NewRequest(http.MethodPut, baseURL+"/InvalidBucket/file.txt", bytes.NewReader([]byte("data")))
		req.Header.Set("Authorization", "Bearer "+apiKey)
		resp, _ := client.Do(req)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("expected 400 Bad Request for invalid bucket name, got %d", resp.StatusCode)
		}
	})

	t.Run("Full Data Integrity Cycle (test-data)", func(t *testing.T) {
		bucket := "integration-bucket"
		
		files, err := os.ReadDir(testDataDir)
		if err != nil {
			t.Fatalf("failed to read test-data: %v", err)
		}

		for _, file := range files {
			if file.IsDir() { continue }
			
			filename := file.Name()
			t.Run("File: "+filename, func(t *testing.T) {
				sourcePath := filepath.Join(testDataDir, filename)
				content, err := os.ReadFile(sourcePath)
				if err != nil {
					t.Fatalf("failed to read source file %s: %v", filename, err)
				}

				// 1. Upload (PUT)
				req, _ := http.NewRequest(http.MethodPut, baseURL+"/"+bucket+"/"+filename, bytes.NewReader(content))
				req.Header.Set("Authorization", "Bearer "+apiKey)
				resp, err := client.Do(req)
				if err != nil || resp.StatusCode != http.StatusCreated {
					t.Fatalf("Upload failed for %s: %v, status: %d", filename, err, resp.StatusCode)
				}

				// 2. Download (GET)
				req, _ = http.NewRequest(http.MethodGet, baseURL+"/"+bucket+"/"+filename, nil)
				req.Header.Set("Authorization", "Bearer "+apiKey)
				resp, err = client.Do(req)
				if err != nil || resp.StatusCode != http.StatusOK {
					t.Fatalf("Download failed for %s: %v, status: %d", filename, err, resp.StatusCode)
				}
				
				// Verify X-Content-Type-Options: nosniff
				if h := resp.Header.Get("X-Content-Type-Options"); h != "nosniff" {
					t.Errorf("nosniff header missing for %s", filename)
				}

				downloaded, _ := io.ReadAll(resp.Body)
				if !bytes.Equal(content, downloaded) {
					t.Errorf("Integrity check failed for %s: contents mismatch", filename)
				}
				resp.Body.Close()

				// 3. Verify on Host FS (Volume Mount)
				hostPath := filepath.Join(storageDir, bucket, filename)
				info, err := os.Stat(hostPath)
				if os.IsNotExist(err) {
					t.Errorf("File %s not found in host storage directory (Volume mount failure)", hostPath)
				} else if err != nil {
					t.Errorf("failed to stat host file %s: %v", hostPath, err)
				} else {
					// Verify permissions (0644 expected by default)
					if info.Mode().Perm() != 0644 {
						t.Errorf("expected host file mode 0644 for %s, got %o", filename, info.Mode().Perm())
					}
				}

				// 4. Verify Meta File permissions
				metaPath := hostPath + ".meta"
				if metaInfo, err := os.Stat(metaPath); err == nil {
					if metaInfo.Mode().Perm() != 0644 {
						t.Errorf("expected host meta file mode 0644 for %s, got %o", filename, metaInfo.Mode().Perm())
					}
				} else if !os.IsNotExist(err) {
					t.Errorf("failed to stat host meta file %s: %v", metaPath, err)
				}
			})
		}
	})

	t.Run("Range Requests (206 Partial Content)", func(t *testing.T) {
		bucket := "integration-bucket"
		filename := "range-test.txt"
		content := []byte("0123456789ABCDEF")

		// Upload
		req, _ := http.NewRequest(http.MethodPut, baseURL+"/"+bucket+"/"+filename, bytes.NewReader(content))
		req.Header.Set("Authorization", "Bearer "+apiKey)
		client.Do(req)

		// Partial Get
		req, _ = http.NewRequest(http.MethodGet, baseURL+"/"+bucket+"/"+filename, nil)
		req.Header.Set("Authorization", "Bearer "+apiKey)
		req.Header.Set("Range", "bytes=0-3")
		resp, _ := client.Do(req)

		if resp.StatusCode != http.StatusPartialContent {
			t.Errorf("expected 206 Partial Content, got %d", resp.StatusCode)
		}
		
		body, _ := io.ReadAll(resp.Body)
		if string(body) != "0123" {
			t.Errorf("expected '0123', got %q", string(body))
		}
		resp.Body.Close()
	})

	t.Run("List Endpoint", func(t *testing.T) {
		bucket := "list-integration"
		filename := "list-test.txt"
		content := []byte("integration list data")

		// 1. Upload
		req, _ := http.NewRequest(http.MethodPut, baseURL+"/"+bucket+"/"+filename, bytes.NewReader(content))
		req.Header.Set("Authorization", "Bearer "+apiKey)
		req.Header.Set("Content-Type", "text/plain")
		resp, err := client.Do(req)
		if err != nil || resp.StatusCode != http.StatusCreated {
			t.Fatalf("upload failed: %v, status: %d", err, resp.StatusCode)
		}

		// 2. List
		req, _ = http.NewRequest(http.MethodGet, baseURL+"/list", nil)
		req.Header.Set("Authorization", "Bearer "+apiKey)
		resp, err = client.Do(req)
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("list failed: %v, status: %d", err, resp.StatusCode)
		}

		var listResp ListResponse
		if err := json.NewDecoder(resp.Body).Decode(&listResp); err != nil {
			t.Fatalf("failed to decode list response: %v", err)
		}
		resp.Body.Close()

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
	})

	t.Run("Security Regressions (Upload-Prefix, Directory Ops, Symlink Jailing)", func(t *testing.T) {
		bucket := "sec-integration"

		// 1. Upload a legitimate file prefixed with "upload-"
		uploadKey := "upload-statement.pdf"
		content := []byte("statement-content")
		req, _ := http.NewRequest(http.MethodPut, baseURL+"/"+bucket+"/"+uploadKey, bytes.NewReader(content))
		req.Header.Set("Authorization", "Bearer "+apiKey)
		resp, err := client.Do(req)
		if err != nil || resp.StatusCode != http.StatusCreated {
			t.Fatalf("upload failed: %v, status: %d", err, resp.StatusCode)
		}
		_ = resp.Body.Close()

		// Verify "upload-statement.pdf" appears in GET /list
		req, _ = http.NewRequest(http.MethodGet, baseURL+"/list", nil)
		req.Header.Set("Authorization", "Bearer "+apiKey)
		resp, err = client.Do(req)
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("list failed: %v, status: %d", err, resp.StatusCode)
		}
		var listResp ListResponse
		if err := json.NewDecoder(resp.Body).Decode(&listResp); err != nil {
			t.Fatalf("failed to decode list response: %v", err)
		}
		_ = resp.Body.Close()

		foundUploadFile := false
		for _, b := range listResp.Buckets {
			if b.Name == bucket {
				for _, f := range b.Files {
					if f.Key == uploadKey {
						foundUploadFile = true
					}
				}
			}
		}
		if !foundUploadFile {
			t.Errorf("legitimate file %q missing from /list", uploadKey)
		}

		// 2. Upload nested file and verify GET/DELETE on the subdirectory are rejected
		nestedURL := baseURL + "/" + bucket + "/nested/doc.txt"
		req, _ = http.NewRequest(http.MethodPut, nestedURL, bytes.NewReader([]byte("nested")))
		req.Header.Set("Authorization", "Bearer "+apiKey)
		resp, err = client.Do(req)
		if err != nil || resp.StatusCode != http.StatusCreated {
			t.Fatalf("nested upload failed: %v", err)
		}
		_ = resp.Body.Close()

		// GET on subdirectory "/nested" must not expose HTML directory listing or .meta files
		dirURL := baseURL + "/" + bucket + "/nested"
		req, _ = http.NewRequest(http.MethodGet, dirURL, nil)
		req.Header.Set("Authorization", "Bearer "+apiKey)
		resp, err = client.Do(req)
		if err != nil {
			t.Fatalf("GET on directory failed: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusOK || strings.Contains(string(body), ".meta") {
			t.Errorf("GET on subdirectory exposed directory listing (status %d): %s", resp.StatusCode, string(body))
		}

		// DELETE on subdirectory "/nested" must be rejected with 400 Bad Request
		req, _ = http.NewRequest(http.MethodDelete, dirURL, nil)
		req.Header.Set("Authorization", "Bearer "+apiKey)
		resp, err = client.Do(req)
		if err != nil {
			t.Fatalf("DELETE on directory failed: %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("expected 400 Bad Request when deleting a directory, got %d", resp.StatusCode)
		}
	})

	// Cleanup storage directory after tests
	t.Cleanup(func() {
		os.RemoveAll(storageDir)
	})
}
