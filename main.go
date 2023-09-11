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
	listen = getEnv("CACHE_LISTEN", ":3333")
	cacheDir = getEnv("CACHE_DIR", "./cache")
	upstream = getEnv("CACHE_UPSTREAM", "https://example.com")
	prefix = getEnv("CACHE_PREFIX", "/")
	reply404 = getEnv("CACHE_REPLY_404", "")
	reply403 = getEnv("CACHE_REPLY_403", "")
	reply500 = getEnv("CACHE_REPLY_500", "")
	reply503 = getEnv("CACHE_REPLY_503", "")
	reply504 = getEnv("CACHE_REPLY_504", "")

	locks = make(map[string]*sync.Mutex)
	mutex = &sync.RWMutex{}
)

const (
	SOFTWARE = "MediaCache"
	VERSION = "v1.0"
)

func getEnv(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}

	return fallback
}

type fileMeta struct {
	Source string
	Status int
	ContentType string
	LastModified time.Time
	Retrieved time.Time
	ETag string
	Size int64
}

func sendPlain(w http.ResponseWriter, message string) {
	w.Header().Set("Content-Type", "text/plain")
	w.Header().Set("Content-Length", strconv.Itoa(len(message)))
	w.Write([]byte(message))
}

func serveFile(w http.ResponseWriter, filename string, eTags []string, ifModifiedSince time.Time, result string) error {
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

	dataFile := path.Join(cacheDir, filename)
	file, err := os.Open(dataFile)
	if err != nil {
		return err
	}

	if meta.Status != 200 {
		w.WriteHeader(meta.Status)
		w.Header().Set("X-Cache", SOFTWARE + " " + VERSION + "; " + result)

		switch {
		case meta.Status == 403 && reply403 != "":
			sendPlain(w, reply403)
			return nil
		case meta.Status == 404 && reply404 != "":
			sendPlain(w, reply404)
			return nil
		case meta.Status == 500 && reply500 != "":
			sendPlain(w, reply500)
			return nil
		case meta.Status == 503 && reply503 != "":
			sendPlain(w, reply503)
			return nil
		case meta.Status == 504 && reply504 != "":
			sendPlain(w, reply504)
			return nil
		}

		_, err = io.Copy(w, file)
		if err != nil {
			return err
		}

		return nil
	}

	// Check if file is modified since
	if !meta.LastModified.IsZero() && !ifModifiedSince.IsZero() && meta.LastModified.Before(ifModifiedSince) {
		w.WriteHeader(http.StatusNotModified)
		w.Header().Set("X-Cache", SOFTWARE + " " + VERSION + "; " + result)
		return nil
	}

	// Check if file has matching ETag
	for _, tag := range eTags {
		tag = strings.TrimSpace(tag)
		if tag == meta.ETag {
			w.WriteHeader(http.StatusNotModified)
			w.Header().Set("X-Cache", SOFTWARE + " " + VERSION + "; " + result)
			return nil
		}
	}

	w.Header().Set("Content-Type", meta.ContentType)
	w.Header().Set("Content-Length", strconv.FormatInt(meta.Size, 10))
	w.Header().Set("Last-Modified", meta.LastModified.Format(http.TimeFormat))
	w.Header().Set("Cache-Control", "max-age=31536000")
	w.Header().Set("Pragma", "cache")
	w.Header().Set("Expires", meta.Retrieved.AddDate(1, 0, 0).Format(http.TimeFormat))	
	w.Header().Set("ETag", meta.ETag)
	w.Header().Set("X-Cache", SOFTWARE + " " + VERSION + "; " + result)

	_, err = io.Copy(w, file)
	if err != nil {
		return err
	}
	return nil
}

func joinUrl(base, path string) string {
	return strings.TrimSuffix(base, "/") + "/" + strings.TrimPrefix(path, "/")
}

func fetchFile(filename string) (err error) {
	url := joinUrl(upstream, filename)
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
	if modified != "" {
		lastModified, err = time.Parse(http.TimeFormat, modified)
		if err != nil {
			return err
		}
	}

	meta := fileMeta{
		Status: resp.StatusCode,
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
	metaData = append(metaData, '\n')

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
	var err error

	// Get filename from URL
	filename := r.URL.Path

	if filename == "/" {
		getRoot(w, r)
		return
	}

	if prefix != "" {
		if !strings.HasPrefix(filename, prefix) {
			http.Error(w, "invalid path", http.StatusBadRequest)
			return
		}
		filename = strings.TrimPrefix(filename, prefix)
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

	// Check for If-Modified-Since header
	var ifModifiedSince time.Time
	if m := r.Header.Get("If-Modified-Since"); m != "" {
		ifModifiedSince, err = time.Parse(http.TimeFormat, m)
		if err != nil {
			log.Printf("error parsing If-Modified-Since header: %v", err)
			http.Error(w, "error parsing If-Modified-Since header", http.StatusBadRequest)
			return
		}
	}

	// Check for If-None-Match header
	match := r.Header.Get("If-None-Match")
	var eTags []string
	if match != "" {
		weak := strings.HasPrefix(match, "W/")
		if weak {
			match = strings.TrimPrefix(match, "W/")
		}
		for _, eTag := range strings.Split(match, ",") {
			eTag = strings.TrimSpace(eTag)
			if weak {
				eTag = "W/" + eTag
			}
			eTags = append(eTags, eTag)
		}
	}
	
	// Check if file exists in ./cache
	err = serveFile(w, filename, eTags, ifModifiedSince, "HIT")
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
	err = serveFile(w, filename, eTags, ifModifiedSince, "MISS")
	if err != nil {
		log.Printf("error serving file: %v", err)
		http.Error(w, "error serving file", http.StatusInternalServerError)
		return
	}
}

func main() {
	http.HandleFunc("/", handleFiles)
	http.HandleFunc("/healthz", getHealthz)

	log.Printf("listening on %s", listen)
	log.Printf("upstream: %s", upstream)
	log.Printf("cache dir: %s", cacheDir)
	log.Printf("prefix: %s", prefix)

	log.Fatal(http.ListenAndServe(listen, nil))
}