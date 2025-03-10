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

all: bssl.so

# Complete BSSL module
bssl.so: bssl.c redismodule.h
	$(CC) $(CFLAGS) $(SHOBJ_CFLAGS) -o $@ $< $(SHOBJ_LDFLAGS)

clean:
	$(RM) *.o *.so


.PHONY: all clean test