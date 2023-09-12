package main

import (
	"sync"
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
