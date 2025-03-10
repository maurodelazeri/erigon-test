# Makefile for BSSL Redis Module

# Find OS type
uname_S := $(shell sh -c 'uname -s 2>/dev/null || echo not')

# Default settings
CFLAGS=-Wall -fPIC -g -O2
CC=cc
RM=rm -f

# Platform specific settings
ifeq ($(uname_S),Linux)
	# Linux settings
	SHOBJ_CFLAGS=-fPIC -g
	SHOBJ_LDFLAGS=-shared
else
	# macOS settings (don't mix -bundle and -shared)
	SHOBJ_CFLAGS=-dynamic -fno-common -g
	SHOBJ_LDFLAGS=-bundle -undefined dynamic_lookup
endif

# Default target: build both minimal and full versions
all: minimal_bssl.so bssl.so

# Minimal module for testing
minimal_bssl.so: minimal_bssl.c redismodule.h
	$(CC) $(CFLAGS) $(SHOBJ_CFLAGS) -o $@ $< $(SHOBJ_LDFLAGS)

# Complete BSSL module
bssl.so: bssl.c redismodule.h
	$(CC) $(CFLAGS) $(SHOBJ_CFLAGS) -o $@ $< $(SHOBJ_LDFLAGS)

clean:
	$(RM) *.o *.so

test: minimal_bssl.so
	@echo "Testing minimal BSSL module..."
	@redis-server --test-memory 1 --loadmodule ./minimal_bssl.so || echo "Module loading failed!"
	@redis-cli ping
	@echo "Done."

.PHONY: all clean test