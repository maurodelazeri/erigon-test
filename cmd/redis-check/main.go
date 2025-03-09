// Copyright 2024 The Erigon Authors
// This file is part of Erigon.
//
// Erigon is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// Erigon is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with Erigon. If not, see <http://www.gnu.org/licenses/>.

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/erigontech/erigon-lib/log/v3"
	"github.com/redis/go-redis/v9"
)

var (
	redisURL      = flag.String("url", "redis://localhost:6379/0", "Redis connection URL")
	redisPassword = flag.String("password", "", "Redis password")
	checkKeys     = flag.Bool("check-keys", false, "Check for existing Erigon keys in Redis")
	writeTest     = flag.Bool("write-test", false, "Perform write test")
	verbose       = flag.Bool("verbose", false, "Show verbose output")
)

func main() {
	flag.Parse()

	fmt.Println("Erigon Redis Connection Check")
	fmt.Println("=============================")
	fmt.Printf("Connecting to Redis at: %s\n", *redisURL)

	// Set up logger
	logLevel := log.LvlInfo
	if *verbose {
		logLevel = log.LvlDebug
	}

	handler := log.LvlFilterHandler(logLevel, log.StdoutHandler)
	log.Root().SetHandler(handler)

	// Parse Redis URL
	opts, err := redis.ParseURL(*redisURL)
	if err != nil {
		fmt.Printf("Error parsing Redis URL: %v\n", err)
		os.Exit(1)
	}

	// Set password if provided
	if *redisPassword != "" {
		opts.Password = *redisPassword
		fmt.Println("Using provided password")
	}

	// Create Redis client
	client := redis.NewClient(opts)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Test connection
	fmt.Println("\nTesting connection...")
	pingStart := time.Now()
	pong, err := client.Ping(ctx).Result()
	pingDuration := time.Since(pingStart)

	if err != nil {
		fmt.Printf("Error connecting to Redis: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Connection successful! Response: %s (%.2fms)\n", pong, float64(pingDuration.Microseconds())/1000.0)

	// Get Redis info
	fmt.Println("\nRedis server info:")
	infoCmd := client.Info(ctx)
	if infoCmd.Err() == nil {
		infoStr := infoCmd.Val()
		info := make(map[string]string)

		// Parse Redis INFO output
		parseRedisInfo(infoStr, info)

		// Print selected info
		if version, ok := info["redis_version"]; ok {
			fmt.Printf("  Version:    %s\n", version)
		}
		if os, ok := info["os"]; ok {
			fmt.Printf("  OS:         %s\n", os)
		}
		if uptime, ok := info["uptime_in_days"]; ok {
			fmt.Printf("  Uptime:     %s days\n", uptime)
		}
		if memory, ok := info["used_memory_human"]; ok {
			fmt.Printf("  Memory:     %s\n", memory)
		}
		if clients, ok := info["connected_clients"]; ok {
			fmt.Printf("  Clients:    %s\n", clients)
		}
		if keyspace, ok := info["db"+fmt.Sprintf("%d", opts.DB)]; ok {
			fmt.Printf("  Keyspace:   %s\n", keyspace)
		}
	} else {
		fmt.Printf("  Could not get Redis info: %v\n", infoCmd.Err())
	}

	// Perform write test if requested
	if *writeTest {
		fmt.Println("\nPerforming write test...")
		writeKey := "erigon:redis-check:test"
		writeValue := fmt.Sprintf("Test value from Erigon Redis Check at %s", time.Now().Format(time.RFC3339))

		// Write
		writeStart := time.Now()
		err := client.Set(ctx, writeKey, writeValue, 1*time.Hour).Err()
		writeDuration := time.Since(writeStart)

		if err != nil {
			fmt.Printf("Error writing to Redis: %v\n", err)
		} else {
			fmt.Printf("Write successful (%.2fms)\n", float64(writeDuration.Microseconds())/1000.0)

			// Read back
			readStart := time.Now()
			readValue, err := client.Get(ctx, writeKey).Result()
			readDuration := time.Since(readStart)

			if err != nil {
				fmt.Printf("Error reading from Redis: %v\n", err)
			} else {
				fmt.Printf("Read successful (%.2fms)\n", float64(readDuration.Microseconds())/1000.0)
				fmt.Printf("Value match: %v\n", readValue == writeValue)
			}
		}
	}

	// Check for Erigon-specific keys if requested
	if *checkKeys {
		fmt.Println("\nChecking for Erigon keys in Redis...")
		checkErigonKeys(ctx, client)
	}

	fmt.Println("\nRedis check completed successfully")
}

func parseRedisInfo(infoStr string, info map[string]string) {
	// Split by lines
	lines := strings.Split(infoStr, "\r\n")
	for _, line := range lines {
		// Skip empty lines and comments
		if len(line) == 0 || line[0] == '#' {
			continue
		}

		// Split by colon for key:value
		parts := strings.SplitN(line, ":", 2)
		if len(parts) == 2 {
			info[parts[0]] = parts[1]
		}
	}
}

func checkErigonKeys(ctx context.Context, client *redis.Client) {
	// Key patterns to check
	patterns := []string{
		"account:*", // Account data
		"storage:*", // Storage slots
		"code:*",    // Contract code
		"block:*",   // Block data
		"tx:*",      // Transactions
		"receipt:*", // Receipts
		"log:*",     // Logs
		"topic:*",   // Topics
	}

	// Check each pattern
	for _, pattern := range patterns {
		// Use SCAN to avoid blocking the Redis server
		var cursor uint64
		var totalKeys int

		for {
			var keys []string
			var err error

			// Scan in batches of 100
			keys, cursor, err = client.Scan(ctx, cursor, pattern, 100).Result()
			if err != nil {
				fmt.Printf("  Error scanning for %s: %v\n", pattern, err)
				break
			}

			totalKeys += len(keys)

			// Sample some keys if verbose
			if *verbose && len(keys) > 0 {
				fmt.Printf("  Sample keys for %s:\n", pattern)
				for i, key := range keys {
					if i >= 3 {
						break // Just show a few examples
					}
					fmt.Printf("    - %s\n", key)
				}
			}

			// Exit when we've scanned all keys
			if cursor == 0 {
				break
			}
		}

		fmt.Printf("  Found %d keys matching %s\n", totalKeys, pattern)
	}
}
