package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type lockable struct {
	Stats

	mu sync.RWMutex

	readers int
	writers int
	touched time.Time
}

func (l *lockable) RLock() {
	l.mu.RLock()
	l.readers++
	l.touched = time.Now()
}

func (l *lockable) RUnlock() {
	l.readers--
	l.mu.RUnlock()
}

func (l *lockable) Lock() {
	l.mu.Lock()
	l.writers++
	l.touched = time.Now()
}

func (l *lockable) Unlock() {
	l.writers--
	l.mu.Unlock()
}

var (
	listen   = getEnv("CACHE_LISTEN", ":3333")
	cacheDir = getEnv("CACHE_DIR", "./cache")
	upstream = getEnv("CACHE_UPSTREAM", "https://example.com")
	prefix   = getEnv("CACHE_PREFIX", "/")
	reply404 = getEnv("CACHE_REPLY_404", "")
	reply403 = getEnv("CACHE_REPLY_403", "")
	reply500 = getEnv("CACHE_REPLY_500", "")
	reply503 = getEnv("CACHE_REPLY_503", "")
	reply504 = getEnv("CACHE_REPLY_504", "")

	printStats = getEnv("CACHE_PRINT_STATS", true)

	maxCacheFiles = getEnv[int64]("CACHE_MAX_FILES", 10_000)
	maxCacheSize  = getEnv[int64]("CACHE_MAX_SIZE", 1_000_000_000)
	cacheClean    = getEnv("CACHE_CLEAN", true)
	dryRun        = getEnv("CACHE_DRY_RUN", false)

	locks = make(map[string]*lockable)
	mutex = &sync.RWMutex{}
)

const (
	SOFTWARE = "MediaCache"
	VERSION  = "v1.0"
)

func getEnv[T int64 | string | bool](key string, fallback T) (result T) {
	if value, ok := os.LookupEnv(key); ok {
		var err error

		switch any(result).(type) {
		case int64:
			var i int64
			i, err = strconv.ParseInt(value, 10, 64)
			if err != nil {
				log.Fatalf("invalid value for %s: %v", key, err)
			}
			result = any(i).(T)

		case bool:
			var b bool
			b, err = strconv.ParseBool(value)
			if err != nil {
				log.Fatalf("invalid value for %s: %v", key, err)
			}
			result = any(b).(T)

		case string:
			result = any(value).(T)
		}
		return result
	}

	return fallback
}

type fileMeta struct {
	Source       string
	Status       int
	ContentType  string
	LastModified time.Time
	Retrieved    time.Time
	ETag         string
	Size         int64
}

func sendPlain(w http.ResponseWriter, message string) int64 {
	w.Header().Set("Content-Type", "text/plain")
	w.Header().Set("Content-Length", strconv.Itoa(len(message)))
	bytes, _ := w.Write([]byte(message))
	return int64(bytes)
}

type Stats struct {
	name          string
	requests      uint64
	completed     uint64
	disconnects   uint64
	sentBytes     uint64
	receivedBytes uint64

	hits      uint64
	hitBytes  uint64
	misses    uint64
	missBytes uint64
	errors    uint64
}

var stats Stats = Stats{name: "TOTALS"}

func (s *Stats) Report(extra ...string) {
	if !printStats {
		return
	}

	rate := fmt.Sprintf("%3.1f×", float64(s.hits)/float64(s.misses))
	if s.misses == 0 {
		rate = "∞"
	}

	sentMB := float64(s.sentBytes) / 1024 / 1024
	receivedMB := float64(s.receivedBytes) / 1024 / 1024
	transferRate := fmt.Sprintf("%3.01f×", sentMB/receivedMB)
	if receivedMB == 0 {
		transferRate = "∞"
	}

	log.Printf(
		"%s%s\n"+
			"req: %6d/%-6d  %3d dc  hit %6d:%-6d %-6s  err: %d\n"+
			"sent: %8.01fMB  recv: %8.01fMB %s",
		s.name,
		strings.Join(extra, ""),
		s.completed, s.requests, s.disconnects,
		s.hits, s.misses, rate,
		s.errors,
		float64(s.sentBytes)/1024/1024,
		float64(s.receivedBytes)/1024/1024,
		transferRate,
	)
}

func serveFile(w http.ResponseWriter, filename string, eTags []string, ifModifiedSince time.Time, result string) (n int64, err error) {
	metaFile := path.Join(cacheDir, filename+".meta")
	metaData, err := os.ReadFile(metaFile)
	if err != nil {
		return 0, err
	}

	var meta fileMeta
	err = json.Unmarshal(metaData, &meta)
	if err != nil {
		return 0, err
	}

	dataFile := path.Join(cacheDir, filename)
	file, err := os.Open(dataFile)
	if err != nil {
		return 0, err
	}

	var bytes int64

	if meta.Status != 200 {
		w.WriteHeader(meta.Status)
		w.Header().Set("X-Cache", SOFTWARE+" "+VERSION+"; "+result)

		switch {
		case meta.Status == 403 && reply403 != "":
			bytes = sendPlain(w, reply403)
			return bytes, nil
		case meta.Status == 404 && reply404 != "":
			bytes = sendPlain(w, reply404)
			return bytes, nil
		case meta.Status == 500 && reply500 != "":
			bytes = sendPlain(w, reply500)
			return bytes, nil
		case meta.Status == 503 && reply503 != "":
			bytes = sendPlain(w, reply503)
			return bytes, nil
		case meta.Status == 504 && reply504 != "":
			bytes = sendPlain(w, reply504)
			return bytes, nil
		}

		bytes, err = io.Copy(w, file)
		if err != nil {
			return bytes, err
		}

		return bytes, nil
	}

	// Check if file is modified since
	if !meta.LastModified.IsZero() && !ifModifiedSince.IsZero() && meta.LastModified.Before(ifModifiedSince) {
		w.WriteHeader(http.StatusNotModified)
		w.Header().Set("X-Cache", SOFTWARE+" "+VERSION+"; "+result)
		return 0, nil
	}

	// Check if file has matching ETag
	for _, tag := range eTags {
		tag = strings.TrimSpace(tag)
		if tag == meta.ETag {
			w.WriteHeader(http.StatusNotModified)
			w.Header().Set("X-Cache", SOFTWARE+" "+VERSION+"; "+result)
			return 0, nil
		}
	}

	w.Header().Set("Content-Type", meta.ContentType)
	w.Header().Set("Content-Length", strconv.FormatInt(meta.Size, 10))
	w.Header().Set("Last-Modified", meta.LastModified.Format(http.TimeFormat))
	w.Header().Set("Cache-Control", "max-age=31536000")
	w.Header().Set("Pragma", "cache")
	w.Header().Set("Expires", meta.Retrieved.AddDate(1, 0, 0).Format(http.TimeFormat))
	w.Header().Set("ETag", meta.ETag)
	w.Header().Set("X-Cache", SOFTWARE+" "+VERSION+"; "+result)

	currentTime := time.Now()
	_ = os.Chtimes(metaFile, currentTime, currentTime)

	bytes, err = io.Copy(w, file)
	if err != nil {
		return bytes, err
	}
	return bytes, nil
}

func joinUrl(base, path string) string {
	return strings.TrimSuffix(base, "/") + "/" + strings.TrimPrefix(path, "/")
}

func fetchFile(filename string) (n int64, err error) {
	url := joinUrl(upstream, filename)
	metaFile := path.Join(cacheDir, filename+".meta")
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
		return 0, err
	}
	defer resp.Body.Close()

	// Add file to cache
	var file *os.File
	file, err = os.Create(cacheFile)
	if err != nil {
		return 0, err
	}
	defer file.Close()

	var bytes int64
	bytes, err = io.Copy(file, resp.Body)
	if err != nil {
		return 0, err
	}

	// Add metadata to cache
	modified := resp.Header.Get("Last-Modified")
	var lastModified time.Time
	if modified != "" {
		lastModified, err = time.Parse(http.TimeFormat, modified)
		if err != nil {
			return bytes, err
		}
	}

	size := resp.ContentLength
	if size <= 0 {
		size = bytes
	}

	meta := fileMeta{
		Status:       resp.StatusCode,
		Source:       url,
		ContentType:  resp.Header.Get("Content-Type"),
		Retrieved:    time.Now(),
		LastModified: lastModified,
		ETag:         resp.Header.Get("ETag"),
		Size:         size,
	}

	var metaData []byte
	metaData, err = json.Marshal(meta)
	if err != nil {
		return bytes, err
	}
	metaData = append(metaData, '\n')

	metaFile = path.Join(cacheDir, filename+".meta")
	err = os.WriteFile(metaFile, metaData, 0644)
	if err != nil {
		return bytes, err
	}

	return bytes, nil
}

func checkExists(filename string) bool {
	metaFile := path.Join(cacheDir, filename+".meta")
	_, err := os.Stat(metaFile)
	if err != nil {
		return false
	}

	cacheFile := path.Join(cacheDir, filename)
	_, err = os.Stat(cacheFile)
	return err == nil
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
			stats.errors++
			return
		}
		filename = strings.TrimPrefix(filename, prefix)
	}

	// Check for invalid characters
	if strings.Contains(filename, "..") ||
		strings.Contains(filename, "~") ||
		strings.Contains(filename, "/") {
		http.Error(w, "invalid path", http.StatusBadRequest)
		stats.errors++
		return
	}

	// Acquire a read lock for the file
	mutex.RLock()
	lock, ok := locks[filename]
	if !ok {
		mutex.RUnlock()
		mutex.Lock()
		lock, ok = locks[filename]
		if !ok {
			lock = &lockable{}
			lock.name = filename
			locks[filename] = lock
		}
		mutex.Unlock()
	} else {
		mutex.RUnlock()
	}

	lock.RLock()
	rLocked := true
	defer func() {
		if rLocked {
			lock.RUnlock()
		}
		lock.completed++
		stats.completed++
	}()

	var n int64

	lock.requests++
	stats.requests++

	// Check for If-Modified-Since header
	var ifModifiedSince time.Time
	if m := r.Header.Get("If-Modified-Since"); m != "" {
		ifModifiedSince, err = time.Parse(http.TimeFormat, m)
		if err != nil {
			log.Printf("error parsing If-Modified-Since header: %v", err)
			http.Error(w, "error parsing If-Modified-Since header", http.StatusBadRequest)
			lock.errors++
			stats.errors++
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
	if checkExists(filename) {
		n, err = serveFile(w, filename, eTags, ifModifiedSince, "HIT")

		// Client disconnected, ignore
		disconnect := errors.Is(err, syscall.EPIPE)

		if err == nil || disconnect {
			lock.hits++
			stats.hits++
			lock.hitBytes += uint64(n)
			stats.hitBytes += uint64(n)
			lock.sentBytes += uint64(n)
			stats.sentBytes += uint64(n)
			return
		}
	}

	lock.RUnlock()
	rLocked = false
	lock.Lock()

	if !checkExists(filename) {
		// File does not exist in cache, fetch it
		n, err = fetchFile(filename)
		if err != nil {
			log.Printf("error fetching file: %v", err)
			http.Error(w, "error fetching file", http.StatusInternalServerError)
			lock.errors++
			stats.errors++
			lock.sentBytes += uint64(n)
			stats.sentBytes += uint64(n)
			lock.Unlock()
			return
		}
	}

	lock.Unlock()
	lock.RLock()
	rLocked = true

	// Serve the file
	n, err = serveFile(w, filename, eTags, ifModifiedSince, "MISS")

	// Client disconnected, ignore
	disconnect := errors.Is(err, syscall.EPIPE)

	if err != nil && !disconnect {
		log.Printf("error serving file: %v", err)
		http.Error(w, "error serving file", http.StatusInternalServerError)
		lock.errors++
		stats.errors++
		lock.sentBytes += uint64(n)
		stats.sentBytes += uint64(n)
		return
	}

	if disconnect {
		lock.disconnects++
		stats.disconnects++
		lock.sentBytes += uint64(n)
		stats.sentBytes += uint64(n)
	}
	lock.misses++
	stats.misses++
	lock.missBytes += uint64(n)
	stats.missBytes += uint64(n)
	lock.sentBytes += uint64(n)
	stats.sentBytes += uint64(n)
}

func main() {
	http.HandleFunc("/", handleFiles)
	http.HandleFunc("/healthz", getHealthz)

	log.Printf("listening on %s", listen)
	log.Printf("upstream: %s", upstream)
	log.Printf("cache dir: %s", cacheDir)
	log.Printf("prefix: %s", prefix)

	go func() {
		tock := time.NewTicker(60 * time.Second)
		c := 0
		for range tock.C {
			c++
			if c%10 == 0 && printStats {
				mutex.Lock()
				for filename, lock := range locks {
					extra := ""
					if lock.readers == 0 && lock.writers == 0 && time.Since(lock.touched) > 10*time.Minute {
						delete(locks, filename)
						extra = " (expired)"
					}
					lock.Report(extra)
				}
				mutex.Unlock()
			}

			if (c%60) == 0 && cacheClean {
				log.Print("cleaning cache")
				mutex.Lock()
				dir, err := os.ReadDir(cacheDir)
				if err != nil {
					log.Printf("error reading cache dir: %v", err)
				}

				type fileInfo struct {
					info  fs.FileInfo
					score float64
				}

				var totalSize int64
				var totalCount int64
				var fileList []fileInfo

				for _, entry := range dir {
					if strings.HasSuffix(entry.Name(), ".meta") {
						continue
					}

					entryName := entry.Name()
					info, err := entry.Info()
					if err != nil {
						log.Printf("error reading file info %s: %v", entryName, err)
						continue
					}

					size := float64(info.Size()) / 1024 / 1024
					age := time.Since(info.ModTime()).Hours()
					fileData := path.Join(cacheDir, entryName)
					fileMeta := path.Join(cacheDir, entryName+".meta")
					metaInfo, err := os.Stat(fileMeta)
					if err != nil {
						log.Printf("error reading file info %s: %v", metaInfo.Name(), err)
						if !dryRun {
							_ = os.Remove(fileData)
							_ = os.Remove(fileMeta)
						}
						continue
					}
					lastRead := time.Since(metaInfo.ModTime()).Hours()

					// big old file without recent reads score higher:
					score := size * age * lastRead

					totalSize += info.Size()
					fileList = append(fileList, fileInfo{
						info:  info,
						score: score,
					})
				}

				// Sort files by score
				sort.Slice(fileList, func(i, j int) bool {
					// Return lowest scores first
					return fileList[i].score < fileList[j].score
				})

				// Remove files once over our limits
				for _, file := range fileList {
					totalCount++
					totalSize += file.info.Size()

					if totalSize < maxCacheSize && totalCount < maxCacheFiles {
						continue
					}

					log.Printf("removing %s", file.info.Name())
					if !dryRun {
						_ = os.Remove(path.Join(cacheDir, file.info.Name()))
						_ = os.Remove(path.Join(cacheDir, file.info.Name()+".meta"))
					}
				}
			}

			stats.Report()
		}
	}()

	log.Fatal(http.ListenAndServe(listen, nil))
}
