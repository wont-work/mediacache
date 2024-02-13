package main

import (
	"log"
	"strings"
	"sync"
)

const (
	SOFTWARE   = "MediaCache"
	VERSION    = "v1.0+kopper1"
	GITHUB_URL = "https://github.com/ShittyKopper/mediacache"
)

var (
	listen    = getEnv("CACHE_LISTEN", ":3333")
	cacheDir  = getEnv("CACHE_DIR", "./cache")
	upstreams = strings.Split(getEnv("CACHE_UPSTREAM", "https://example.com"), " ")
	prefix    = getEnv("CACHE_PREFIX", "/")
	reply404  = getEnv("CACHE_REPLY_404", "")
	reply403  = getEnv("CACHE_REPLY_403", "")
	reply500  = getEnv("CACHE_REPLY_500", "")
	reply503  = getEnv("CACHE_REPLY_503", "")
	reply504  = getEnv("CACHE_REPLY_504", "")

	printStats = getEnv("CACHE_PRINT_STATS", true)

	maxCacheFiles = getEnv[int64]("CACHE_MAX_FILES", 10_000)
	maxCacheSize  = float64(getEnv[int64]("CACHE_MAX_SIZE_MB", 1_000))
	maxAge        = float64(getEnv[int64]("CACHE_MAX_AGE_HOURS", 3))
	cacheClean    = getEnv("CACHE_CLEAN", true)
	dryRun        = getEnv("CACHE_DRY_RUN", false)

	locks = make(map[string]*lockable)
	mutex = &sync.RWMutex{}
)

func main() {
	log.Printf("listening on %s", listen)
	log.Printf("upstreams: %s", strings.Join(upstreams, ", "))
	log.Printf("cache dir: %s", cacheDir)
	log.Printf("prefix: %s", prefix)

	go maintain()
	serve()
}
