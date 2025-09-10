package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"time"
	"crypto/sha256"
	"encoding/base64"
)

const (
	ErrCacheExpired = ErrorStr("cache expired")
)

var httpClient = &http.Client{
	Timeout: 60 * time.Second,
}

type ErrorStr string

func (e ErrorStr) Error() string {
	return string(e)
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

type rangeRequest struct {
	start  int64
	end    int64
	length int64
}

func parseRangeHeader(rangeHeader string, fileSize int64) (*rangeRequest, error) {
	if rangeHeader == "" {
		return nil, nil
	}

	// Remove "bytes=" prefix if present
	rangeStr := strings.TrimPrefix(rangeHeader, "bytes=")

	// Split into start and end
	parts := strings.Split(rangeStr, "-")
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid range format")
	}

	var start, end int64
	var err error

	// Parse start
	if parts[0] != "" {
		start, err = strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid start value")
		}
	}

	// Parse end
	if parts[1] != "" {
		end, err = strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid end value")
		}
	} else {
		end = fileSize - 1
	}

	// Validate range
	if start < 0 {
		start = fileSize + start
		if start < 0 {
			return nil, fmt.Errorf("invalid range: start position out of bounds")
		}
	}
	if end >= fileSize {
		end = fileSize - 1
	}
	if start > end {
		return nil, fmt.Errorf("invalid range: start > end")
	}

	return &rangeRequest{
		start:  start,
		end:    end,
		length: end - start + 1,
	}, nil
}

func hashUrl(url string) string {
	sha := sha256.New()
	sha.Write([]byte(url))
	encoded := base64.URLEncoding.EncodeToString(sha.Sum(nil))
	return strings.ReplaceAll(encoded, "=", "")
}

func checkExists(origFilename string) bool {
	filename := hashUrl(origFilename)
	metaFile := path.Join(cacheDir, filename+".meta")
	_, err := os.Stat(metaFile)
	if err != nil {
		return false
	}

	cacheFile := path.Join(cacheDir, filename)
	_, err = os.Stat(cacheFile)
	return err == nil
}

func fetchFile(origFilename string) (n int64, err error) {
	filename := hashUrl(origFilename)
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
	var url string
	for _, upstream := range upstreams {
		url = joinUrl(upstream, origFilename)
		resp, err = httpClient.Get(url)
		if err == nil && resp.StatusCode == 200 {
			break
		}
		log.Printf("url %s: %v", url, err)
	}

	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	// Add file to cache
	var file *os.File
	file, err = os.Create(cacheFile)
	if err != nil {
		log.Printf("error creating file: %v", err)
		return 0, err
	}
	defer file.Close()

	var bytes int64
	bytes, err = io.Copy(file, resp.Body)
	if err != nil {
		log.Printf("error writing file: %d, %v", bytes, err)
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

	err = os.WriteFile(metaFile, metaData, 0644)
	if err != nil {
		return bytes, err
	}

	return bytes, nil
}

func serveFile(w http.ResponseWriter, r *http.Request, filename string, eTags []string, ifModifiedSince time.Time, result string) (n int64, err error) {
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
	defer file.Close()

	var bytes int64

	if maxAge > 0 && time.Since(meta.Retrieved).Hours() > float64(maxAge) {
		// File is too old, fetch a new one
		_ = os.Remove(metaFile)
		_ = os.Remove(dataFile)
		return 0, ErrCacheExpired
	}

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

	// Handle range request
	rangeHeader := r.Header.Get("Range")
	rangeReq, err := parseRangeHeader(rangeHeader, meta.Size)
	if err != nil {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(fmt.Sprintf("Invalid range request: %v", err)))
		return 0, err
	}

	w.Header().Set("Content-Type", meta.ContentType)
	w.Header().Set("Last-Modified", meta.LastModified.Format(http.TimeFormat))
	w.Header().Set("Cache-Control", "max-age=31536000")
	w.Header().Set("Pragma", "cache")
	w.Header().Set("Expires", meta.Retrieved.AddDate(1, 0, 0).Format(http.TimeFormat))
	w.Header().Set("ETag", meta.ETag)
	w.Header().Set("X-Cache", SOFTWARE+" "+VERSION+"; "+result)

	if rangeReq != nil {
		// Seek to the start position
		_, err = file.Seek(rangeReq.start, 0)
		if err != nil {
			log.Printf("error seeking file: %v", err)
			return 0, err
		}

		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", rangeReq.start, rangeReq.end, meta.Size))
		w.Header().Set("Content-Length", strconv.FormatInt(rangeReq.length, 10))
		w.WriteHeader(http.StatusPartialContent)

		// Create a limited reader for the range
		reader := io.LimitReader(file, rangeReq.length)
		bytes, err = io.Copy(w, reader)
	} else {
		w.Header().Set("Content-Length", strconv.FormatInt(meta.Size, 10))
		bytes, err = io.Copy(w, file)
	}

	if err != nil {
		log.Printf("error copying file: %v", err)
		return bytes, err
	}

	return bytes, nil
}
