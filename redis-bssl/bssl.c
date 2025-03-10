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
 #include "redismodule.h"

 #define BSSL_ENCODING_VERSION 1
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
         next = current->forward[0];

         if (current->state_data) {
             RedisModule_Free(current->state_data);
         }

         RedisModule_Free(current->forward);
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
 BSSLNode* bsslInsert(BSSLSkipList *list, uint64_t block_number, uint8_t *state_data, uint32_t state_size) {
     if (!list) return NULL;

     // Create update array to store pointers that need updating
     BSSLNode *update[BSSL_MAX_LEVEL];
     BSSLNode *current = list->header;

     // Find position to insert in each level
     for (int i = list->level - 1; i >= 0; i--) {
         while (current->forward[i] && current->forward[i]->block_number < block_number) {
             current = current->forward[i];
         }
         update[i] = current;
     }

     // Check if block already exists
     if (current->forward[0] && current->forward[0]->block_number == block_number) {
         // Update existing node
         BSSLNode *node = current->forward[0];

         // Free old state data
         if (node->state_data) {
             RedisModule_Free(node->state_data);
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

     // Allocate forward pointers
     new_node->forward = RedisModule_Alloc(new_level * sizeof(BSSLNode*));
     if (!new_node->forward) {
         RedisModule_Free(new_node);
         return NULL;
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
     if (!list || !list->node_count) return NULL;

     BSSLNode *current = list->header;

     // Start from highest level and work down
     for (int i = list->level - 1; i >= 0; i--) {
         while (current->forward[i] && current->forward[i]->block_number <= block_number) {
             current = current->forward[i];
         }

         // If we found exact match, return immediately
         if (current != list->header && current->block_number == block_number) {
             return current;
         }
     }

     // We found the largest block <= target
     if (current != list->header) {
         return current;
     }

     // No block <= target exists
     return NULL;
 }

 /* Binary Encoding/Decoding Functions */

 // Encode a skip list to binary format
 size_t bsslEncodeSkipList(BSSLSkipList *list, uint8_t **output) {
     if (!list || !list->node_count) {
         *output = NULL;
         return 0;
     }

     // Calculate total size needed
     size_t total_size = 16;  // 16-byte header
     BSSLNode *current = list->header->forward[0];

     while (current) {
         // Each node has: block number (8), level (2), state size (4),
         // forward pointers (8 * level), state data (state_size)
         total_size += 14 + (current->level * 8) + current->state_size;
         current = current->forward[0];
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

     // Write nodes
     current = list->header->forward[0];
     while (current) {
         // Block number (8 bytes)
         *(uint64_t*)ptr = current->block_number;
         ptr += 8;

         // Level (2 bytes)
         *(uint16_t*)ptr = current->level;
         ptr += 2;

         // State size (4 bytes)
         *(uint32_t*)ptr = current->state_size;
         ptr += 4;

         // Forward pointers (8 bytes each)
         // Store as offsets from start of buffer
         for (int i = 0; i < current->level; i++) {
             if (current->forward[i]) {
                 // Calculate offset to target node
                 size_t offset = (uint8_t*)current->forward[i] - *output;
                 *(uint64_t*)ptr = offset;
             } else {
                 *(uint64_t*)ptr = 0;
             }
             ptr += 8;
         }

         // State data
         memcpy(ptr, current->state_data, current->state_size);
         ptr += current->state_size;

         current = current->forward[0];
     }

     return total_size;
 }

 // Decode a binary encoded skip list
 BSSLSkipList* bsslDecodeSkipList(uint8_t *data, size_t size) {
     if (!data || size < 16) return NULL;

     // Check magic
     if (memcmp(data, "BSSL", 4) != 0) return NULL;

     // Check version
     if (data[4] != BSSL_ENCODING_VERSION) return NULL;

     // Create new skip list
     BSSLSkipList *list = bsslCreateSkipList();
     if (!list) return NULL;

     // Parse header - skip level byte since we don't use it
     uint32_t node_count = *(uint32_t*)(data + 6);

     // Skip reserved bytes
     uint8_t *ptr = data + 16;

     // Parse nodes
     for (uint32_t i = 0; i < node_count; i++) {
         // Get block number
         uint64_t block_number = *(uint64_t*)ptr;
         ptr += 8;

         // Get level
         uint16_t level = *(uint16_t*)ptr;
         ptr += 2;

         // Get state size
         uint32_t state_size = *(uint32_t*)ptr;
         ptr += 4;

         // Skip forward pointers for now
         ptr += level * 8;

         // Get state data
         uint8_t *state_data = ptr;
         ptr += state_size;

         // Insert node
         bsslInsert(list, block_number, state_data, state_size);
     }

     return list;
 }

 /* Address Metadata Functions */

 // Create a new address metadata object
 BSSLAddressMetadata* bsslCreateAddressMetadata(uint64_t block_number, RedisModuleString *state) {
     BSSLAddressMetadata *metadata = RedisModule_Alloc(sizeof(BSSLAddressMetadata));
     if (!metadata) return NULL;

     metadata->first_block = block_number;
     metadata->last_block = block_number;
     metadata->state_count = 1;
     metadata->last_state = RedisModule_CreateStringFromString(NULL, state);

     return metadata;
 }

 // Free an address metadata object
 void bsslFreeAddressMetadata(BSSLAddressMetadata *metadata) {
     if (!metadata) return;

     if (metadata->last_state) {
         RedisModule_FreeString(NULL, metadata->last_state);
     }

     RedisModule_Free(metadata);
 }

 // Encode address metadata to string
 RedisModuleString* bsslEncodeAddressMetadata(RedisModuleCtx *ctx, BSSLAddressMetadata *metadata) {
     char buffer[1024];
     int len = snprintf(buffer, sizeof(buffer),
                      "%llu,%llu,%u",
                      (unsigned long long)metadata->first_block,
                      (unsigned long long)metadata->last_block,
                      metadata->state_count);

     RedisModuleString *metaString = RedisModule_CreateString(ctx, buffer, len);
     return metaString;
 }

 // Decode address metadata from string
 BSSLAddressMetadata* bsslDecodeAddressMetadata(RedisModuleCtx *ctx, RedisModuleString *str, RedisModuleString *last_state) {
     size_t len;
     const char *s = RedisModule_StringPtrLen(str, &len);

     BSSLAddressMetadata *metadata = RedisModule_Alloc(sizeof(BSSLAddressMetadata));
     if (!metadata) return NULL;

     // Parse fields from string
     if (sscanf(s, "%llu,%llu,%u",
                (unsigned long long*)&metadata->first_block,
                (unsigned long long*)&metadata->last_block,
                &metadata->state_count) != 3) {
         RedisModule_Free(metadata);
         return NULL;
     }

     if (last_state) {
         metadata->last_state = RedisModule_CreateStringFromString(ctx, last_state);
     } else {
         metadata->last_state = NULL;
     }

     return metadata;
 }

 /* Command Implementations */

 // BSSL.PING command - for basic testing
 int BSSLPing_RedisCommand(RedisModuleCtx *ctx, RedisModuleString **argv, int argc) {
     RedisModule_ReplyWithSimpleString(ctx, "PONG");
     return REDISMODULE_OK;
 }

 // BSSL.SET address block_num state
 int BSSLSet_RedisCommand(RedisModuleCtx *ctx, RedisModuleString **argv, int argc) {
     if (argc != 4) {
         RedisModule_WrongArity(ctx);
         return REDISMODULE_ERR;
     }

     RedisModule_AutoMemory(ctx);

     // Parse arguments
     RedisModuleString *address = argv[1];

     long long block_number;
     if (RedisModule_StringToLongLong(argv[2], &block_number) != REDISMODULE_OK) {
         RedisModule_ReplyWithError(ctx, "ERR invalid block number");
         return REDISMODULE_ERR;
     }

     RedisModuleString *state = argv[3];

     // Open or create address index
     RedisModuleKey *indexKey = RedisModule_OpenKey(ctx,
         RedisModule_CreateString(ctx, "bssl:address_index", 17),
         REDISMODULE_READ | REDISMODULE_WRITE);

     // Check if key exists and is a hash
     if (RedisModule_KeyType(indexKey) != REDISMODULE_KEYTYPE_EMPTY &&
         RedisModule_KeyType(indexKey) != REDISMODULE_KEYTYPE_HASH) {
         RedisModule_ReplyWithError(ctx, REDISMODULE_ERRORMSG_WRONGTYPE);
         return REDISMODULE_ERR;
     }

     // If empty, create hash
     if (RedisModule_KeyType(indexKey) == REDISMODULE_KEYTYPE_EMPTY) {
         RedisModule_DeleteKey(indexKey);
         RedisModule_CloseKey(indexKey);
         indexKey = RedisModule_OpenKey(ctx,
             RedisModule_CreateString(ctx, "bssl:address_index", 17),
             REDISMODULE_READ | REDISMODULE_WRITE);
         if (RedisModule_HashSet(indexKey, REDISMODULE_HASH_NONE, NULL, NULL, NULL) != REDISMODULE_OK) {
             RedisModule_ReplyWithError(ctx, "ERR could not create hash");
             return REDISMODULE_ERR;
         }
     }

     // Look up address in index
     RedisModuleString *metadataStr = NULL;
     RedisModuleString *last_state_str = NULL;

     RedisModule_HashGet(indexKey, REDISMODULE_HASH_CFIELDS,
         address, &metadataStr,
         RedisModule_CreateStringPrintf(ctx, "%s:last_state",
             RedisModule_StringPtrLen(address, NULL)), &last_state_str,
         NULL);

     BSSLAddressMetadata *metadata = NULL;

     if (metadataStr) {
         // Update existing metadata
         metadata = bsslDecodeAddressMetadata(ctx, metadataStr, last_state_str);

         // Update fields
         if ((uint64_t)block_number < metadata->first_block) {
             metadata->first_block = (uint64_t)block_number;
         }
         if ((uint64_t)block_number > metadata->last_block) {
             metadata->last_block = (uint64_t)block_number;
         }
         metadata->state_count++;

         // Free old last state and set new one
         if (metadata->last_state) {
             RedisModule_FreeString(ctx, metadata->last_state);
         }
         metadata->last_state = RedisModule_CreateStringFromString(ctx, state);
     } else {
         // Create new metadata
         metadata = bsslCreateAddressMetadata((uint64_t)block_number, state);
     }

     // Format skip list key
     RedisModuleString *skipListKey = RedisModule_CreateStringPrintf(ctx,
         "bssl:state_list:%s", RedisModule_StringPtrLen(address, NULL));

     // Open or create skip list
     RedisModuleKey *listKey = RedisModule_OpenKey(ctx, skipListKey,
         REDISMODULE_READ | REDISMODULE_WRITE);

     BSSLSkipList *skipList = NULL;

     if (RedisModule_KeyType(listKey) == REDISMODULE_KEYTYPE_STRING) {
         // Load existing skip list
         size_t dataSize;
         uint8_t *data = (uint8_t*)RedisModule_StringDMA(listKey, &dataSize, REDISMODULE_READ);

         skipList = bsslDecodeSkipList(data, dataSize);
     } else if (RedisModule_KeyType(listKey) == REDISMODULE_KEYTYPE_EMPTY) {
         // Create new skip list
         skipList = bsslCreateSkipList();
     } else {
         bsslFreeAddressMetadata(metadata);
         RedisModule_ReplyWithError(ctx, REDISMODULE_ERRORMSG_WRONGTYPE);
         return REDISMODULE_ERR;
     }

     if (!skipList) {
         bsslFreeAddressMetadata(metadata);
         RedisModule_ReplyWithError(ctx, "ERR could not create/load skip list");
         return REDISMODULE_ERR;
     }

     // Get state data
     size_t stateSize;
     const char *stateData = RedisModule_StringPtrLen(state, &stateSize);

     // Insert state into skip list
     BSSLNode *node = bsslInsert(skipList, (uint64_t)block_number, (uint8_t*)stateData, stateSize);

     if (!node) {
         bsslFreeSkipList(skipList);
         bsslFreeAddressMetadata(metadata);
         RedisModule_ReplyWithError(ctx, "ERR could not insert state");
         return REDISMODULE_ERR;
     }

     // Encode skip list and store
     uint8_t *encodedList;
     size_t encodedSize = bsslEncodeSkipList(skipList, &encodedList);

     if (encodedSize == 0) {
         bsslFreeSkipList(skipList);
         bsslFreeAddressMetadata(metadata);
         RedisModule_ReplyWithError(ctx, "ERR could not encode skip list");
         return REDISMODULE_ERR;
     }

     RedisModule_StringTruncate(listKey, encodedSize);
     uint8_t *listData = (uint8_t*)RedisModule_StringDMA(listKey, &encodedSize, REDISMODULE_WRITE);
     memcpy(listData, encodedList, encodedSize);
     RedisModule_Free(encodedList);

     // Update metadata in index
     RedisModuleString *newMetadataStr = bsslEncodeAddressMetadata(ctx, metadata);

     RedisModule_HashSet(indexKey, REDISMODULE_HASH_CFIELDS,
         address, newMetadataStr,
         RedisModule_CreateStringPrintf(ctx, "%s:last_state",
             RedisModule_StringPtrLen(address, NULL)), state,
         NULL);

     // Clean up
     bsslFreeSkipList(skipList);
     bsslFreeAddressMetadata(metadata);

     RedisModule_ReplyWithLongLong(ctx, 1);
     return REDISMODULE_OK;
 }

 // BSSL.GETSTATEATBLOCK address block_num
 int BSSLGetStateAtBlock_RedisCommand(RedisModuleCtx *ctx, RedisModuleString **argv, int argc) {
     if (argc != 3) {
         RedisModule_WrongArity(ctx);
         return REDISMODULE_ERR;
     }

     RedisModule_AutoMemory(ctx);

     // Parse arguments
     RedisModuleString *address = argv[1];

     long long block_number;
     if (RedisModule_StringToLongLong(argv[2], &block_number) != REDISMODULE_OK) {
         RedisModule_ReplyWithError(ctx, "ERR invalid block number");
         return REDISMODULE_ERR;
     }

     // Open address index
     RedisModuleKey *indexKey = RedisModule_OpenKey(ctx,
         RedisModule_CreateString(ctx, "bssl:address_index", 17),
         REDISMODULE_READ);

     // Check if key exists and is a hash
     if (RedisModule_KeyType(indexKey) == REDISMODULE_KEYTYPE_EMPTY) {
         RedisModule_ReplyWithNull(ctx);
         return REDISMODULE_OK;
     }

     if (RedisModule_KeyType(indexKey) != REDISMODULE_KEYTYPE_HASH) {
         RedisModule_ReplyWithError(ctx, REDISMODULE_ERRORMSG_WRONGTYPE);
         return REDISMODULE_ERR;
     }

     // Look up address in index
     RedisModuleString *metadataStr = NULL;
     RedisModuleString *last_state_str = NULL;

     RedisModule_HashGet(indexKey, REDISMODULE_HASH_CFIELDS,
         address, &metadataStr,
         RedisModule_CreateStringPrintf(ctx, "%s:last_state",
             RedisModule_StringPtrLen(address, NULL)), &last_state_str,
         NULL);

     // If address not found, return null
     if (!metadataStr) {
         RedisModule_ReplyWithNull(ctx);
         return REDISMODULE_OK;
     }

     // Parse metadata
     BSSLAddressMetadata *metadata = bsslDecodeAddressMetadata(ctx, metadataStr, last_state_str);

     if (!metadata) {
         RedisModule_ReplyWithError(ctx, "ERR could not parse metadata");
         return REDISMODULE_ERR;
     }

     // Fast path: if block is before first block, return null
     if ((uint64_t)block_number < metadata->first_block) {
         bsslFreeAddressMetadata(metadata);
         RedisModule_ReplyWithNull(ctx);
         return REDISMODULE_OK;
     }

     // Fast path: if block is after or equal to last block, return last state
     if ((uint64_t)block_number >= metadata->last_block) {
         RedisModule_ReplyWithString(ctx, metadata->last_state);
         bsslFreeAddressMetadata(metadata);
         return REDISMODULE_OK;
     }

     // Need to query skip list
     RedisModuleString *skipListKey = RedisModule_CreateStringPrintf(ctx,
         "bssl:state_list:%s", RedisModule_StringPtrLen(address, NULL));

     RedisModuleKey *listKey = RedisModule_OpenKey(ctx, skipListKey, REDISMODULE_READ);

     // If skip list doesn't exist, something is wrong
     if (RedisModule_KeyType(listKey) != REDISMODULE_KEYTYPE_STRING) {
         bsslFreeAddressMetadata(metadata);
         RedisModule_ReplyWithError(ctx, "ERR skip list not found");
         return REDISMODULE_ERR;
     }

     // Load skip list
     size_t dataSize;
     uint8_t *data = (uint8_t*)RedisModule_StringDMA(listKey, &dataSize, REDISMODULE_READ);

     BSSLSkipList *skipList = bsslDecodeSkipList(data, dataSize);

     if (!skipList) {
         bsslFreeAddressMetadata(metadata);
         RedisModule_ReplyWithError(ctx, "ERR could not decode skip list");
         return REDISMODULE_ERR;
     }

     // Find state at block
     BSSLNode *node = bsslFindLessThanOrEqual(skipList, (uint64_t)block_number);

     if (!node) {
         bsslFreeSkipList(skipList);
         bsslFreeAddressMetadata(metadata);
         RedisModule_ReplyWithNull(ctx);
         return REDISMODULE_OK;
     }

     // Return state
     RedisModule_ReplyWithStringBuffer(ctx, (char*)node->state_data, node->state_size);

     // Clean up
     bsslFreeSkipList(skipList);
     bsslFreeAddressMetadata(metadata);
     return REDISMODULE_OK;
 }

 // BSSL.INFO address
 int BSSLInfo_RedisCommand(RedisModuleCtx *ctx, RedisModuleString **argv, int argc) {
     if (argc != 2) {
         RedisModule_WrongArity(ctx);
         return REDISMODULE_ERR;
     }

     RedisModule_AutoMemory(ctx);

     // Parse arguments
     RedisModuleString *address = argv[1];

     // Open address index
     RedisModuleKey *indexKey = RedisModule_OpenKey(ctx,
         RedisModule_CreateString(ctx, "bssl:address_index", 17),
         REDISMODULE_READ);

     // Check if key exists and is a hash
     if (RedisModule_KeyType(indexKey) == REDISMODULE_KEYTYPE_EMPTY) {
         RedisModule_ReplyWithNull(ctx);
         return REDISMODULE_OK;
     }

     if (RedisModule_KeyType(indexKey) != REDISMODULE_KEYTYPE_HASH) {
         RedisModule_ReplyWithError(ctx, REDISMODULE_ERRORMSG_WRONGTYPE);
         return REDISMODULE_ERR;
     }

     // Look up address in index
     RedisModuleString *metadataStr = NULL;

     RedisModule_HashGet(indexKey, REDISMODULE_HASH_CFIELDS,
         address, &metadataStr,
         NULL);

     // If address not found, return null
     if (!metadataStr) {
         RedisModule_ReplyWithNull(ctx);
         return REDISMODULE_OK;
     }

     // Parse metadata
     size_t len;
     const char *s = RedisModule_StringPtrLen(metadataStr, &len);

     unsigned long long first_block, last_block;
     unsigned int state_count;

     if (sscanf(s, "%llu,%llu,%u", &first_block, &last_block, &state_count) != 3) {
         RedisModule_ReplyWithError(ctx, "ERR could not parse metadata");
         return REDISMODULE_ERR;
     }

     // Return info
     RedisModule_ReplyWithArray(ctx, 3);
     RedisModule_ReplyWithLongLong(ctx, first_block);
     RedisModule_ReplyWithLongLong(ctx, last_block);
     RedisModule_ReplyWithLongLong(ctx, state_count);
     return REDISMODULE_OK;
 }

 /* Module Entry Point */

 int RedisModule_OnLoad(RedisModuleCtx *ctx, RedisModuleString **argv, int argc) {
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

     return REDISMODULE_OK;
 }