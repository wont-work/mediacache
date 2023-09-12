package main

import (
	"log"
	"os"
	"strconv"
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
