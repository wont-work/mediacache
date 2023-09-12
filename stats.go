package main

import (
	"fmt"
	"log"
	"strings"
)

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

func (s *Stats) Hit(bytes int64) {
	s.hits++
	s.hitBytes += uint64(bytes)

	stats.hits++
	stats.sentBytes += uint64(bytes)
}

func (s *Stats) Miss(bytes int64) {
	s.misses++
	s.missBytes += uint64(bytes)

	stats.misses++
	stats.sentBytes += uint64(bytes)
}

func (s *Stats) Error(bytes int64) {
	s.errors++
	s.sentBytes += uint64(bytes)

	stats.errors++
	stats.sentBytes += uint64(bytes)
}

func (s *Stats) Disconnect(bytes int64) {
	s.disconnects++
	s.sentBytes += uint64(bytes)

	stats.disconnects++
	stats.sentBytes += uint64(bytes)
}

func (s *Stats) Wrote(bytes int64) {
	s.receivedBytes += uint64(bytes)
	stats.receivedBytes += uint64(bytes)
}

func (s *Stats) Completed() {
	s.completed++
	stats.completed++
}

func (s *Stats) Requested() {
	s.requests++
	stats.requests++
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
