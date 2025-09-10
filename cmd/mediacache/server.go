package main

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func joinUrl(base, path string) string {
	return strings.TrimSuffix(base, "/") + "/" + strings.TrimPrefix(path, "/")
}

func sendPlain(w http.ResponseWriter, message string) int64 {
	w.Header().Set("Content-Type", "text/plain")
	w.Header().Set("Content-Length", strconv.Itoa(len(message)))
	bytes, _ := w.Write([]byte(message))
	return int64(bytes)
}

func getRoot(w http.ResponseWriter, r *http.Request) {
	w.Write([]byte(
		fmt.Sprintf(
			"%s %s\n%s\n",
			SOFTWARE, VERSION,
			GITHUB_URL,
		),
	))
}

func getHealthz(w http.ResponseWriter, r *http.Request) {
	w.Write([]byte("OK"))
}

func handleCache(w http.ResponseWriter, r *http.Request) {
	var err error

	// Get filename from URL
	path := r.URL.Path
	query := r.URL.RawQuery

	filename := path + "?" + query

	if filename == "/" {
		getRoot(w, r)
		return
	}

	/*if prefix != "" {
		if !strings.HasPrefix(filename, prefix) {
			log.Printf("error with request for `%s`, no match to prefix: %s", filename, prefix);
			http.Error(w, "invalid path", http.StatusBadRequest)
			stats.errors++
			return
		}
		filename = strings.TrimPrefix(filename, prefix)
	}*/

	// Check for invalid characters
	if strings.Contains(filename, "..") ||
		strings.Contains(filename, "~") {
		log.Printf("error with request for `%s`, contains invalid character", filename)
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
		n, err = serveFile(w, r, filename, eTags, ifModifiedSince, "HIT")

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
		n, err = fetchFile(r.URL.Path)
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
	n, err = serveFile(w, r, filename, eTags, ifModifiedSince, "MISS")

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

func serve() {
	http.HandleFunc("/", handleCache)
	http.HandleFunc("/healthz", getHealthz)

	log.Fatal(http.ListenAndServe(listen, nil))
}
