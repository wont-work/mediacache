package main

import (
	"io/fs"
	"log"
	"os"
	"path"
	"sort"
	"strings"
	"time"
)

func cleanCache() {
	log.Print("cleaning cache")
	mutex.Lock()
	dir, err := os.ReadDir(cacheDir)
	if err != nil {
		log.Printf("error reading cache dir: %v", err)
	}

	type fileInfo struct {
		info  fs.FileInfo
		score float64
		age   float64
		size  float64
		used  float64
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
			log.Printf("error reading meta info %s: %v", metaInfo.Name(), err)
			if !dryRun {
				_ = os.Remove(fileData)
				_ = os.Remove(fileMeta)
			}
			continue
		}
		used := time.Since(metaInfo.ModTime()).Hours()

		// big old file without recent reads score higher:
		score := size * age * used

		totalSize += info.Size()
		fileList = append(fileList, fileInfo{
			info:  info,
			score: score,
			age:   age,
			size:  size,
			used:  used,
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

		if !dryRun {
			log.Printf(
				"removing %s\n"+
					"  age: %.01fh size: %.01fMb  used: %.01fh\n"+
					"(%d > %d files / %0.01f > %0.01fMb, score: %.03f)",
				file.info.Name(),
				file.age, file.size, file.used,
				totalCount, maxCacheFiles,
				float64(totalSize)/1024/1024,
				float64(maxCacheSize)/1024/1024,
				file.score,
			)
			_ = os.Remove(path.Join(cacheDir, file.info.Name()))
			_ = os.Remove(path.Join(cacheDir, file.info.Name()+".meta"))
		} else {
			log.Printf(
				"would remove (%d > %d files / %0.01f > %0.01fMb, score: %.03f) %s",
				totalCount, maxCacheFiles,
				float64(totalSize)/1024/1024,
				float64(maxCacheSize)/1024/1024,
				file.score,
				file.info.Name(),
			)
		}
	}
}

func reportStats() {
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

func maintain() {
	tock := time.NewTicker(60 * time.Second)
	c := 0
	for range tock.C {
		c++
		if c%10 == 0 && printStats {
			reportStats()
		}
		if (c%60) == 0 && cacheClean {
			cleanCache()
		}
		stats.Report()
	}
}
