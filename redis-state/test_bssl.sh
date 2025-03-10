#!/bin/bash
# Test script for BSSL Redis module

set -e  # Exit on error

echo "==== Building minimal BSSL module ===="
make minimal_bssl.so

echo "==== Starting Redis with minimal module ===="
redis-server --loadmodule ./minimal_bssl.so &
REDIS_PID=$!

# Give Redis time to start
sleep 1

echo "==== Testing minimal BSSL module ===="
RESULT=$(redis-cli BSSL.PING)
if [ "$RESULT" != "PONG" ]; then
    echo "Error: Expected PONG, got $RESULT"
    kill $REDIS_PID
    exit 1
fi
echo "Minimal module test successful"

# Stop Redis
kill $REDIS_PID
sleep 1

# If the minimal version works, build the full version
echo "==== Building full BSSL module ===="
make bssl.so

echo "==== Starting Redis with full module ===="
redis-server --loadmodule ./bssl.so &
REDIS_PID=$!

# Give Redis time to start
sleep 1

echo "==== Testing full BSSL module ===="
# Basic info test
RESULT=$(redis-cli BSSL.INFO test)
if [[ $RESULT == "ERR"* ]]; then
    echo "Error testing BSSL.INFO: $RESULT"
    kill $REDIS_PID
    exit 1
fi
echo "BSSL.INFO test passed"

# Test set operation
redis-cli BSSL.SET "0x1234" 100 '{"value":"test"}' > /dev/null
if [ $? -ne 0 ]; then
    echo "Error setting value"
    kill $REDIS_PID
    exit 1
fi
echo "BSSL.SET test passed"

# Test get operation
RESULT=$(redis-cli BSSL.GETSTATEATBLOCK "0x1234" 100)
if [ $? -ne 0 ] || [ -z "$RESULT" ]; then
    echo "Error getting value"
    kill $REDIS_PID
    exit 1
fi
echo "BSSL.GETSTATEATBLOCK test passed"

# Stop Redis
kill $REDIS_PID

echo "==== All tests passed ===="
