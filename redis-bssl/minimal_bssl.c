/*
 * Ultra-minimal BSSL module for Redis
 * Use this to verify your Redis module system works correctly
 */
#include "redismodule.h"

/* Simple PING command implementation */
int BSSLPing_RedisCommand(RedisModuleCtx *ctx, RedisModuleString **argv, int argc) {
    RedisModule_ReplyWithSimpleString(ctx, "PONG");
    return REDISMODULE_OK;
}

/* Module initialization */
int RedisModule_OnLoad(RedisModuleCtx *ctx, RedisModuleString **argv, int argc) {
    /* Initialize the module */
    if (RedisModule_Init(ctx, "bssl", 1, REDISMODULE_APIVER_1) == REDISMODULE_ERR)
        return REDISMODULE_ERR;

    /* Register the ping command */
    if (RedisModule_CreateCommand(ctx, "bssl.ping", BSSLPing_RedisCommand,
                               "readonly", 0, 0, 0) == REDISMODULE_ERR)
        return REDISMODULE_ERR;

    return REDISMODULE_OK;
}