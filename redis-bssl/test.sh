#!/bin/bash
# BSSL Debugging Script

REDIS_PASSWORD="jerry"
REDIS_CMD="redis-cli -a $REDIS_PASSWORD"

echo "=== BSSL Module Deep Debugging ==="

# 1. Make sure Redis is clean
echo "1. Cleaning Redis..."
$REDIS_CMD FLUSHALL > /dev/null
echo "   Done."

# 2. Check if module is responding
echo "2. Checking BSSL module..."
RESULT=$($REDIS_CMD BSSL.PING)
if [[ "$RESULT" == "PONG" ]]; then
  echo "   BSSL module is loaded and responsive."
else
  echo "   ERROR: BSSL module is not responding properly. Got: $RESULT"
  exit 1
fi

# 3. Test Redis hash operation specifically on bssl:address_index
echo "3. Testing hash operations on bssl:address_index..."
$REDIS_CMD HSET bssl:address_index test_key "test_value" > /dev/null
TEST_VALUE=$($REDIS_CMD HGET bssl:address_index test_key)
if [[ "$TEST_VALUE" == "test_value" ]]; then
  echo "   Hash operations on bssl:address_index work correctly."
else
  echo "   ERROR: Hash operations failed. Expected 'test_value', got: $TEST_VALUE"
  exit 1
fi

# 4. Test simple BSSL.SET operation with verbose output
echo "4. Testing BSSL.SET with minimal parameters..."
$REDIS_CMD BSSL.SET test_addr 100 '{"test":"value"}'
echo "   Checking if data was stored..."

# 5. Examine the address index structure after BSSL.SET
echo "5. Examining address index structure..."
echo "   Keys in bssl:address_index:"
$REDIS_CMD HKEYS bssl:address_index

# 6. Try to retrieve the state
echo "6. Retrieving state with BSSL.GETSTATEATBLOCK..."
STATE=$($REDIS_CMD BSSL.GETSTATEATBLOCK test_addr 100)
echo "   Retrieved state: $STATE"

# 7. Debug lastStateKey issue
echo "7. Looking for special lastStateKey pattern in hash..."
LAST_STATE_KEYS=$($REDIS_CMD HKEYS bssl:address_index | grep "last_state")
echo "   Last state keys found: $LAST_STATE_KEYS"

# 8. Inspect Redis memory usage
echo "8. Checking Redis memory usage..."
$REDIS_CMD INFO memory | grep "used_memory_human"

echo "=== Debug session completed ==="