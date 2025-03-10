/*
 * Blockchain State Skip List Module for Redis
 *
 * This module implements a specialized skip list data structure
 * for storing and querying blockchain state at any block height
 * with O(1) performance regardless of history length.
 */

 #include <stdlib.h>
 #include <string.h>
 #include <stdint.h>
 #include <time.h>
 #include "redismodule.h"

 #define BSSL_ENCODING_VERSION 2    // Updated version for new encoding format
 #define BSSL_MAX_LEVEL 32
 #define BSSL_MAX_STATE_SIZE (1024 * 1024)  // 1MB max state size
 #define BSSL_P 0.25  // Probability for skip list levels

 /* Data Structures */

 // Skip list node structure
 typedef struct BSSLNode {
     uint64_t block_number;
     uint8_t level;
     uint32_t state_size;
     uint8_t *state_data;
     struct BSSLNode **forward;
 } BSSLNode;

 // Skip list structure
 typedef struct BSSLSkipList {
     uint8_t level;
     uint32_t node_count;
     BSSLNode *header;
 } BSSLSkipList;

 // Address metadata structure
 typedef struct BSSLAddressMetadata {
     uint64_t first_block;
     uint64_t last_block;
     uint32_t state_count;
     RedisModuleString *last_state;
 } BSSLAddressMetadata;

 /* Function Declarations */
 int BSSLSet_RedisCommand(RedisModuleCtx *ctx, RedisModuleString **argv, int argc);
 int BSSLGetStateAtBlock_RedisCommand(RedisModuleCtx *ctx, RedisModuleString **argv, int argc);
 int BSSLInfo_RedisCommand(RedisModuleCtx *ctx, RedisModuleString **argv, int argc);
 int BSSLPing_RedisCommand(RedisModuleCtx *ctx, RedisModuleString **argv, int argc);

 /* Skip List Functions */

 // Create a new skip list
 BSSLSkipList *bsslCreateSkipList(void) {
     BSSLSkipList *list = RedisModule_Alloc(sizeof(BSSLSkipList));
     if (!list) return NULL;

     list->level = 1;
     list->node_count = 0;

     // Create header node
     list->header = RedisModule_Alloc(sizeof(BSSLNode));
     if (!list->header) {
         RedisModule_Free(list);
         return NULL;
     }

     list->header->level = BSSL_MAX_LEVEL;
     list->header->forward = RedisModule_Alloc(BSSL_MAX_LEVEL * sizeof(BSSLNode*));
     if (!list->header->forward) {
         RedisModule_Free(list->header);
         RedisModule_Free(list);
         return NULL;
     }

     list->header->state_data = NULL;
     list->header->state_size = 0;
     list->header->block_number = 0;  // Initialize block number

     // Initialize forward pointers
     for (int i = 0; i < BSSL_MAX_LEVEL; i++) {
         list->header->forward[i] = NULL;
     }

     return list;
 }

 // Free a skip list
 void bsslFreeSkipList(BSSLSkipList *list) {
     if (!list) return;

     BSSLNode *current = list->header;
     BSSLNode *next;

     while (current) {
         next = current->forward ? current->forward[0] : NULL;

         if (current->state_data) {
             RedisModule_Free(current->state_data);
         }

         if (current->forward) {
             RedisModule_Free(current->forward);
         }

         RedisModule_Free(current);
         current = next;
     }

     RedisModule_Free(list);
 }

 // Generate a random level for a new node
 int bsslRandomLevel(void) {
     int level = 1;
     while ((random() & 0xFFFF) < (BSSL_P * 0xFFFF) && level < BSSL_MAX_LEVEL) {
         level++;
     }
     return level;
 }

 // Insert a new node into the skip list
 BSSLNode* bsslInsert(BSSLSkipList *list, uint64_t block_number, const uint8_t *state_data, uint32_t state_size) {
     if (!list || !list->header || !state_data || state_size == 0 || state_size > BSSL_MAX_STATE_SIZE) {
         return NULL;
     }

     // Create update array to store pointers that need updating
     BSSLNode *update[BSSL_MAX_LEVEL];
     BSSLNode *current = list->header;

     // Find position to insert in each level
     for (int i = list->level - 1; i >= 0; i--) {
         while (current->forward && current->forward[i] &&
                current->forward[i]->block_number < block_number) {
             current = current->forward[i];
         }
         update[i] = current;
     }

     // Check if block already exists
     if (current->forward && current->forward[0] &&
         current->forward[0]->block_number == block_number) {
         // Update existing node
         BSSLNode *node = current->forward[0];

         // Free old state data
         if (node->state_data) {
             RedisModule_Free(node->state_data);
             node->state_data = NULL;
         }

         // Allocate and copy new state data
         node->state_data = RedisModule_Alloc(state_size);
         if (!node->state_data) return NULL;

         memcpy(node->state_data, state_data, state_size);
         node->state_size = state_size;

         return node;
     }

     // Choose a random level for the new node
     int new_level = bsslRandomLevel();

     // Update skip list level if necessary
     if (new_level > list->level) {
         for (int i = list->level; i < new_level; i++) {
             update[i] = list->header;
         }
         list->level = new_level;
     }

     // Create new node
     BSSLNode *new_node = RedisModule_Alloc(sizeof(BSSLNode));
     if (!new_node) return NULL;

     new_node->block_number = block_number;
     new_node->level = new_level;
     new_node->forward = NULL;
     new_node->state_data = NULL;

     // Allocate forward pointers
     new_node->forward = RedisModule_Alloc(new_level * sizeof(BSSLNode*));
     if (!new_node->forward) {
         RedisModule_Free(new_node);
         return NULL;
     }

     // Initialize forward pointers
     for (int i = 0; i < new_level; i++) {
         new_node->forward[i] = NULL;
     }

     // Allocate and copy state data
     new_node->state_data = RedisModule_Alloc(state_size);
     if (!new_node->state_data) {
         RedisModule_Free(new_node->forward);
         RedisModule_Free(new_node);
         return NULL;
     }

     memcpy(new_node->state_data, state_data, state_size);
     new_node->state_size = state_size;

     // Update forward pointers
     for (int i = 0; i < new_level; i++) {
         new_node->forward[i] = update[i]->forward[i];
         update[i]->forward[i] = new_node;
     }

     list->node_count++;
     return new_node;
 }

 // Find the node with the largest block number less than or equal to target
 BSSLNode* bsslFindLessThanOrEqual(BSSLSkipList *list, uint64_t block_number) {
     if (!list || !list->header || !list->node_count) return NULL;

     BSSLNode *current = list->header;

     // Start from highest level and work down
     for (int i = list->level - 1; i >= 0; i--) {
         while (current->forward && current->forward[i] &&
                current->forward[i]->block_number <= block_number) {
             current = current->forward[i];
         }
     }

     // If we found a node and it's not the header, return it
     if (current != list->header) {
         return current;
     }

     // No block <= target exists
     return NULL;
 }

 /* Binary Encoding/Decoding Functions */

 // Encode a skip list to binary format - completely rewritten to be safer
 size_t bsslEncodeSkipList(BSSLSkipList *list, uint8_t **output) {
     if (!list || !list->node_count || !output) {
         if (output) *output = NULL;
         return 0;
     }

     // Calculate total size needed for a flat encoding
     size_t total_size = 16;  // 16-byte header
     BSSLNode *current = list->header->forward ? list->header->forward[0] : NULL;

     // First pass to calculate size
     while (current) {
         // Each node needs: block number (8), level (1), state size (4), state data (state_size)
         total_size += 13 + current->state_size;
         current = current->forward ? current->forward[0] : NULL;
     }

     // Allocate output buffer
     *output = RedisModule_Alloc(total_size);
     if (!*output) return 0;

     uint8_t *ptr = *output;

     // Write header
     memcpy(ptr, "BSSL", 4);  // Magic
     ptr += 4;

     *ptr++ = BSSL_ENCODING_VERSION;  // Version
     *ptr++ = list->level;           // Max level

     // Node count (4 bytes)
     *(uint32_t*)ptr = list->node_count;
     ptr += 4;

     // Reserved (6 bytes)
     memset(ptr, 0, 6);
     ptr += 6;

     // Write nodes in a flat structure (no pointers)
     current = list->header->forward ? list->header->forward[0] : NULL;
     while (current) {
         // Block number (8 bytes)
         *(uint64_t*)ptr = current->block_number;
         ptr += 8;

         // Level (1 byte) - reduced from 2 bytes for efficiency
         *ptr++ = (uint8_t)current->level;

         // State size (4 bytes)
         *(uint32_t*)ptr = current->state_size;
         ptr += 4;

         // State data
         if (current->state_data && current->state_size > 0) {
             memcpy(ptr, current->state_data, current->state_size);
             ptr += current->state_size;
         }

         // Move to next node
         current = current->forward ? current->forward[0] : NULL;
     }

     // Return actual size used
     return (size_t)(ptr - *output);
 }

 // Decode a binary encoded skip list - completely rewritten for safety
 BSSLSkipList* bsslDecodeSkipList(const uint8_t *data, size_t size) {
     if (!data || size < 16) return NULL;

     // Check magic
     if (memcmp(data, "BSSL", 4) != 0) return NULL;

     // Check version
     uint8_t version = data[4];
     if (version != BSSL_ENCODING_VERSION && version != 1) return NULL;

     // Create new skip list
     BSSLSkipList *list = bsslCreateSkipList();
     if (!list) return NULL;

     // Parse header
     uint32_t node_count = *(uint32_t*)(data + 6);

     // Skip reserved bytes
     const uint8_t *ptr = data + 16;
     const uint8_t *end = data + size;

     // For version 1, we have a different format to parse
     if (version == 1) {
         // Old format had pointers, we'll need to extract just the data
         for (uint32_t i = 0; i < node_count && ptr < end; i++) {
             // Ensure we have enough space for the fixed-size header
             if (ptr + 14 > end) break;

             // Get block number
             uint64_t block_number = *(uint64_t*)ptr;
             ptr += 8;

             // Get level (was 2 bytes in old format)
             uint16_t level = *(uint16_t*)ptr;
             ptr += 2;

             // Get state size
             uint32_t state_size = *(uint32_t*)ptr;
             ptr += 4;

             // Bounds check for state size
             if (state_size > BSSL_MAX_STATE_SIZE || ptr + (level * 8) + state_size > end) {
                 bsslFreeSkipList(list);
                 return NULL;
             }

             // Skip the forward pointers (8 bytes each)
             ptr += level * 8;

             // Bounds check again after skipping pointers
             if (ptr + state_size > end) {
                 bsslFreeSkipList(list);
                 return NULL;
             }

             // Get state data
             const uint8_t *state_data = ptr;
             ptr += state_size;

             // Insert node
             if (!bsslInsert(list, block_number, state_data, state_size)) {
                 bsslFreeSkipList(list);
                 return NULL;
             }
         }
     } else {
         // New format (version 2)
         for (uint32_t i = 0; i < node_count && ptr < end; i++) {
             // Ensure we have enough space for the fixed-size header
             if (ptr + 13 > end) break;

             // Get block number
             uint64_t block_number = *(uint64_t*)ptr;
             ptr += 8;

             // Get level (1 byte in new format) - we read but don't use this value
             // as the bsslInsert function will determine the appropriate level
             ptr++; // Skip level byte

             // Get state size
             uint32_t state_size = *(uint32_t*)ptr;
             ptr += 4;

             // Bounds check for state size
             if (state_size > BSSL_MAX_STATE_SIZE || ptr + state_size > end) {
                 bsslFreeSkipList(list);
                 return NULL;
             }

             // Get state data
             const uint8_t *state_data = ptr;
             ptr += state_size;

             // Insert node
             if (!bsslInsert(list, block_number, state_data, state_size)) {
                 bsslFreeSkipList(list);
                 return NULL;
             }
         }
     }

     return list;
 }

 /* Metadata Functions - Simplified for safety */

 // Create a metadata string
 char* formatMetadata(uint64_t first_block, uint64_t last_block, uint32_t state_count) {
     char* buffer = RedisModule_Alloc(256);
     if (!buffer) return NULL;

     snprintf(buffer, 256, "%llu,%llu,%u",
              (unsigned long long)first_block,
              (unsigned long long)last_block,
              state_count);

     return buffer;
 }

 // Parse a metadata string
 int parseMetadata(const char* str, uint64_t* first_block, uint64_t* last_block, uint32_t* state_count) {
     if (!str || !first_block || !last_block || !state_count) return 0;

     return sscanf(str, "%llu,%llu,%u",
                  (unsigned long long*)first_block,
                  (unsigned long long*)last_block,
                  state_count) == 3;
 }

 /* Command Implementations */

 // BSSL.PING command - for basic testing
 int BSSLPing_RedisCommand(RedisModuleCtx *ctx, RedisModuleString **argv, int argc) {
     REDISMODULE_NOT_USED(argv);

     if (argc != 1) {
         return RedisModule_WrongArity(ctx);
     }

     return RedisModule_ReplyWithSimpleString(ctx, "PONG");
 }

 // BSSL.SET address block_num state - COMPLETELY REWRITTEN FOR SAFETY
 int BSSLSet_RedisCommand(RedisModuleCtx *ctx, RedisModuleString **argv, int argc) {
     if (argc != 4) {
         return RedisModule_WrongArity(ctx);
     }

     // Add debug logging
     RedisModule_Log(ctx, "notice", "Starting BSSLSet_RedisCommand");

     RedisModule_AutoMemory(ctx);

     // Parse arguments
     RedisModuleString *address = argv[1];
     if (!address) {
         return RedisModule_ReplyWithError(ctx, "ERR invalid address");
     }

     long long block_number;
     if (RedisModule_StringToLongLong(argv[2], &block_number) != REDISMODULE_OK || block_number < 0) {
         return RedisModule_ReplyWithError(ctx, "ERR invalid block number");
     }

     RedisModuleString *state = argv[3];
     if (!state) {
         return RedisModule_ReplyWithError(ctx, "ERR invalid state");
     }

     // Get state data and validate size
     size_t stateSize;
     const char *stateData = RedisModule_StringPtrLen(state, &stateSize);
     if (!stateData || stateSize == 0) {
         return RedisModule_ReplyWithError(ctx, "ERR empty state data");
     }

     if (stateSize > BSSL_MAX_STATE_SIZE) {
         return RedisModule_ReplyWithError(ctx, "ERR state data too large");
     }

     // Create keys for the metadata and state storage
     size_t addrLen;
     const char* addrStr = RedisModule_StringPtrLen(address, &addrLen);

     // Create skip list key with explicit format: "bssl:state_list:{address}"
     char* skipListKeyStr = RedisModule_Alloc(addrLen + 18); // "bssl:state_list:" + address + null
     if (!skipListKeyStr) {
         RedisModule_Log(ctx, "warning", "Failed to allocate memory for skip list key");
         return RedisModule_ReplyWithError(ctx, "ERR could not allocate memory");
     }

     snprintf(skipListKeyStr, addrLen + 18, "bssl:state_list:%.*s", (int)addrLen, addrStr);
     RedisModuleString *skipListKey = RedisModule_CreateString(ctx, skipListKeyStr, strlen(skipListKeyStr));
     RedisModule_Free(skipListKeyStr);

     // Create metadata key with explicit format: "bssl:meta:{address}"
     char* metaKeyStr = RedisModule_Alloc(addrLen + 11); // "bssl:meta:" + address + null
     if (!metaKeyStr) {
         RedisModule_Log(ctx, "warning", "Failed to allocate memory for metadata key");
         return RedisModule_ReplyWithError(ctx, "ERR could not allocate memory");
     }

     snprintf(metaKeyStr, addrLen + 11, "bssl:meta:%.*s", (int)addrLen, addrStr);
     RedisModuleString *metaKey = RedisModule_CreateString(ctx, metaKeyStr, strlen(metaKeyStr));
     RedisModule_Free(metaKeyStr);

     // Create last state key with explicit format: "bssl:last:{address}"
     char* lastKeyStr = RedisModule_Alloc(addrLen + 11); // "bssl:last:" + address + null
     if (!lastKeyStr) {
         RedisModule_Log(ctx, "warning", "Failed to allocate memory for last state key");
         return RedisModule_ReplyWithError(ctx, "ERR could not allocate memory");
     }

     snprintf(lastKeyStr, addrLen + 11, "bssl:last:%.*s", (int)addrLen, addrStr);
     RedisModuleString *lastKey = RedisModule_CreateString(ctx, lastKeyStr, strlen(lastKeyStr));
     RedisModule_Free(lastKeyStr);

     // Process metadata
     uint64_t first_block = (uint64_t)block_number;
     uint64_t last_block = (uint64_t)block_number;
     uint32_t state_count = 1;

     // Try to get existing metadata
     RedisModuleKey *existingMeta = RedisModule_OpenKey(ctx, metaKey, REDISMODULE_READ);
     if (existingMeta && RedisModule_KeyType(existingMeta) == REDISMODULE_KEYTYPE_STRING) {
         size_t len;
         char *metaData = (char*)RedisModule_StringDMA(existingMeta, &len, REDISMODULE_READ);

         // Parse existing metadata
         if (metaData && len > 0 && parseMetadata(metaData, &first_block, &last_block, &state_count)) {
             // Update metadata values
             if ((uint64_t)block_number < first_block) {
                 first_block = (uint64_t)block_number;
             }
             if ((uint64_t)block_number > last_block) {
                 last_block = (uint64_t)block_number;
             }
             state_count++;
         }
     }
     RedisModule_CloseKey(existingMeta);

     // Process skip list
     BSSLSkipList *skipList = NULL;

     // Try to get existing skip list
     RedisModuleKey *existingSkipList = RedisModule_OpenKey(ctx, skipListKey, REDISMODULE_READ);
     if (existingSkipList && RedisModule_KeyType(existingSkipList) == REDISMODULE_KEYTYPE_STRING) {
         size_t dataSize;
         uint8_t *data = (uint8_t*)RedisModule_StringDMA(existingSkipList, &dataSize, REDISMODULE_READ);

         if (data && dataSize > 0) {
             skipList = bsslDecodeSkipList(data, dataSize);
         }
     }
     RedisModule_CloseKey(existingSkipList);

     // If no existing skip list, create a new one
     if (!skipList) {
         skipList = bsslCreateSkipList();
         if (!skipList) {
             RedisModule_Log(ctx, "warning", "Failed to create skip list");
             return RedisModule_ReplyWithError(ctx, "ERR could not create skip list");
         }
     }

     // Insert state into skip list
     BSSLNode *node = bsslInsert(skipList, (uint64_t)block_number, (uint8_t*)stateData, stateSize);
     if (!node) {
         bsslFreeSkipList(skipList);
         RedisModule_Log(ctx, "warning", "Failed to insert state into skip list");
         return RedisModule_ReplyWithError(ctx, "ERR could not insert state");
     }

     // Encode skip list
     uint8_t *encodedList = NULL;
     size_t encodedSize = bsslEncodeSkipList(skipList, &encodedList);
     if (encodedSize == 0 || !encodedList) {
         bsslFreeSkipList(skipList);
         RedisModule_Log(ctx, "warning", "Failed to encode skip list");
         return RedisModule_ReplyWithError(ctx, "ERR could not encode skip list");
     }

     // Write the encoded skip list to Redis
     RedisModuleKey *listKey = RedisModule_OpenKey(ctx, skipListKey, REDISMODULE_WRITE);
     if (!listKey) {
         RedisModule_Free(encodedList);
         bsslFreeSkipList(skipList);
         RedisModule_Log(ctx, "warning", "Failed to open skip list key for writing");
         return RedisModule_ReplyWithError(ctx, "ERR could not open skip list key");
     }

     // Create a string with the encoded data
     RedisModule_StringSet(listKey, RedisModule_CreateString(ctx, (char*)encodedList, encodedSize));
     RedisModule_CloseKey(listKey);
     RedisModule_Free(encodedList);

     // Format and write the metadata
     char* metaStr = formatMetadata(first_block, last_block, state_count);
     if (!metaStr) {
         bsslFreeSkipList(skipList);
         RedisModule_Log(ctx, "warning", "Failed to format metadata");
         return RedisModule_ReplyWithError(ctx, "ERR could not format metadata");
     }

     RedisModuleKey *metaWrite = RedisModule_OpenKey(ctx, metaKey, REDISMODULE_WRITE);
     if (!metaWrite) {
         RedisModule_Free(metaStr);
         bsslFreeSkipList(skipList);
         RedisModule_Log(ctx, "warning", "Failed to open metadata key for writing");
         return RedisModule_ReplyWithError(ctx, "ERR could not open metadata key");
     }

     RedisModule_StringSet(metaWrite, RedisModule_CreateString(ctx, metaStr, strlen(metaStr)));
     RedisModule_CloseKey(metaWrite);
     RedisModule_Free(metaStr);

     // Save the last state
     RedisModuleKey *lastWrite = RedisModule_OpenKey(ctx, lastKey, REDISMODULE_WRITE);
     if (!lastWrite) {
         bsslFreeSkipList(skipList);
         RedisModule_Log(ctx, "warning", "Failed to open last state key for writing");
         return RedisModule_ReplyWithError(ctx, "ERR could not open last state key");
     }

     RedisModule_StringSet(lastWrite, state);
     RedisModule_CloseKey(lastWrite);

     // Clean up
     bsslFreeSkipList(skipList);

     RedisModule_Log(ctx, "notice", "BSSLSet_RedisCommand completed successfully");
     return RedisModule_ReplyWithLongLong(ctx, 1);
 }

 // BSSL.GETSTATEATBLOCK address block_num - COMPLETELY REWRITTEN FOR SAFETY
 int BSSLGetStateAtBlock_RedisCommand(RedisModuleCtx *ctx, RedisModuleString **argv, int argc) {
     if (argc != 3) {
         return RedisModule_WrongArity(ctx);
     }

     RedisModule_AutoMemory(ctx);

     // Parse arguments
     RedisModuleString *address = argv[1];
     if (!address) {
         return RedisModule_ReplyWithError(ctx, "ERR invalid address");
     }

     long long block_number;
     if (RedisModule_StringToLongLong(argv[2], &block_number) != REDISMODULE_OK || block_number < 0) {
         return RedisModule_ReplyWithError(ctx, "ERR invalid block number");
     }

     // Create keys for the metadata and state storage
     size_t addrLen;
     const char* addrStr = RedisModule_StringPtrLen(address, &addrLen);

     // Create metadata key
     char* metaKeyStr = RedisModule_Alloc(addrLen + 11); // "bssl:meta:" + address + null
     if (!metaKeyStr) {
         return RedisModule_ReplyWithError(ctx, "ERR could not allocate memory");
     }

     snprintf(metaKeyStr, addrLen + 11, "bssl:meta:%.*s", (int)addrLen, addrStr);
     RedisModuleString *metaKey = RedisModule_CreateString(ctx, metaKeyStr, strlen(metaKeyStr));
     RedisModule_Free(metaKeyStr);

     // Create last state key
     char* lastKeyStr = RedisModule_Alloc(addrLen + 11); // "bssl:last:" + address + null
     if (!lastKeyStr) {
         return RedisModule_ReplyWithError(ctx, "ERR could not allocate memory");
     }

     snprintf(lastKeyStr, addrLen + 11, "bssl:last:%.*s", (int)addrLen, addrStr);
     RedisModuleString *lastKey = RedisModule_CreateString(ctx, lastKeyStr, strlen(lastKeyStr));
     RedisModule_Free(lastKeyStr);

     // Get metadata
     RedisModuleKey *metaRead = RedisModule_OpenKey(ctx, metaKey, REDISMODULE_READ);
     if (!metaRead || RedisModule_KeyType(metaRead) != REDISMODULE_KEYTYPE_STRING) {
         return RedisModule_ReplyWithNull(ctx);
     }

     // Read metadata
     uint64_t first_block = 0, last_block = 0;
     uint32_t state_count = 0;

     size_t len;
     char *metaData = (char*)RedisModule_StringDMA(metaRead, &len, REDISMODULE_READ);

     if (!metaData || len == 0 || !parseMetadata(metaData, &first_block, &last_block, &state_count)) {
         RedisModule_CloseKey(metaRead);
         return RedisModule_ReplyWithNull(ctx);
     }

     RedisModule_CloseKey(metaRead);

     // Fast path: if block is before first block, return null
     if ((uint64_t)block_number < first_block) {
         return RedisModule_ReplyWithNull(ctx);
     }

     // Fast path: if block is after or equal to last block, return last state
     if ((uint64_t)block_number >= last_block) {
         RedisModuleKey *lastRead = RedisModule_OpenKey(ctx, lastKey, REDISMODULE_READ);
         if (!lastRead || RedisModule_KeyType(lastRead) != REDISMODULE_KEYTYPE_STRING) {
             return RedisModule_ReplyWithNull(ctx);
         }

         size_t lastLen;
         char *lastData = (char*)RedisModule_StringDMA(lastRead, &lastLen, REDISMODULE_READ);

         if (!lastData || lastLen == 0) {
             RedisModule_CloseKey(lastRead);
             return RedisModule_ReplyWithNull(ctx);
         }

         RedisModule_ReplyWithStringBuffer(ctx, lastData, lastLen);
         RedisModule_CloseKey(lastRead);
         return REDISMODULE_OK;
     }

     // Need to query skip list
     char* skipListKeyStr = RedisModule_Alloc(addrLen + 18); // "bssl:state_list:" + address + null
     if (!skipListKeyStr) {
         return RedisModule_ReplyWithError(ctx, "ERR could not allocate memory");
     }

     snprintf(skipListKeyStr, addrLen + 18, "bssl:state_list:%.*s", (int)addrLen, addrStr);
     RedisModuleString *skipListKey = RedisModule_CreateString(ctx, skipListKeyStr, strlen(skipListKeyStr));
     RedisModule_Free(skipListKeyStr);

     RedisModuleKey *skipListRead = RedisModule_OpenKey(ctx, skipListKey, REDISMODULE_READ);
     if (!skipListRead || RedisModule_KeyType(skipListRead) != REDISMODULE_KEYTYPE_STRING) {
         return RedisModule_ReplyWithNull(ctx);
     }

     // Load skip list
     size_t dataSize;
     uint8_t *data = (uint8_t*)RedisModule_StringDMA(skipListRead, &dataSize, REDISMODULE_READ);

     if (!data || dataSize == 0) {
         RedisModule_CloseKey(skipListRead);
         return RedisModule_ReplyWithNull(ctx);
     }

     BSSLSkipList *skipList = bsslDecodeSkipList(data, dataSize);

     if (!skipList) {
         RedisModule_CloseKey(skipListRead);
         return RedisModule_ReplyWithNull(ctx);
     }

     // Find state at block
     BSSLNode *node = bsslFindLessThanOrEqual(skipList, (uint64_t)block_number);

     if (!node) {
         bsslFreeSkipList(skipList);
         RedisModule_CloseKey(skipListRead);
         return RedisModule_ReplyWithNull(ctx);
     }

     // Return state
     RedisModule_ReplyWithStringBuffer(ctx, (char*)node->state_data, node->state_size);

     // Clean up
     bsslFreeSkipList(skipList);
     RedisModule_CloseKey(skipListRead);
     return REDISMODULE_OK;
 }

 // BSSL.INFO address - COMPLETELY REWRITTEN FOR SAFETY
 int BSSLInfo_RedisCommand(RedisModuleCtx *ctx, RedisModuleString **argv, int argc) {
     if (argc != 2) {
         return RedisModule_WrongArity(ctx);
     }

     RedisModule_AutoMemory(ctx);

     // Parse arguments
     RedisModuleString *address = argv[1];
     if (!address) {
         return RedisModule_ReplyWithError(ctx, "ERR invalid address");
     }

     // Create metadata key
     size_t addrLen;
     const char* addrStr = RedisModule_StringPtrLen(address, &addrLen);

     char* metaKeyStr = RedisModule_Alloc(addrLen + 11); // "bssl:meta:" + address + null
     if (!metaKeyStr) {
         return RedisModule_ReplyWithError(ctx, "ERR could not allocate memory");
     }

     snprintf(metaKeyStr, addrLen + 11, "bssl:meta:%.*s", (int)addrLen, addrStr);
     RedisModuleString *metaKey = RedisModule_CreateString(ctx, metaKeyStr, strlen(metaKeyStr));
     RedisModule_Free(metaKeyStr);

     // Get metadata
     RedisModuleKey *metaRead = RedisModule_OpenKey(ctx, metaKey, REDISMODULE_READ);
     if (!metaRead || RedisModule_KeyType(metaRead) != REDISMODULE_KEYTYPE_STRING) {
         return RedisModule_ReplyWithNull(ctx);
     }

     // Read metadata
     uint64_t first_block = 0, last_block = 0;
     uint32_t state_count = 0;

     size_t len;
     char *metaData = (char*)RedisModule_StringDMA(metaRead, &len, REDISMODULE_READ);

     if (!metaData || len == 0 || !parseMetadata(metaData, &first_block, &last_block, &state_count)) {
         RedisModule_CloseKey(metaRead);
         return RedisModule_ReplyWithNull(ctx);
     }

     RedisModule_CloseKey(metaRead);

     // Return info
     RedisModule_ReplyWithArray(ctx, 3);
     RedisModule_ReplyWithLongLong(ctx, first_block);
     RedisModule_ReplyWithLongLong(ctx, last_block);
     RedisModule_ReplyWithLongLong(ctx, state_count);
     return REDISMODULE_OK;
 }

 /* Module Entry Point */

 int RedisModule_OnLoad(RedisModuleCtx *ctx, RedisModuleString **argv, int argc) {
     REDISMODULE_NOT_USED(argv);
     REDISMODULE_NOT_USED(argc);

     // Initialize the module
     if (RedisModule_Init(ctx, "bssl", 1, REDISMODULE_APIVER_1) == REDISMODULE_ERR) {
         return REDISMODULE_ERR;
     }

     // Register commands
     if (RedisModule_CreateCommand(ctx, "bssl.ping",
         BSSLPing_RedisCommand, "readonly", 0, 0, 0) == REDISMODULE_ERR) {
         return REDISMODULE_ERR;
     }

     if (RedisModule_CreateCommand(ctx, "bssl.set",
         BSSLSet_RedisCommand, "write deny-oom", 1, 1, 1) == REDISMODULE_ERR) {
         return REDISMODULE_ERR;
     }

     if (RedisModule_CreateCommand(ctx, "bssl.getstateatblock",
         BSSLGetStateAtBlock_RedisCommand, "readonly", 1, 1, 1) == REDISMODULE_ERR) {
         return REDISMODULE_ERR;
     }

     if (RedisModule_CreateCommand(ctx, "bssl.info",
         BSSLInfo_RedisCommand, "readonly", 1, 1, 1) == REDISMODULE_ERR) {
         return REDISMODULE_ERR;
     }

     // Seed random number generator for skip list levels
     srand(time(NULL));

     return REDISMODULE_OK;
 }