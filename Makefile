BINARY := git-agent
OUT := bin/$(BINARY)
PREFIX ?= ~/.local
BINDIR ?= $(PREFIX)/bin

.PHONY: all build test install clean

all: build

build:
	go build -o $(OUT) -ldflags='-s -w' ./cmd/git-agent

test:
	go test ./...

install: build
	install -d $(DESTDIR)$(BINDIR)
	install -m 0755 $(OUT) $(DESTDIR)$(BINDIR)/$(BINARY)

clean:
	rm -f $(OUT)
