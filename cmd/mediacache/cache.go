package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"time"
)

type fileMeta struct {
	Source       string
	Status       int
	ContentType  string
	LastModified time.Time
	Retrieved    time.Time
	ETag         string
	Size         int64
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
