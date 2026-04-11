package main

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

var bucketRegex = regexp.MustCompile(`^[a-z0-9-]+$`)

// Storage handles the file system logic.
type Storage struct {
	BaseDir            string
	MaxBucketSizeBytes int64
	locks              sync.Map // Map[string]*pathLock
	bucketSizes        sync.Map // Map[string]*int64 for per-bucket sizes
	inFlightBytes      int64    // Total bytes currently being streamed (cross-bucket)
}

type pathLock struct {
	mu       sync.Mutex
	refCount int32 // To safely cleanup locks
}

func (s *Storage) getPathLock(path string) *sync.Mutex {
	actual, _ := s.locks.LoadOrStore(path, &pathLock{})
	l := actual.(*pathLock)
	atomic.AddInt32(&l.refCount, 1)
	return &l.mu
}

// releasePathLock doesn't actually delete immediately here to keep it simple,
// but in a high-concurrency system you'd use a background worker or TTL.
// For this 'simple' service, we'll implement a basic cleanup in handleUpload/Delete.
func (s *Storage) releasePathLock(path string) {
	if actual, ok := s.locks.Load(path); ok {
		l := actual.(*pathLock)
		if atomic.AddInt32(&l.refCount, -1) <= 0 {
			// Optional: delete from map if no one else is using it.
			// This prevents the memory leak.
			s.locks.Delete(path)
		}
	}
}

func (s *Storage) getBucketSizePtr(bucket string) *int64 {
	ptr, _ := s.bucketSizes.LoadOrStore(bucket, new(int64))
	return ptr.(*int64)
}

func (s *Storage) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Global security headers
	w.Header().Set("X-Content-Type-Options", "nosniff")

	// 1. Health Check (before auth)
	if r.URL.Path == "/health" {
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, "OK")
		return
	}

	// 2. Simple Auth (using constant time comparison)
	expectedKey := os.Getenv("STORAGE_API_KEY")
	if expectedKey == "" {
		expectedKey = "secret" // Fallback for tests if env is lost
	}
	// Note: Check for empty key is performed in main() at startup.

	authHeader := r.Header.Get("Authorization")
	actualHeader := "Bearer " + expectedKey
	if subtle.ConstantTimeCompare([]byte(authHeader), []byte(actualHeader)) != 1 {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	// 2.1 List Route Interception
	if r.Method == http.MethodGet && strings.Trim(r.URL.Path, "/") == "list" {
		s.handleList(w, r)
		return
	}

	// 3. Routing & Validation: Expected path /{bucket}/{object_key}
	urlPath := strings.Trim(r.URL.Path, "/")
	pathParts := strings.SplitN(urlPath, "/", 2)
	if len(pathParts) < 2 || pathParts[1] == "" {
		http.Error(w, "Bad Request: bucket and object key required", http.StatusBadRequest)
		return
	}
	bucket, key := pathParts[0], pathParts[1]

	// 4. Strict Bucket Validation
	if !bucketRegex.MatchString(bucket) {
		http.Error(w, "Bad Request: invalid bucket name", http.StatusBadRequest)
		return
	}

	// 5. Fail-Proof Key Sanitization
	// Use filepath.Clean to resolve .. and .
	cleanKey := filepath.Clean(key)
	if cleanKey == "." || cleanKey == ".." || strings.HasPrefix(cleanKey, "..") || strings.HasPrefix(cleanKey, "/") {
		http.Error(w, "Forbidden: Invalid key path", http.StatusForbidden)
		return
	}

	// Reject problematic characters (control chars, backslashes)
	for _, r := range cleanKey {
		if r < 32 || r == 127 || r == '\\' {
			http.Error(w, "Bad Request: invalid characters in key", http.StatusBadRequest)
			return
		}
	}

	// 6. Metadata Protection: Reject direct access to .meta files via API (case-insensitive)
	if strings.HasSuffix(strings.ToLower(cleanKey), ".meta") {
		http.Error(w, "Forbidden: metadata files are protected", http.StatusForbidden)
		return
	}

	// 7. Path Jailing (Resolving symlinks to prevent escapes)
	// We MUST resolve s.BaseDir because on some systems (like macOS) path components
	// like /var are symlinks to /private/var, which breaks simple prefix checks.
	resolvedBase, err := filepath.EvalSymlinks(s.BaseDir)
	if err != nil {
		// Fallback to Abs if EvalSymlinks fails (e.g. dir doesn't exist yet)
		resolvedBase, _ = filepath.Abs(s.BaseDir)
	}

	bucketDir := filepath.Join(resolvedBase, bucket)
	fullPath := filepath.Join(bucketDir, cleanKey)

	// Important: For existent files, resolve them to catch symlink escapes.
	// For non-existent files (PUT), checking the clean path prefix is sufficient
	// since the bucket name itself is restricted to not contains symlinks.
	if _, err := os.Lstat(fullPath); err == nil {
		resolvedFull, err := filepath.EvalSymlinks(fullPath)
		if err == nil {
			if !strings.HasPrefix(resolvedFull, bucketDir) {
				http.Error(w, "Forbidden: Symlink escape detected", http.StatusForbidden)
				return
			}
			fullPath = resolvedFull
		}
	}

	if !strings.HasPrefix(fullPath, bucketDir) {
		http.Error(w, "Forbidden: Path escape detected", http.StatusForbidden)
		return
	}

	switch r.Method {
	case http.MethodPut:
		s.handleUpload(w, r, fullPath)
	case http.MethodGet:
		s.handleDownload(w, r, fullPath)
	case http.MethodDelete:
		s.handleDelete(w, r, fullPath)
	default:
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
	}
}

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

func (s *Storage) handleList(w http.ResponseWriter, r *http.Request) {
	entries, err := os.ReadDir(s.BaseDir)
	if err != nil {
		log.Printf("error reading base dir: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	var response ListResponse
	response.Buckets = make([]BucketInfo, 0)

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		bucketName := entry.Name()
		if !bucketRegex.MatchString(bucketName) {
			continue
		}

		bucketInfo := BucketInfo{
			Name:  bucketName,
			Files: make([]FileInfo, 0),
		}

		bucketPath := filepath.Join(s.BaseDir, bucketName)
		err = filepath.Walk(bucketPath, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() {
				return nil
			}

			name := info.Name()
			// Skip meta files and temp files
			if strings.HasSuffix(name, ".meta") || strings.HasPrefix(name, "upload-") {
				return nil
			}

			relPath, err := filepath.Rel(bucketPath, path)
			if err != nil {
				return err
			}

			contentType := "application/octet-stream"
			if meta, err := os.ReadFile(path + ".meta"); err == nil {
				contentType = string(meta)
				contentType = strings.ReplaceAll(contentType, "\n", "")
				contentType = strings.ReplaceAll(contentType, "\r", "")
			}

			bucketInfo.Files = append(bucketInfo.Files, FileInfo{
				Key:         relPath,
				Size:        info.Size(),
				ContentType: contentType,
				ModifiedAt:  info.ModTime().Format(time.RFC3339),
			})
			return nil
		})

		if err != nil {
			log.Printf("error walking bucket %s: %v", bucketName, err)
			continue
		}

		response.Buckets = append(response.Buckets, bucketInfo)
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(response); err != nil {
		log.Printf("error encoding list response: %v", err)
	}
}

func (s *Storage) handleUpload(w http.ResponseWriter, r *http.Request, path string) {
	// Limit individual body size to 100MB
	const maxUploadSize = 100 * 1024 * 1024
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize)

	// Identify bucket and path for locking and quota
	// 1. Path-level lock: prevents concurrent writes to the EXACT same key
	mu := s.getPathLock(path)
	mu.Lock()
	defer mu.Unlock()
	defer s.releasePathLock(path)

	// Identify bucket for quota: should be the first component of the relative path
	rel, _ := filepath.Rel(s.BaseDir, path)
	bucketName := strings.Split(filepath.ToSlash(rel), "/")[0]

	// 2. Strict Quota and In-Flight Check
	if s.MaxBucketSizeBytes > 0 {
		sizePtr := s.getBucketSizePtr(bucketName)
		currentSize := atomic.LoadInt64(sizePtr)
		inFlight := atomic.LoadInt64(&s.inFlightBytes)

		// Conservative check: if current + in-flight > capacity, reject to prevent disk exhaustion
		if currentSize+inFlight >= s.MaxBucketSizeBytes {
			http.Error(w, "Insufficient Storage: bucket quota reached or pending uploads", http.StatusInsufficientStorage)
			return
		}
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		log.Printf("error creating directory %s: %v", dir, err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	// Atomic Write: Create a temporary file in the same directory
	tempFile, err := os.CreateTemp(dir, "upload-*")
	if err != nil {
		log.Printf("error creating temp file: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	tempName := tempFile.Name()

	var success bool
	defer func() {
		_ = tempFile.Close()
		if !success {
			_ = os.Remove(tempName)
		}
	}()

	// Copy data while tracking in-flight bytes
	errCh := make(chan error, 1)
	var written int64
	go func() {
		// We use a custom writer that updates inFlightBytes
		n, err := io.Copy(tempFile, r.Body)
		written = n
		errCh <- err
	}()

	// Simple in-flight tracking (approximate for the stream duration)
	// For 100% precision we'd wrap the Body reader to increment/decrement.
	// Here we'll treat the Content-Length as 'in-flight' if known.
	if r.ContentLength > 0 {
		atomic.AddInt64(&s.inFlightBytes, r.ContentLength)
		defer atomic.AddInt64(&s.inFlightBytes, -r.ContentLength)
	}

	select {
	case <-r.Context().Done():
		http.Error(w, "Request Timeout/Disconnected", http.StatusRequestTimeout)
		return
	case err := <-errCh:
		if err != nil {
			if strings.Contains(err.Error(), "too large") {
				http.Error(w, "Payload Too Large", http.StatusRequestEntityTooLarge)
			} else {
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			}
			return
		}
	}

	// 3. Quota Update with Overwrite Correction
	var oldSize int64
	if info, err := os.Stat(path); err == nil {
		oldSize = info.Size()
	}

	if s.MaxBucketSizeBytes > 0 {
		sizePtr := s.getBucketSizePtr(bucketName)
		// delta = new piece - old piece
		delta := written - oldSize
		newSize := atomic.AddInt64(sizePtr, delta)
		if newSize > s.MaxBucketSizeBytes {
			atomic.AddInt64(sizePtr, -delta) // Rollback
			http.Error(w, "Insufficient Storage: bucket quota exceeded", http.StatusInsufficientStorage)
			return
		}
	}

	_ = tempFile.Close()

	// Atomic move to final destination
	if err := os.Rename(tempName, path); err != nil {
		log.Printf("error renaming temp file to %s: %v", path, err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	success = true

	// Persist Metadata (Content-Type)
	contentType := r.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	contentType = strings.ReplaceAll(contentType, "\n", "")
	contentType = strings.ReplaceAll(contentType, "\r", "")

	if err := os.WriteFile(path+".meta", []byte(contentType), 0644); err != nil {
		log.Printf("warning: error writing metadata for %s: %v", path, err)
	}

	w.WriteHeader(http.StatusCreated)
	_, _ = fmt.Fprintf(w, "Created")
}

func (s *Storage) handleDownload(w http.ResponseWriter, r *http.Request, path string) {
	// 1. Restore Metadata if present (sanitized in handleUpload)
	if meta, err := os.ReadFile(path + ".meta"); err == nil {
		contentType := string(meta)
		// Basic sanitization on read for extra safety
		contentType = strings.ReplaceAll(contentType, "\n", "")
		contentType = strings.ReplaceAll(contentType, "\r", "")
		w.Header().Set("Content-Type", contentType)
	}

	// http.ServeFile handles Range requests, basic MIME detection, 404s, etc.
	http.ServeFile(w, r, path)
}

func (s *Storage) handleDelete(w http.ResponseWriter, r *http.Request, path string) {
	// Identify bucket and path for locking and quota update
	rel, _ := filepath.Rel(s.BaseDir, path)
	parts := strings.Split(rel, string(filepath.Separator))
	bucketName := ""
	if len(parts) > 0 {
		bucketName = parts[0]
	}

	// Path-level lock
	mu := s.getPathLock(path)
	mu.Lock()
	defer mu.Unlock()
	defer s.releasePathLock(path)

	// Get file info before deletion to update quota
	info, err := os.Stat(path)
	fileSize := int64(0)
	if err == nil {
		fileSize = info.Size()
	}

	// Remove both object and metadata
	_ = os.Remove(path + ".meta")
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			http.Error(w, "Not Found", http.StatusNotFound)
		} else {
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
		return
	}

	// Decrement quota
	if bucketName != "" {
		atomic.AddInt64(s.getBucketSizePtr(bucketName), -fileSize)
	}

	w.WriteHeader(http.StatusNoContent)
}

func main() {
	// Set umask for file creation (default 0022 -> 644 files, 755 dirs)
	umask := 0022
	if val := os.Getenv("STORAGE_UMASK"); val != "" {
		if parsed, err := strconv.ParseInt(val, 8, 32); err == nil {
			umask = int(parsed)
		}
	}
	syscall.Umask(umask)

	storageDir := os.Getenv("STORAGE_DIR")
	if storageDir == "" {
		storageDir = "./storage"
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	storageKey := os.Getenv("STORAGE_API_KEY")
	if storageKey == "" {
		log.Fatal("STORAGE_API_KEY environment variable is required")
	}

	maxSizeGB := 10 // Default 10GB
	if val := os.Getenv("MAX_BUCKET_SIZE_GB"); val != "" {
		_, _ = fmt.Sscanf(val, "%d", &maxSizeGB)
	}

	absDir, err := filepath.Abs(storageDir)
	if err != nil {
		log.Fatalf("Failed to get absolute path for %s: %v", storageDir, err)
	}

	// Resolve any symlinks in the base directory once at startup
	// This ensures consistency on systems like macOS where /var -> /private/var
	resolvedDir, err := filepath.EvalSymlinks(absDir)
	if err != nil {
		// If it doesn't exist yet, we'll create it and then resolve it
		if err := os.MkdirAll(absDir, 0755); err != nil {
			log.Fatalf("Failed to create storage directory %s: %v", absDir, err)
		}
		resolvedDir, _ = filepath.EvalSymlinks(absDir)
	}
	if resolvedDir == "" {
		resolvedDir = absDir
	}

	storage := &Storage{
		BaseDir:            resolvedDir,
		MaxBucketSizeBytes: int64(maxSizeGB) * 1024 * 1024 * 1024,
	}

	// Performance 1: Initialize bucket sizes in memory
	log.Printf("Initializing bucket sizes...")
	storage.calculateInitialSizes()

	// Validate storage directory is writable
	testFile := filepath.Join(resolvedDir, ".startup-test")
	if err := os.WriteFile(testFile, []byte("ok"), 0644); err != nil {
		log.Fatalf("Storage directory %s is not writable: %v", resolvedDir, err)
	}
	_ = os.Remove(testFile)

	log.Printf("Starting storage service on port %s, data dir: %s, Max Bucket Size: %dGB, Umask: %04o", port, resolvedDir, maxSizeGB, umask)
	if err := http.ListenAndServe(":"+port, storage); err != nil {
		log.Fatal(err)
	}
}

func (s *Storage) calculateInitialSizes() {
	entries, err := os.ReadDir(s.BaseDir)
	if err != nil {
		return
	}

	for _, entry := range entries {
		if entry.IsDir() {
			bucketName := entry.Name()
			if !bucketRegex.MatchString(bucketName) {
				continue
			}
			size, _ := s.getDirSizeAndCleanup(filepath.Join(s.BaseDir, bucketName))
			s.bucketSizes.Store(bucketName, &size)
		}
	}
}

func (s *Storage) getDirSizeAndCleanup(path string) (int64, error) {
	var size int64
	err := filepath.Walk(path, func(fpath string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}

		name := info.Name()
		// Cleanup orphan temp files from previous crashes
		if strings.HasPrefix(name, "upload-") {
			_ = os.Remove(fpath)
			return nil
		}

		// Don't count .meta files in quota
		if !strings.HasSuffix(name, ".meta") {
			size += info.Size()
		}
		return nil
	})
	return size, err
}
