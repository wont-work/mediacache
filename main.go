package main

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	cacheDir = getEnv("CACHE_DIR", "./cache")
	upstream = getEnv("CACHE_UPSTREAM", "https://example.com")
	stripPrefix = getEnv("CACHE_PREFIX", "/files")

	locks = make(map[string]*sync.Mutex)
	mutex = &sync.RWMutex{}
)

func getEnv(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}

	return fallback
}

type fileMeta struct {
	Source string
	ContentType string
	LastModified time.Time
	Retrieved time.Time
	ETag string
	Size int64
}

func serveFile(w http.ResponseWriter, filename string) error {
	metaFile := path.Join(cacheDir, filename + ".meta")
	metaData, err := os.ReadFile(metaFile)
	if err != nil {
		return err
	}

	var meta fileMeta
	err = json.Unmarshal(metaData, &meta)
	if err != nil {
		return err
	}

	w.Header().Set("Content-Type", meta.ContentType)
	w.Header().Set("Content-Length", strconv.FormatInt(meta.Size, 10))
	w.Header().Set("Last-Modified", meta.LastModified.Format(http.TimeFormat))
	w.Header().Set("ETag", meta.ETag)

	dataFile := path.Join(cacheDir, filename)
	file, err := os.Open(dataFile)
	if err != nil {
		return err
	}

	_, err = io.Copy(w, file)
	if err != nil {
		return err
	}

	return nil
}

func fetchFile(filename string) (err error) {
	url := path.Join(upstream, filename)
	metaFile := path.Join(cacheDir, filename + ".meta")
	cacheFile := path.Join(cacheDir, filename)

	defer func() {
		if err != nil {
			os.Remove(metaFile)
			os.Remove(cacheFile)
		}
	}()

	// Get file from source
	var resp *http.Response
	resp, err = http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// Add file to cache
	var file *os.File
	file, err = os.Create(cacheFile)
	if err != nil {
		return err
	}
	defer file.Close()

	_, err = io.Copy(file, resp.Body)
	if err != nil {
		return err
	}

	// Add metadata to cache
	modified := resp.Header.Get("Last-Modified")
	var lastModified time.Time
	lastModified, err = time.Parse(http.TimeFormat, modified)
	if err != nil {
		return err
	}

	meta := fileMeta{
		Source: url,
		ContentType: resp.Header.Get("Content-Type"),
		Retrieved: time.Now(),
		LastModified: lastModified,
		ETag: resp.Header.Get("ETag"),
		Size: resp.ContentLength,
	}

	var metaData []byte
	metaData, err = json.Marshal(meta)
	if err != nil {
		return err
	}

	metaFile = path.Join(cacheDir, filename + ".meta")
	err = os.WriteFile(metaFile, metaData, 0644)
	if err != nil {
		return err
	}

	return nil
}

func getRoot(w http.ResponseWriter, r *http.Request) {
	w.Write([]byte("MediaCache 1.0\nhttps://git.hajkey.org/hajkey/mediacache"))
}

func getHealthz(w http.ResponseWriter, r *http.Request) {
	w.Write([]byte("OK"))
}

func handleFiles(w http.ResponseWriter, r *http.Request) {
	// Get filename from URL
	filename := r.URL.Path

	if filename == "/" {
		getRoot(w, r)
		return
	}

	if stripPrefix != "" {
		if !strings.HasPrefix(filename, stripPrefix) {
			http.Error(w, "invalid path", http.StatusBadRequest)
			return
		}
		filename = strings.TrimPrefix(filename, stripPrefix)
	}

	// Check for invalid characters
	if strings.Contains(filename, "..") ||
		strings.Contains(filename, "~") ||
		strings.Contains(filename, "/") {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}

	// Acquire a read lock for the file
	mutex.Lock()
	lock, ok := locks[filename]
	if !ok {
		lock = &sync.Mutex{}
	}
	lock.Lock()
	locks[filename] = lock
	mutex.Unlock()

	// Release the lock when we're done
	defer func() {
		mutex.Lock()
		delete(locks, filename)
		mutex.Unlock()
		lock.Unlock()
	}()
	
	// Check if file exists in ./cache
	err := serveFile(w, filename)
	if err == nil {
		return
	}

	// File does not exist in cache, fetch it
	err = fetchFile(filename)
	if err != nil {
		log.Printf("error fetching file: %v", err)
		http.Error(w, "error fetching file", http.StatusInternalServerError)
		return
	}

	// Serve the file
	err = serveFile(w, filename)
	if err != nil {
		log.Printf("error serving file: %v", err)
		http.Error(w, "error serving file", http.StatusInternalServerError)
		return
	}
}

func main() {
	http.HandleFunc("/", handleFiles)
	http.HandleFunc("/healthz", getHealthz)

	log.Fatal(http.ListenAndServe(":3333", nil))
}